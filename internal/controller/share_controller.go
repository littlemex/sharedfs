package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	sharedfsv1alpha1 "github.com/littlemex/sharedfs/api/v1alpha1"
	"github.com/littlemex/sharedfs/internal/driver"
	"github.com/littlemex/sharedfs/internal/sharepath"
	"github.com/littlemex/sharedfs/internal/volume"
)

// ShareReconciler checks that a FilesystemShare is safe to provision from, owns its two
// StorageClasses, and keeps the share until no volume provisioned from it remains. It writes only
// the share's status.
type ShareReconciler struct {
	client.Client
	APIReader client.Reader
	Recorder  events.EventRecorder
}

// +kubebuilder:rbac:groups=sharedfs.littlemex.github.io,resources=filesystemshares,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=sharedfs.littlemex.github.io,resources=filesystemshares/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=sharedfs.littlemex.github.io,resources=filesystemshares/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=persistentvolumes,verbs=get;list;watch
// +kubebuilder:rbac:groups=storage.k8s.io,resources=storageclasses,verbs=get;list;watch;create
// +kubebuilder:rbac:groups=storage.k8s.io,resources=csidrivers,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

func (r *ShareReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var share sharedfsv1alpha1.FilesystemShare
	if err := r.Get(ctx, req.NamespacedName, &share); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !share.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.finalize(ctx, &share)
	}
	if !controllerutil.ContainsFinalizer(&share, sharedfsv1alpha1.Finalizer) {
		patch := client.MergeFrom(share.DeepCopy())
		controllerutil.AddFinalizer(&share, sharedfsv1alpha1.Finalizer)
		if err := r.Patch(ctx, &share, patch); err != nil {
			return ctrl.Result{}, err
		}
	}
	before := share.DeepCopy()
	share.Status.ObservedGeneration = share.Generation

	volumes, err := r.volumes(ctx, r.Client, &share)
	if err != nil {
		return ctrl.Result{}, err
	}
	share.Status.Volumes = int32(len(volumes))

	reason, msg, err := r.check(ctx, &share)
	if err != nil {
		return ctrl.Result{}, err
	}
	if reason == sharedfsv1alpha1.ReasonResolved {
		if taken, err := r.ensureClasses(ctx, &share); err != nil {
			return ctrl.Result{}, err
		} else if taken != "" {
			reason, msg = sharedfsv1alpha1.ReasonStorageClassTaken,
				fmt.Sprintf("StorageClass %s exists and is not this share's; rename the share or remove that class", taken)
		}
	}
	setCondition(&share.Status.Conditions, share.Generation, sharedfsv1alpha1.ConditionReady,
		reason == sharedfsv1alpha1.ReasonResolved, reason, msg)
	return ctrl.Result{}, r.updateStatus(ctx, &share, before)
}

// check runs every safety check in order and returns the first failing reason, or Resolved with
// any warnings in the message. Transient read errors are returned to be retried.
func (r *ShareReconciler) check(ctx context.Context, share *sharedfsv1alpha1.FilesystemShare) (string, string, error) {
	path, err := sharepath.Parse(share.Spec.Path)
	if err != nil {
		return sharedfsv1alpha1.ReasonInvalidPath, err.Error(), nil
	}
	if err := validateGrants(share); err != nil {
		return sharedfsv1alpha1.ReasonInvalidGrant, err.Error(), nil
	}
	var pv corev1.PersistentVolume
	err = r.Get(ctx, types.NamespacedName{Name: share.Spec.PersistentVolumeRef.Name}, &pv)
	if apierrors.IsNotFound(err) {
		return sharedfsv1alpha1.ReasonSourceNotFound, fmt.Sprintf("PersistentVolume %s not found", share.Spec.PersistentVolumeRef.Name), nil
	}
	if err != nil {
		return "", "", err
	}
	src, err := volume.Resolve(&pv)
	if err != nil {
		var se *volume.SourceError
		if errors.As(err, &se) {
			return se.Reason, se.Message, nil
		}
		return "", "", err
	}
	share.Status.Resolved = src.Status()
	if !path.IsRoot() && src.Profile.SubPath == nil {
		return sharedfsv1alpha1.ReasonSubPathUnsupported,
			fmt.Sprintf("driver %s cannot mount a directory below the root; use path \"/\"", src.Profile.Name), nil
	}
	if !src.Reserved {
		return sharedfsv1alpha1.ReasonSourceUnreserved, fmt.Sprintf(
			"PersistentVolume %s is neither bound nor reserved with claimRef, so any namespace could bind it directly and skip the grants; reserve it", pv.Name), nil
	}
	if reason, msg, err := r.checkDriver(ctx, src.Profile.Name); err != nil || reason != "" {
		return reason, msg, err
	}
	var warnings []string
	if overlap, warn, err := r.checkOverlap(ctx, share, src); err != nil {
		return "", "", err
	} else if overlap != "" {
		return sharedfsv1alpha1.ReasonOverlappingShare, overlap, nil
	} else if warn != "" {
		warnings = append(warnings, warn)
	}
	if src.ReservedFor != "" {
		var ns corev1.Namespace
		if err := r.Get(ctx, types.NamespacedName{Name: src.ReservedFor}, &ns); err == nil {
			if ok, _ := grantFor(share, &ns, sharedfsv1alpha1.AccessReadOnly); ok {
				warnings = append(warnings, fmt.Sprintf(
					"the source is bound or reserved to namespace %s, which this share also grants: that namespace can mount the whole file system", ns.Name))
			}
		}
	}
	msg := fmt.Sprintf("%s on %s, path %s", src.VolumeHandle, src.Profile.Name, share.Spec.Path)
	for _, w := range warnings {
		msg += "; warning: " + w
	}
	return sharedfsv1alpha1.ReasonResolved, msg, nil
}

