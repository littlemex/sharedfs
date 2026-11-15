/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	sharedfsv1alpha1 "github.com/littlemex/sharedfs/api/v1alpha1"
	"github.com/littlemex/sharedfs/internal/volume"
)

// SharedFilesystemClaimReconciler turns a claim into a reserved PersistentVolume and a
// PersistentVolumeClaim of the claim's name, keeps the pair bound, and withdraws it when the
// namespace is no longer granted.
//
// It enforces one invariant: a generated volume exists only while a SharedFilesystem exists and
// grants its namespace the access the volume has.
type SharedFilesystemClaimReconciler struct {
	client.Client
	// APIReader reads around the cache where acting on a stale read would be unsafe.
	APIReader client.Reader
	Scheme    *runtime.Scheme
	Recorder  events.EventRecorder
	// RevocationGracePeriod is how long a rejection that withdraws access must last before the
	// volumes are deleted, so a label that disappears for a moment (a GitOps sync, a typo fixed
	// seconds later) does not take a running workload's volume away. It is counted from the
	// Accepted condition's lastTransitionTime, which has one-second resolution.
	RevocationGracePeriod time.Duration
}

// +kubebuilder:rbac:groups=sharedfs.littlemex.github.io,resources=sharedfilesystemclaims,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=sharedfs.littlemex.github.io,resources=sharedfilesystemclaims/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=sharedfs.littlemex.github.io,resources=sharedfilesystemclaims/finalizers,verbs=update
// +kubebuilder:rbac:groups=sharedfs.littlemex.github.io,resources=sharedfilesystems,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=persistentvolumes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

// admission is the decision on whether a claim may mount now.
type admission struct {
	reason  string
	message string
	// revoke is set for the reasons that withdraw access: the grant is gone.
	revoke bool
	fs     *sharedfsv1alpha1.SharedFilesystem
	src    *volume.Source
}

func (a admission) accepted() bool { return a.reason == sharedfsv1alpha1.ReasonAccepted }

func (r *SharedFilesystemClaimReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var claim sharedfsv1alpha1.SharedFilesystemClaim
	if err := r.Get(ctx, req.NamespacedName, &claim); err != nil {
		// A claim gone without its finalizer having run leaves its volumes to the orphan
		// reconciler, which works from the PV side.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !claim.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, &claim)
	}
	if !controllerutil.ContainsFinalizer(&claim, sharedfsv1alpha1.Finalizer) {
		patch := client.MergeFrom(claim.DeepCopy())
		controllerutil.AddFinalizer(&claim, sharedfsv1alpha1.Finalizer)
		if err := r.Patch(ctx, &claim, patch); err != nil {
			return ctrl.Result{}, err
		}
	}
	before := claim.DeepCopy()
	claim.Status.ObservedGeneration = claim.Generation

	a, err := r.admit(ctx, r.Client, &claim)
	if err != nil {
		return ctrl.Result{}, err
	}
	r.setAccepted(&claim, a)

	var (
		result        ctrl.Result
		seen          *corev1.PersistentVolumeClaim
		createErr     error
		sourceChanged bool
		revoking      bool
	)
	switch {
	case a.accepted():
		seen, createErr, sourceChanged, err = r.ensure(ctx, &claim, a)
	case a.revoke:
		result, revoking, err = r.revoke(ctx, &claim)
	}
	if obsErr := r.observeBinding(ctx, &claim, seen, createErr); obsErr != nil && err == nil {
		err = obsErr
	}
	r.setReady(&claim, sourceChanged, revoking)
	// Status first, so a failure is visible on the claim and not only in the controller log.
	if statusErr := r.updateStatus(ctx, &claim, before); statusErr != nil && err == nil {
		err = statusErr
	}
	return result, errors.Join(err, createErr)
}

