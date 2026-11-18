package persistentrootfs

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const (
	DefaultConfigPath        = "/etc/persistent-rootfs/config.json"
	DefaultStateDir          = "/run/persistent-rootfs"
	DefaultVolumeDestination = "/run/.persistent-rootfs"
	AnnotationEnabled        = "persistent-rootfs.io/enabled"
	AnnotationRootfsID       = "persistent-rootfs.io/rootfs-id"
	AnnotationBaseID         = "persistent-rootfs.io/base-id"
	AnnotationContainerName  = "persistent-rootfs.io/container-name"
	AnnotationVolumeName     = "persistent-rootfs.io/volume-name"
	LabelStateVolume         = "persistent-rootfs.io/state-volume"
	CRIContainerType         = "io.kubernetes.cri.container-type"
	CRIContainerName         = "io.kubernetes.cri.container-name"
	CRIImageName             = "io.kubernetes.cri.image-name"
)

// Config controls the host-side trust boundary around the Runtime v2 shim.
type Config struct {
	StateDir              string   `json:"stateDir"`
	VolumeDestination     string   `json:"volumeDestination"`
	AllowedVolumePrefixes []string `json:"allowedVolumePrefixes"`
	RequireBaseID         bool     `json:"requireBaseID"`
}

func DefaultConfig() Config {
	return Config{
		StateDir:              DefaultStateDir,
		VolumeDestination:     DefaultVolumeDestination,
		AllowedVolumePrefixes: []string{"/var/lib/kubelet/pods"},
		RequireBaseID:         true,
	}
}

// LoadConfig allows a missing default file so conservative defaults work.
func LoadConfig(path string) (Config, error) {
	cfg := DefaultConfig()
	if path == "" {
		path = DefaultConfigPath
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, cfg.Validate()
	}
	if err != nil {
		return Config{}, fmt.Errorf("read config %q: %w", path, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("decode config %q: %w", path, err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
		return Config{}, fmt.Errorf("decode config %q: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, fmt.Errorf("validate config %q: %w", path, err)
	}
	return cfg, nil
}

func (c Config) Validate() error {
	for name, value := range map[string]string{
		"stateDir":          c.StateDir,
		"volumeDestination": c.VolumeDestination,
	} {
		if value == "" {
			return fmt.Errorf("%s must not be empty", name)
		}
		if !filepath.IsAbs(value) {
			return fmt.Errorf("%s must be absolute: %q", name, value)
		}
		if filepath.Clean(value) != value {
			return fmt.Errorf("%s must be a clean path: %q", name, value)
		}
		if value == string(filepath.Separator) {
			return fmt.Errorf("%s must not be the filesystem root", name)
		}
	}
	if len(c.AllowedVolumePrefixes) == 0 {
		return errors.New("allowedVolumePrefixes must not be empty")
	}
	for _, prefix := range c.AllowedVolumePrefixes {
		if !filepath.IsAbs(prefix) {
			return fmt.Errorf("allowed volume prefix must be absolute: %q", prefix)
		}
		if filepath.Clean(prefix) != prefix {
			return fmt.Errorf("allowed volume prefix must be a clean path: %q", prefix)
		}
		if prefix == string(filepath.Separator) {
			return errors.New("allowed volume prefix must not be the filesystem root")
		}
	}
	return nil
}
