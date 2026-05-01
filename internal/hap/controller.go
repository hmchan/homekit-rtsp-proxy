package hap

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/hkontrol/hkontroller"
)

// HAP service and characteristic type UUIDs (short form).
const (
	// CameraRTPStreamManagement service
	ServiceCameraRTPStreamMgmt = "110"
	CharSetupEndpoints         = "118"
	CharSelectedRTPConfig      = "117"
	CharStreamingStatus        = "120"
	CharSupportedVideoConfig   = "114"
	CharSupportedAudioConfig   = "115"

	// MotionSensor service
	ServiceMotionSensor        = "85"
	CharMotionDetected         = "22"
)

// cameraCharIDs holds the discovered characteristic IIDs for a camera.
type cameraCharIDs struct {
	aid                   int
	setupEndpointsIID     int
	selectedConfigIID     int
	streamingStatusIID    int
	supportedAudioCfgIID  int
	motionDetectedAID     int
	motionDetectedIID     int
}

// Controller manages HAP connections to HomeKit cameras.
type Controller struct {
	store      *PairingStore
	logger     *slog.Logger
	bindAddr   string
	storePath  string // path to hkontroller file store

	mu              sync.Mutex
	devices         map[string]*hkontroller.Device
	verified        map[string]*VerifiedConn // our custom verified connections
	charIDs         map[string]*cameraCharIDs
	motionCallbacks map[string]func(bool) // stored for re-subscribe after reconnect
	motionCtx       context.Context       // context for motion subscriptions
	controller      *hkontroller.Controller
	cancelFunc      context.CancelFunc

	// recoveredCallback is fired after a successful auto-recovery (e.g. after
	// a camera reboot). main.go uses it to restart any active stream so the
	// new HAP session and fresh SRTP keys take effect.
	recoveredCallback func(deviceName string)
}

// NewController creates a new HAP controller.
func NewController(store *PairingStore, bindAddr string, logger *slog.Logger) *Controller {
	return &Controller{
		store:     store,
		logger:    logger,
		bindAddr:  bindAddr,
		storePath:       "./.hkontroller",
		devices:         make(map[string]*hkontroller.Device),
		verified:        make(map[string]*VerifiedConn),
		charIDs:         make(map[string]*cameraCharIDs),
		motionCallbacks: make(map[string]func(bool)),
	}
}

// Start begins mDNS discovery and connects to known devices.
func (c *Controller) Start(ctx context.Context) error {
	ctrl, err := hkontroller.NewController(
		hkontroller.NewFsStore(c.storePath),
		"homekit-rtsp-proxy",
	)
	if err != nil {
		return fmt.Errorf("create hkontroller: %w", err)
	}
	c.controller = ctrl

	// Load previously paired devices.
	if err := ctrl.LoadPairings(); err != nil {
		c.logger.Warn("failed to load pairings", "error", err)
	}

	// Start mDNS discovery.
	dctx, cancel := context.WithCancel(ctx)
	c.cancelFunc = cancel

	discoverCh, lostCh := ctrl.StartDiscoveryWithContext(dctx)
	go c.discoveryLoop(dctx, discoverCh, lostCh)

	return nil
}

// SetRecoveredCallback registers a function to be called after a successful
// auto-recovery (post-reconnect of a previously-lost device). main.go uses
// this to restart any active stream so the fresh HAP session/SRTP keys
// drive new packets without disturbing connected RTSP clients.
func (c *Controller) SetRecoveredCallback(cb func(deviceName string)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.recoveredCallback = cb
}

// discoveryLoop merges the discover/lost channels so we can track which
// devices are currently lost and trigger an auto-recovery the moment a
// paired device reappears (typical pattern for a camera reboot).
func (c *Controller) discoveryLoop(ctx context.Context, discoverCh, lostCh <-chan *hkontroller.Device) {
	lost := make(map[string]bool)
	for {
		select {
		case <-ctx.Done():
			return
		case device, ok := <-discoverCh:
			if !ok {
				discoverCh = nil
				if lostCh == nil {
					return
				}
				continue
			}
			wasLost := lost[device.Name]
			delete(lost, device.Name)
			c.mu.Lock()
			c.devices[device.Name] = device
			c.mu.Unlock()
			c.logger.Info("device discovered",
				"name", device.Name,
				"paired", device.IsPaired(),
				"wasLost", wasLost)
			if wasLost && device.IsPaired() {
				go c.recoverDevice(ctx, device.Name)
			}
		case device, ok := <-lostCh:
			if !ok {
				lostCh = nil
				if discoverCh == nil {
					return
				}
				continue
			}
			c.logger.Warn("device lost", "name", device.Name)
			lost[device.Name] = true
		}
	}
}

