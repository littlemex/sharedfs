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
	"strings"

	corev1 "k8s.io/api/core/v1"
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
	"github.com/littlemex/sharedfs/internal/volume"
)

// SharedFilesystemReconciler reports whether a SharedFilesystem's source can be distributed and
// keeps the SharedFilesystem until no generated volume points at it. It writes only the
// SharedFilesystem's status; what to mount is decided by the claim controller from the source
// PersistentVolume itself.
type SharedFilesystemReconciler struct {
	client.Client
	// APIReader confirms there are no generated volumes left before the finalizer is removed.
	APIReader client.Reader
	Recorder  events.EventRecorder
}

// +kubebuilder:rbac:groups=sharedfs.littlemex.github.io,resources=sharedfilesystems,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=sharedfs.littlemex.github.io,resources=sharedfilesystems/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=sharedfs.littlemex.github.io,resources=sharedfilesystems/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=persistentvolumes,verbs=get;list;watch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

func (r *SharedFilesystemReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var fs sharedfsv1alpha1.SharedFilesystem
	if err := r.Get(ctx, req.NamespacedName, &fs); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !fs.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, &fs)
	}
	if !controllerutil.ContainsFinalizer(&fs, sharedfsv1alpha1.Finalizer) {
		patch := client.MergeFrom(fs.DeepCopy())
		controllerutil.AddFinalizer(&fs, sharedfsv1alpha1.Finalizer)
		if err := r.Patch(ctx, &fs, patch); err != nil {
			return ctrl.Result{}, err
		}
	}

	before := fs.DeepCopy()
	pvs, err := r.generatedVolumes(ctx, r.Client, &fs)
	if err != nil {
		return ctrl.Result{}, err
	}
	fs.Status.Volumes = int32(len(pvs))
	fs.Status.ObservedGeneration = fs.Generation

	reason, msg, err := r.resolve(ctx, &fs)
	if err != nil {
		return ctrl.Result{}, err
	}
	status := metav1.ConditionFalse
	if reason == sharedfsv1alpha1.ReasonResolved {
		status = metav1.ConditionTrue
	}
	setCondition(&fs.Status.Conditions, fs.Generation, sharedfsv1alpha1.ConditionReady, status, reason, msg)
	return ctrl.Result{}, r.updateStatus(ctx, &fs, before)
}

// resolve checks the grants and the source PersistentVolume. Transient read errors are
// returned, to be retried, rather than written as a reason.
func (r *SharedFilesystemReconciler) resolve(ctx context.Context, fs *sharedfsv1alpha1.SharedFilesystem) (string, string, error) {
	if err := validateGrants(fs); err != nil {
		return sharedfsv1alpha1.ReasonInvalidGrant, err.Error(), nil
	}
	var pv corev1.PersistentVolume
	err := r.Get(ctx, types.NamespacedName{Name: fs.Spec.PersistentVolumeRef.Name}, &pv)
	if apierrors.IsNotFound(err) {
		return sharedfsv1alpha1.ReasonSourceNotFound, fmt.Sprintf("PersistentVolume %s not found", fs.Spec.PersistentVolumeRef.Name), nil
	}
	if err != nil {
		return "", "", err
	}
	src, err := volume.Resolve(&pv)
	if err != nil {
		var se *volume.SourceError
		if !errors.As(err, &se) {
			return "", "", err
		}
		return se.Reason, se.Message, nil
	}
	fs.Status.Resolved = src.Status()
	msg := fmt.Sprintf("%s on %s", src.VolumeHandle, src.Profile.Name)
	prev := meta.FindStatusCondition(fs.Status.Conditions, sharedfsv1alpha1.ConditionReady)
	if pv.Spec.ClaimRef == nil {
		// Not sharedfs's to fix, but it defeats the grants: any namespace can bind the source
		// itself by naming it in volumeName.
		msg += unreservedWarning
		if prev == nil || !strings.Contains(prev.Message, unreservedWarning) {
			r.Recorder.Eventf(fs, &pv, corev1.EventTypeWarning, "SourceUnreserved", "Resolve",
				"PersistentVolume %s has no claimRef: any namespace can bind it directly by naming it in a PVC's volumeName, bypassing the grants; reserve it with claimRef", pv.Name)
		}
	}
	if pv.Spec.PersistentVolumeReclaimPolicy == corev1.PersistentVolumeReclaimDelete {
		// Not a failure: generated volumes are Retain. But whoever deletes the source's own PVC
		// deletes the file system under every tenant, so say so where the admin looks.
		msg += reclaimDeleteWarning
		if prev == nil || !strings.Contains(prev.Message, reclaimDeleteWarning) {
			r.Recorder.Eventf(fs, &pv, corev1.EventTypeWarning, "SourceReclaimDelete", "Resolve",
				"PersistentVolume %s has reclaimPolicy Delete: deleting its PersistentVolumeClaim deletes the file system for every tenant", pv.Name)
		}
	}
	return sharedfsv1alpha1.ReasonResolved, msg, nil
}

