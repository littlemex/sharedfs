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
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	sharedfsv1alpha1 "github.com/littlemex/sharedfs/api/v1alpha1"
	"github.com/littlemex/sharedfs/internal/driver"
	"github.com/littlemex/sharedfs/internal/volume"
)

// SharedFilesystemClaimReconciler turns a claim into a reserved PersistentVolume and a
// PersistentVolumeClaim of the claim's name, and keeps the pair bound.
type SharedFilesystemClaimReconciler struct {
	client.Client
	// APIReader reads around the cache where acting on a stale read would be unsafe.
	APIReader client.Reader
	Scheme    *runtime.Scheme
	Recorder  events.EventRecorder
}

// +kubebuilder:rbac:groups=sharedfs.littlemex.github.io,resources=sharedfilesystemclaims,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=sharedfs.littlemex.github.io,resources=sharedfilesystemclaims/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=sharedfs.littlemex.github.io,resources=sharedfilesystemclaims/finalizers,verbs=update
// +kubebuilder:rbac:groups=sharedfs.littlemex.github.io,resources=sharedfilesystems,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=persistentvolumes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

func (r *SharedFilesystemClaimReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var claim sharedfsv1alpha1.SharedFilesystemClaim
	if err := r.Get(ctx, req.NamespacedName, &claim); err != nil {
		// A claim that is gone without its finalizer having run leaves its PV to the
		// orphan reconciler, which works from the PV side.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	before := claim.Status.DeepCopy()
	claim.Status.ObservedGeneration = claim.Generation

	if !claim.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, &claim, before)
	}
	if controllerutil.AddFinalizer(&claim, volume.Finalizer) {
		if err := r.Update(ctx, &claim); err != nil {
			return ctrl.Result{}, err
		}
	}

	fs, profile, reason, msg, err := r.admit(ctx, &claim)
	if err != nil {
		return ctrl.Result{}, err
	}
	if reason != sharedfsv1alpha1.ReasonAccepted {
		r.setAccepted(&claim, false, reason, msg)
		if sharedfsv1alpha1.Revokes(reason) {
			if _, err := r.release(ctx, &claim); err != nil {
				return ctrl.Result{}, err
			}
		}
		r.setReady(&claim)
		return ctrl.Result{}, r.patchStatus(ctx, &claim, before)
	}

	if err := r.ensure(ctx, &claim, fs, profile); err != nil {
		return ctrl.Result{}, err
	}
	r.setReady(&claim)
	return ctrl.Result{}, r.patchStatus(ctx, &claim, before)
}

// admit decides whether the claim may mount the filesystem now. The rules depend on objects
// other than the claim (namespace labels, the filesystem's selector and maxAccess) that change
// after admission, which is why they live in reconcile and not in a webhook.
func (r *SharedFilesystemClaimReconciler) admit(ctx context.Context, claim *sharedfsv1alpha1.SharedFilesystemClaim) (*sharedfsv1alpha1.SharedFilesystem, driver.Profile, string, string, error) {
	var none driver.Profile
	var fs sharedfsv1alpha1.SharedFilesystem
	err := r.Get(ctx, types.NamespacedName{Name: claim.Spec.FilesystemRef.Name}, &fs)
	if apierrors.IsNotFound(err) {
		return nil, none, sharedfsv1alpha1.ReasonFilesystemNotFound,
			fmt.Sprintf("SharedFilesystem %s not found", claim.Spec.FilesystemRef.Name), nil
	}
	if err != nil {
		return nil, none, "", "", err
	}
	if !fs.DeletionTimestamp.IsZero() {
		return nil, none, sharedfsv1alpha1.ReasonFilesystemDeleting,
			fmt.Sprintf("SharedFilesystem %s is being deleted", fs.Name), nil
	}

	// Authorization first: it does not depend on the source being readable.
	var ns corev1.Namespace
	if err := r.Get(ctx, types.NamespacedName{Name: claim.Namespace}, &ns); err != nil {
		return nil, none, "", "", err
	}
	allowed, err := namespaceAllowed(&fs, &ns)
	if err != nil {
		return nil, none, sharedfsv1alpha1.ReasonFilesystemNotReady, err.Error(), nil
	}
	if !allowed {
		return nil, none, sharedfsv1alpha1.ReasonNamespaceNotAllowed,
			fmt.Sprintf("namespace %s is not selected by SharedFilesystem %s", claim.Namespace, fs.Name), nil
	}
	if !maxAccess(&fs).Allows(claim.Spec.Access) {
		return nil, none, sharedfsv1alpha1.ReasonAccessNotAllowed,
			fmt.Sprintf("SharedFilesystem %s allows at most %s", fs.Name, maxAccess(&fs)), nil
	}

	// status.resolved is the contract between the two controllers: read it only while it was
	// computed from the current spec and is known good.
	if fs.Status.ObservedGeneration != fs.Generation ||
		!meta.IsStatusConditionTrue(fs.Status.Conditions, sharedfsv1alpha1.ConditionSourceResolved) ||
		fs.Status.Resolved == nil {
		return nil, none, sharedfsv1alpha1.ReasonFilesystemNotReady,
			fmt.Sprintf("SharedFilesystem %s has not resolved its source", fs.Name), nil
	}
	profile, ok := driver.Lookup(fs.Status.Resolved.Driver)
	if !ok {
		return nil, none, sharedfsv1alpha1.ReasonFilesystemNotReady,
			fmt.Sprintf("driver %s is not supported", fs.Status.Resolved.Driver), nil
	}
	return &fs, profile, sharedfsv1alpha1.ReasonAccepted, "", nil
}