// recoverDevice re-establishes the HAP session for a device that was
// rediscovered after being lost (most commonly after a camera reboot).
// It retries with backoff to handle the camera still booting, then fires
// the registered recovered callback so any in-flight stream can be
// restarted with new SRTP keys.
func (c *Controller) recoverDevice(ctx context.Context, deviceName string) {
	// Long enough to span a typical camera boot (~30–60s) without being
	// excessive. After exhaustion we wait for the next mDNS lost/rediscover
	// cycle to trigger another attempt.
	backoff := []time.Duration{0, 5 * time.Second, 15 * time.Second, 30 * time.Second, 60 * time.Second}
	for i, wait := range backoff {
		if wait > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
		}
		c.logger.Info("auto-recovery attempt", "name", deviceName, "attempt", i+1)
		if err := c.reconnect(deviceName); err != nil {
			c.logger.Warn("auto-recovery attempt failed",
				"name", deviceName, "attempt", i+1, "error", err)
			continue
		}
		c.logger.Info("auto-recovery succeeded", "name", deviceName)
		c.mu.Lock()
		cb := c.recoveredCallback
		c.mu.Unlock()
		if cb != nil {
			cb(deviceName)
		}
		return
	}
	c.logger.Error("auto-recovery exhausted; waiting for next mDNS event",
		"name", deviceName)
}

// PairCamera pairs with a camera using its setup code, then performs pair-verify
// using our custom implementation (which correctly omits the Method TLV).
func (c *Controller) PairCamera(ctx context.Context, deviceName, setupCode string) error {
	c.mu.Lock()
	device, ok := c.devices[deviceName]
	if !ok {
		// mDNS names may contain backslash-escaped spaces; try matching.
		for name, dev := range c.devices {
			cleanName := strings.ReplaceAll(name, "\\", "")
			if cleanName == deviceName {
				device = dev
				ok = true
				break
			}
		}
	}
	c.mu.Unlock()

	if !ok {
		// Try looking up via controller.
		device = c.controller.GetDevice(deviceName)
		if device == nil {
			// Try all discovered devices.
			for _, d := range c.controller.GetAllDevices() {
				cleanName := strings.ReplaceAll(d.Name, "\\", "")
				if cleanName == deviceName || d.Name == deviceName {
					device = d
					break
				}
			}
		}
		if device == nil {
			return fmt.Errorf("device %q not found via mDNS", deviceName)
		}
	}

	// Step 1: Pair-setup via hkontroller (this part works fine).
	if !device.IsPaired() {
		c.logger.Info("performing pair-setup", "name", deviceName)
		if err := device.PairSetup(setupCode); err != nil {
			return fmt.Errorf("pair setup: %w", err)
		}
		c.logger.Info("pair-setup complete", "name", deviceName)
		// Close the pair-setup connection.
		device.Close()
	}

	// Step 2: Get the keys we need for pair-verify.
	// Read controller keypair from hkontroller store.
	controllerID, controllerLTSK, controllerLTPK, err := c.readControllerKeys()
	if err != nil {
		return fmt.Errorf("read controller keys: %w", err)
	}

	// Read accessory pairing info.
	pairingInfo := device.GetPairingInfo()
	if len(pairingInfo.PublicKey) == 0 {
		return fmt.Errorf("no accessory public key for %q (not paired?)", deviceName)
	}

	c.logger.Info("accessory pairing info",
		"name", deviceName,
		"id", pairingInfo.Id,
		"pubkey_len", len(pairingInfo.PublicKey))

	// Step 3: Get the device's IP:port from mDNS.
	entry := device.GetDnssdEntry()
	if len(entry.IPs) == 0 {
		return fmt.Errorf("no IPs known for %q", deviceName)
	}

	// Prefer IPv4.
	var deviceAddr string
	for _, ip := range entry.IPs {
		if ip.To4() != nil {
			deviceAddr = fmt.Sprintf("%s:%d", ip.String(), entry.Port)
			break
		}
	}
	if deviceAddr == "" {
		deviceAddr = fmt.Sprintf("[%s]:%d", entry.IPs[0].String(), entry.Port)
	}

	// Step 4: Perform pair-verify using our custom implementation.
	c.logger.Info("performing pair-verify", "name", deviceName, "addr", deviceAddr)

	var vc *VerifiedConn
	var verifyErr error
	for attempt := 1; attempt <= 3; attempt++ {
		vc, verifyErr = DoPairVerify(deviceAddr, controllerID, controllerLTSK, controllerLTPK, pairingInfo.PublicKey)
		if verifyErr == nil {
			break
		}
		c.logger.Warn("pair-verify attempt failed", "name", deviceName, "attempt", attempt, "error", verifyErr)
		time.Sleep(time.Duration(attempt) * time.Second)
	}
	if verifyErr != nil {
		return fmt.Errorf("pair verify: %w", verifyErr)
	}

	// Step 5: Discover accessory database to find characteristic IIDs.
	c.logger.Info("fetching accessory database", "name", deviceName)
	accessories, err := vc.Client.GetAccessories()
	if err != nil {
		vc.Client.Close()
		return fmt.Errorf("get accessories: %w", err)
	}

	ids, err := findCameraCharIDs(accessories)
	if err != nil {
		vc.Client.Close()
		return fmt.Errorf("find camera characteristics: %w", err)
	}

	c.logger.Info("camera characteristics found",
		"name", deviceName,
		"aid", ids.aid,
		"setupEndpoints_iid", ids.setupEndpointsIID,
		"selectedConfig_iid", ids.selectedConfigIID,
		"hasMotion", ids.motionDetectedIID != 0)

	// Read SupportedAudioStreamConfiguration to discover supported codecs.
	if ids.supportedAudioCfgIID != 0 {
		audioResp, err := vc.Client.GetCharacteristics(
			fmt.Sprintf("%d.%d", ids.aid, ids.supportedAudioCfgIID),
		)
		if err == nil && len(audioResp.Characteristics) > 0 {
			c.logger.Info("supported audio config (raw base64)",
				"name", deviceName, "value", audioResp.Characteristics[0].Value)
		}
	}

	c.mu.Lock()
	c.verified[deviceName] = vc
	c.charIDs[deviceName] = ids
	c.mu.Unlock()

	c.logger.Info("camera paired and verified", "name", deviceName)
	return nil
}

