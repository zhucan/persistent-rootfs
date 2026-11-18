//go:build linux

package persistentrootfs

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

const rootfsDataDirectory = ".persistent-rootfs-runtime"

func resolveAllowedVolume(source string, prefixes []string) (string, error) {
	if !filepath.IsAbs(source) {
		return "", fmt.Errorf("state volume source is not absolute: %q", source)
	}
	resolved, err := filepath.EvalSymlinks(source)
	if err != nil {
		return "", fmt.Errorf("resolve state volume source %q: %w", source, err)
	}
	for _, prefix := range prefixes {
		resolvedPrefix, err := filepath.EvalSymlinks(prefix)
		if err != nil {
			continue
		}
		if pathWithin(resolvedPrefix, resolved) {
			return resolved, nil
		}
	}
	return "", fmt.Errorf("state volume source %q is outside allowed prefixes", resolved)
}

func pathWithin(root, candidate string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(candidate))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func pathWithinAnyRoot(candidate string, roots []string) bool {
	resolvedCandidate, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return false
	}
	for _, root := range roots {
		resolvedRoot, err := filepath.EvalSymlinks(root)
		if err == nil && pathWithin(resolvedRoot, resolvedCandidate) {
			return true
		}
	}
	return false
}

func ensureDirectoryTree(root, relative string, mode os.FileMode) (string, error) {
	if err := ValidateRootfsID(relative); err != nil {
		return "", err
	}
	current := root
	for _, part := range strings.Split(filepath.ToSlash(relative), "/") {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			if err := os.Mkdir(current, mode); err != nil && !errors.Is(err, os.ErrExist) {
				return "", err
			}
			info, err = os.Lstat(current)
		}
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return "", fmt.Errorf("state path component is not a directory: %q", current)
		}
	}
	return current, nil
}

func ensurePlainDirectory(path string, mode os.FileMode) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.Mkdir(path, mode); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
		info, err = os.Lstat(path)
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("path is not a plain directory: %q", path)
	}
	return nil
}

func prepareMergedDirectory(path string) error {
	if err := ensurePlainDirectory(path, 0o700); err != nil {
		return err
	}
	mounted, err := isMountPoint(path)
	if err != nil {
		return fmt.Errorf("inspect mount target %q: %w", path, err)
	}
	if mounted {
		return fmt.Errorf("mount target %q is already mounted", path)
	}
	return nil
}

func lockFile(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(fd, unix.LOCK_EX); err != nil {
		unix.Close(fd)
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}

func loadJSONNoFollow(path string, value any) error {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		unix.Close(fd)
		return fmt.Errorf("open %q: invalid file descriptor", path)
	}
	defer file.Close()
	raw, err := io.ReadAll(file)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, value)
}

func validateOverlayPath(path string) error {
	if strings.ContainsAny(path, ",:\\") {
		return fmt.Errorf("overlay path contains an unsupported separator: %q", path)
	}
	return nil
}

func sameFilesystem(a, b string) error {
	var sa, sb unix.Stat_t
	if err := unix.Stat(a, &sa); err != nil {
		return err
	}
	if err := unix.Stat(b, &sb); err != nil {
		return err
	}
	if sa.Dev != sb.Dev {
		return fmt.Errorf("overlay upperdir and workdir are on different filesystems")
	}
	return nil
}

func mountOverlay(lower, upper, work, merged string) error {
	for _, path := range []string{lower, upper, work, merged} {
		if err := validateOverlayPath(path); err != nil {
			return err
		}
	}
	if err := sameFilesystem(upper, work); err != nil {
		return err
	}
	data := "lowerdir=" + lower + ",upperdir=" + upper + ",workdir=" + work
	parameters, err := os.ReadDir("/sys/module/overlay/parameters")
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read OverlayFS module parameters: %w", err)
	}
	for _, parameter := range parameters {
		switch parameter.Name() {
		case "index", "metacopy", "nfs_export":
			data += "," + parameter.Name() + "=off"
		case "redirect_always_follow":
			data += ",redirect_dir=nofollow"
		case "xino_auto":
			data += ",xino=off"
		}
	}
	if err := unix.Mount("overlay", merged, "overlay", 0, data); err != nil {
		return fmt.Errorf("mount overlay on %q: %w", merged, err)
	}
	return nil
}

func unmountOverlay(path string) error {
	mounted, err := isMountPoint(path)
	if err != nil || !mounted {
		return err
	}
	if err := unix.Unmount(path, 0); err == nil {
		return nil
	}
	if err := unix.Unmount(path, unix.MNT_DETACH); err != nil {
		return fmt.Errorf("unmount overlay %q: %w", path, err)
	}
	return nil
}

func isMountPoint(path string) (bool, error) {
	file, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return false, err
	}
	defer file.Close()
	want := filepath.Clean(path)
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) > 4 && filepath.Clean(unescapeMountInfo(fields[4])) == want {
			return true, nil
		}
	}
	return false, scanner.Err()
}

func unescapeMountInfo(value string) string {
	replacer := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	return replacer.Replace(value)
}
