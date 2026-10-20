package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

type Settings struct {
	HTTP        string       `json:"http"`
	Port        string       `json:"port"`
	XMLPath     string       `json:"xml_path"`
	StopOnFail  bool         `json:"stop_on_fail"`
	ConsoleAddr string       `json:"console_addr"`
	Tunnel      TunnelConfig `json:"tunnel"`
}

func DefaultSettings() Settings {
	return Settings{HTTP: "127.0.0.1:8090", ConsoleAddr: "6A", Tunnel: DefaultTunnelConfig()}
}

var settingsMu sync.Mutex

func settingsPath() string {
	exe, err := os.Executable()
	if err == nil {
		if r, err := filepath.EvalSymlinks(exe); err == nil {
			exe = r
		}
		return filepath.Join(filepath.Dir(exe), "a2b_master.json")
	}
	return "a2b_master.json"
}

func LoadSettings(path string) Settings {
	s := DefaultSettings()
	b, err := os.ReadFile(path)
	if err == nil {
		if err := json.Unmarshal(b, &s); err != nil {
			logf("settings %s: %v (using defaults)", path, err)
			s = DefaultSettings()
		}
	}
	if s.HTTP == "" {
		s.HTTP = "127.0.0.1:8090"
	}
	if s.Tunnel.BasePort == 0 {
		s.Tunnel = DefaultTunnelConfig()
	}
	return s
}

func SaveSettings(path string, s Settings) {
	settingsMu.Lock()
	defer settingsMu.Unlock()
	b, _ := json.MarshalIndent(s, "", "  ")
	if err := os.WriteFile(path, b, 0o644); err != nil {
		logf("cannot save settings to %s: %v", path, err)
	}
}
