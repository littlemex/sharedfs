package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"golang.org/x/time/rate"

	sharedfsv1alpha1 "github.com/littlemex/sharedfs/api/v1alpha1"
	"github.com/littlemex/sharedfs/internal/metrics"
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
	// RevocationLimiter bounds how fast claims are deleted across the whole cluster.
	RevocationLimiter *rate.Limiter
	// ProjectIDs hands out project IDs to directory records of shares with a quota.
	ProjectIDs *ProjectIDs
	// QuotaRequiresRestricted refuses read-write claims of shares with a quota from namespaces
	// that do not enforce Pod Security restricted.
	QuotaRequiresRestricted bool
	// MaxConcurrentReconciles is how many claims are handled at once.
	MaxConcurrentReconciles int
}

// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups="",resources=persistentvolumes,verbs=get;list;watch;create;patch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch
// +kubebuilder:rbac:groups=storage.k8s.io,resources=storageclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups=sharedfs.io,resources=filesystemshares,verbs=get;list;watch
// +kubebuilder:rbac:groups=sharedfs.io,resources=filesystemsharedirectories,verbs=get;list;watch;create
// +kubebuilder:rbac:groups=sharedfs.io,resources=filesystemshares/finalizers,verbs=update
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

func (r *ProvisionerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var pvc corev1.PersistentVolumeClaim
	if err := r.Get(ctx, req.NamespacedName, &pvc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	// Labels on a claim are the tenant's to set, so nothing here trusts them.
	if !pvc.DeletionTimestamp.IsZero() {
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
		return r.pending(pvc, sharedfsv1alpha1.EventUnsupportedClaim, fmt.Sprintf(
			"StorageClass %s names this provisioner but is not one of a FilesystemShare's classes", *pvc.Spec.StorageClassName))
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	if why := unsupported(pvc); why != "" {
		return r.pending(pvc, sharedfsv1alpha1.EventUnsupportedClaim, why)
	}
	ready := meta.FindStatusCondition(share.Status.Conditions, sharedfsv1alpha1.ConditionReady)
	if share.DeletionTimestamp.IsZero() && (ready == nil || ready.ObservedGeneration != share.Generation) {
		// Not checked since its last change: wait for the verdict instead of reporting a stale one.
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}
	if ready == nil || ready.Status != metav1.ConditionTrue || ready.ObservedGeneration != share.Generation || !share.DeletionTimestamp.IsZero() {
		// Only the reason: the message names the source and other shares, which are the admin's.
		msg := "it has not been checked yet"
		if !share.DeletionTimestamp.IsZero() {
			msg = "it is being deleted"
		} else if ready != nil && ready.Status == metav1.ConditionFalse {
			msg = ready.Reason
		}
		return r.pending(pvc, sharedfsv1alpha1.EventShareNotReady, fmt.Sprintf("FilesystemShare %s is not Ready: %s", share.Name, msg))
	}
	// Live: the namespace's UID decides whether a recreated namespace inherits a directory, and a
	// cache that lags a delete and re-create would get that wrong.
	var ns corev1.Namespace
	if err := r.APIReader.Get(ctx, types.NamespacedName{Name: pvc.Namespace}, &ns); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !ns.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	if ok, err := grantFor(share, &ns, access); err != nil || !ok {
		return r.pending(pvc, sharedfsv1alpha1.EventNotGranted, fmt.Sprintf(
			"FilesystemShare %s grants namespace %s no %s access", share.Name, ns.Name, access))
	}
	if r.QuotaRequiresRestricted && access == sharedfsv1alpha1.AccessReadWrite && ns.Labels[podSecurityEnforce] != "restricted" {
		// A root process can move its files to any project (measured), into a quota's directory
		// or out of it, so where any share of the file system has a quota, every read-write writer
		// must be unable to run as root.
		var sourcePV corev1.PersistentVolume
		if err := r.Get(ctx, types.NamespacedName{Name: share.Spec.PersistentVolumeRef.Name}, &sourcePV); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
		if src, err := volume.Resolve(&sourcePV); err == nil {
			fs, _ := src.Identity()
			if quoted, err := quotaOnFilesystem(ctx, r.Client, fs); err != nil {
				return ctrl.Result{}, err
			} else if quoted {
				return r.pending(pvc, sharedfsv1alpha1.EventUnsupportedClaim, fmt.Sprintf(
					"shares of this file system have quotas, which hold only if no writer can run as root: label namespace %s "+
						"pod-security.kubernetes.io/enforce=restricted (or run the controller with --quota-requires-restricted=false where root squash is on)", ns.Name))
			}
		}
	}

	var sourcePV corev1.PersistentVolume
	if err := r.Get(ctx, types.NamespacedName{Name: share.Spec.PersistentVolumeRef.Name}, &sourcePV); err != nil {
		return ctrl.Result{}, err
	}
	src, err := volume.Resolve(&sourcePV)
	if err == nil && !src.Reserved {
		err = errors.New("the source is no longer reserved")
	}
	tmpl, perr := sharepath.Parse(share.Spec.Path)
	if err != nil || perr != nil {
		// The share was Ready a moment ago; its own reconcile turns it not Ready.
		return r.pending(pvc, sharedfsv1alpha1.EventShareNotReady, fmt.Sprintf("FilesystemShare %s is not Ready: its source changed", share.Name))
	}
	expanded, err := tmpl.Expand(ns.Name)
	if err != nil {
		return r.pending(pvc, sharedfsv1alpha1.EventUnsupportedClaim, fmt.Sprintf("namespace %s does not make a valid path for FilesystemShare %s: %v", ns.Name, share.Name, err))
	}

	if res, blocked, err := r.directory(ctx, pvc, share, &ns, tmpl, expanded); err != nil || blocked {
		return res, err
	}

	pv := volume.PV(volume.Claim{
		PVC: pvc, Share: share, Source: src, Access: access,
		ExpandedPath: expanded, ClassName: *pvc.Spec.StorageClassName,
	})
	if err := r.Create(ctx, pv); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return ctrl.Result{}, nil
		}
		if apierrors.IsInvalid(err) {
			return r.pending(pvc, sharedfsv1alpha1.EventUnsupportedClaim, "the volume for this claim is invalid: "+err.Error())
		}
		// Already provisioned; the PV controller has yet to bind it.
		return ctrl.Result{}, client.IgnoreAlreadyExists(err)
	}
	metrics.Provisioned.Inc()
	metrics.ProvisionLatency.Observe(time.Since(pvc.CreationTimestamp.Time).Seconds())
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
		create := func(id int64) error {
			dir.Spec.ProjectID = id
			return r.Create(ctx, &dir)
		}
		if share.Spec.Provision != nil && share.Spec.Provision.Quota != nil {
			err = r.ProjectIDs.assign(ctx, create)
		} else {
			err = create(0)
		}
		if err != nil {
			if apierrors.IsAlreadyExists(err) {
				// Created a moment ago: check it on the next pass.
				return ctrl.Result{RequeueAfter: time.Second}, true, nil
			}
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
	if dir.Spec.NamespaceUID != nsUID {
		res, err := r.pending(pvc, sharedfsv1alpha1.EventNamespaceRecreated, fmt.Sprintf(
			"directory %s belongs to an earlier namespace of the same name; "+
				"if this namespace should own it, an admin deletes FilesystemShareDirectory %s", expanded, name))
		return res, true, err
	}
	if share.Spec.Provision == nil {
		return ctrl.Result{}, false, nil
	}
	if c := meta.FindStatusCondition(dir.Status.Conditions, sharedfsv1alpha1.ConditionPrepared); c == nil || c.Status != metav1.ConditionTrue {
		// Only the reason: the message describes the controller's namespace and the parent.
		msg := "preparing"
		if c != nil {
			msg = c.Reason
		}
		res, err := r.pending(pvc, sharedfsv1alpha1.EventDirectoryNotReady, fmt.Sprintf("directory %s (%s) is not prepared: %s", expanded, name, msg))
		return res, true, err
	}
	if share.Spec.Provision.Quota != nil && dir.Status.Quota == nil {
		// A directory of a share with a quota is handed out only once a limit holds; while a
		// changed limit is being applied, the earlier one still does.
		if c := meta.FindStatusCondition(dir.Status.Conditions, sharedfsv1alpha1.ConditionQuotaApplied); c == nil || c.Status != metav1.ConditionTrue {
			msg := sharedfsv1alpha1.ReasonQuotaPending
			if c != nil {
				msg = c.Reason
			}
			res, err := r.pending(pvc, sharedfsv1alpha1.EventDirectoryNotReady, fmt.Sprintf("the quota of directory %s (%s) is not applied: %s", expanded, name, msg))
			return res, true, err
		}
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
	case slices.Contains(pvc.Spec.AccessModes, corev1.ReadWriteOncePod):
		return "ReadWriteOncePod is not supported: a share is mounted by many pods"
	case pvc.Spec.VolumeAttributesClassName != nil:
		return "volumeAttributesClassName is not supported"
	}
	return ""
}

func (r *ProvisionerReconciler) pending(pvc *corev1.PersistentVolumeClaim, reason, msg string) (ctrl.Result, error) {
	metrics.ClaimsPending.WithLabelValues(reason).Inc()
	r.Recorder.Eventf(pvc, nil, corev1.EventTypeWarning, reason, "Provision", "%s", msg)
	return ctrl.Result{RequeueAfter: eventRefresh}, nil
}

// reconcileBound withdraws a claim bound to a sharedfs volume once its namespace has gone
// without the grant for the grace period. The deadline is kept on the PersistentVolume, which
// tenants cannot edit, and checked again against live objects before the claim is deleted.
// A share that is not Ready only stops new claims. Revocation is held, never executed, when the
// share to judge by is missing or was replaced, or when the share pauses revocation; and it is
// rate-limited, so one mistaken label change cannot delete every claim at once.
func (r *ProvisionerReconciler) reconcileBound(ctx context.Context, pvc *corev1.PersistentVolumeClaim) (ctrl.Result, error) {
	var pv corev1.PersistentVolume
	if err := r.Get(ctx, types.NamespacedName{Name: pvc.Spec.VolumeName}, &pv); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !volume.ProvisionedBySharedfs(&pv) || pv.Spec.ClaimRef == nil || pv.Spec.ClaimRef.UID != pvc.UID {
		return ctrl.Result{}, nil
	}
	v, err := r.verdict(ctx, r.Client, pvc, &pv)
	if err != nil {
		return ctrl.Result{}, err
	}
	deadline, hasDeadline := pv.Annotations[sharedfsv1alpha1.AnnotationWithdrawAfter]
	switch {
	case v.granted:
		if !hasDeadline {
			return ctrl.Result{}, nil
		}
		metrics.Revocations.WithLabelValues("cancelled").Inc()
		patch := client.MergeFrom(pv.DeepCopy())
		delete(pv.Annotations, sharedfsv1alpha1.AnnotationWithdrawAfter)
		return ctrl.Result{}, client.IgnoreNotFound(r.Patch(ctx, &pv, patch))
	case v.hold != "":
		metrics.Revocations.WithLabelValues("held").Inc()
		r.Recorder.Eventf(pvc, &pv, corev1.EventTypeWarning, sharedfsv1alpha1.EventRevocationHeld, "Revoke",
			"revocation held: %s", v.hold)
		return ctrl.Result{RequeueAfter: eventRefresh}, nil
	}
	if !hasDeadline {
		at := time.Now().Add(r.RevocationGracePeriod).UTC().Truncate(time.Second)
		patch := client.MergeFrom(pv.DeepCopy())
		pv.Annotations[sharedfsv1alpha1.AnnotationWithdrawAfter] = at.Format(time.RFC3339)
		if err := r.Patch(ctx, &pv, patch); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
		metrics.Revocations.WithLabelValues("scheduled").Inc()
		r.Recorder.Eventf(pvc, &pv, corev1.EventTypeWarning, sharedfsv1alpha1.EventRevoking, "Revoke",
			"namespace %s is no longer granted FilesystemShare %s; the claim is deleted after %s unless the grant returns",
			pvc.Namespace, pv.Annotations[sharedfsv1alpha1.AnnotationShareName], at.Format(time.RFC3339))
		return ctrl.Result{RequeueAfter: time.Until(at) + time.Second}, nil
	}
	at, err := time.Parse(time.RFC3339, deadline)
	if err != nil {
		// Not ours to guess: restart the grace period.
		patch := client.MergeFrom(pv.DeepCopy())
		pv.Annotations[sharedfsv1alpha1.AnnotationWithdrawAfter] = time.Now().Add(r.RevocationGracePeriod).UTC().Truncate(time.Second).Format(time.RFC3339)
		return ctrl.Result{RequeueAfter: r.RevocationGracePeriod}, client.IgnoreNotFound(r.Patch(ctx, &pv, patch))
	}
	if wait := time.Until(at); wait > 0 {
		return ctrl.Result{RequeueAfter: wait + time.Second}, nil
	}
	// The rate limit first, so a long queue of revocations does not spend API calls each retry.
	if r.RevocationLimiter != nil && !r.RevocationLimiter.Allow() {
		metrics.Revocations.WithLabelValues("ratelimited").Inc()
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
	// The cache may lag a grant that was just restored: decide on live objects.
	if v, err := r.verdict(ctx, r.APIReader, pvc, &pv); err != nil || v.granted || v.hold != "" {
		return ctrl.Result{RequeueAfter: time.Second}, err
	}
	if err := r.Delete(ctx, pvc, client.Preconditions{UID: &pvc.UID}); err != nil {
		if apierrors.IsNotFound(err) || apierrors.IsConflict(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	metrics.Revocations.WithLabelValues("executed").Inc()
	r.Recorder.Eventf(pvc, &pv, corev1.EventTypeWarning, sharedfsv1alpha1.EventRevoked, "Revoke",
		"deleted: namespace %s lost its grant on FilesystemShare %s; running pods keep their mounts until they stop",
		pvc.Namespace, pv.Annotations[sharedfsv1alpha1.AnnotationShareName])
	return ctrl.Result{}, nil
}

type verdict struct {
	granted bool
	// hold names why a revocation must not proceed even though no grant is found.
	hold string
}

// verdict decides whether the claim's namespace is still granted the volume's access. The
// volume's access is read from the volume itself; unknown is judged as ReadWrite.
func (r *ProvisionerReconciler) verdict(ctx context.Context, reader client.Reader, pvc *corev1.PersistentVolumeClaim, pv *corev1.PersistentVolume) (verdict, error) {
	name := pv.Annotations[sharedfsv1alpha1.AnnotationShareName]
	var share sharedfsv1alpha1.FilesystemShare
	err := reader.Get(ctx, types.NamespacedName{Name: name}, &share)
	if apierrors.IsNotFound(err) {
		// The finalizer keeps a share while it has volumes, so it was removed by force: judging
		// every claim of it ungranted would delete them all.
		return verdict{hold: fmt.Sprintf("FilesystemShare %s is gone although this volume remains; delete the claim or recreate the share", name)}, nil
	}
	if err != nil {
		return verdict{}, err
	}
	if string(share.UID) != pv.Labels[sharedfsv1alpha1.LabelShareUID] {
		return verdict{hold: fmt.Sprintf("FilesystemShare %s was recreated after this volume was provisioned from it; delete the claim to move it to the new share", name)}, nil
	}
	if share.Annotations[sharedfsv1alpha1.AnnotationPauseRevocation] == "true" {
		return verdict{hold: fmt.Sprintf("FilesystemShare %s pauses revocation", name)}, nil
	}
	var ns corev1.Namespace
	if err := reader.Get(ctx, types.NamespacedName{Name: pvc.Namespace}, &ns); err != nil {
		return verdict{}, client.IgnoreNotFound(err)
	}
	access := sharedfsv1alpha1.Access(pv.Annotations[sharedfsv1alpha1.AnnotationAccess])
	if access != sharedfsv1alpha1.AccessReadOnly {
		access = sharedfsv1alpha1.AccessReadWrite
	}
	ok, err := grantFor(&share, &ns, access)
	if err != nil {
		return verdict{hold: fmt.Sprintf("the grants of FilesystemShare %s cannot be evaluated: %v", name, err)}, nil
	}
	return verdict{granted: ok}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ProvisionerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.PersistentVolumeClaim{}).
		Watches(&corev1.Namespace{}, handler.EnqueueRequestsFromMapFunc(r.claimsInNamespace)).
		Watches(&sharedfsv1alpha1.FilesystemShare{}, handler.EnqueueRequestsFromMapFunc(r.claimsOfShare),
			builder.WithPredicates(shareChanged)).
		Watches(&sharedfsv1alpha1.FilesystemShareDirectory{}, handler.EnqueueRequestsFromMapFunc(r.claimsOfDirectory)).
		Watches(&corev1.PersistentVolume{}, handler.EnqueueRequestsFromMapFunc(claimOfPV)).
		WithOptions(controller.Options{MaxConcurrentReconciles: max(r.MaxConcurrentReconciles, 1)}).
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
		if volume.ClaimedProvisioner(p) == sharedfsv1alpha1.ProvisionerName || volume.IsTenantPVName(p.Spec.VolumeName) {
			keys = append(keys, types.NamespacedName{Namespace: p.Namespace, Name: p.Name})
		}
	}
	return toRequests(keys)
}
