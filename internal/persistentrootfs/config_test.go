package persistentrootfs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
