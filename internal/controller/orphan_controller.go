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

// OrphanReconciler removes generated volumes whose claim no longer exists.
//
// The claim's finalizer normally does this. It cannot when the finalizer was removed by hand or
// the claim came back from a backup with a new UID. Such a PV still reserves a namespace and PVC
// name with no UID, so a namespace recreated later under that name would bind it without any
// grant. Keyed on the PV, this reconciler also sweeps every leftover once at startup, when the
// cache first lists all PVs.
type OrphanReconciler struct {
	client.Client
	// APIReader confirms a claim is gone without trusting a cache that may not have seen it yet.
	APIReader client.Reader
}

func (r *OrphanReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var pv corev1.PersistentVolume
	if err := r.Get(ctx, req.NamespacedName, &pv); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	// The claim UID is read from the PV name, which only cluster admins can change.
	uid, ok := volume.ClaimUIDOf(pv.Name)
	if !ok || !volume.IsGenerated(&pv) {
		return ctrl.Result{}, nil
	}
	key, ok := claimRefOf(&pv)
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
	logf.FromContext(ctx).Info("releasing volumes of a claim that no longer exists", "pv", pv.Name, "pvc", key)
	_, err = releaseVolumes(ctx, r.Client, r.APIReader, uid, key)
	return ctrl.Result{}, err
}

// SetupWithManager sets up the controller with the Manager.
func (r *OrphanReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.PersistentVolume{}, builder.WithPredicates(predicate.NewPredicateFuncs(func(o client.Object) bool {
			return volume.IsGenerated(o)
		}))).
		// The two events that can make a PV an orphan, or move its release on, without
		// changing the PV itself: the claim going away and its PVC going away.
		Watches(&sharedfsv1alpha1.SharedFilesystemClaim{}, handler.EnqueueRequestsFromMapFunc(pvForClaim)).
		Watches(&corev1.PersistentVolumeClaim{}, handler.EnqueueRequestsFromMapFunc(pvForPVC)).
		Named("orphan-volume").
		Complete(r)
}

func pvForClaim(_ context.Context, c client.Object) []reconcile.Request {
	return toRequests([]types.NamespacedName{{Name: volume.PVName(c.GetUID())}})
}

// pvForPVC maps by volumeName, which the tenant cannot repoint, rather than by labels.
func pvForPVC(_ context.Context, o client.Object) []reconcile.Request {
	pvc, ok := o.(*corev1.PersistentVolumeClaim)
	if !ok {
		return nil
	}
	if _, ok := volume.ClaimUIDOf(pvc.Spec.VolumeName); !ok {
		return nil
	}
	return toRequests([]types.NamespacedName{{Name: pvc.Spec.VolumeName}})
}
