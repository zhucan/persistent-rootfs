package persistentrootfs

import "testing"

func TestValidateRootfsID(t *testing.T) {
	valid := []string{"app", "namespace/workload-0/app", "a_b.c-1"}
	for _, value := range valid {
		if err := ValidateRootfsID(value); err != nil {
			t.Errorf("ValidateRootfsID(%q): %v", value, err)
		}
	}
	invalid := []string{"", "/absolute", "../escape", "a//b", "a:b", "a\\b"}
	for _, value := range invalid {
		if err := ValidateRootfsID(value); err == nil {
			t.Errorf("ValidateRootfsID(%q) succeeded", value)
		}
	}
}

func TestStateFileDoesNotExposeContainerIDAsPath(t *testing.T) {
	path := StateFile("/run/state", "../../container")
	if path == "/run/state/../../container.json" {
		t.Fatalf("unsafe state path: %q", path)
	}
}
