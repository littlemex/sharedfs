package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	sharedfsv1alpha1 "github.com/littlemex/sharedfs/api/v1alpha1"
	"github.com/littlemex/sharedfs/internal/volume"
)

const (
	indexPVCClass    = ".spec.storageClassName"
	indexShareSource = ".spec.persistentVolumeRef.name"
	indexDirShare    = ".spec.shareRef.uid"
)

// SetupIndexes registers the field indexes the reconcilers list by.
func SetupIndexes(ctx context.Context, mgr ctrl.Manager) error {
	idx := mgr.GetFieldIndexer()
	if err := idx.IndexField(ctx, &corev1.PersistentVolumeClaim{}, indexPVCClass, func(o client.Object) []string {
		if c := o.(*corev1.PersistentVolumeClaim).Spec.StorageClassName; c != nil {
			return []string{*c}
		}
		return nil
	}); err != nil {
		return err
	}
	if err := idx.IndexField(ctx, &sharedfsv1alpha1.FilesystemShare{}, indexShareSource, func(o client.Object) []string {
		return []string{o.(*sharedfsv1alpha1.FilesystemShare).Spec.PersistentVolumeRef.Name}
	}); err != nil {
		return err
	}
	return idx.IndexField(ctx, &sharedfsv1alpha1.FilesystemShareDirectory{}, indexDirShare, func(o client.Object) []string {
		return []string{string(o.(*sharedfsv1alpha1.FilesystemShareDirectory).Spec.ShareRef.UID)}
	})
}

// shareChanged passes share events that can change a verdict about the share or its claims:
// spec, annotations, deletion, readiness and a new verdict. A volume count that merely moves does
// not re-run every claim and directory of the share.
var shareChanged = predicate.Funcs{UpdateFunc: func(e event.UpdateEvent) bool {
	a, b := e.ObjectOld.(*sharedfsv1alpha1.FilesystemShare), e.ObjectNew.(*sharedfsv1alpha1.FilesystemShare)
	return a.Generation != b.Generation ||
		!maps.Equal(a.Annotations, b.Annotations) ||
		a.DeletionTimestamp.IsZero() != b.DeletionTimestamp.IsZero() ||
		isReady(a) != isReady(b) ||
		a.Status.ObservedGeneration != b.Status.ObservedGeneration
}}

const podSecurityEnforce = "pod-security.kubernetes.io/enforce"

// quotaOnFilesystem reports whether some share on the file system identified by fs has a quota.
// A root process can move its own files to any project (measured), so a quota on one share is
// only as safe as every read-write writer of the same file system.
func quotaOnFilesystem(ctx context.Context, c client.Reader, fs string) (bool, error) {
	var list sharedfsv1alpha1.FilesystemShareList
	if err := c.List(ctx, &list); err != nil {
		return false, err
	}
	for i := range list.Items {
		s := &list.Items[i]
		if s.Spec.Provision == nil || s.Spec.Provision.Quota == nil {
			continue
		}
		var pv corev1.PersistentVolume
		if err := c.Get(ctx, types.NamespacedName{Name: s.Spec.PersistentVolumeRef.Name}, &pv); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return false, err
		}
		src, err := volume.Resolve(&pv)
		if err != nil {
			continue
		}
		if f, _ := src.Identity(); f == fs {
			return true, nil
		}
	}
	return false, nil
}

// ClassNames are the StorageClasses of a share.
func ClassNames(share *sharedfsv1alpha1.FilesystemShare) sharedfsv1alpha1.StorageClassNames {
	return sharedfsv1alpha1.StorageClassNames{ReadWrite: share.Name + "-rw", ReadOnly: share.Name + "-ro"}
}

