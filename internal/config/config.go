package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	LogLevel     string `yaml:"log_level"`
	PairingStore string `yaml:"pairing_store"`
	// BindAddress forces a single local IP for the camera-side SRTP return
	// path across all cameras. Empty (default) = auto-detect per camera from
	// the route to its mDNS-discovered IP, which is the right behavior when
	// cameras live on different interfaces or VLANs. Only set this if you
	// need to pin one specific source IP.
	BindAddress string `yaml:"bind_address"`
	// ListenAddress restricts the RTSP and ONVIF listeners to a specific
	// interface (e.g. "127.0.0.1" to expose them only to local consumers).
	// Empty = bind to all interfaces (default). Independent of BindAddress.
	ListenAddress string         `yaml:"listen_address"`
	Cameras       []CameraConfig `yaml:"cameras"`
	// MQTT enables reporting camera stream health to Home Assistant over MQTT.
	MQTT MQTTConfig `yaml:"mqtt"`
}

// MQTTConfig configures Home Assistant health reporting over MQTT. When
// enabled, the proxy publishes an HA-discovered binary_sensor (device_class
// "problem") per camera that turns on when the camera's stream stays
// unstartable (e.g. the SetupEndpoints HTTP 204 wedge), so an HA automation
// can alert a human to power-cycle the camera.
type MQTTConfig struct {
	Enabled         bool          `yaml:"enabled"`
	Broker          string        `yaml:"broker"` // e.g. tcp://127.0.0.1:1883
	Username        string        `yaml:"username"`
	Password        string        `yaml:"password"`
	ClientID        string        `yaml:"client_id"`
	BaseTopic       string        `yaml:"base_topic"`       // state/availability topic root
	DiscoveryPrefix string        `yaml:"discovery_prefix"` // HA discovery prefix
	WedgeAfter      time.Duration `yaml:"wedge_after"`      // sustained failure before flagging a problem
	// DeviceIdentifier attaches the sensors to an existing HA device by id (e.g.
	// Scrypted's camera device) instead of a dedicated one. Single-camera only.
	DeviceIdentifier string `yaml:"device_identifier"`
	// DeviceName must match that device's name exactly when DeviceIdentifier is
	// set (HA requires a name on shared-device entities), e.g. "Aqara Camera E1 (ONVIF)".
	DeviceName string `yaml:"device_name"`
	// Frame-drop reporting (see health.Config).
	DropThreshold   float64       `yaml:"drop_threshold"`    // interval packet-loss fraction counted as high loss (e.g. 0.05)
	DropWindow      time.Duration `yaml:"drop_window"`       // sustained high loss before flagging frame drops
	DropClearWindow time.Duration `yaml:"drop_clear_window"` // sustained low loss before clearing
}

type CameraConfig struct {
	Name      string      `yaml:"name"`
	SetupCode string      `yaml:"setup_code"`
	DeviceID  string      `yaml:"device_id"`
	RTSP      RTSPConfig  `yaml:"rtsp"`
	Video     VideoConfig `yaml:"video"`
	Audio     AudioConfig `yaml:"audio"`
	ONVIF     ONVIFConfig `yaml:"onvif"`
}

type RTSPConfig struct {
	Port        int           `yaml:"port"`
	Path        string        `yaml:"path"`
	IdleTimeout time.Duration `yaml:"idle_timeout"` // Keep camera streaming this long after the last RTSP client disconnects (0 = stop immediately).
}

type VideoConfig struct {
	Width      int `yaml:"width"`
	Height     int `yaml:"height"`
	FPS        int `yaml:"fps"`
	MaxBitrate int `yaml:"max_bitrate"`
}

type AudioConfig struct {
	Enabled    bool   `yaml:"enabled"`
	Codec      string `yaml:"codec"`
	SampleRate int    `yaml:"sample_rate"`
	Gain       *int   `yaml:"gain"` // PCM gain factor applied during AAC-ELD→AAC-LC transcoding (0 = mute, 512 = ~54dB)
}

type ONVIFConfig struct {
	Enabled bool `yaml:"enabled"`
	Port    int  `yaml:"port"`
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	cfg := &Config{
		LogLevel:     "info",
		PairingStore: "./pairings.json",
	}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	for i := range cfg.Cameras {
		applyDefaults(&cfg.Cameras[i])
	}

	if len(cfg.Cameras) == 0 {
		return nil, fmt.Errorf("no cameras configured")
	}

	applyMQTTDefaults(&cfg.MQTT)

	return cfg, nil
}

func applyMQTTDefaults(m *MQTTConfig) {
	if !m.Enabled {
		return
	}
	if m.Broker == "" {
		m.Broker = "tcp://127.0.0.1:1883"
	}
	if m.ClientID == "" {
		m.ClientID = "homekit-rtsp-proxy"
	}
	if m.BaseTopic == "" {
		m.BaseTopic = "homekit-rtsp-proxy"
	}
	if m.DiscoveryPrefix == "" {
		m.DiscoveryPrefix = "homeassistant"
	}
	if m.WedgeAfter <= 0 {
		m.WedgeAfter = 90 * time.Second
	}
	if m.DropThreshold <= 0 {
		m.DropThreshold = 0.05
	}
	if m.DropWindow <= 0 {
		m.DropWindow = 20 * time.Second
	}
	if m.DropClearWindow <= 0 {
		m.DropClearWindow = 30 * time.Second
	}
}

// normalizeSetupCode ensures the setup code is in XXX-XX-XXX format.
func normalizeSetupCode(code string) string {
	// Strip existing dashes and spaces.
	code = strings.ReplaceAll(code, "-", "")
	code = strings.ReplaceAll(code, " ", "")
	if len(code) == 8 {
		return code[:3] + "-" + code[3:5] + "-" + code[5:]
	}
	return code
}

func applyDefaults(c *CameraConfig) {
	c.SetupCode = normalizeSetupCode(c.SetupCode)
	if c.RTSP.Port == 0 {
		c.RTSP.Port = 8554
	}
	if c.RTSP.Path == "" {
		c.RTSP.Path = "/live"
	}
	if c.Video.Width == 0 {
		c.Video.Width = 1920
	}
	if c.Video.Height == 0 {
		c.Video.Height = 1080
	}
	if c.Video.FPS == 0 {
		c.Video.FPS = 30
	}
	if c.Video.MaxBitrate == 0 {
		c.Video.MaxBitrate = 2000
	}
	if c.Audio.Codec == "" {
		c.Audio.Codec = "aac-eld"
	}
	if c.Audio.SampleRate == 0 {
		c.Audio.SampleRate = 16000
	}
	if c.Audio.Gain == nil {
		c.Audio.Gain = intPtr(512)
	}
	if c.ONVIF.Port == 0 {
		c.ONVIF.Port = 8580
	}
}

func intPtr(v int) *int { return &v }