// checkDriver refuses drivers whose CSIDriver object would undo the guarantees: a "File"
// fsGroupPolicy lets kubelet chown the shared tree recursively for any pod's fsGroup, and the
// Ephemeral lifecycle lets pods mount the file system inline without any PersistentVolume.
func (r *ShareReconciler) checkDriver(ctx context.Context, name string) (string, string, error) {
	var d storagev1.CSIDriver
	if err := r.Get(ctx, types.NamespacedName{Name: name}, &d); err != nil {
		if apierrors.IsNotFound(err) {
			return sharedfsv1alpha1.ReasonUnsafeDriverPolicy, fmt.Sprintf("CSIDriver %s not found", name), nil
		}
		return "", "", err
	}
	if p := d.Spec.FSGroupPolicy; p != nil && *p == storagev1.FileFSGroupPolicy {
		return sharedfsv1alpha1.ReasonUnsafeDriverPolicy,
			fmt.Sprintf("CSIDriver %s has fsGroupPolicy File: kubelet would chown the shared directory for every pod's fsGroup", name), nil
	}
	if slices.Contains(d.Spec.VolumeLifecycleModes, storagev1.VolumeLifecycleEphemeral) {
		return sharedfsv1alpha1.ReasonUnsafeDriverPolicy,
			fmt.Sprintf("CSIDriver %s allows inline ephemeral volumes: pods could mount the file system without a grant", name), nil
	}
	return "", "", nil
}

// checkOverlap compares the share with every other share on the same file system. When one
// share's path may contain the other's and the containing share grants ReadWrite, a writer of
// the outer path can rename or plant files in what the inner one exposes. The newer share of
// such a pair is not Ready, so creating a share never stops one already in use. A read-only
// container only warns: its readers can read the inner path.
func (r *ShareReconciler) checkOverlap(ctx context.Context, share *sharedfsv1alpha1.FilesystemShare, src *volume.Source) (string, string, error) {
	var list sharedfsv1alpha1.FilesystemShareList
	if err := r.List(ctx, &list); err != nil {
		return "", "", err
	}
	fs, base := src.Identity()
	self, err := fullPath(base, share.Spec.Path)
	if err != nil {
		// Fail closed: a path that cannot be compared is not provisioned from.
		return fmt.Sprintf("cannot compare path %s on %s with other shares: %v", share.Spec.Path, fs, err), "", nil
	}
	var warnings []string
	for i := range list.Items {
		other := &list.Items[i]
		if other.UID == share.UID || other.Status.Resolved == nil || !other.DeletionTimestamp.IsZero() {
			continue
		}
		osrc := &volume.Source{VolumeAttributes: other.Status.Resolved.VolumeAttributes}
		if p, ok := driver.Lookup(other.Status.Resolved.Driver); ok {
			osrc.Profile = p
		}
		ofs, obase := osrc.Identity()
		if ofs != fs {
			continue
		}
		them, err := fullPath(obase, other.Spec.Path)
		if err != nil {
			continue
		}
		var outer *sharedfsv1alpha1.FilesystemShare
		switch {
		case sharepath.MayContain(them, self):
			outer = other
		case sharepath.MayContain(self, them):
			outer = share
		default:
			continue
		}
		if outer.Annotations[sharedfsv1alpha1.AnnotationAllowOverlap] == "true" {
			continue
		}
		if !grantsReadWrite(outer) {
			warnings = append(warnings, fmt.Sprintf("share %s (path %s) can read what share %s exposes", outer.Name, outer.Spec.Path, other.Name))
			continue
		}
		if newer(share, other) {
			return fmt.Sprintf("share %s (path %s) grants ReadWrite on a path that may contain or be contained by %s; "+
				"a writer of the outer path can replace the inner directory. Annotate the outer share with %s=true if that is intended",
				outer.Name, outer.Spec.Path, share.Spec.Path, sharedfsv1alpha1.AnnotationAllowOverlap), "", nil
		}
	}
	return "", strings.Join(warnings, "; "), nil
}