const (
	reclaimDeleteWarning = "; warning: the source PersistentVolume has reclaimPolicy Delete"
	unreservedWarning    = "; warning: the source PersistentVolume has no claimRef, so any namespace can bind it directly"
)

// finalize keeps the SharedFilesystem while any generated volume points at it.
func (r *SharedFilesystemReconciler) finalize(ctx context.Context, fs *sharedfsv1alpha1.SharedFilesystem) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(fs, sharedfsv1alpha1.Finalizer) {
		return ctrl.Result{}, nil
	}
	// Live: a volume created a moment ago may not be in the cache, and removing the finalizer
	// would leave it with no filesystem to answer to.
	pvs, err := r.generatedVolumes(ctx, r.APIReader, fs)
	if err != nil {
		return ctrl.Result{}, err
	}
	if len(pvs) > 0 {
		before := fs.DeepCopy()
		fs.Status.Volumes = int32(len(pvs))
		setCondition(&fs.Status.Conditions, fs.Generation, sharedfsv1alpha1.ConditionReady, metav1.ConditionFalse,
			sharedfsv1alpha1.ReasonVolumesRemaining, remainingMessage(pvs))
		return ctrl.Result{}, client.IgnoreNotFound(r.updateStatus(ctx, fs, before))
	}
	patch := client.MergeFrom(fs.DeepCopy())
	controllerutil.RemoveFinalizer(fs, sharedfsv1alpha1.Finalizer)
	return ctrl.Result{}, client.IgnoreNotFound(r.Patch(ctx, fs, patch))
}

func remainingMessage(pvs []corev1.PersistentVolume) string {
	const show = 5
	var claims []string
	for i := range pvs {
		if len(claims) == show {
			claims = append(claims, "...")
			break
		}
		if key, ok := claimRefOf(&pvs[i]); ok {
			claims = append(claims, key.String())
		}
	}
	return fmt.Sprintf("waiting for %d generated PersistentVolume(s) to go; delete the claims (%s) or remove their grants",
		len(pvs), strings.Join(claims, ", "))
}

func (r *SharedFilesystemReconciler) generatedVolumes(ctx context.Context, reader client.Reader, fs *sharedfsv1alpha1.SharedFilesystem) ([]corev1.PersistentVolume, error) {
	var pvs corev1.PersistentVolumeList
	err := reader.List(ctx, &pvs, client.MatchingLabels{sharedfsv1alpha1.LabelFilesystemUID: string(fs.UID)})
	return pvs.Items, err
}

func (r *SharedFilesystemReconciler) updateStatus(ctx context.Context, fs *sharedfsv1alpha1.SharedFilesystem, before *sharedfsv1alpha1.SharedFilesystem) error {
	if equality.Semantic.DeepEqual(&before.Status, &fs.Status) {
		return nil
	}
	return r.Status().Patch(ctx, fs, client.MergeFrom(before))
}

// SetupWithManager sets up the controller with the Manager.
func (r *SharedFilesystemReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&sharedfsv1alpha1.SharedFilesystem{}).
		// A PV event reaches a SharedFilesystem as its source, or as a volume generated from it
		// (the volume count and the deletion guard).
		Watches(&corev1.PersistentVolume{}, handler.EnqueueRequestsFromMapFunc(r.filesystemsForPV)).
		Named("sharedfilesystem").
		Complete(r)
}

func (r *SharedFilesystemReconciler) filesystemsForPV(ctx context.Context, o client.Object) []reconcile.Request {
	if volume.IsGenerated(o) {
		if name := o.GetAnnotations()[sharedfsv1alpha1.AnnotationFilesystemName]; name != "" {
			return toRequests([]types.NamespacedName{{Name: name}})
		}
		return nil
	}
	keys, err := filesystemsUsing(ctx, r.Client, o.GetName())
	if err != nil {
		logf.FromContext(ctx).Error(err, "listing filesystems for a source", "pv", o.GetName())
	}
	return toRequests(keys)
}

// filesystemsUsing lists the SharedFilesystems whose source is the named PersistentVolume.
func filesystemsUsing(ctx context.Context, c client.Reader, pvName string) ([]types.NamespacedName, error) {
	var list sharedfsv1alpha1.SharedFilesystemList
	if err := c.List(ctx, &list, client.MatchingFields{indexFilesystemVolume: pvName}); err != nil {
		return nil, err
	}
	keys := make([]types.NamespacedName, 0, len(list.Items))
	for _, fs := range list.Items {
		keys = append(keys, types.NamespacedName{Name: fs.Name})
	}
	return keys, nil
}
