package controller

import (
	"context"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/littlemex/sharedfs/internal/volume"
)

// orphanMinAge keeps the sweep away from a volume created a moment ago, whose claim the cache
// may not show yet.
const orphanMinAge = 2 * time.Minute

// ReclaimReconciler deletes sharedfs volumes whose claims are gone. A volume marked
// provisioned-by sharedfs is ignored by the PV controller and by the driver's provisioner, so
// when its claim is deleted it stays Released until sharedfs deletes it. Only the
// PersistentVolume object goes: the handle is not a file system, and nothing is asked of the
// driver.
type ReclaimReconciler struct {
	client.Client
	APIReader client.Reader
}

// +kubebuilder:rbac:groups="",resources=persistentvolumes,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch

func (r *ReclaimReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var pv corev1.PersistentVolume
	if err := r.Get(ctx, req.NamespacedName, &pv); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !volume.ProvisionedBySharedfs(&pv) || !pv.DeletionTimestamp.IsZero() ||
		pv.Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimDelete {
		return ctrl.Result{}, nil
	}
	if pv.Status.Phase != corev1.VolumeReleased {
		// Not released yet: delete only when the claim is gone on a live read and the volume is
		// old enough that the claim cannot just be lagging in the cache.
		ref := pv.Spec.ClaimRef
		if ref == nil {
			return ctrl.Result{}, nil
		}
		var pvc corev1.PersistentVolumeClaim
		err := r.APIReader.Get(ctx, types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}, &pvc)
		if err == nil && pvc.UID == ref.UID {
			return ctrl.Result{}, nil
		}
		if err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		if age := time.Since(pv.CreationTimestamp.Time); age < orphanMinAge {
			return ctrl.Result{RequeueAfter: orphanMinAge - age}, nil
		}
	}
	if err := r.Delete(ctx, &pv, client.Preconditions{UID: &pv.UID}); client.IgnoreNotFound(err) != nil && !apierrors.IsConflict(err) {
		return ctrl.Result{}, err
	}
	logf.FromContext(ctx).Info("deleted volume whose claim is gone", "pv", pv.Name, "phase", pv.Status.Phase)
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ReclaimReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.PersistentVolume{}).
		Named("reclaim").
		Complete(r)
}
