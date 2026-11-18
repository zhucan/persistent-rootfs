package persistentrootfs

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSpecDocumentPreservesFieldsAndRemovesStateMount(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	raw := []byte(`{
  "ociVersion":"1.1.0",
  "root":{"path":"rootfs","readonly":false},
  "process":{"terminal":false,"args":["sleep","1"]},
  "annotations":{"persistent-rootfs.io/enabled":"true"},
  "mounts":[
    {"destination":"/proc","type":"proc","source":"proc"},
    {"destination":"/run/.persistent-rootfs","type":"bind","source":"/var/lib/kubelet/pods/p1/volumes/csi/mount","options":["rbind","rw"]}
  ]
}`)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	doc, _, _, err := ReadSpec(path)
	if err != nil {
		t.Fatal(err)
	}
	source, err := doc.RemoveMount(DefaultVolumeDestination)
	if err != nil {
		t.Fatal(err)
	}
	if source != "/var/lib/kubelet/pods/p1/volumes/csi/mount" {
		t.Fatalf("source=%q", source)
	}
	if err := doc.SetRootPath("/run/bundle/.persistent-rootfs-merged"); err != nil {
		t.Fatal(err)
	}
	encoded, err := doc.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) == "" || !containsAll(string(encoded), `"process"`, `"/proc"`, `/run/bundle/.persistent-rootfs-merged`) {
		t.Fatalf("unexpected encoded spec: %s", encoded)
	}
}

func containsAll(value string, needles ...string) bool {
	for _, needle := range needles {
		found := false
		for i := 0; i+len(needle) <= len(value); i++ {
			if value[i:i+len(needle)] == needle {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