// admit decides whether the claim may mount. The rules depend on objects other than the claim
// (namespace labels, the grants, the source) that change after admission, which is why they run
// here and not in an admission webhook. Authorization comes before every lifecycle check, so a
// grant removed while the filesystem is being deleted still withdraws access.
func (r *SharedFilesystemClaimReconciler) admit(ctx context.Context, reader client.Reader, claim *sharedfsv1alpha1.SharedFilesystemClaim) (admission, error) {
	var fs sharedfsv1alpha1.SharedFilesystem
	err := reader.Get(ctx, types.NamespacedName{Name: claim.Spec.FilesystemRef.Name}, &fs)
	if apierrors.IsNotFound(err) {
		return admission{reason: sharedfsv1alpha1.ReasonFilesystemNotFound, revoke: true,
			message: fmt.Sprintf("SharedFilesystem %s does not exist", claim.Spec.FilesystemRef.Name)}, nil
	}
	if err != nil {
		return admission{}, err
	}
	var ns corev1.Namespace
	if err := reader.Get(ctx, types.NamespacedName{Name: claim.Namespace}, &ns); err != nil {
		return admission{}, err
	}
	access, granted, err := grantedAccess(&fs, &ns)
	if err != nil {
		return admission{reason: sharedfsv1alpha1.ReasonFilesystemNotReady,
			message: fmt.Sprintf("SharedFilesystem %s: %v", fs.Name, err)}, nil
	}
	if !granted {
		return admission{reason: sharedfsv1alpha1.ReasonNamespaceNotGranted, revoke: true,
			message: fmt.Sprintf("no grant of SharedFilesystem %s selects namespace %s", fs.Name, ns.Name)}, nil
	}
	if !access.Allows(claim.Spec.Access) {
		return admission{reason: sharedfsv1alpha1.ReasonAccessNotGranted, revoke: true,
			message: fmt.Sprintf("SharedFilesystem %s grants namespace %s %s access at most", fs.Name, ns.Name, access)}, nil
	}
	if !fs.DeletionTimestamp.IsZero() {
		return admission{reason: sharedfsv1alpha1.ReasonFilesystemDeleting,
			message: fmt.Sprintf("SharedFilesystem %s is being deleted; existing volumes stay until the claim is deleted", fs.Name)}, nil
	}

	// The source is read here rather than from the SharedFilesystem status, so there is no
	// second copy to go stale between the two controllers.
	var pv corev1.PersistentVolume
	err = reader.Get(ctx, types.NamespacedName{Name: fs.Spec.PersistentVolumeRef.Name}, &pv)
	if apierrors.IsNotFound(err) {
		return admission{reason: sharedfsv1alpha1.ReasonFilesystemNotReady,
			message: fmt.Sprintf("source PersistentVolume %s of SharedFilesystem %s not found", fs.Spec.PersistentVolumeRef.Name, fs.Name)}, nil
	}
	if err != nil {
		return admission{}, err
	}
	src, err := volume.Resolve(&pv)
	var se *volume.SourceError
	if errors.As(err, &se) {
		return admission{reason: sharedfsv1alpha1.ReasonFilesystemNotReady,
			message: fmt.Sprintf("SharedFilesystem %s: %s", fs.Name, se.Message)}, nil
	}
	if err != nil {
		return admission{}, err
	}
	return admission{reason: sharedfsv1alpha1.ReasonAccepted, fs: &fs, src: src,
		message: fmt.Sprintf("%s access to SharedFilesystem %s", claim.Spec.Access, fs.Name)}, nil
}

// ensure creates or repairs the PVC and the PV of an accepted claim. It returns the PVC it saw,
// the error of a failed PVC creation (to show on the claim), and whether the source changed
// under an existing PV.
func (r *SharedFilesystemClaimReconciler) ensure(ctx context.Context, claim *sharedfsv1alpha1.SharedFilesystemClaim, a admission) (
	pvc *corev1.PersistentVolumeClaim, createErr error, sourceChanged bool, err error) {
	desired := volume.PV(claim, a.fs, a.src)

	var pv corev1.PersistentVolume
	err = r.Get(ctx, types.NamespacedName{Name: volume.PVName(claim.UID)}, &pv)
	switch {
	case apierrors.IsNotFound(err):
	case err != nil:
		return nil, nil, false, err
	case !pv.DeletionTimestamp.IsZero():
		// Deleted by someone; apply would not bring it back. The claim reports
		// VolumeTerminating and is reconciled again when the PV is gone.
		return nil, nil, false, nil
	case len(volume.CSIDrift(&pv, desired)) > 0:
		// The volume source of a PV cannot change. Keep the existing one so the mutable fields
		// (the ro option, the claimRef) are still enforced, and report it.
		sourceChanged = true
		volume.KeepCSI(desired, &pv)
	}

	// The PVC comes first: if its name is taken by someone else's PVC, no PV is reserved for it.
	pvc, createErr, err = r.ensurePVC(ctx, claim, a.src)
	if err != nil || createErr != nil || pvc == nil || !volume.OwnsPVC(claim.UID, pvc) || !pvc.DeletionTimestamp.IsZero() {
		return pvc, createErr, sourceChanged, err
	}

	if err := r.Apply(ctx, desired, client.FieldOwner(volume.FieldOwner), client.ForceOwnership); err != nil {
		return pvc, nil, sourceChanged, fmt.Errorf("applying PersistentVolume %s: %w", *desired.Name, err)
	}
	if pv.Status.Phase == corev1.VolumeReleased {
		if err := r.heal(ctx, claim, pvc); err != nil {
			return pvc, nil, sourceChanged, err
		}
	}
	return pvc, nil, sourceChanged, nil
}