// grantFor reports whether a single grant of the share selects the namespace and allows access.
// Path and access always come from the same share, so nothing is composed across grants.
func grantFor(share *sharedfsv1alpha1.FilesystemShare, ns *corev1.Namespace, access sharedfsv1alpha1.Access) (bool, error) {
	for i, g := range share.Spec.Grants {
		sel, err := metav1.LabelSelectorAsSelector(&g.NamespaceSelector)
		if err != nil {
			return false, fmt.Errorf("grants[%d].namespaceSelector: %w", i, err)
		}
		if sel.Empty() {
			return false, fmt.Errorf("grants[%d].namespaceSelector is empty", i)
		}
		if sel.Matches(labels.Set(ns.Labels)) && g.Access.Allows(access) {
			return true, nil
		}
	}
	return false, nil
}

// grantsReadWrite reports whether any grant of the share allows ReadWrite.
func grantsReadWrite(share *sharedfsv1alpha1.FilesystemShare) bool {
	for _, g := range share.Spec.Grants {
		if g.Access == sharedfsv1alpha1.AccessReadWrite {
			return true
		}
	}
	return false
}

func validateGrants(share *sharedfsv1alpha1.FilesystemShare) error {
	for i, g := range share.Spec.Grants {
		sel, err := metav1.LabelSelectorAsSelector(&g.NamespaceSelector)
		if err != nil {
			return fmt.Errorf("grants[%d].namespaceSelector: %w", i, err)
		}
		if sel.Empty() {
			return fmt.Errorf("grants[%d].namespaceSelector is empty", i)
		}
	}
	return nil
}

// classOwner resolves a StorageClass to the share that controls it and the access it grants.
// A class sharedfs does not control is reported as not found, so a class someone else created
// with our provisioner name is never trusted.
func classOwner(ctx context.Context, c client.Reader, name string) (*sharedfsv1alpha1.FilesystemShare, sharedfsv1alpha1.Access, error) {
	var sc storagev1.StorageClass
	if err := c.Get(ctx, types.NamespacedName{Name: name}, &sc); err != nil {
		return nil, "", err
	}
	if sc.Provisioner != sharedfsv1alpha1.ProvisionerName {
		return nil, "", apierrors.NewNotFound(storagev1.Resource("storageclasses"), name)
	}
	var share sharedfsv1alpha1.FilesystemShare
	if err := c.Get(ctx, types.NamespacedName{Name: sc.Parameters[sharedfsv1alpha1.ParameterShare]}, &share); err != nil {
		return nil, "", err
	}
	if string(share.UID) != sc.Parameters[sharedfsv1alpha1.ParameterShareUID] || !metav1.IsControlledBy(&sc, &share) {
		return nil, "", apierrors.NewNotFound(storagev1.Resource("storageclasses"), name)
	}
	return &share, sharedfsv1alpha1.Access(sc.Parameters[sharedfsv1alpha1.ParameterAccess]), nil
}

// DirectoryName is the name of the record of a share's directory for a namespace ("" for a
// share-wide directory). It is a hash: share and namespace names together exceed name limits.
func DirectoryName(shareUID types.UID, namespace string) string {
	sum := sha256.Sum256([]byte(string(shareUID) + "/" + namespace))
	return "sfd-" + hex.EncodeToString(sum[:])[:24]
}

func setCondition(conds *[]metav1.Condition, generation int64, typ string, ok bool, reason, msg string) {
	status := metav1.ConditionFalse
	if ok {
		status = metav1.ConditionTrue
	}
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

// PreparerConfig is where and how preparer pods run.
type PreparerConfig struct {
	// Namespace of the controller; preparer pods and their internal claims live here.
	Namespace string
	// Image is the controller's own image, which carries the prepare-directory subcommand.
	Image string
	// ServiceAccount of preparer pods: no permissions, and its token is not mounted.
	ServiceAccount string
	NodeSelector   map[string]string
	Tolerations    []corev1.Toleration
	// ImagePullSecrets are the controller pod's own, so the preparer pulls the same image.
	ImagePullSecrets []corev1.LocalObjectReference
	// MaxPods bounds the preparer pods running at once.
	MaxPods int
}