// readControllerKeys reads the controller's Ed25519 keypair from the hkontroller store.
func (c *Controller) readControllerKeys() (controllerID string, ltsk ed25519.PrivateKey, ltpk ed25519.PublicKey, err error) {
	data, err := os.ReadFile(c.storePath + "/keypair")
	if err != nil {
		return "", nil, nil, fmt.Errorf("read keypair: %w", err)
	}

	var kp struct {
		Public  []byte `json:"Public"`
		Private []byte `json:"Private"`
	}
	if err := json.Unmarshal(data, &kp); err != nil {
		return "", nil, nil, fmt.Errorf("parse keypair: %w", err)
	}

	// hkontroller uses the controller name (passed to NewController) as the controllerId
	// during pair-setup M5. We must use the same ID for pair-verify.
	controllerID = "homekit-rtsp-proxy"

	return controllerID, ed25519.PrivateKey(kp.Private), ed25519.PublicKey(kp.Public), nil
}

// reconnect re-establishes the HAP pair-verify session for a camera whose
// TCP connection has dropped (broken pipe, EOF, etc.). It discovers the
// camera's current address via mDNS, performs pair-verify, and re-fetches
// the accessory database to update characteristic IIDs.
func (c *Controller) reconnect(deviceName string) error {
	c.logger.Info("reconnecting to camera", "name", deviceName)

	controllerID, controllerLTSK, controllerLTPK, err := c.readControllerKeys()
	if err != nil {
		return fmt.Errorf("read controller keys: %w", err)
	}

	c.mu.Lock()
	device, ok := c.devices[deviceName]
	c.mu.Unlock()
	if !ok {
		device = c.controller.GetDevice(deviceName)
		if device == nil {
			return fmt.Errorf("device %q not found via mDNS", deviceName)
		}
	}

	pairingInfo := device.GetPairingInfo()
	if len(pairingInfo.PublicKey) == 0 {
		return fmt.Errorf("no accessory public key for %q", deviceName)
	}

	entry := device.GetDnssdEntry()
	if len(entry.IPs) == 0 {
		return fmt.Errorf("no IPs known for %q", deviceName)
	}

	var deviceAddr string
	for _, ip := range entry.IPs {
		if ip.To4() != nil {
			deviceAddr = fmt.Sprintf("%s:%d", ip.String(), entry.Port)
			break
		}
	}
	if deviceAddr == "" {
		deviceAddr = fmt.Sprintf("[%s]:%d", entry.IPs[0].String(), entry.Port)
	}

	c.logger.Info("performing pair-verify (reconnect)", "name", deviceName, "addr", deviceAddr)

	var vc *VerifiedConn
	var verifyErr error
	for attempt := 1; attempt <= 3; attempt++ {
		vc, verifyErr = DoPairVerify(deviceAddr, controllerID, controllerLTSK, controllerLTPK, pairingInfo.PublicKey)
		if verifyErr == nil {
			break
		}
		c.logger.Warn("pair-verify attempt failed (reconnect)", "name", deviceName, "attempt", attempt, "error", verifyErr)
		time.Sleep(time.Duration(attempt) * time.Second)
	}
	if verifyErr != nil {
		return fmt.Errorf("pair verify: %w", verifyErr)
	}

	c.logger.Info("fetching accessory database (reconnect)", "name", deviceName)
	accessories, err := vc.Client.GetAccessories()
	if err != nil {
		vc.Client.Close()
		return fmt.Errorf("get accessories: %w", err)
	}

	ids, err := findCameraCharIDs(accessories)
	if err != nil {
		vc.Client.Close()
		return fmt.Errorf("find camera characteristics: %w", err)
	}

	c.mu.Lock()
	c.verified[deviceName] = vc
	c.charIDs[deviceName] = ids
	motionCb := c.motionCallbacks[deviceName]
	motionCtx := c.motionCtx
	c.mu.Unlock()

	// Re-subscribe to motion events on the new connection.
	if motionCb != nil && motionCtx != nil {
		if err := c.doSubscribeMotion(motionCtx, deviceName, motionCb); err != nil {
			c.logger.Warn("failed to re-subscribe motion after reconnect", "name", deviceName, "error", err)
		} else {
			c.logger.Info("motion sensor re-subscribed after reconnect", "name", deviceName)
		}
	}

	c.logger.Info("camera reconnected", "name", deviceName)
	return nil
}

