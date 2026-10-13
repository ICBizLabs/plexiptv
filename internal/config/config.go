// Package config holds the small bootstrap configuration (listen address,
// data directory, ffmpeg path). Everything else — sources, rules, devices,
// channel mappings — lives in the SQLite database and is edited from the web UI.
package config

import (
	"errors"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

type Config struct {
	// Listen is the address of the web panel. The default device also
	// answers HDHomeRun requests on this port, so Plex can be pointed at it.
	Listen string `yaml:"listen"`
	// AdvertiseHost overrides the IP/host announced over SSDP and used in
	// lineup URLs when the request Host header is not usable. Empty = auto.
	AdvertiseHost string `yaml:"advertise_host"`
	// FFmpegPath is the ffmpeg binary used for HLS remuxing. Empty = "ffmpeg" in PATH.
	FFmpegPath string `yaml:"ffmpeg_path"`
	// SSDP enables HDHomeRun discovery broadcasts on UDP 1900.
	SSDP bool `yaml:"ssdp"`
	// LogLevel: debug, info, warn, error.
	LogLevel string `yaml:"log_level"`

	DataDir string `yaml:"-"`
}

func Default() Config {
	return Config{
		Listen:   ":34500",
		SSDP:     true,
		LogLevel: "info",
	}
}

// Load reads <dataDir>/config.yaml, writing a default one if it doesn't exist.
func Load(dataDir string) (Config, error) {
	cfg := Default()
	cfg.DataDir = dataDir
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return cfg, err
	}
	path := filepath.Join(dataDir, "config.yaml")
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		out, _ := yaml.Marshal(cfg)
		return cfg, os.WriteFile(path, out, 0o644)
	}
	if err != nil {
		return cfg, err
	}
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return cfg, err
	}
	cfg.DataDir = dataDir
	if cfg.Listen == "" {
		cfg.Listen = ":34500"
	}
	return cfg, nil
}

// DefaultDataDir is a "data" folder next to the executable, which keeps the
// app portable and works the same when run as a Windows service.
func DefaultDataDir() string {
	exe, err := os.Executable()
	if err != nil {
		return "data"
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return filepath.Join(filepath.Dir(exe), "data")
}