func namespaceAllowed(fs *sharedfsv1alpha1.SharedFilesystem, ns *corev1.Namespace) (bool, error) {
	if fs.Spec.AllowedNamespaces == nil {
		return false, nil
	}
	sel, err := metav1.LabelSelectorAsSelector(&fs.Spec.AllowedNamespaces.Selector)
	if err != nil {
		return false, fmt.Errorf("SharedFilesystem %s has an invalid namespace selector: %w", fs.Name, err)
	}
	return sel.Matches(labels.Set(ns.Labels)), nil
}

func maxAccess(fs *sharedfsv1alpha1.SharedFilesystem) sharedfsv1alpha1.Access {
	if fs.Spec.MaxAccess == "" {
		return sharedfsv1alpha1.AccessReadOnly
	}
	return fs.Spec.MaxAccess
}

// ensure creates or repairs the PV and the PVC for an accepted claim.
func (r *SharedFilesystemClaimReconciler) ensure(ctx context.Context, claim *sharedfsv1alpha1.SharedFilesystemClaim,
	fs *sharedfsv1alpha1.SharedFilesystem, profile driver.Profile) error {
	desired, err := volume.PV(claim, fs, profile)
	if err != nil {
		r.setAccepted(claim, false, sharedfsv1alpha1.ReasonFilesystemNotReady, err.Error())
		return nil
	}
	r.setAccepted(claim, true, sharedfsv1alpha1.ReasonAccepted,
		fmt.Sprintf("%s access to SharedFilesystem %s", claim.Spec.Access, fs.Name))

	pvName := volume.PVName(claim.UID)
	var pv corev1.PersistentVolume
	err = r.Get(ctx, types.NamespacedName{Name: pvName}, &pv)
	switch {
	case apierrors.IsNotFound(err):
	case err != nil:
		return err
	case !pv.DeletionTimestamp.IsZero():
		// A revoked PV is still going away; apply would not bring it back.
		setCondition(&claim.Status.Conditions, claim.Generation, sharedfsv1alpha1.ConditionBound, false,
			sharedfsv1alpha1.ReasonPending, fmt.Sprintf("waiting for the previous PersistentVolume %s to be deleted", pvName))
		return nil
	default:
		// The volume source of a PV cannot change; applying a different one only fails.
		if drift := volume.ImmutableDrift(&pv, desired); len(drift) > 0 {
			setCondition(&claim.Status.Conditions, claim.Generation, sharedfsv1alpha1.ConditionBound, false,
				sharedfsv1alpha1.ReasonSourceChanged,
				fmt.Sprintf("the source changed (%s); delete and recreate the claim to remount", strings.Join(drift, ", ")))
			return nil
		}
	}
	// The PVC comes first: if its name is taken by someone else's PVC, no PV is reserved for
	// that name at all.
	pvc, err := r.ensurePVC(ctx, claim)
	if err != nil || pvc == nil {
		return err
	}
	claim.Status.PersistentVolumeClaimName = pvc.Name

	if err := r.Apply(ctx, desired, client.FieldOwner(volume.FieldOwner), client.ForceOwnership); err != nil {
		return fmt.Errorf("applying PersistentVolume %s: %w", pvName, err)
	}
	claim.Status.PersistentVolumeName = pvName

	if err := r.heal(ctx, claim, pvName, pvc); err != nil {
		return err
	}

	switch {
	case pvc.Status.Phase == corev1.ClaimBound && pvc.Spec.VolumeName == pvName:
		setCondition(&claim.Status.Conditions, claim.Generation, sharedfsv1alpha1.ConditionBound, true,
			sharedfsv1alpha1.ReasonBound, fmt.Sprintf("PersistentVolumeClaim %s is bound to %s", pvc.Name, pvName))
	default:
		setCondition(&claim.Status.Conditions, claim.Generation, sharedfsv1alpha1.ConditionBound, false,
			sharedfsv1alpha1.ReasonPending, fmt.Sprintf("PersistentVolumeClaim %s is %s", pvc.Name, pvc.Status.Phase))
	}
	return nil
}