// isConnError returns true if the error indicates a broken TCP connection
// (broken pipe, connection reset, EOF) that warrants a reconnect.
func isConnError(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "broken pipe") ||
		strings.Contains(s, "connection reset") ||
		strings.Contains(s, "EOF") ||
		strings.Contains(s, "use of closed network connection")
}

// StartStream initiates a camera stream and returns the stream response.
// If the HAP connection is broken, it automatically reconnects and retries once.
func (c *Controller) StartStream(ctx context.Context, deviceName string, localIP net.IP, videoPort, audioPort uint16, videoConfig VideoSelection, audioConfig AudioSelection) (*StreamResponse, error) {
	resp, err := c.doStartStream(ctx, deviceName, localIP, videoPort, audioPort, videoConfig, audioConfig)
	if err != nil && isConnError(err) {
		c.logger.Warn("stream start failed with connection error, reconnecting", "name", deviceName, "error", err)
		if reconErr := c.reconnect(deviceName); reconErr != nil {
			return nil, fmt.Errorf("reconnect failed: %w (original: %v)", reconErr, err)
		}
		return c.doStartStream(ctx, deviceName, localIP, videoPort, audioPort, videoConfig, audioConfig)
	}
	return resp, err
}

func (c *Controller) doStartStream(ctx context.Context, deviceName string, localIP net.IP, videoPort, audioPort uint16, videoConfig VideoSelection, audioConfig AudioSelection) (*StreamResponse, error) {
	c.mu.Lock()
	vc := c.verified[deviceName]
	ids := c.charIDs[deviceName]
	c.mu.Unlock()

	if vc == nil {
		return nil, fmt.Errorf("device %q not verified", deviceName)
	}
	if ids == nil {
		return nil, fmt.Errorf("device %q characteristics not discovered", deviceName)
	}

	client := vc.Client

	// Step 1: Generate stream endpoints with random SRTP keys.
	ep, err := GenerateStreamEndpoints(localIP, videoPort, audioPort)
	if err != nil {
		return nil, fmt.Errorf("generate endpoints: %w", err)
	}

	// Step 2: Write SetupEndpoints characteristic (TLV8 encoded, base64).
	setupTLV := EncodeSetupEndpoints(ep)
	setupB64 := base64.StdEncoding.EncodeToString(setupTLV)

	c.logger.Debug("writing SetupEndpoints", "name", deviceName,
		"aid", ids.aid, "iid", ids.setupEndpointsIID,
		"localIP", localIP, "videoPort", videoPort, "audioPort", audioPort)

	err = client.PutCharacteristics([]Characteristic{
		{AID: ids.aid, IID: ids.setupEndpointsIID, Value: setupB64},
	})
	if err != nil {
		return nil, fmt.Errorf("write SetupEndpoints: %w", err)
	}

	// Step 3: Read back SetupEndpoints to get camera's response.
	readResp, err := client.GetCharacteristics(
		fmt.Sprintf("%d.%d", ids.aid, ids.setupEndpointsIID),
	)
	if err != nil {
		return nil, fmt.Errorf("read SetupEndpoints response: %w", err)
	}

	if len(readResp.Characteristics) == 0 {
		return nil, fmt.Errorf("empty SetupEndpoints response")
	}

	respB64, ok := readResp.Characteristics[0].Value.(string)
	if !ok {
		return nil, fmt.Errorf("SetupEndpoints response not a string: %T", readResp.Characteristics[0].Value)
	}

	respTLV, err := base64.StdEncoding.DecodeString(respB64)
	if err != nil {
		return nil, fmt.Errorf("decode response base64: %w", err)
	}

	streamResp, err := DecodeStreamResponse(respTLV)
	if err != nil {
		return nil, fmt.Errorf("decode stream response: %w", err)
	}

	if streamResp.Status != 0 {
		return nil, fmt.Errorf("camera rejected stream setup (status=%d)", streamResp.Status)
	}

	c.logger.Info("stream setup accepted",
		"name", deviceName,
		"remoteIP", streamResp.RemoteIP,
		"videoPort", streamResp.RemoteVideoPort,
		"audioPort", streamResp.RemoteAudioPort,
		"videoSSRC", streamResp.VideoSSRC,
		"audioSSRC", streamResp.AudioSSRC)

	// Step 4: Set SSRCs and write SelectedRTPStreamConfiguration with start command.
	videoConfig.SSRC = randomUint32()
	audioConfig.SSRC = randomUint32()

	selectedTLV := EncodeSelectedStreamConfig(ep.SessionID, SessionCommandStart, videoConfig, audioConfig)
	selectedB64 := base64.StdEncoding.EncodeToString(selectedTLV)

	c.logger.Debug("SelectedRTPStreamConfiguration TLV8",
		"hex", fmt.Sprintf("%x", selectedTLV),
		"base64", selectedB64)

	err = client.PutCharacteristics([]Characteristic{
		{AID: ids.aid, IID: ids.selectedConfigIID, Value: selectedB64},
	})
	if err != nil {
		return nil, fmt.Errorf("write SelectedRTPStreamConfiguration: %w", err)
	}

	c.logger.Info("stream started", "name", deviceName)

	// Use the camera's SRTP keys for decryption. If the camera didn't return
	// keys (some cameras use the controller's keys for both directions),
	// fall back to the keys we sent.
	if len(streamResp.VideoSRTPKey) == 0 {
		c.logger.Debug("camera returned no video SRTP key, using ours")
		streamResp.VideoSRTPKey = ep.VideoSRTPKey[:]
		streamResp.VideoSRTPSalt = ep.VideoSRTPSalt[:]
	} else {
		c.logger.Debug("using camera's video SRTP key",
			"key_len", len(streamResp.VideoSRTPKey),
			"salt_len", len(streamResp.VideoSRTPSalt))
	}
	if len(streamResp.AudioSRTPKey) == 0 {
		c.logger.Debug("camera returned no audio SRTP key, using ours")
		streamResp.AudioSRTPKey = ep.AudioSRTPKey[:]
		streamResp.AudioSRTPSalt = ep.AudioSRTPSalt[:]
	} else {
		c.logger.Debug("using camera's audio SRTP key",
			"key_len", len(streamResp.AudioSRTPKey),
			"salt_len", len(streamResp.AudioSRTPSalt))
	}

	// Store controller's own keys and SSRCs for SRTCP encryption and audio return.
	// Outbound RTCP/RTP from controller must be encrypted with controller's keys.
	streamResp.ControllerVideoKey = ep.VideoSRTPKey[:]
	streamResp.ControllerVideoSalt = ep.VideoSRTPSalt[:]
	streamResp.ControllerVideoSSRC = videoConfig.SSRC
	streamResp.ControllerAudioKey = ep.AudioSRTPKey[:]
	streamResp.ControllerAudioSalt = ep.AudioSRTPSalt[:]
	streamResp.ControllerAudioSSRC = audioConfig.SSRC

	return streamResp, nil
}

