package config

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

type Config struct {
	LogLevel     string         `yaml:"log_level"`
	PairingStore string         `yaml:"pairing_store"`
	BindAddress  string         `yaml:"bind_address"`
	Cameras      []CameraConfig `yaml:"cameras"`
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
	Port int    `yaml:"port"`
	Path string `yaml:"path"`
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

	return cfg, nil
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
