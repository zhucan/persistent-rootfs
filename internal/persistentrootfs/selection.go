package persistentrootfs

import (
	"fmt"
	"strings"
)

type Selection struct {
	Enabled  bool
	Selected bool
	RootfsID string
	BaseID   string
}

func SelectContainer(annotations map[string]string, requireBaseID bool) (Selection, error) {
	if annotations[AnnotationEnabled] != "true" {
		return Selection{}, nil
	}
	selection := Selection{Enabled: true}
	if annotations[CRIContainerType] == "sandbox" {
		return selection, nil
	}
	target := strings.TrimSpace(annotations[AnnotationContainerName])
	if target == "" {
		return Selection{}, fmt.Errorf("required OCI annotation %q is missing", AnnotationContainerName)
	}
	containerName := strings.TrimSpace(annotations[CRIContainerName])
	if containerName == "" {
		return Selection{}, fmt.Errorf("required CRI annotation %q is missing", CRIContainerName)
	}
	if containerName != target {
		return selection, nil
	}

	selection.Selected = true
	selection.RootfsID = annotations[AnnotationRootfsID]
	if err := ValidateRootfsID(selection.RootfsID); err != nil {
		return Selection{}, err
	}
	imageName := strings.TrimSpace(annotations[CRIImageName])
	selection.BaseID = imageName
	declaredBaseID := strings.TrimSpace(annotations[AnnotationBaseID])
	if declaredBaseID != "" {
		if imageName != "" && declaredBaseID != imageName {
			return Selection{}, fmt.Errorf("base ID %q does not match CRI image name %q", declaredBaseID, imageName)
		}
		selection.BaseID = declaredBaseID
	}
	if requireBaseID && selection.BaseID == "" {
		return Selection{}, fmt.Errorf("required CRI annotation %q is missing", CRIImageName)
	}
	return selection, nil
}