// StopStream sends the end command to stop a camera stream.
func (c *Controller) StopStream(ctx context.Context, deviceName string, sessionID [16]byte) error {
	c.mu.Lock()
	vc := c.verified[deviceName]
	ids := c.charIDs[deviceName]
	c.mu.Unlock()

	if vc == nil {
		return fmt.Errorf("device %q not verified", deviceName)
	}
	if ids == nil {
		return fmt.Errorf("device %q characteristics not discovered", deviceName)
	}

	// Send end command via SelectedRTPStreamConfiguration.
	endTLV := TLV8Encode([]TLV8Item{
		{Type: TLVSelectedSessionControl, Value: TLV8Encode([]TLV8Item{
			{Type: TLVSessionControlID, Value: sessionID[:]},
			{Type: TLVSessionControlCommand, Value: []byte{SessionCommandEnd}},
		})},
	})
	endB64 := base64.StdEncoding.EncodeToString(endTLV)

	err := vc.Client.PutCharacteristics([]Characteristic{
		{AID: ids.aid, IID: ids.selectedConfigIID, Value: endB64},
	})
	if err != nil {
		return fmt.Errorf("write end command: %w", err)
	}

	c.logger.Info("stream stopped", "name", deviceName)
	return nil
}

// SubscribeMotionSensor subscribes to the MotionSensor.MotionDetected characteristic.
// The callback and context are stored so the subscription can be re-established
// after a HAP reconnection.
func (c *Controller) SubscribeMotionSensor(ctx context.Context, deviceName string, callback func(detected bool)) error {
	// Store for re-subscribe after reconnect.
	c.mu.Lock()
	c.motionCallbacks[deviceName] = callback
	c.motionCtx = ctx
	c.mu.Unlock()

	return c.doSubscribeMotion(ctx, deviceName, callback)
}

