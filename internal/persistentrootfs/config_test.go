package persistentrootfs

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConfigRejectsMissingCustomFile(t *testing.T) {
	_, err := LoadConfig(filepath.Join(t.TempDir(), "absent.json"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("want missing-file error, got %v", err)
	}
}

func TestLoadConfigPreservesOmittedDefaults(t *testing.T) {
	dir := t.TempDir()
	raw, err := json.Marshal(map[string]any{"stateDir": dir, "volumeDestination": dir, "allowedVolumePrefixes": []string{dir}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil || cfg.StateDir != dir || !cfg.RequireBaseID {
		t.Fatalf("unexpected config %+v, error %v", cfg, err)
	}
}

func TestLoadConfigRejectsNullDocument(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(" null\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), "object") {
		t.Fatalf("want config object error, got %v", err)
	}
}

func TestLoadConfigRejectsUnknownFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	raw := `{"stateDir":"/run/persistent-rootfs","volumeDestination":"/run/.persistent-rootfs","allowedVolumePrefix":["/var/lib/kubelet/pods"]}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown config field was accepted: %v", err)
	}
}

func TestConfigRejectsFilesystemRoot(t *testing.T) {
	tests := []Config{
		{StateDir: "/", VolumeDestination: DefaultVolumeDestination, AllowedVolumePrefixes: []string{"/var/lib/kubelet/pods"}},
		{StateDir: DefaultStateDir, VolumeDestination: "/", AllowedVolumePrefixes: []string{"/var/lib/kubelet/pods"}},
		{StateDir: DefaultStateDir, VolumeDestination: DefaultVolumeDestination, AllowedVolumePrefixes: []string{"/"}},
	}
	for _, config := range tests {
		if err := config.Validate(); err == nil {
			t.Fatalf("unsafe config was accepted: %+v", config)
		}
	}
}