// fullPath is a share's path from the file system root: the source's own sub-path base (for
// Lustre the mountname, which may itself name a directory) joined with the share's path.
func fullPath(base, path string) (sharepath.Template, error) {
	if base == "" {
		return sharepath.Parse(path)
	}
	return sharepath.Parse(sharepath.Join(base, path))
}

// newer reports whether a should give way to b. Creation times have a resolution of one
// second, so within the same second the share already Ready keeps its place, and the name
// decides only between two shares in the same state. Both shares reach the same verdict.
func newer(a, b *sharedfsv1alpha1.FilesystemShare) bool {
	if !a.CreationTimestamp.Equal(&b.CreationTimestamp) {
		return b.CreationTimestamp.Before(&a.CreationTimestamp)
	}
	if ra, rb := isReady(a), isReady(b); ra != rb {
		return rb
	}
	return a.Name > b.Name
}

func isReady(s *sharedfsv1alpha1.FilesystemShare) bool {
	return meta.IsStatusConditionTrue(s.Status.Conditions, sharedfsv1alpha1.ConditionReady)
}

// ensureClasses creates the share's two StorageClasses. Expansion is disallowed: the API server
// then refuses to resize a claim, so a resize never reaches the driver with a handle that is not
// a file system. It returns the name of a class that exists but is not the share's.
func (r *ShareReconciler) ensureClasses(ctx context.Context, share *sharedfsv1alpha1.FilesystemShare) (string, error) {
	names := ClassNames(share)
	share.Status.StorageClasses = &names
	for _, c := range []struct {
		name   string
		access sharedfsv1alpha1.Access
	}{{names.ReadWrite, sharedfsv1alpha1.AccessReadWrite}, {names.ReadOnly, sharedfsv1alpha1.AccessReadOnly}} {
		var existing storagev1.StorageClass
		err := r.Get(ctx, types.NamespacedName{Name: c.name}, &existing)
		if err == nil {
			if !metav1.IsControlledBy(&existing, share) {
				return c.name, nil
			}
			continue
		}
		if !apierrors.IsNotFound(err) {
			return "", err
		}
		reclaim := corev1.PersistentVolumeReclaimDelete
		immediate := storagev1.VolumeBindingImmediate
		sc := &storagev1.StorageClass{
			ObjectMeta: metav1.ObjectMeta{
				Name:   c.name,
				Labels: map[string]string{sharedfsv1alpha1.LabelShareUID: string(share.UID)},
			},
			Provisioner:          sharedfsv1alpha1.ProvisionerName,
			ReclaimPolicy:        &reclaim,
			AllowVolumeExpansion: new(bool),
			VolumeBindingMode:    &immediate,
			Parameters: map[string]string{
				sharedfsv1alpha1.ParameterShare:    share.Name,
				sharedfsv1alpha1.ParameterShareUID: string(share.UID),
				sharedfsv1alpha1.ParameterAccess:   string(c.access),
			},
		}
		if err := controllerutil.SetControllerReference(share, sc, r.Scheme()); err != nil {
			return "", err
		}
		if err := r.Create(ctx, sc); err != nil && !apierrors.IsAlreadyExists(err) {
			return "", err
		}
	}
	return "", nil
}

