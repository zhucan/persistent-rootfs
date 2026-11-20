package persistentrootfs

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
)

// SpecDocument edits only the needed OCI fields and preserves unknown fields.
type SpecDocument struct {
	fields map[string]json.RawMessage
}

func ReadSpec(path string) (*SpecDocument, []byte, os.FileMode, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("open OCI spec: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, nil, 0, fmt.Errorf("stat OCI spec: %w", err)
	}
	raw, err := io.ReadAll(file)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("read OCI spec: %w", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, nil, 0, fmt.Errorf("decode OCI spec: %w", err)
	}
	if fields == nil {
		return nil, nil, 0, fmt.Errorf("decode OCI spec: expected a JSON object")
	}
	return &SpecDocument{fields: fields}, raw, info.Mode().Perm(), nil
}

func (s *SpecDocument) Annotations() (map[string]string, error) {
	annotations := map[string]string{}
	raw, ok := s.fields["annotations"]
	if !ok || string(raw) == "null" {
		return annotations, nil
	}
	if err := json.Unmarshal(raw, &annotations); err != nil {
		return nil, fmt.Errorf("decode OCI annotations: %w", err)
	}
	return annotations, nil
}

func (s *SpecDocument) RootPath() (string, error) {
	root, err := s.object("root")
	if err != nil {
		return "", err
	}
	var path string
	if err := json.Unmarshal(root["path"], &path); err != nil || path == "" {
		return "", fmt.Errorf("OCI root.path is missing or invalid")
	}
	return path, nil
}

func (s *SpecDocument) SetRootPath(path string) error {
	root, err := s.object("root")
	if err != nil {
		return err
	}
	raw, _ := json.Marshal(path)
	root["path"] = raw
	return s.setObject("root", root)
}

// RemoveMount returns the host source and hides the state volume from the
// container so upper/work cannot be modified directly.
func (s *SpecDocument) RemoveMount(destination string) (string, error) {
	var mounts []map[string]json.RawMessage
	raw, ok := s.fields["mounts"]
	if !ok {
		return "", fmt.Errorf("OCI mounts are missing")
	}
	if err := json.Unmarshal(raw, &mounts); err != nil {
		return "", fmt.Errorf("decode OCI mounts: %w", err)
	}

	kept := make([]map[string]json.RawMessage, 0, len(mounts))
	var source string
	for _, mount := range mounts {
		var dst string
		if err := json.Unmarshal(mount["destination"], &dst); err != nil {
			return "", fmt.Errorf("decode OCI mount destination: %w", err)
		}
		if filepath.Clean(dst) != filepath.Clean(destination) {
			kept = append(kept, mount)
			continue
		}
		if source != "" {
			return "", fmt.Errorf("multiple OCI mounts use destination %q", destination)
		}
		if err := json.Unmarshal(mount["source"], &source); err != nil || source == "" {
			return "", fmt.Errorf("OCI mount %q has no valid source", destination)
		}
	}
	if source == "" {
		return "", fmt.Errorf("OCI mount destination %q not found", destination)
	}
	encoded, err := json.Marshal(kept)
	if err != nil {
		return "", fmt.Errorf("encode OCI mounts: %w", err)
	}
	s.fields["mounts"] = encoded
	return source, nil
}

func (s *SpecDocument) Marshal() ([]byte, error) {
	raw, err := json.MarshalIndent(s.fields, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode OCI spec: %w", err)
	}
	return append(raw, '\n'), nil
}

func (s *SpecDocument) object(name string) (map[string]json.RawMessage, error) {
	raw, ok := s.fields[name]
	if !ok {
		return nil, fmt.Errorf("OCI %s object is missing", name)
	}
	var value map[string]json.RawMessage
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, fmt.Errorf("decode OCI %s: %w", name, err)
	}
	if value == nil {
		return nil, fmt.Errorf("OCI %s must be an object", name)
	}
	return value, nil
}

func (s *SpecDocument) setObject(name string, value map[string]json.RawMessage) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode OCI %s: %w", name, err)
	}
	s.fields[name] = raw
	return nil
}

func WriteFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".persistent-rootfs-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		return nil
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
