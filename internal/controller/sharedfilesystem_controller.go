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
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	sharedfsv1alpha1 "github.com/littlemex/sharedfs/api/v1alpha1"
	"github.com/littlemex/sharedfs/internal/volume"
)

// SharedFilesystemReconciler resolves a SharedFilesystem's source and guards its deletion.
// It writes the SharedFilesystem status only; claims read status.resolved.
type SharedFilesystemReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=sharedfs.littlemex.github.io,resources=sharedfilesystems,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=sharedfs.littlemex.github.io,resources=sharedfilesystems/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=sharedfs.littlemex.github.io,resources=sharedfilesystems/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=persistentvolumes,verbs=get;list;watch

func (r *SharedFilesystemReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var fs sharedfsv1alpha1.SharedFilesystem
	if err := r.Get(ctx, req.NamespacedName, &fs); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	before := fs.Status.DeepCopy()

	// What holds the file system is the generated PVs, not the claims: a rejected claim holds
	// nothing and must not block the admin.
	var pvs corev1.PersistentVolumeList
	if err := r.List(ctx, &pvs, client.MatchingLabels{volume.LabelFilesystemUID: string(fs.UID)}); err != nil {
		return ctrl.Result{}, err
	}
	var bound int32
	for i := range pvs.Items {
		if pvs.Items[i].Status.Phase == corev1.VolumeBound {
			bound++
		}
	}
	fs.Status.BoundClaims = bound

	if !fs.DeletionTimestamp.IsZero() {
		if len(pvs.Items) > 0 {
			setCondition(&fs.Status.Conditions, fs.Generation, sharedfsv1alpha1.ConditionDeleting, true,
				sharedfsv1alpha1.ReasonVolumesRemaining,
				fmt.Sprintf("waiting for %d generated PersistentVolume(s); delete the SharedFilesystemClaims that use this filesystem", len(pvs.Items)))
			return ctrl.Result{}, r.patchStatus(ctx, &fs, before)
		}
		if controllerutil.RemoveFinalizer(&fs, volume.Finalizer) {
			log.Info("no generated volumes left, releasing filesystem")
			return ctrl.Result{}, r.Update(ctx, &fs)
		}
		return ctrl.Result{}, nil
	}

	if controllerutil.AddFinalizer(&fs, volume.Finalizer) {
		if err := r.Update(ctx, &fs); err != nil {
			return ctrl.Result{}, err
		}
	}

	r.resolve(ctx, &fs)
	fs.Status.ObservedGeneration = fs.Generation
	return ctrl.Result{}, r.patchStatus(ctx, &fs, before)
}

// resolve reads the source PV into status. On failure the last resolved copy is kept for
// reference, and SourceResolved=False tells claims not to use it.
func (r *SharedFilesystemReconciler) resolve(ctx context.Context, fs *sharedfsv1alpha1.SharedFilesystem) {
	set := func(ok bool, reason, msg string) {
		setCondition(&fs.Status.Conditions, fs.Generation, sharedfsv1alpha1.ConditionSourceResolved, ok, reason, msg)
		setCondition(&fs.Status.Conditions, fs.Generation, sharedfsv1alpha1.ConditionReady, ok, reason, msg)
	}

	var src corev1.PersistentVolume
	err := r.Get(ctx, types.NamespacedName{Name: fs.Spec.Source.PersistentVolumeName}, &src)
	if apierrors.IsNotFound(err) {
		set(false, sharedfsv1alpha1.ReasonSourceNotFound,
			fmt.Sprintf("PersistentVolume %s not found", fs.Spec.Source.PersistentVolumeName))
		return
	}
	if err != nil {
		set(false, sharedfsv1alpha1.ReasonSourceNotFound, err.Error())
		return
	}
	resolved, err := volume.Resolve(&src)
	var se *volume.SourceError
	if errors.As(err, &se) {
		set(false, se.Reason, se.Message)
		return
	}
	fs.Status.Resolved = resolved
	set(true, sharedfsv1alpha1.ReasonResolved,
		fmt.Sprintf("%s on %s", resolved.VolumeHandle, resolved.Driver))

	reclaimDelete := src.Spec.PersistentVolumeReclaimPolicy == corev1.PersistentVolumeReclaimDelete
	if reclaimDelete {
		setCondition(&fs.Status.Conditions, fs.Generation, sharedfsv1alpha1.ConditionSourceReclaimDelete, true,
			sharedfsv1alpha1.ReasonReclaimDelete,
			"the source PersistentVolume has reclaimPolicy Delete: deleting its PersistentVolumeClaim deletes the file system for every tenant")
	} else {
		setCondition(&fs.Status.Conditions, fs.Generation, sharedfsv1alpha1.ConditionSourceReclaimDelete, false,
			sharedfsv1alpha1.ReasonReclaimRetain, "")
	}
}

func (r *SharedFilesystemReconciler) patchStatus(ctx context.Context, fs *sharedfsv1alpha1.SharedFilesystem, before *sharedfsv1alpha1.SharedFilesystemStatus) error {
	if equality.Semantic.DeepEqual(before, &fs.Status) {
		return nil
	}
	return r.Status().Update(ctx, fs)
}

// SetupWithManager sets up the controller with the Manager.
func (r *SharedFilesystemReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&sharedfsv1alpha1.SharedFilesystem{}).
		// A PV event reaches a SharedFilesystem two ways: as its source, or as a volume
		// generated from it (bound count, deletion guard).
		Watches(&corev1.PersistentVolume{}, handler.EnqueueRequestsFromMapFunc(r.filesystemsForPV)).
		Named("sharedfilesystem").
		Complete(r)
}

func (r *SharedFilesystemReconciler) filesystemsForPV(ctx context.Context, pv client.Object) []reconcile.Request {
	if isGenerated(pv) {
		if name := pv.GetAnnotations()[volume.AnnotationFilesystemName]; name != "" {
			return requests(types.NamespacedName{Name: name})
		}
		return nil
	}
	var list sharedfsv1alpha1.SharedFilesystemList
	if err := r.List(ctx, &list, client.MatchingFields{indexFilesystemSource: pv.GetName()}); err != nil {
		logf.FromContext(ctx).Error(err, "listing filesystems for source", "pv", pv.GetName())
		return nil
	}
	out := make([]types.NamespacedName, 0, len(list.Items))
	for _, fs := range list.Items {
		out = append(out, types.NamespacedName{Name: fs.Name})
	}
	return requests(out...)
}
