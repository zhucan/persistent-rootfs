package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/persistent-rootfs/persistent-rootfs/internal/persistentrootfs"
	corev1 "k8s.io/api/core/v1"
)

func checkClaimUses(ctx context.Context, req *persistentrootfs.AdmissionRequest, protected func(context.Context, string, string) (bool, error)) error {
	var pod corev1.Pod
	if err := json.Unmarshal(req.Object, &pod); err != nil {
		return err
	}
	enabled := pod.Annotations[persistentrootfs.AnnotationEnabled] == "true"
	stateVolume := pod.Annotations[persistentrootfs.AnnotationVolumeName]
	if stateVolume == "" {
		stateVolume = "rootfs-state"
	}
	for _, volume := range pod.Spec.Volumes {
		if volume.Ephemeral != nil && volume.Ephemeral.VolumeClaimTemplate != nil && volume.Ephemeral.VolumeClaimTemplate.Labels[persistentrootfs.LabelStateVolume] == "true" {
			return fmt.Errorf("protected PVCs require a dedicated persistent volume claim")
		}
		claim := volume.PersistentVolumeClaim
		if claim == nil {
			continue
		}
		isProtected, err := protected(ctx, req.Namespace, claim.ClaimName)
		if err != nil {
			return fmt.Errorf("verify volume %q: %w", volume.Name, err)
		}
		if isProtected != (enabled && volume.Name == stateVolume) {
			return fmt.Errorf("protected PVCs may only be used as the declared persistent rootfs state volume: %q", volume.Name)
		}
	}
	return nil
}