// doSubscribeMotion performs the actual motion subscription on the current
// verified connection. Called by SubscribeMotionSensor and by reconnect.
func (c *Controller) doSubscribeMotion(ctx context.Context, deviceName string, callback func(detected bool)) error {
	c.mu.Lock()
	vc := c.verified[deviceName]
	ids := c.charIDs[deviceName]
	c.mu.Unlock()

	if vc == nil {
		return fmt.Errorf("device %q not verified", deviceName)
	}
	if ids == nil || ids.motionDetectedIID == 0 {
		return fmt.Errorf("device %q has no motion sensor", deviceName)
	}

	// Subscribe to event notifications.
	if err := vc.Client.SubscribeCharacteristic(ids.motionDetectedAID, ids.motionDetectedIID); err != nil {
		return fmt.Errorf("subscribe motion: %w", err)
	}

	// Capture the current client so goroutines exit when this connection dies.
	client := vc.Client

	// Read events from the event channel in background.
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case event, ok := <-client.Events():
				if !ok {
					c.logger.Warn("motion event channel closed (will re-subscribe on reconnect)")
					return
				}
				c.logger.Debug("HAP event received", "characteristics", fmt.Sprintf("%+v", event.Characteristics))
				for _, ch := range event.Characteristics {
					if ch.AID == ids.motionDetectedAID && ch.IID == ids.motionDetectedIID {
						switch v := ch.Value.(type) {
						case bool:
							callback(v)
						case float64:
							callback(v != 0)
						default:
							c.logger.Warn("unexpected motion value type", "type", fmt.Sprintf("%T", ch.Value), "value", ch.Value)
						}
					}
				}
			}
		}
	}()

	// Also poll motion characteristic periodically as fallback,
	// since some cameras don't reliably push EVENT notifications.
	// Stops when the client's Done channel closes (connection lost) or ctx is cancelled.
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		var lastDetected *bool
		for {
			select {
			case <-ctx.Done():
				return
			case <-client.Done():
				c.logger.Debug("motion poll loop exiting (connection closed)")
				return
			case <-ticker.C:
				resp, err := client.GetCharacteristics(
					fmt.Sprintf("%d.%d", ids.motionDetectedAID, ids.motionDetectedIID),
				)
				if err != nil {
					c.logger.Debug("poll motion failed", "error", err)
					if isConnError(err) {
						c.logger.Debug("motion poll loop exiting (connection error)")
						return
					}
					continue
				}
				if len(resp.Characteristics) == 0 {
					continue
				}
				var detected bool
				switch v := resp.Characteristics[0].Value.(type) {
				case bool:
					detected = v
				case float64:
					detected = v != 0
				default:
					continue
				}
				if lastDetected == nil || *lastDetected != detected {
					c.logger.Info("motion poll", "detected", detected)
					lastDetected = &detected
					callback(detected)
				}
			}
		}
	}()

	c.logger.Info("subscribed to motion sensor", "name", deviceName)
	return nil
}