// ensurePVC creates the PVC once and afterwards only checks it. The second error is a failed
// creation, shown on the claim; the third is any other failure.
func (r *SharedFilesystemClaimReconciler) ensurePVC(ctx context.Context, claim *sharedfsv1alpha1.SharedFilesystemClaim, src *volume.Source) (
	*corev1.PersistentVolumeClaim, error, error) {
	key := client.ObjectKeyFromObject(claim)
	var pvc corev1.PersistentVolumeClaim
	err := r.Get(ctx, key, &pvc)
	if apierrors.IsNotFound(err) {
		want := volume.PVC(claim, src)
		if err := controllerutil.SetControllerReference(claim, want, r.Scheme); err != nil {
			return nil, nil, err
		}
		err = r.Create(ctx, want)
		if err == nil {
			return want, nil, nil
		}
		if !apierrors.IsAlreadyExists(err) {
			// For example a ResourceQuota or LimitRange in the namespace.
			return nil, err, nil
		}
		// The cache had not seen it yet: decide on the live object.
		err = r.APIReader.Get(ctx, key, &pvc)
	}
	if err != nil {
		return nil, nil, err
	}
	if !volume.OwnsPVC(claim.UID, &pvc) || !pvc.DeletionTimestamp.IsZero() {
		return &pvc, nil, nil
	}
	if pvc.Status.Phase == corev1.ClaimLost {
		// Its PV was deleted from under it. A Lost PVC never rebinds, so start over; the
		// deletion event brings the claim back here to create a fresh one.
		err := r.Delete(ctx, &pvc, client.Preconditions{UID: &pvc.UID})
		return &pvc, nil, client.IgnoreNotFound(err)
	}
	// The ownerReference only lets the garbage collector delete the PVC with the claim. The
	// tenant can remove it, so put it back; ownership was decided by OwnsPVC above.
	if !metav1.IsControlledBy(&pvc, claim) {
		patch := client.MergeFrom(pvc.DeepCopy())
		if err := controllerutil.SetControllerReference(claim, &pvc, r.Scheme); err == nil {
			if err := r.Patch(ctx, &pvc, patch); err != nil {
				return &pvc, nil, err
			}
		}
	}
	return &pvc, nil, nil
}

// heal clears the stale claim UID a Retain PV keeps after its PVC was deleted and recreated
// under the same name. Only uid and resourceVersion go: removing the whole claimRef would make
// the PV claimable by any namespace that names it. Both objects are read live and the patch is
// conditional on the PV not having changed since.
func (r *SharedFilesystemClaimReconciler) heal(ctx context.Context, claim *sharedfsv1alpha1.SharedFilesystemClaim, cached *corev1.PersistentVolumeClaim) error {
	var pv corev1.PersistentVolume
	if err := r.APIReader.Get(ctx, types.NamespacedName{Name: volume.PVName(claim.UID)}, &pv); err != nil {
		return client.IgnoreNotFound(err)
	}
	var live corev1.PersistentVolumeClaim
	if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(cached), &live); err != nil {
		return client.IgnoreNotFound(err)
	}
	if !volume.OwnsPVC(claim.UID, &live) || !volume.StaleClaimUID(&pv, &live) {
		return nil
	}
	ops := []map[string]any{
		{"op": "test", "path": "/metadata/resourceVersion", "value": pv.ResourceVersion},
		{"op": "remove", "path": "/spec/claimRef/uid"},
	}
	if pv.Spec.ClaimRef.ResourceVersion != "" {
		ops = append(ops, map[string]any{"op": "remove", "path": "/spec/claimRef/resourceVersion"})
	}
	body, err := json.Marshal(ops)
	if err != nil {
		return err
	}
	if err := r.Patch(ctx, &pv, client.RawPatch(types.JSONPatchType, body)); err != nil {
		return fmt.Errorf("clearing the stale claim UID on %s: %w", pv.Name, err)
	}
	r.Recorder.Eventf(claim, &pv, corev1.EventTypeNormal, "Rebinding", "Heal",
		"cleared the stale claim UID on %s so PersistentVolumeClaim %s can bind", pv.Name, live.Name)
	return nil
}