// finalize keeps the share while any volume provisioned from it remains (read live: a volume
// created a moment ago may not be cached).
func (r *ShareReconciler) finalize(ctx context.Context, share *sharedfsv1alpha1.FilesystemShare) error {
	if !controllerutil.ContainsFinalizer(share, sharedfsv1alpha1.Finalizer) {
		return nil
	}
	volumes, err := r.volumes(ctx, r.APIReader, share)
	if err != nil {
		return err
	}
	if len(volumes) > 0 {
		before := share.DeepCopy()
		share.Status.Volumes = int32(len(volumes))
		claims := make([]string, 0, 5)
		for i := range volumes {
			if len(claims) == 5 {
				claims = append(claims, "...")
				break
			}
			if ref := volumes[i].Spec.ClaimRef; ref != nil {
				claims = append(claims, ref.Namespace+"/"+ref.Name)
			}
		}
		setCondition(&share.Status.Conditions, share.Generation, sharedfsv1alpha1.ConditionReady, false,
			sharedfsv1alpha1.ReasonVolumesRemaining, fmt.Sprintf(
				"waiting for %d provisioned PersistentVolume(s) to go; delete the claims (%s) or remove their grants",
				len(volumes), strings.Join(claims, ", ")))
		return client.IgnoreNotFound(r.updateStatus(ctx, share, before))
	}
	patch := client.MergeFrom(share.DeepCopy())
	controllerutil.RemoveFinalizer(share, sharedfsv1alpha1.Finalizer)
	return client.IgnoreNotFound(r.Patch(ctx, share, patch))
}

func (r *ShareReconciler) volumes(ctx context.Context, reader client.Reader, share *sharedfsv1alpha1.FilesystemShare) ([]corev1.PersistentVolume, error) {
	var pvs corev1.PersistentVolumeList
	if err := reader.List(ctx, &pvs, client.MatchingLabels{sharedfsv1alpha1.LabelShareUID: string(share.UID)}); err != nil {
		return nil, err
	}
	return slices.DeleteFunc(pvs.Items, func(pv corev1.PersistentVolume) bool { return volume.IsInternal(&pv) }), nil
}

func (r *ShareReconciler) updateStatus(ctx context.Context, share, before *sharedfsv1alpha1.FilesystemShare) error {
	if equality.Semantic.DeepEqual(&before.Status, &share.Status) {
		return nil
	}
	return r.Status().Patch(ctx, share, client.MergeFrom(before))
}

// SetupWithManager sets up the controller with the Manager.
func (r *ShareReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&sharedfsv1alpha1.FilesystemShare{}).
		Owns(&storagev1.StorageClass{}).
		Watches(&corev1.PersistentVolume{}, handler.EnqueueRequestsFromMapFunc(r.sharesForPV)).
		// Any share change can change another share's overlap verdict.
		Watches(&sharedfsv1alpha1.FilesystemShare{}, handler.EnqueueRequestsFromMapFunc(r.allShares)).
		Named("filesystemshare").
		Complete(r)
}

func (r *ShareReconciler) sharesForPV(ctx context.Context, o client.Object) []reconcile.Request {
	if name := o.GetAnnotations()[sharedfsv1alpha1.AnnotationShareName]; name != "" {
		return toRequests([]types.NamespacedName{{Name: name}})
	}
	var list sharedfsv1alpha1.FilesystemShareList
	if err := r.List(ctx, &list, client.MatchingFields{indexShareSource: o.GetName()}); err != nil {
		logf.FromContext(ctx).Error(err, "listing shares of a source", "pv", o.GetName())
		return nil
	}
	keys := make([]types.NamespacedName, 0, len(list.Items))
	for _, s := range list.Items {
		keys = append(keys, types.NamespacedName{Name: s.Name})
	}
	return toRequests(keys)
}

func (r *ShareReconciler) allShares(ctx context.Context, _ client.Object) []reconcile.Request {
	var list sharedfsv1alpha1.FilesystemShareList
	if err := r.List(ctx, &list); err != nil {
		return nil
	}
	keys := make([]types.NamespacedName, 0, len(list.Items))
	for _, s := range list.Items {
		keys = append(keys, types.NamespacedName{Name: s.Name})
	}
	return toRequests(keys)
}
