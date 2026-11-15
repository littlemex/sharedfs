package controller

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	sharedfsv1alpha1 "github.com/littlemex/sharedfs/api/v1alpha1"
	"github.com/littlemex/sharedfs/internal/volume"
)

// OrphanReconciler removes generated PersistentVolumes whose claim no longer exists.
//
// The claim's finalizer normally does this. It cannot when the finalizer was removed by hand or
// the claim came back from a backup with a new UID. Such a PV still reserves a namespace and PVC
// name with no UID, so a namespace recreated later under the same name would bind it without
// ever being allowed by a SharedFilesystem. Keyed on the PV, this reconciler also sweeps every
// leftover once at startup, when the cache first lists all PVs.
type OrphanReconciler struct {
	client.Client
	// APIReader confirms a claim is gone without trusting a cache that may not have seen it yet.
	APIReader client.Reader
}

func (r *OrphanReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var pv corev1.PersistentVolume
	if err := r.Get(ctx, req.NamespacedName, &pv); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	key, uid, ok := claimOfPV(&pv)
	if !ok {
		return ctrl.Result{}, nil
	}

	var claim sharedfsv1alpha1.SharedFilesystemClaim
	err := r.APIReader.Get(ctx, key, &claim)
	if err == nil && claim.UID == uid {
		return ctrl.Result{}, nil
	}
	if err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}

	// The claim is gone (or is a different claim of the same name). PVC first, then the PV.
	if pvcKey, ok := pvcOf(&pv); ok {
		var pvc corev1.PersistentVolumeClaim
		err := r.APIReader.Get(ctx, pvcKey, &pvc)
		if err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		if err == nil && volume.OwnsPVC(uid, &pvc) {
			if pvc.DeletionTimestamp.IsZero() {
				log.Info("deleting PVC of a claim that no longer exists", "pvc", pvcKey, "claimUID", uid)
				if err := r.Delete(ctx, &pvc); client.IgnoreNotFound(err) != nil {
					return ctrl.Result{}, err
				}
			}
			// This reconciler watches PVs only, and the PVC's disappearance is not guaranteed to
			// change the PV, so come back for the PV rather than wait for an event.
			return ctrl.Result{RequeueAfter: releasePollInterval}, nil
		}
	}
	if pv.DeletionTimestamp.IsZero() {
		log.Info("deleting PV of a claim that no longer exists", "pv", pv.Name, "claimUID", uid)
		return ctrl.Result{}, client.IgnoreNotFound(r.Delete(ctx, &pv))
	}
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *OrphanReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.PersistentVolume{}, builder.WithPredicates(predicate.NewPredicateFuncs(isGenerated))).
		// The two events that can make a PV an orphan without changing the PV itself.
		Watches(&sharedfsv1alpha1.SharedFilesystemClaim{}, handler.EnqueueRequestsFromMapFunc(pvOfClaim)).
		Watches(&corev1.PersistentVolumeClaim{}, handler.EnqueueRequestsFromMapFunc(pvOfPVC),
			builder.WithPredicates(predicate.NewPredicateFuncs(isGenerated))).
		Named("orphan-volume").
		Complete(r)
}

func pvOfClaim(_ context.Context, c client.Object) []reconcile.Request {
	return requests(types.NamespacedName{Name: volume.PVName(c.GetUID())})
}

func pvOfPVC(_ context.Context, o client.Object) []reconcile.Request {
	pvc, ok := o.(*corev1.PersistentVolumeClaim)
	if !ok || pvc.Spec.VolumeName == "" {
		return nil
	}
	return requests(types.NamespacedName{Name: pvc.Spec.VolumeName})
}