// revoke withdraws access once the rejection has lasted the grace period and still holds on a
// live read. It reports whether volumes are being withdrawn.
func (r *SharedFilesystemClaimReconciler) revoke(ctx context.Context, claim *sharedfsv1alpha1.SharedFilesystemClaim) (ctrl.Result, bool, error) {
	cond := meta.FindStatusCondition(claim.Status.Conditions, sharedfsv1alpha1.ConditionAccepted)
	if wait := r.RevocationGracePeriod - time.Since(cond.LastTransitionTime.Time); wait > 0 {
		return ctrl.Result{RequeueAfter: wait}, false, nil
	}
	live, err := r.admit(ctx, r.APIReader, claim)
	if err != nil {
		return ctrl.Result{}, false, err
	}
	if !live.revoke {
		// The cache was behind; the next event brings the claim back with the real state.
		return ctrl.Result{}, false, nil
	}
	done, err := releaseVolumes(ctx, r.Client, r.APIReader, claim.UID, client.ObjectKeyFromObject(claim))
	return ctrl.Result{}, !done, err
}

// finalize releases the volumes, then lets the claim go.
func (r *SharedFilesystemClaimReconciler) finalize(ctx context.Context, claim *sharedfsv1alpha1.SharedFilesystemClaim) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(claim, sharedfsv1alpha1.Finalizer) {
		return ctrl.Result{}, nil
	}
	done, err := releaseVolumes(ctx, r.Client, r.APIReader, claim.UID, client.ObjectKeyFromObject(claim))
	if err != nil {
		return ctrl.Result{}, err
	}
	if !done {
		before := claim.DeepCopy()
		setCondition(&claim.Status.Conditions, claim.Generation, sharedfsv1alpha1.ConditionReady, metav1.ConditionFalse,
			sharedfsv1alpha1.ReasonDeleting,
			"deleting the PersistentVolumeClaim, then the PersistentVolume; pods that mount the PVC keep it until they stop")
		return ctrl.Result{}, r.updateStatus(ctx, claim, before)
	}
	patch := client.MergeFrom(claim.DeepCopy())
	controllerutil.RemoveFinalizer(claim, sharedfsv1alpha1.Finalizer)
	return ctrl.Result{}, r.Patch(ctx, claim, patch)
}

