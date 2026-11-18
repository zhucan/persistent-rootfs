//go:build linux

package persistentrootfs

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadJSONNoFollowRejectsSymlink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target.json")
	if err := os.WriteFile(target, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "metadata.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := loadJSONNoFollow(link, &value); err == nil || errors.Is(err, os.ErrNotExist) {
		t.Fatalf("symlink JSON was not rejected: %v", err)
	}
}

func TestEnsurePlainDirectoryRejectsSymlink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := ensurePlainDirectory(link, 0o700); err == nil {
		t.Fatal("symlink directory was accepted")
	}
}

func TestEnsureDirectoryTreeRejectsSymlinkComponent(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "namespace")); err != nil {
		t.Fatal(err)
	}
	if _, err := ensureDirectoryTree(root, "namespace/app", 0o700); err == nil {
		t.Fatal("symlink path component was accepted")
	}
}
