package hap

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
)

// PairingStore persists HAP controller pairing data to a JSON file.
type PairingStore struct {
	mu       sync.RWMutex
	path     string
	pairings map[string]*PairingData
}

type PairingData struct {
	DeviceID       string `json:"device_id"`
	DeviceLTPK     []byte `json:"device_ltpk"`      // Accessory's Ed25519 public key
	ControllerLTSK []byte `json:"controller_ltsk"`   // Our Ed25519 private key
	ControllerLTPK []byte `json:"controller_ltpk"`   // Our Ed25519 public key
	ControllerID   string `json:"controller_id"`     // Our pairing identifier
	IPAddress      string `json:"ip_address"`        // Last known IP
	Port           int    `json:"port"`              // Last known port
}

func NewPairingStore(path string) (*PairingStore, error) {
	ps := &PairingStore{
		path:     path,
		pairings: make(map[string]*PairingData),
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return ps, nil
		}
		return nil, fmt.Errorf("read pairing store: %w", err)
	}

	if err := json.Unmarshal(data, &ps.pairings); err != nil {
		return nil, fmt.Errorf("parse pairing store: %w", err)
	}

	return ps, nil
}

func (ps *PairingStore) Get(deviceID string) *PairingData {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	return ps.pairings[deviceID]
}

func (ps *PairingStore) Put(data *PairingData) error {
	ps.mu.Lock()
	defer ps.mu.Unlock()

	ps.pairings[data.DeviceID] = data
	return ps.save()
}

func (ps *PairingStore) save() error {
	data, err := json.MarshalIndent(ps.pairings, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal pairing store: %w", err)
	}
	if err := os.WriteFile(ps.path, data, 0600); err != nil {
		return fmt.Errorf("write pairing store: %w", err)
	}
	return nil
}