// observeBinding sets Bound from what exists, on every path, so it is never left over from an
// earlier state. seen is the PVC ensure just read or created, newer than the cache.
func (r *SharedFilesystemClaimReconciler) observeBinding(ctx context.Context, claim *sharedfsv1alpha1.SharedFilesystemClaim,
	seen *corev1.PersistentVolumeClaim, createErr error) error {
	pvName := volume.PVName(claim.UID)
	pvc := seen
	if pvc == nil {
		var got corev1.PersistentVolumeClaim
		err := r.Get(ctx, client.ObjectKeyFromObject(claim), &got)
		if err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		if err == nil {
			pvc = &got
		}
	}
	var pv corev1.PersistentVolume
	err := r.Get(ctx, types.NamespacedName{Name: pvName}, &pv)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	pvExists := err == nil
	claim.Status.PersistentVolumeName = ""
	if pvExists {
		claim.Status.PersistentVolumeName = pvName
	}

	set := func(ok bool, reason, msg string) {
		status := metav1.ConditionFalse
		if ok {
			status = metav1.ConditionTrue
		}
		setCondition(&claim.Status.Conditions, claim.Generation, sharedfsv1alpha1.ConditionBound, status, reason, msg)
	}
	switch {
	case pvc == nil && createErr != nil:
		set(false, sharedfsv1alpha1.ReasonPVCCreateFailed, fmt.Sprintf("creating PersistentVolumeClaim %s: %v", claim.Name, createErr))
	case pvc == nil:
		set(false, sharedfsv1alpha1.ReasonNoVolume, "no PersistentVolumeClaim exists for this claim")
	case !volume.OwnsPVC(claim.UID, pvc):
		set(false, sharedfsv1alpha1.ReasonPVCConflict,
			fmt.Sprintf("PersistentVolumeClaim %s already exists and is not this claim's; rename the claim or delete that PVC", pvc.Name))
	case !pvc.DeletionTimestamp.IsZero():
		set(false, sharedfsv1alpha1.ReasonPVCTerminating,
			fmt.Sprintf("PersistentVolumeClaim %s is being deleted; pods that mount it keep it until they stop", pvc.Name))
	case pvc.Status.Phase == corev1.ClaimLost:
		set(false, sharedfsv1alpha1.ReasonPVCLost, fmt.Sprintf("PersistentVolumeClaim %s lost its volume and is recreated", pvc.Name))
	case pvExists && !pv.DeletionTimestamp.IsZero():
		set(false, sharedfsv1alpha1.ReasonVolumeTerminating,
			fmt.Sprintf("PersistentVolume %s is being deleted; it is recreated once gone", pvName))
	case pvc.Status.Phase == corev1.ClaimBound:
		set(true, sharedfsv1alpha1.ReasonBound, fmt.Sprintf("PersistentVolumeClaim %s is bound to %s", pvc.Name, pvName))
	default:
		set(false, sharedfsv1alpha1.ReasonPVCPending, fmt.Sprintf("PersistentVolumeClaim %s is %s", pvc.Name, pvc.Status.Phase))
	}
	return nil
}

func (r *SharedFilesystemClaimReconciler) setAccepted(claim *sharedfsv1alpha1.SharedFilesystemClaim, a admission) {
	status := metav1.ConditionFalse
	if a.accepted() {
		status = metav1.ConditionTrue
	}
	setCondition(&claim.Status.Conditions, claim.Generation, sharedfsv1alpha1.ConditionAccepted, status, a.reason, a.message)
}

// setReady derives Ready from Accepted and Bound and carries the cause of the first one that
// does not hold.
func (r *SharedFilesystemClaimReconciler) setReady(claim *sharedfsv1alpha1.SharedFilesystemClaim, sourceChanged, revoking bool) {
	set := func(ok bool, reason, msg string) {
		status := metav1.ConditionFalse
		if ok {
			status = metav1.ConditionTrue
		}
		setCondition(&claim.Status.Conditions, claim.Generation, sharedfsv1alpha1.ConditionReady, status, reason, msg)
	}
	accepted := meta.FindStatusCondition(claim.Status.Conditions, sharedfsv1alpha1.ConditionAccepted)
	bound := meta.FindStatusCondition(claim.Status.Conditions, sharedfsv1alpha1.ConditionBound)
	switch {
	case revoking:
		set(false, sharedfsv1alpha1.ReasonRevoking, accepted.Message+
			"; the PersistentVolumeClaim is deleted, and pods already running keep their mount until they stop")
	case accepted.Status != metav1.ConditionTrue:
		set(false, accepted.Reason, accepted.Message)
	case bound.Status != metav1.ConditionTrue:
		set(false, bound.Reason, bound.Message)
	case sourceChanged:
		set(false, sharedfsv1alpha1.ReasonSourceChanged,
			"the source PersistentVolume's CSI fields changed and this volume keeps the old ones; recreate the claim to remount")
	default:
		set(true, sharedfsv1alpha1.ReasonReady, fmt.Sprintf("mount PersistentVolumeClaim %s", claim.Name))
	}
}

func (r *SharedFilesystemClaimReconciler) updateStatus(ctx context.Context, claim *sharedfsv1alpha1.SharedFilesystemClaim,
	before *sharedfsv1alpha1.SharedFilesystemClaim) error {
	if equality.Semantic.DeepEqual(&before.Status, &claim.Status) {
		return nil
	}
	// A merge patch of the status this controller alone writes: no read-modify-write
	// conflict with the finalizer patch or a cache that is one write behind.
	return r.Status().Patch(ctx, claim, client.MergeFrom(before))
}