// ensurePVC creates the PVC once and afterwards only checks it. It returns nil and sets a
// condition when the PVC cannot be used yet.
func (r *SharedFilesystemClaimReconciler) ensurePVC(ctx context.Context, claim *sharedfsv1alpha1.SharedFilesystemClaim) (*corev1.PersistentVolumeClaim, error) {
	key := types.NamespacedName{Namespace: claim.Namespace, Name: volume.PVCName(claim)}
	var pvc corev1.PersistentVolumeClaim
	err := r.Get(ctx, key, &pvc)
	if apierrors.IsNotFound(err) {
		want := volume.PVC(claim)
		if err := controllerutil.SetControllerReference(claim, want, r.Scheme); err != nil {
			return nil, err
		}
		err = r.Create(ctx, want)
		if err == nil {
			return want, nil
		}
		if !apierrors.IsAlreadyExists(err) {
			return nil, err
		}
		// The cache had not seen it yet: decide on the live object, not on a guess.
		err = r.APIReader.Get(ctx, key, &pvc)
	}
	if err != nil {
		return nil, err
	}

	if !volume.OwnsPVC(claim.UID, &pvc) {
		r.setAccepted(claim, false, sharedfsv1alpha1.ReasonPVCConflict,
			fmt.Sprintf("PersistentVolumeClaim %s already exists and is not bound to this claim's volume; rename the claim or delete that PVC", pvc.Name))
		return nil, nil
	}
	if !pvc.DeletionTimestamp.IsZero() {
		setCondition(&claim.Status.Conditions, claim.Generation, sharedfsv1alpha1.ConditionBound, false,
			sharedfsv1alpha1.ReasonPVCTerminating,
			fmt.Sprintf("PersistentVolumeClaim %s is being deleted (pods may still use it); it is recreated once gone", pvc.Name))
		return nil, nil
	}
	// The ownerReference only lets the garbage collector delete the PVC with the claim. The
	// tenant can remove it, so put it back; ownership itself was decided by OwnsPVC above.
	if !metav1.IsControlledBy(&pvc, claim) {
		patch := client.MergeFrom(pvc.DeepCopy())
		if err := controllerutil.SetControllerReference(claim, &pvc, r.Scheme); err != nil {
			r.setAccepted(claim, false, sharedfsv1alpha1.ReasonPVCConflict, err.Error())
			return nil, nil
		}
		if err := r.Patch(ctx, &pvc, patch); err != nil {
			return nil, err
		}
	}
	return &pvc, nil
}

