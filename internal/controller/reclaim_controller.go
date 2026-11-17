package controller

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/littlemex/sharedfs/internal/metrics"
	"github.com/littlemex/sharedfs/internal/volume"
)

// ReclaimReconciler deletes sharedfs volumes whose claims are gone. A volume marked
// provisioned-by sharedfs is ignored by the PV controller and by the driver's provisioner, so
// when its claim is deleted it stays Released until sharedfs deletes it. Only the
// PersistentVolume object goes: the handle is not a file system, and nothing is asked of the
// driver.
type ReclaimReconciler struct {
	client.Client
}

// +kubebuilder:rbac:groups="",resources=persistentvolumes,verbs=get;list;watch;delete

func (r *ReclaimReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var pv corev1.PersistentVolume
	if err := r.Get(ctx, req.NamespacedName, &pv); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !volume.ProvisionedBySharedfs(&pv) || !pv.DeletionTimestamp.IsZero() ||
		pv.Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimDelete {
		return ctrl.Result{}, nil
	}
	// The PV controller marks a volume Released once its claim is gone, bound or not; sharedfs
	// acts on nothing earlier, so a claim the cache has not seen yet cannot cost a volume.
	if pv.Status.Phase != corev1.VolumeReleased {
		return ctrl.Result{}, nil
	}
	err := r.Delete(ctx, &pv, client.Preconditions{UID: &pv.UID})
	switch {
	case err == nil:
		metrics.Reclaimed.Inc()
		logf.FromContext(ctx).Info("deleted released volume", "pv", pv.Name)
	case apierrors.IsNotFound(err), apierrors.IsConflict(err):
	default:
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ReclaimReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.PersistentVolume{}).
		Named("reclaim").
		Complete(r)
}
