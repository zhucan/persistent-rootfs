package persistentrootfs

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const stateVersion = 1

type RuntimeState struct {
	Version     int       `json:"version"`
	ContainerID string    `json:"containerID"`
	RuncRoot    string    `json:"runcRoot"`
	Bundle      string    `json:"bundle"`
	Merged      string    `json:"merged"`
	Lower       string    `json:"lower,omitempty"`
	StateRoot   string    `json:"stateRoot"`
	RootfsID    string    `json:"rootfsID"`
	BaseID      string    `json:"baseID"`
	Status      string    `json:"status"`
	Lease       ShimLease `json:"lease"`
	CreatedAt   time.Time `json:"createdAt"`
}

type ShimLease struct {
	Version          int       `json:"version"`
	ContainerID      string    `json:"containerID"`
	MachineID        string    `json:"machineID"`
	BootID           string    `json:"bootID"`
	PID              int       `json:"pid"`
	ProcessStartTime string    `json:"processStartTime,omitempty"`
	CreatedAt        time.Time `json:"createdAt"`
}

type RootfsMetadata struct {
	Version   int       `json:"version"`
	RootfsID  string    `json:"rootfsID"`
	BaseID    string    `json:"baseID"`
	CreatedAt time.Time `json:"createdAt"`
}

type ActiveLease struct {
	Version     int       `json:"version"`
	ContainerID string    `json:"containerID"`
	RuncRoot    string    `json:"runcRoot"`
	CreatedAt   time.Time `json:"createdAt"`
}

func ValidateRootfsID(value string) error {
	if value == "" || len(value) > 255 || filepath.IsAbs(value) || strings.Contains(value, `\`) {
		return fmt.Errorf("invalid rootfs ID %q", value)
	}
	for _, part := range strings.Split(filepath.ToSlash(value), "/") {
		if part == "" || part == "." || part == ".." {
			return fmt.Errorf("invalid rootfs ID component in %q", value)
		}
		for _, r := range part {
			if !(r >= 'a' && r <= 'z') && !(r >= 'A' && r <= 'Z') &&
				!(r >= '0' && r <= '9') && r != '.' && r != '_' && r != '-' {
				return fmt.Errorf("invalid character in rootfs ID %q", value)
			}
		}
	}
	return nil
}

func StateFile(stateDir, containerID string) string {
	sum := sha256.Sum256([]byte(containerID))
	return filepath.Join(stateDir, hex.EncodeToString(sum[:])+".json")
}

func SaveJSON(path string, value any, mode os.FileMode) error {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return WriteFileAtomic(path, append(raw, '\n'), mode)
}

func LoadJSON(path string, value any) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, value)
}