// SetupWithManager sets up the controller with the Manager.
func (r *SharedFilesystemClaimReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&sharedfsv1alpha1.SharedFilesystemClaim{}).
		// The PVC carries the claim's name, so any PVC event maps straight to the claim of that
		// name: also a foreign PVC whose deletion clears a PVCConflict, which an
		// ownerReference-based watch would never see.
		Watches(&corev1.PersistentVolumeClaim{}, handler.EnqueueRequestsFromMapFunc(claimForPVC)).
		Watches(&corev1.PersistentVolume{}, handler.EnqueueRequestsFromMapFunc(r.claimsForPV)).
		Watches(&sharedfsv1alpha1.SharedFilesystem{}, handler.EnqueueRequestsFromMapFunc(r.claimsForFilesystem),
			builder.WithPredicates(specOrDeletionChanged)).
		Watches(&corev1.Namespace{}, handler.EnqueueRequestsFromMapFunc(r.claimsForNamespace),
			builder.WithPredicates(predicate.LabelChangedPredicate{})).
		Named("sharedfilesystemclaim").
		Complete(r)
}

// specOrDeletionChanged passes the SharedFilesystem events that can change a claim's admission,
// and not the status updates the SharedFilesystem controller makes as volumes bind.
var specOrDeletionChanged = predicate.Funcs{
	UpdateFunc: func(e event.UpdateEvent) bool {
		return e.ObjectOld.GetGeneration() != e.ObjectNew.GetGeneration() ||
			e.ObjectOld.GetDeletionTimestamp().IsZero() != e.ObjectNew.GetDeletionTimestamp().IsZero()
	},
}

func claimForPVC(_ context.Context, pvc client.Object) []reconcile.Request {
	return toRequests([]types.NamespacedName{client.ObjectKeyFromObject(pvc)})
}

// claimsForPV maps a generated PV to its claim, and a source PV to every claim of the
// SharedFilesystems that use it.
func (r *SharedFilesystemClaimReconciler) claimsForPV(ctx context.Context, o client.Object) []reconcile.Request {
	pv, ok := o.(*corev1.PersistentVolume)
	if !ok {
		return nil
	}
	if volume.IsGenerated(pv) {
		if key, ok := claimRefOf(pv); ok {
			return toRequests([]types.NamespacedName{key})
		}
		return nil
	}
	filesystems, err := filesystemsUsing(ctx, r.Client, pv.Name)
	if err != nil {
		logf.FromContext(ctx).Error(err, "listing filesystems for a source", "pv", pv.Name)
		return nil
	}
	var keys []types.NamespacedName
	for _, fs := range filesystems {
		keys = append(keys, r.claimsOf(ctx, fs.Name)...)
	}
	return toRequests(keys)
}

func (r *SharedFilesystemClaimReconciler) claimsForFilesystem(ctx context.Context, fs client.Object) []reconcile.Request {
	return toRequests(r.claimsOf(ctx, fs.GetName()))
}

func (r *SharedFilesystemClaimReconciler) claimsOf(ctx context.Context, filesystem string) []types.NamespacedName {
	var list sharedfsv1alpha1.SharedFilesystemClaimList
	if err := r.List(ctx, &list, client.MatchingFields{indexClaimFilesystem: filesystem}); err != nil {
		logf.FromContext(ctx).Error(err, "listing claims of a filesystem", "filesystem", filesystem)
		return nil
	}
	keys := make([]types.NamespacedName, 0, len(list.Items))
	for i := range list.Items {
		keys = append(keys, client.ObjectKeyFromObject(&list.Items[i]))
	}
	return keys
}

func (r *SharedFilesystemClaimReconciler) claimsForNamespace(ctx context.Context, ns client.Object) []reconcile.Request {
	var list sharedfsv1alpha1.SharedFilesystemClaimList
	if err := r.List(ctx, &list, client.InNamespace(ns.GetName())); err != nil {
		logf.FromContext(ctx).Error(err, "listing claims in a namespace", "namespace", ns.GetName())
		return nil
	}
	keys := make([]types.NamespacedName, 0, len(list.Items))
	for i := range list.Items {
		keys = append(keys, client.ObjectKeyFromObject(&list.Items[i]))
	}
	return toRequests(keys)
}
