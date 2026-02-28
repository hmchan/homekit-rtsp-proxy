package main

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"time"

	"github.com/hkontrol/hkontroller"
	hapLib "github.com/hmchan/homekit-rtsp-proxy/internal/hap"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintf(os.Stderr, "Usage: %s <device-name> <setup-code>\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "  device-name: HomeKit device name (e.g., Camera-E1-XXXX)\n")
		fmt.Fprintf(os.Stderr, "  setup-code:  HomeKit setup code (e.g., 031-45-154)\n")
		os.Exit(1)
	}
	deviceName := os.Args[1]
	setupCode := os.Args[2]

	storePath := "./.hkontroller"

	ctrl, err := hkontroller.NewController(
		hkontroller.NewFsStore(storePath),
		"homekit-rtsp-proxy",
	)
	if err != nil {
		log.Fatal(err)
	}

	if err := ctrl.LoadPairings(); err != nil {
		log.Printf("LoadPairings: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	discoverCh, _ := ctrl.StartDiscoveryWithContext(ctx)

	log.Println("waiting for discovery...")

	timeout := time.After(30 * time.Second)
	var found bool
	for !found {
		select {
		case dev := <-discoverCh:
			log.Printf("discovered: %q (paired=%v)", dev.Name, dev.IsPaired())
			// mDNS names may contain escaped backslashes, strip them for comparison.
			cleanName := strings.ReplaceAll(dev.Name, "\\", "")
			if cleanName == deviceName || dev.Name == deviceName {
				found = true
			}
		case <-timeout:
			log.Fatal("device not discovered in time")
		}
	}

	// Try both the user-supplied name and the raw mDNS name.
	device := ctrl.GetDevice(deviceName)
	if device == nil {
		// Try all known devices for a match.
		for _, d := range ctrl.GetAllDevices() {
			cleanName := strings.ReplaceAll(d.Name, "\\", "")
			if cleanName == deviceName || d.Name == deviceName {
				device = d
				break
			}
		}
	}
	if device == nil {
		log.Fatal("device not found after discovery")
	}
	log.Printf("using device with raw name: %q", device.Name)

	log.Printf("device: paired=%v verified=%v", device.IsPaired(), device.IsVerified())

	// Pair-setup if needed.
	if !device.IsPaired() {
		log.Println("performing pair-setup...")
		if err := device.PairSetup(setupCode); err != nil {
			log.Fatalf("pair-setup: %v", err)
		}
		log.Println("pair-setup succeeded!")
		device.Close()
	}

	// Read controller keys from store.
	data, err := os.ReadFile(storePath + "/keypair")
	if err != nil {
		log.Fatalf("read keypair: %v", err)
	}
	var kp struct {
		Public  []byte `json:"Public"`
		Private []byte `json:"Private"`
	}
	if err := json.Unmarshal(data, &kp); err != nil {
		log.Fatalf("parse keypair: %v", err)
	}

	log.Printf("controller LTPK: %x (len=%d)", kp.Public[:8], len(kp.Public))
	log.Printf("controller LTSK len=%d", len(kp.Private))

	// Get accessory pairing info.
	pairingInfo := device.GetPairingInfo()
	log.Printf("accessory ID: %s", pairingInfo.Id)
	log.Printf("accessory LTPK: %x (len=%d)", pairingInfo.PublicKey[:8], len(pairingInfo.PublicKey))

	// Get device address from mDNS.
	entry := device.GetDnssdEntry()
	if len(entry.IPs) == 0 {
		log.Fatal("no IPs for device")
	}

	var deviceAddr string
	for _, ip := range entry.IPs {
		if ip.To4() != nil {
			deviceAddr = fmt.Sprintf("%s:%d", ip.String(), entry.Port)
			break
		}
	}
	if deviceAddr == "" {
		deviceAddr = fmt.Sprintf("[%s%%%s]:%d", entry.IPs[0].String(), entry.IfaceName, entry.Port)
	}
	log.Printf("device address: %s", deviceAddr)

	// Custom pair-verify (without Method TLV).
	log.Println("performing custom pair-verify...")
	vc, err := hapLib.DoPairVerify(
		deviceAddr,
		"homekit-rtsp-proxy",
		ed25519.PrivateKey(kp.Private),
		ed25519.PublicKey(kp.Public),
		ed25519.PublicKey(pairingInfo.PublicKey),
	)
	if err != nil {
		log.Fatalf("custom pair-verify FAILED: %v", err)
	}

	log.Println("PAIR-VERIFY SUCCEEDED!")
	log.Printf("connection: %s -> %s", vc.Conn.LocalAddr(), vc.Conn.RemoteAddr())
	log.Printf("read key:  %x", vc.ReadKey[:8])
	log.Printf("write key: %x", vc.WriteKey[:8])

	// Test encrypted HAP layer: fetch accessory database.
	log.Println("fetching accessory database over encrypted connection...")
	accessories, err := vc.Client.GetAccessories()
	if err != nil {
		log.Fatalf("GetAccessories FAILED: %v", err)
	}

	log.Printf("GET /accessories SUCCEEDED! Found %d accessories", len(accessories.Accessories))
	for _, acc := range accessories.Accessories {
		log.Printf("  Accessory AID=%d, %d services", acc.AID, len(acc.Services))
		for _, svc := range acc.Services {
			log.Printf("    Service IID=%d type=%s, %d characteristics", svc.IID, svc.Type, len(svc.Characteristics))
			for _, ch := range svc.Characteristics {
				log.Printf("      Char IID=%d type=%s value=%v", ch.IID, ch.Type, ch.Value)
			}
		}
	}

	_ = net.IP{}

	vc.Client.Close()
	ctrl.StopDiscovery()
}
