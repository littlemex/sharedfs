package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	sharedfsv1alpha1 "github.com/littlemex/sharedfs/api/v1alpha1"
	"github.com/littlemex/sharedfs/internal/volume"
)

const (
	// indexClaimFilesystem indexes SharedFilesystemClaims by the SharedFilesystem they name.
	indexClaimFilesystem = ".spec.filesystemRef.name"
	// indexFilesystemVolume indexes SharedFilesystems by their source PersistentVolume.
	indexFilesystemVolume = ".spec.persistentVolumeRef.name"
)

// SetupIndexes registers the field indexes the reconcilers list by.
func SetupIndexes(ctx context.Context, mgr ctrl.Manager) error {
	idx := mgr.GetFieldIndexer()
	if err := idx.IndexField(ctx, &sharedfsv1alpha1.SharedFilesystemClaim{}, indexClaimFilesystem,
		func(o client.Object) []string {
			return []string{o.(*sharedfsv1alpha1.SharedFilesystemClaim).Spec.FilesystemRef.Name}
		}); err != nil {
		return err
	}
	return idx.IndexField(ctx, &sharedfsv1alpha1.SharedFilesystem{}, indexFilesystemVolume,
		func(o client.Object) []string {
			return []string{o.(*sharedfsv1alpha1.SharedFilesystem).Spec.PersistentVolumeRef.Name}
		})
}

// grantedAccess is the strongest access the filesystem's grants give the namespace.
func grantedAccess(fs *sharedfsv1alpha1.SharedFilesystem, ns *corev1.Namespace) (sharedfsv1alpha1.Access, bool, error) {
	var best sharedfsv1alpha1.Access
	granted := false
	for i, g := range fs.Spec.Grants {
		sel, err := metav1.LabelSelectorAsSelector(&g.NamespaceSelector)
		if err != nil {
			return "", false, fmt.Errorf("grants[%d].namespaceSelector: %w", i, err)
		}
		if !sel.Matches(labels.Set(ns.Labels)) {
			continue
		}
		access := g.Access
		if access == "" {
			access = sharedfsv1alpha1.AccessReadOnly
		}
		if !granted || access == sharedfsv1alpha1.AccessReadWrite {
			best = access
		}
		granted = true
	}
	return best, granted, nil
}

// validateGrants reports the first grant whose selector cannot be parsed.
func validateGrants(fs *sharedfsv1alpha1.SharedFilesystem) error {
	for i, g := range fs.Spec.Grants {
		if _, err := metav1.LabelSelectorAsSelector(&g.NamespaceSelector); err != nil {
			return fmt.Errorf("grants[%d].namespaceSelector: %w", i, err)
		}
	}
	return nil
}

// releaseVolumes deletes a claim's PVC and then its PV, one step per call, and reports whether
// both are gone. It is the one release path, used when a claim is deleted, when its access is
// revoked, and when its claim is found missing. Reads are live: "gone" ends a finalizer and must
// not come from a cache that has not seen the objects yet. The PVC stays (Terminating) while pods
// use it, which keeps running pods working and stops new ones; its deletion event brings the
// next step.
func releaseVolumes(ctx context.Context, c client.Client, live client.Reader, claimUID types.UID, pvc types.NamespacedName) (bool, error) {
	var claimPVC corev1.PersistentVolumeClaim
	err := live.Get(ctx, pvc, &claimPVC)
	if err != nil && !apierrors.IsNotFound(err) {
		return false, err
	}
	if err == nil && volume.OwnsPVC(claimUID, &claimPVC) {
		if claimPVC.DeletionTimestamp.IsZero() {
			if err := c.Delete(ctx, &claimPVC, client.Preconditions{UID: &claimPVC.UID}); client.IgnoreNotFound(err) != nil {
				return false, err
			}
		}
		return false, nil
	}

	var pv corev1.PersistentVolume
	err = live.Get(ctx, types.NamespacedName{Name: volume.PVName(claimUID)}, &pv)
	if apierrors.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if pv.DeletionTimestamp.IsZero() {
		if err := c.Delete(ctx, &pv); client.IgnoreNotFound(err) != nil {
			return false, err
		}
	}
	return false, nil
}

// setCondition sets a condition stamped with the object's generation.
func setCondition(conds *[]metav1.Condition, generation int64, typ string, status metav1.ConditionStatus, reason, msg string) {
	meta.SetStatusCondition(conds, metav1.Condition{
		Type: typ, Status: status, Reason: reason, Message: msg, ObservedGeneration: generation,
	})
}

func toRequests(keys []types.NamespacedName) []reconcile.Request {
	out := make([]reconcile.Request, 0, len(keys))
	for _, k := range keys {
		out = append(out, reconcile.Request{NamespacedName: k})
	}
	return out
}

// claimRefOf is the PVC a PersistentVolume is reserved for.
func claimRefOf(pv *corev1.PersistentVolume) (types.NamespacedName, bool) {
	if pv.Spec.ClaimRef == nil || pv.Spec.ClaimRef.Name == "" {
		return types.NamespacedName{}, false
	}
	return types.NamespacedName{Namespace: pv.Spec.ClaimRef.Namespace, Name: pv.Spec.ClaimRef.Name}, true
}