// heal clears the stale claim UID that a Retain PV keeps after its PVC was deleted and
// recreated under the same name. Only uid and resourceVersion go: removing the whole claimRef
// would make the PV claimable by any namespace that names it.
func (r *SharedFilesystemClaimReconciler) heal(ctx context.Context, claim *sharedfsv1alpha1.SharedFilesystemClaim,
	pvName string, cached *corev1.PersistentVolumeClaim) error {
	var pv corev1.PersistentVolume
	if err := r.APIReader.Get(ctx, types.NamespacedName{Name: pvName}, &pv); err != nil {
		return client.IgnoreNotFound(err)
	}
	if pv.Status.Phase != corev1.VolumeReleased {
		return nil
	}
	var live corev1.PersistentVolumeClaim
	if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(cached), &live); err != nil {
		return client.IgnoreNotFound(err)
	}
	if !volume.OwnsPVC(claim.UID, &live) || !volume.StaleClaimUID(&pv, &live) {
		return nil
	}
	ops := []map[string]any{
		// Fails the patch if the PV changed since the read above.
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
		return fmt.Errorf("clearing the stale claim on %s: %w", pvName, err)
	}
	r.Recorder.Eventf(claim, &pv, corev1.EventTypeNormal, "Rebound", "Heal",
		"cleared the stale claim UID on %s so PersistentVolumeClaim %s can bind", pvName, live.Name)
	return nil
}

// release deletes the claim's PVC and then its PV, one step per call, and reports whether both
// are gone. The PVC stays (Terminating) while pods use it, which keeps running pods working and
// stops new ones; the PVC's deletion event brings the next step.
func (r *SharedFilesystemClaimReconciler) release(ctx context.Context, claim *sharedfsv1alpha1.SharedFilesystemClaim) (bool, error) {
	key := types.NamespacedName{Namespace: claim.Namespace, Name: volume.PVCName(claim)}
	var pvc corev1.PersistentVolumeClaim
	err := r.Get(ctx, key, &pvc)
	if err != nil && !apierrors.IsNotFound(err) {
		return false, err
	}
	if err == nil && volume.OwnsPVC(claim.UID, &pvc) {
		if pvc.DeletionTimestamp.IsZero() {
			if err := r.Delete(ctx, &pvc); client.IgnoreNotFound(err) != nil {
				return false, err
			}
		}
		return false, nil
	}

	var pv corev1.PersistentVolume
	err = r.Get(ctx, types.NamespacedName{Name: volume.PVName(claim.UID)}, &pv)
	if apierrors.IsNotFound(err) {
		claim.Status.PersistentVolumeClaimName = ""
		claim.Status.PersistentVolumeName = ""
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if pv.DeletionTimestamp.IsZero() {
		if err := r.Delete(ctx, &pv); client.IgnoreNotFound(err) != nil {
			return false, err
		}
	}
	return false, nil
}

// finalize releases the volumes, then lets the claim go.
func (r *SharedFilesystemClaimReconciler) finalize(ctx context.Context, claim *sharedfsv1alpha1.SharedFilesystemClaim,
	before *sharedfsv1alpha1.SharedFilesystemClaimStatus) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(claim, volume.Finalizer) {
		return ctrl.Result{}, nil
	}
	done, err := r.release(ctx, claim)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !done {
		setCondition(&claim.Status.Conditions, claim.Generation, sharedfsv1alpha1.ConditionReady, false,
			sharedfsv1alpha1.ReasonDeleting,
			"waiting for the PersistentVolumeClaim and PersistentVolume to be deleted; pods that mount the PVC keep it alive")
		// The PVC's ownerReference may have been removed by the tenant, so its deletion is not
		// guaranteed to bring an event back here.
		return ctrl.Result{RequeueAfter: releasePollInterval}, r.patchStatus(ctx, claim, before)
	}
	controllerutil.RemoveFinalizer(claim, volume.Finalizer)
	return ctrl.Result{}, r.Update(ctx, claim)
}

func (r *SharedFilesystemClaimReconciler) setAccepted(claim *sharedfsv1alpha1.SharedFilesystemClaim, ok bool, reason, msg string) {
	setCondition(&claim.Status.Conditions, claim.Generation, sharedfsv1alpha1.ConditionAccepted, ok, reason, msg)
}

