//go:build linux

package persistentrootfs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
)

const defaultLayerDirectory = "@default"

func selectLayerContext(ctx context.Context, dataRoot, requested, baseID string, explicit bool) (string, string, error) {
	if err := ValidateRootfsID(requested); err != nil {
		return "", "", err
	}
	lock, err := lockFile(ctx, filepath.Join(dataRoot, "@layout.lock"))
	if err != nil {
		return "", "", err
	}
	defer lock.Close()
	entries, err := os.ReadDir(dataRoot)
	if err != nil {
		return "", "", err
	}
	fixed := filepath.Join(dataRoot, defaultLayerDirectory)
	_, err = os.Lstat(fixed)
	switch {
	case err == nil:
		if explicit || len(entries) != 2 {
			return "", "", errors.New("default layer conflicts with explicit ID or legacy layout")
		}
	case !errors.Is(err, os.ErrNotExist):
		return "", "", err
	case len(entries) > 1:
		legacy := filepath.Join(dataRoot, requested)
		if explicit {
			if err := resumeExplicitLayer(dataRoot, requested, baseID); err != nil {
				return "", "", err
			}
		}
		resolved, err := filepath.EvalSymlinks(legacy)
		if err != nil || resolved != legacy {
			return "", "", fmt.Errorf("cannot select existing layer %q on nonempty PVC", requested)
		}
		var metadata RootfsMetadata
		if err := loadJSONNoFollow(filepath.Join(legacy, "metadata.json"), &metadata); err != nil {
			return "", "", fmt.Errorf("read legacy layer identity: %w", err)
		}
		if metadata.Version != shimStateVersion || metadata.RootfsID != requested {
			return "", "", errors.New("legacy layer identity or version mismatch")
		}
		return legacy, requested, nil
	case explicit:
		legacy, err := ensureDirectoryTree(dataRoot, requested, 0o700)
		if err != nil {
			return "", "", err
		}
		return legacy, requested, validateShimMetadata(legacy, requested, baseID)
	}
	if err := ensurePlainDirectory(fixed, 0o700); err != nil {
		return "", "", err
	}
	metadataPath := filepath.Join(fixed, "metadata.json")
	var metadata RootfsMetadata
	if err := loadJSONNoFollow(metadataPath, &metadata); err == nil {
		if _, err := uuid.Parse(metadata.RootfsID); err != nil || metadata.Version != shimStateVersion {
			return "", "", errors.New("invalid default layer identity or version")
		}
		return fixed, metadata.RootfsID, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", "", err
	}
	contents, err := os.ReadDir(fixed)
	if err != nil {
		return "", "", err
	}
	for _, entry := range contents {
		if !entry.Type().IsRegular() || !strings.HasPrefix(entry.Name(), ".persistent-rootfs-") {
			return "", "", errors.New("default layer without metadata contains unexpected data")
		}
	}
	identity, err := uuid.NewRandom()
	if err != nil {
		return "", "", err
	}
	metadata = RootfsMetadata{Version: shimStateVersion, RootfsID: identity.String(), BaseID: baseID, CreatedAt: time.Now().UTC()}
	return fixed, identity.String(), SaveJSON(metadataPath, metadata, 0o600)
}

func resumeExplicitLayer(root, id, baseID string) error {
	current := root
	for _, component := range strings.Split(id, "/") {
		entries, err := os.ReadDir(current)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if current == root && entry.Name() == "@layout.lock" {
				continue
			}
			if entry.Name() != component || !entry.IsDir() {
				return nil
			}
		}
		current = filepath.Join(current, component)
		if err := ensurePlainDirectory(current, 0o700); err != nil {
			return err
		}
	}
	entries, err := os.ReadDir(current)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.Type().IsRegular() || !strings.HasPrefix(entry.Name(), ".persistent-rootfs-") {
			return nil
		}
	}
	return validateShimMetadata(current, id, baseID)
}
