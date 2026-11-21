package main

import (
	"context"
	"encoding/json"
	"fmt"

	"golang.org/x/sync/errgroup"
	corev1 "k8s.io/api/core/v1"

	"github.com/persistent-rootfs/persistent-rootfs/internal/persistentrootfs"
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
	claims := make(map[string][]string)
	for _, volume := range pod.Spec.Volumes {
		if volume.Ephemeral != nil && volume.Ephemeral.VolumeClaimTemplate != nil && volume.Ephemeral.VolumeClaimTemplate.Labels[persistentrootfs.LabelStateVolume] == "true" {
			return fmt.Errorf("protected PVCs require a dedicated persistent volume claim")
		}
		claim := volume.PersistentVolumeClaim
		if claim == nil {
			continue
		}
		claims[claim.ClaimName] = append(claims[claim.ClaimName], volume.Name)
	}
	group, lookupCtx := errgroup.WithContext(ctx)
	group.SetLimit(4)
	for claimName, volumes := range claims {
		if lookupCtx.Err() != nil {
			break
		}
		group.Go(func() error {
			isProtected, err := protected(lookupCtx, req.Namespace, claimName)
			if err != nil {
				return fmt.Errorf("verify PVC %q: %w", claimName, err)
			}
			for _, name := range volumes {
				if isProtected != (enabled && name == stateVolume) {
					return fmt.Errorf("protected PVCs may only be used as the declared persistent rootfs state volume: %q", name)
				}
			}
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return err
	}
	return ctx.Err()
}
