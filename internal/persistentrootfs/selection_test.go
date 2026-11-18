package persistentrootfs

import "testing"

func TestSelectContainer(t *testing.T) {
	base := map[string]string{
		AnnotationEnabled:       "true",
		AnnotationContainerName: "app",
		AnnotationRootfsID:      "ns/workload-0/app",
		CRIContainerType:        "container",
		CRIContainerName:        "app",
		CRIImageName:            "registry.example/app@sha256:abc",
	}
	selection, err := SelectContainer(base, true)
	if err != nil {
		t.Fatal(err)
	}
	if !selection.Enabled || !selection.Selected || selection.RootfsID != "ns/workload-0/app" || selection.BaseID != base[CRIImageName] {
		t.Fatalf("unexpected selection: %+v", selection)
	}
	delete(base, AnnotationRootfsID)
	selection, err = SelectContainer(base, true)
	if err != nil || selection.RootfsID != "app" {
		t.Fatalf("default selection=%+v err=%v", selection, err)
	}
}

func TestSelectContainerPassesSandboxAndSidecar(t *testing.T) {
	sandbox, err := SelectContainer(map[string]string{
		AnnotationEnabled: "true",
		CRIContainerType:  "sandbox",
	}, true)
	if err != nil || !sandbox.Enabled || sandbox.Selected {
		t.Fatalf("sandbox selection=%+v err=%v", sandbox, err)
	}
	sidecar, err := SelectContainer(map[string]string{
		AnnotationEnabled:       "true",
		AnnotationContainerName: "app",
		CRIContainerName:        "sidecar",
	}, true)
	if err != nil || !sidecar.Enabled || sidecar.Selected {
		t.Fatalf("sidecar selection=%+v err=%v", sidecar, err)
	}
}

func TestSelectContainerRejectsMismatchedBaseID(t *testing.T) {
	_, err := SelectContainer(map[string]string{
		AnnotationEnabled:       "true",
		AnnotationContainerName: "app",
		AnnotationRootfsID:      "app",
		CRIContainerName:        "app",
		CRIImageName:            "registry.example/app@sha256:abc",
		AnnotationBaseID:        "registry.example/app@sha256:def",
	}, true)
	if err == nil {
		t.Fatal("mismatched base ID was accepted")
	}
}