// setReady derives Ready from Accepted and Bound, carrying the reason of the first one that
// is not True.
func (r *SharedFilesystemClaimReconciler) setReady(claim *sharedfsv1alpha1.SharedFilesystemClaim) {
	for _, t := range []string{sharedfsv1alpha1.ConditionAccepted, sharedfsv1alpha1.ConditionBound} {
		c := meta.FindStatusCondition(claim.Status.Conditions, t)
		if c == nil || c.Status != metav1.ConditionTrue || c.ObservedGeneration != claim.Generation {
			reason, msg := sharedfsv1alpha1.ReasonPending, t+" is not yet known"
			if c != nil && c.Status != metav1.ConditionTrue {
				reason, msg = c.Reason, c.Message
			}
			setCondition(&claim.Status.Conditions, claim.Generation, sharedfsv1alpha1.ConditionReady, false, reason, msg)
			return
		}
	}
	setCondition(&claim.Status.Conditions, claim.Generation, sharedfsv1alpha1.ConditionReady, true,
		sharedfsv1alpha1.ReasonReady, fmt.Sprintf("mount PersistentVolumeClaim %s", claim.Status.PersistentVolumeClaimName))
}

func (r *SharedFilesystemClaimReconciler) patchStatus(ctx context.Context, claim *sharedfsv1alpha1.SharedFilesystemClaim,
	before *sharedfsv1alpha1.SharedFilesystemClaimStatus) error {
	if equality.Semantic.DeepEqual(before, &claim.Status) {
		return nil
	}
	return r.Status().Update(ctx, claim)
}

// SetupWithManager sets up the controller with the Manager.
func (r *SharedFilesystemClaimReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&sharedfsv1alpha1.SharedFilesystemClaim{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		// The generated PV is cluster-scoped and cannot carry an ownerReference to a namespaced
		// claim, so it is mapped back through its labels.
		Watches(&corev1.PersistentVolume{}, handler.EnqueueRequestsFromMapFunc(claimForPV),
			builder.WithPredicates(predicate.NewPredicateFuncs(isGenerated))).
		Watches(&sharedfsv1alpha1.SharedFilesystem{}, handler.EnqueueRequestsFromMapFunc(r.claimsForFilesystem)).
		Watches(&corev1.Namespace{}, handler.EnqueueRequestsFromMapFunc(r.claimsInNamespace),
			builder.WithPredicates(predicate.LabelChangedPredicate{})).
		Named("sharedfilesystemclaim").
		Complete(r)
}

func claimForPV(_ context.Context, pv client.Object) []reconcile.Request {
	key, _, ok := claimOfPV(pv)
	if !ok {
		return nil
	}
	return requests(key)
}

func (r *SharedFilesystemClaimReconciler) claimsForFilesystem(ctx context.Context, fs client.Object) []reconcile.Request {
	var list sharedfsv1alpha1.SharedFilesystemClaimList
	if err := r.List(ctx, &list, client.MatchingFields{indexClaimFilesystem: fs.GetName()}); err != nil {
		logf.FromContext(ctx).Error(err, "listing claims for filesystem", "filesystem", fs.GetName())
		return nil
	}
	out := make([]types.NamespacedName, 0, len(list.Items))
	for _, c := range list.Items {
		out = append(out, client.ObjectKeyFromObject(&c))
	}
	return requests(out...)
}

func (r *SharedFilesystemClaimReconciler) claimsInNamespace(ctx context.Context, ns client.Object) []reconcile.Request {
	var list sharedfsv1alpha1.SharedFilesystemClaimList
	if err := r.List(ctx, &list, client.InNamespace(ns.GetName())); err != nil {
		logf.FromContext(ctx).Error(err, "listing claims in namespace", "namespace", ns.GetName())
		return nil
	}
	out := make([]types.NamespacedName, 0, len(list.Items))
	for _, c := range list.Items {
		out = append(out, client.ObjectKeyFromObject(&c))
	}
	return requests(out...)
}
