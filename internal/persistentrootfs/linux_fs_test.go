//go:build linux

package persistentrootfs

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestOverlayReusesUpperOnNewImage(t *testing.T) {
	oldLower, newLower, upper, work, merged := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
	write := func(dir, name, value string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, lower := range []string{oldLower, newLower} {
		write(lower, "edited", "image default")
		write(lower, "deleted", "image default")
	}
	write(newLower, "added", "new image")
	if err := mountOverlay(oldLower, upper, work, merged); err != nil {
		if errors.Is(err, unix.EPERM) || errors.Is(err, unix.ENODEV) || errors.Is(err, unix.EACCES) {
			t.Skipf("requires OverlayFS mount privileges: %v", err)
		}
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := unmountOverlay(merged); err != nil {
			t.Error(err)
		}
	})
	write(merged, "edited", "user edit")
	if err := os.Remove(filepath.Join(merged, "deleted")); err != nil {
		t.Fatal(err)
	}
	if err := unmountOverlay(merged); err != nil {
		t.Fatal(err)
	}
	if err := mountOverlay(newLower, upper, work, merged); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"edited": "user edit", "added": "new image"} {
		got, err := os.ReadFile(filepath.Join(merged, name))
		if err != nil || string(got) != want {
			t.Fatalf("%s: got %q, want %q, err=%v", name, got, want, err)
		}
	}
	if _, err := os.Stat(filepath.Join(merged, "deleted")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected deleted file to remain hidden, got %v", err)
	}
}

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
