package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
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
	"github.com/littlemex/sharedfs/internal/sharepath"
	"github.com/littlemex/sharedfs/internal/volume"
)

// eventRefresh re-emits a Pending claim's reason before its Event expires (about an hour), so
// `kubectl describe pvc` keeps saying why.
const eventRefresh = 30 * time.Minute

// ProvisionerReconciler is the external provisioner of the shares' StorageClasses. For a
// Pending claim it checks the grant and the directory record and creates the PersistentVolume;
// for a claim bound to one of its volumes it withdraws the claim once the namespace has lost the
// grant for the grace period.
type ProvisionerReconciler struct {
	client.Client
	APIReader client.Reader
	Recorder  events.EventRecorder

	// RevocationGracePeriod is how long a claim stays after its namespace lost the grant.
	RevocationGracePeriod time.Duration
}

// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups="",resources=persistentvolumes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch
// +kubebuilder:rbac:groups=storage.k8s.io,resources=storageclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups=sharedfs.littlemex.github.io,resources=filesystemshares,verbs=get;list;watch
// +kubebuilder:rbac:groups=sharedfs.littlemex.github.io,resources=filesystemsharedirectories,verbs=get;list;watch;create
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

func (r *ProvisionerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var pvc corev1.PersistentVolumeClaim
	if err := r.Get(ctx, req.NamespacedName, &pvc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !pvc.DeletionTimestamp.IsZero() || volume.IsInternal(&pvc) {
		return ctrl.Result{}, nil
	}
	if pvc.Spec.VolumeName != "" {
		return r.reconcileBound(ctx, &pvc)
	}
	if volume.ClaimedProvisioner(&pvc) != sharedfsv1alpha1.ProvisionerName || pvc.Spec.StorageClassName == nil {
		return ctrl.Result{}, nil
	}
	return r.provision(ctx, &pvc)
}

// provision creates the PersistentVolume of a Pending claim, or records on the claim why not.
func (r *ProvisionerReconciler) provision(ctx context.Context, pvc *corev1.PersistentVolumeClaim) (ctrl.Result, error) {
	share, access, err := classOwner(ctx, r.Client, *pvc.Spec.StorageClassName)
	if apierrors.IsNotFound(err) {
		return ctrl.Result{}, nil
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	if why := unsupported(pvc); why != "" {
		return r.pending(pvc, sharedfsv1alpha1.EventUnsupportedClaim, why)
	}
	ready := meta.FindStatusCondition(share.Status.Conditions, sharedfsv1alpha1.ConditionReady)
	if ready == nil || ready.Status != metav1.ConditionTrue || ready.ObservedGeneration != share.Generation || !share.DeletionTimestamp.IsZero() {
		msg := "the share has not been checked yet"
		if !share.DeletionTimestamp.IsZero() {
			msg = "the share is being deleted"
		} else if ready != nil && ready.Status == metav1.ConditionFalse {
			msg = ready.Reason + ": " + ready.Message
		}
		return r.pending(pvc, sharedfsv1alpha1.EventShareNotReady, fmt.Sprintf("FilesystemShare %s is not Ready: %s", share.Name, msg))
	}
	var ns corev1.Namespace
	if err := r.Get(ctx, types.NamespacedName{Name: pvc.Namespace}, &ns); err != nil {
		return ctrl.Result{}, err
	}
	if ok, err := grantFor(share, &ns, access); err != nil || !ok {
		return r.pending(pvc, sharedfsv1alpha1.EventNotGranted, fmt.Sprintf(
			"FilesystemShare %s grants namespace %s no %s access", share.Name, ns.Name, access))
	}

	var sourcePV corev1.PersistentVolume
	if err := r.Get(ctx, types.NamespacedName{Name: share.Spec.PersistentVolumeRef.Name}, &sourcePV); err != nil {
		return ctrl.Result{}, err
	}
	src, err := volume.Resolve(&sourcePV)
	if err != nil {
		// The share was Ready a moment ago; its own reconcile will turn it not Ready.
		return ctrl.Result{RequeueAfter: time.Minute}, nil
	}
	tmpl, err := sharepath.Parse(share.Spec.Path)
	if err != nil {
		return ctrl.Result{}, nil
	}
	expanded, err := tmpl.Expand(ns.Name)
	if err != nil {
		return r.pending(pvc, sharedfsv1alpha1.EventUnsupportedClaim, fmt.Sprintf("namespace %s does not make a valid path: %v", ns.Name, err))
	}

	if res, blocked, err := r.directory(ctx, pvc, share, &ns, tmpl, expanded); err != nil || blocked {
		return res, err
	}

	pv := volume.PV(volume.Claim{
		PVC: pvc, Share: share, Source: src, Access: access,
		ExpandedPath: expanded, ClassName: *pvc.Spec.StorageClassName,
	})
	if err := r.Create(ctx, pv); err != nil {
		// Already provisioned; the PV controller has yet to bind it.
		return ctrl.Result{}, client.IgnoreAlreadyExists(err)
	}
	r.Recorder.Eventf(pvc, pv, corev1.EventTypeNormal, sharedfsv1alpha1.EventProvisioned, "Provision",
		"provisioned %s from FilesystemShare %s, path %s, %s", pv.Name, share.Name, expanded, access)
	return ctrl.Result{}, nil
}

// directory finds or creates the record of the claim's directory. Per-namespace shares always
// get one, which pins the namespace UID: a namespace deleted and created again under the same
// name is a different tenant and does not inherit the directory. A fixed-path share gets one
// only when it prepares the directory. With provision set, the directory must be Prepared.
func (r *ProvisionerReconciler) directory(ctx context.Context, pvc *corev1.PersistentVolumeClaim, share *sharedfsv1alpha1.FilesystemShare,
	ns *corev1.Namespace, tmpl sharepath.Template, expanded string) (ctrl.Result, bool, error) {
	perNamespace := tmpl.PerNamespace()
	if !perNamespace && share.Spec.Provision == nil {
		return ctrl.Result{}, false, nil
	}
	nsName, nsUID := "", types.UID("")
	if perNamespace {
		nsName, nsUID = ns.Name, ns.UID
	}
	name := DirectoryName(share.UID, nsName)
	var dir sharedfsv1alpha1.FilesystemShareDirectory
	err := r.APIReader.Get(ctx, types.NamespacedName{Name: name}, &dir)
	if apierrors.IsNotFound(err) {
		dir = sharedfsv1alpha1.FilesystemShareDirectory{
			ObjectMeta: metav1.ObjectMeta{
				Name:        name,
				Labels:      map[string]string{sharedfsv1alpha1.LabelShareUID: string(share.UID)},
				Annotations: map[string]string{sharedfsv1alpha1.AnnotationShareName: share.Name},
			},
			Spec: sharedfsv1alpha1.FilesystemShareDirectorySpec{
				ShareRef:     sharedfsv1alpha1.ShareReference{Name: share.Name, UID: share.UID},
				Namespace:    nsName,
				NamespaceUID: nsUID,
				Path:         expanded,
			},
		}
		if err := controllerutil.SetOwnerReference(share, &dir, r.Scheme()); err != nil {
			return ctrl.Result{}, true, err
		}
		if err := r.Create(ctx, &dir); err != nil && !apierrors.IsAlreadyExists(err) {
			return ctrl.Result{}, true, err
		}
		if share.Spec.Provision == nil {
			return ctrl.Result{}, false, nil
		}
		res, err := r.pending(pvc, sharedfsv1alpha1.EventDirectoryNotReady, fmt.Sprintf("preparing directory %s (%s)", expanded, name))
		return res, true, err
	}
	if err != nil {
		return ctrl.Result{}, true, err
	}
	if dir.Spec.ShareRef.UID != share.UID || dir.Spec.Path != expanded {
		res, err := r.pending(pvc, sharedfsv1alpha1.EventDirectoryNotReady, fmt.Sprintf(
			"FilesystemShareDirectory %s does not belong to this share and path; delete it", name))
		return res, true, err
	}
	if dir.Spec.NamespaceUID != nsUID {
		res, err := r.pending(pvc, sharedfsv1alpha1.EventNamespaceRecreated, fmt.Sprintf(
			"directory %s was recorded for an earlier namespace %s (uid %s); this namespace has uid %s. "+
				"If this namespace should own the directory, an admin deletes FilesystemShareDirectory %s",
			expanded, ns.Name, dir.Spec.NamespaceUID, ns.UID, name))
		return res, true, err
	}
	if share.Spec.Provision == nil {
		return ctrl.Result{}, false, nil
	}
	if c := meta.FindStatusCondition(dir.Status.Conditions, sharedfsv1alpha1.ConditionPrepared); c == nil || c.Status != metav1.ConditionTrue {
		msg := "preparing"
		if c != nil {
			msg = c.Reason + ": " + c.Message
		}
		res, err := r.pending(pvc, sharedfsv1alpha1.EventDirectoryNotReady, fmt.Sprintf("directory %s (%s) is not prepared: %s", expanded, name, msg))
		return res, true, err
	}
	return ctrl.Result{}, false, nil
}

// unsupported names a claim field sharedfs cannot honour. The PV controller leaves selectors to
// the provisioner, so ignoring one would bind a volume the claim did not ask for.
func unsupported(pvc *corev1.PersistentVolumeClaim) string {
	switch {
	case pvc.Spec.Selector != nil:
		return "a claim with a selector cannot be provisioned: the volume is chosen by the StorageClass"
	case pvc.Spec.VolumeMode != nil && *pvc.Spec.VolumeMode == corev1.PersistentVolumeBlock:
		return "volumeMode Block is not supported: a share is a directory"
	case pvc.Spec.DataSource != nil || pvc.Spec.DataSourceRef != nil:
		return "dataSource is not supported: a share is an existing directory, not a copy"
	}
	return ""
}

func (r *ProvisionerReconciler) pending(pvc *corev1.PersistentVolumeClaim, reason, msg string) (ctrl.Result, error) {
	r.Recorder.Eventf(pvc, nil, corev1.EventTypeWarning, reason, "Provision", "%s", msg)
	return ctrl.Result{RequeueAfter: eventRefresh}, nil
}

// reconcileBound withdraws a claim bound to a sharedfs volume once its namespace has gone
// without the grant for the grace period. The deadline is kept on the PersistentVolume, which
// tenants cannot edit, and checked again against live objects before the claim is deleted.
// A share that is not Ready or is being deleted does not withdraw claims: it only stops new ones.
func (r *ProvisionerReconciler) reconcileBound(ctx context.Context, pvc *corev1.PersistentVolumeClaim) (ctrl.Result, error) {
	var pv corev1.PersistentVolume
	if err := r.Get(ctx, types.NamespacedName{Name: pvc.Spec.VolumeName}, &pv); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !volume.ProvisionedBySharedfs(&pv) || pv.Spec.ClaimRef == nil || pv.Spec.ClaimRef.UID != pvc.UID {
		return ctrl.Result{}, nil
	}
	granted, err := r.granted(ctx, r.Client, pvc, &pv)
	if err != nil {
		return ctrl.Result{}, err
	}
	deadline, hasDeadline := pv.Annotations[sharedfsv1alpha1.AnnotationWithdrawAfter]
	if granted {
		if !hasDeadline {
			return ctrl.Result{}, nil
		}
		patch := client.MergeFrom(pv.DeepCopy())
		delete(pv.Annotations, sharedfsv1alpha1.AnnotationWithdrawAfter)
		return ctrl.Result{}, client.IgnoreNotFound(r.Patch(ctx, &pv, patch))
	}
	if !hasDeadline {
		at := time.Now().Add(r.RevocationGracePeriod).UTC().Truncate(time.Second)
		patch := client.MergeFrom(pv.DeepCopy())
		pv.Annotations[sharedfsv1alpha1.AnnotationWithdrawAfter] = at.Format(time.RFC3339)
		if err := r.Patch(ctx, &pv, patch); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
		r.Recorder.Eventf(pvc, &pv, corev1.EventTypeWarning, sharedfsv1alpha1.EventRevoking, "Revoke",
			"namespace %s is no longer granted FilesystemShare %s; the claim is deleted after %s unless the grant returns",
			pvc.Namespace, pv.Annotations[sharedfsv1alpha1.AnnotationShareName], at.Format(time.RFC3339))
		return ctrl.Result{RequeueAfter: time.Until(at) + time.Second}, nil
	}
	at, err := time.Parse(time.RFC3339, deadline)
	if err != nil {
		// Not ours to guess: restart the grace period.
		at = time.Now().Add(r.RevocationGracePeriod)
		patch := client.MergeFrom(pv.DeepCopy())
		pv.Annotations[sharedfsv1alpha1.AnnotationWithdrawAfter] = at.UTC().Truncate(time.Second).Format(time.RFC3339)
		return ctrl.Result{RequeueAfter: r.RevocationGracePeriod}, client.IgnoreNotFound(r.Patch(ctx, &pv, patch))
	}
	if wait := time.Until(at); wait > 0 {
		return ctrl.Result{RequeueAfter: wait + time.Second}, nil
	}
	// The cache may lag a grant that was just restored: decide on live objects.
	if granted, err := r.granted(ctx, r.APIReader, pvc, &pv); err != nil || granted {
		return ctrl.Result{Requeue: granted}, err
	}
	if err := r.Delete(ctx, pvc, client.Preconditions{UID: &pvc.UID}); err != nil {
		if apierrors.IsNotFound(err) || apierrors.IsConflict(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	r.Recorder.Eventf(pvc, &pv, corev1.EventTypeWarning, sharedfsv1alpha1.EventRevoked, "Revoke",
		"deleted: namespace %s lost its grant on FilesystemShare %s; running pods keep their mounts until they stop",
		pvc.Namespace, pv.Annotations[sharedfsv1alpha1.AnnotationShareName])
	return ctrl.Result{}, nil
}

// granted reports whether the claim's namespace is still granted the volume's access. A share
// that is gone or replaced (another UID) grants nothing; the volume's access is read from the
// volume itself.
func (r *ProvisionerReconciler) granted(ctx context.Context, reader client.Reader, pvc *corev1.PersistentVolumeClaim, pv *corev1.PersistentVolume) (bool, error) {
	var share sharedfsv1alpha1.FilesystemShare
	err := reader.Get(ctx, types.NamespacedName{Name: pv.Annotations[sharedfsv1alpha1.AnnotationShareName]}, &share)
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if string(share.UID) != pv.Labels[sharedfsv1alpha1.LabelShareUID] {
		return false, nil
	}
	var ns corev1.Namespace
	if err := reader.Get(ctx, types.NamespacedName{Name: pvc.Namespace}, &ns); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	access := sharedfsv1alpha1.Access(pv.Annotations[sharedfsv1alpha1.AnnotationAccess])
	if access != sharedfsv1alpha1.AccessReadOnly {
		// Unknown is judged as the stronger access.
		access = sharedfsv1alpha1.AccessReadWrite
	}
	ok, err := grantFor(&share, &ns, access)
	if err != nil {
		// A grant that cannot be evaluated is not a reason to delete someone's claim.
		logf.FromContext(ctx).Error(err, "evaluating grants", "share", share.Name)
		return true, nil
	}
	return ok, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ProvisionerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.PersistentVolumeClaim{}).
		Watches(&corev1.Namespace{}, handler.EnqueueRequestsFromMapFunc(r.claimsInNamespace)).
		Watches(&sharedfsv1alpha1.FilesystemShare{}, handler.EnqueueRequestsFromMapFunc(r.claimsOfShare)).
		Watches(&sharedfsv1alpha1.FilesystemShareDirectory{}, handler.EnqueueRequestsFromMapFunc(r.claimsOfDirectory)).
		Watches(&corev1.PersistentVolume{}, handler.EnqueueRequestsFromMapFunc(claimOfPV)).
		Named("provisioner").
		Complete(r)
}

func claimOfPV(_ context.Context, o client.Object) []reconcile.Request {
	pv := o.(*corev1.PersistentVolume)
	if !volume.ProvisionedBySharedfs(pv) || pv.Spec.ClaimRef == nil {
		return nil
	}
	return toRequests([]types.NamespacedName{{Namespace: pv.Spec.ClaimRef.Namespace, Name: pv.Spec.ClaimRef.Name}})
}

func (r *ProvisionerReconciler) claimsInNamespace(ctx context.Context, o client.Object) []reconcile.Request {
	var list corev1.PersistentVolumeClaimList
	if err := r.List(ctx, &list, client.InNamespace(o.GetName())); err != nil {
		logf.FromContext(ctx).Error(err, "listing claims", "namespace", o.GetName())
		return nil
	}
	return ours(list.Items)
}

func (r *ProvisionerReconciler) claimsOfShare(ctx context.Context, o client.Object) []reconcile.Request {
	share := o.(*sharedfsv1alpha1.FilesystemShare)
	return r.claimsOfClasses(ctx, ClassNames(share), "")
}

func (r *ProvisionerReconciler) claimsOfDirectory(ctx context.Context, o client.Object) []reconcile.Request {
	dir := o.(*sharedfsv1alpha1.FilesystemShareDirectory)
	var share sharedfsv1alpha1.FilesystemShare
	if err := r.Get(ctx, types.NamespacedName{Name: dir.Spec.ShareRef.Name}, &share); err != nil {
		return nil
	}
	return r.claimsOfClasses(ctx, ClassNames(&share), dir.Spec.Namespace)
}

func (r *ProvisionerReconciler) claimsOfClasses(ctx context.Context, names sharedfsv1alpha1.StorageClassNames, namespace string) []reconcile.Request {
	var out []corev1.PersistentVolumeClaim
	for _, c := range []string{names.ReadWrite, names.ReadOnly} {
		var list corev1.PersistentVolumeClaimList
		opts := []client.ListOption{client.MatchingFields{indexPVCClass: c}}
		if namespace != "" {
			opts = append(opts, client.InNamespace(namespace))
		}
		if err := r.List(ctx, &list, opts...); err != nil {
			logf.FromContext(ctx).Error(err, "listing claims", "storageClass", c)
			continue
		}
		out = append(out, list.Items...)
	}
	return ours(out)
}

// ours keeps the claims this reconciler acts on: Pending ones addressed to sharedfs and ones
// bound to a sharedfs volume.
func ours(pvcs []corev1.PersistentVolumeClaim) []reconcile.Request {
	keys := make([]types.NamespacedName, 0, len(pvcs))
	for i := range pvcs {
		p := &pvcs[i]
		if volume.IsInternal(p) {
			continue
		}
		if volume.ClaimedProvisioner(p) == sharedfsv1alpha1.ProvisionerName || strings.HasPrefix(p.Spec.VolumeName, volume.PVName("")) {
			keys = append(keys, types.NamespacedName{Namespace: p.Namespace, Name: p.Name})
		}
	}
	return toRequests(keys)
}