// findCameraCharIDs discovers the characteristic IIDs for camera streaming and motion.
func findCameraCharIDs(accessories *AccessoriesResponse) (*cameraCharIDs, error) {
	ids := &cameraCharIDs{}

	for _, acc := range accessories.Accessories {
		for _, svc := range acc.Services {
			svcType := strings.TrimPrefix(svc.Type, "public.hap.service.")
			// Also handle short UUID form (e.g., "110" or full UUID).
			svcTypeShort := trimHAPUUID(svcType)

			if svcTypeShort == ServiceCameraRTPStreamMgmt && ids.setupEndpointsIID == 0 {
				// Use the first CameraRTPStreamManagement service (typically the higher-res one).
				ids.aid = acc.AID
				for _, ch := range svc.Characteristics {
					chType := trimHAPUUID(strings.TrimPrefix(ch.Type, "public.hap.characteristic."))
					switch chType {
					case CharSetupEndpoints:
						ids.setupEndpointsIID = ch.IID
					case CharSelectedRTPConfig:
						ids.selectedConfigIID = ch.IID
					case CharStreamingStatus:
						ids.streamingStatusIID = ch.IID
					case CharSupportedAudioConfig:
						ids.supportedAudioCfgIID = ch.IID
					}
				}
			}

			if svcTypeShort == ServiceMotionSensor {
				for _, ch := range svc.Characteristics {
					chType := trimHAPUUID(strings.TrimPrefix(ch.Type, "public.hap.characteristic."))
					if chType == CharMotionDetected {
						ids.motionDetectedAID = acc.AID
						ids.motionDetectedIID = ch.IID
					}
				}
			}
		}
	}

	if ids.setupEndpointsIID == 0 || ids.selectedConfigIID == 0 {
		return nil, fmt.Errorf("camera streaming characteristics not found in accessory database")
	}

	return ids, nil
}

// trimHAPUUID extracts the short form from a full HAP UUID like "00000110-0000-1000-8000-0026BB765291".
func trimHAPUUID(s string) string {
	// If it's already a short form, return as-is.
	if !strings.Contains(s, "-") {
		return s
	}
	// Full HAP UUID format: XXXXXXXX-0000-1000-8000-0026BB765291
	// Extract the first segment and strip leading zeros.
	parts := strings.SplitN(s, "-", 2)
	short := strings.TrimLeft(parts[0], "0")
	if short == "" {
		short = "0"
	}
	return short
}

// Stop disconnects all devices and stops discovery.
func (c *Controller) Stop() {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.cancelFunc != nil {
		c.cancelFunc()
	}
	if c.controller != nil {
		c.controller.StopDiscovery()
	}

	for _, vc := range c.verified {
		if vc.Client != nil {
			vc.Client.Close()
		} else {
			vc.Conn.Close()
		}
	}
	for _, dev := range c.devices {
		dev.Close()
	}

	c.logger.Info("HAP controller stopped")
}

func randomUint32() uint32 {
	b := make([]byte, 4)
	rand.Read(b)
	return binary.LittleEndian.Uint32(b)
}
