package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"

	sharedfsv1alpha1 "github.com/littlemex/sharedfs/api/v1alpha1"
	"github.com/littlemex/sharedfs/internal/quota"
	"github.com/littlemex/sharedfs/internal/volume"
)

// quotaMountPath is where the quota pod sees the directory.
const quotaMountPath = "/target"

// QuotaConfig is where and how quota pods run. They need CAP_SYS_ADMIN, which Pod Security
// baseline does not allow, so they run in a namespace of their own.
type QuotaConfig struct {
	// Namespace of quota pods and their internal claims; empty turns quotas off.
	Namespace string
	// Image carries the controller binary and lfs.
	Image            string
	ServiceAccount   string
	NodeSelector     map[string]string
	Tolerations      []corev1.Toleration
	ImagePullSecrets []corev1.LocalObjectReference
	// MaxPods bounds the quota pods running at once.
	MaxPods int
}

// ProjectIDs hands out Lustre project IDs that are never handed out twice, even after the record
// that held one is deleted: the next free ID is kept in a ConfigMap of the controller's
// namespace and raised before a record takes an ID. Only the leader reconciles, and the lock
// covers reading, reserving and writing.
type ProjectIDs struct {
	mu     sync.Mutex
	Client client.Client
	Reader client.Reader
	// Namespace holds the ConfigMap.
	Namespace string
	// Base is the first ID handed out; IDs below it are left to the admin.
	Base int64
}

const (
	projectIDsConfigMap = "sharedfs-project-ids"
	projectIDsKey       = "next"
)

// assign reserves the next ID and calls write with it, under the lock. An ID reserved but not
// written (a failed write) is skipped, never reused.
func (p *ProjectIDs) assign(ctx context.Context, write func(id int64) error) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	var list sharedfsv1alpha1.FilesystemShareDirectoryList
	if err := p.Reader.List(ctx, &list); err != nil {
		return err
	}
	next := p.Base
	for _, d := range list.Items {
		next = max(next, d.Spec.ProjectID+1)
	}
	var cm corev1.ConfigMap
	key := types.NamespacedName{Namespace: p.Namespace, Name: projectIDsConfigMap}
	err := p.Reader.Get(ctx, key, &cm)
	switch {
	case apierrors.IsNotFound(err):
		cm = corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: p.Namespace, Name: projectIDsConfigMap,
			Labels: map[string]string{"app.kubernetes.io/name": "sharedfs"}}}
	case err != nil:
		return err
	default:
		if v, err := strconv.ParseInt(cm.Data[projectIDsKey], 10, 64); err == nil {
			next = max(next, v)
		}
	}
	if next > maxProjectID {
		return fmt.Errorf("no project IDs left above %d", maxProjectID)
	}
	if cm.Data == nil {
		cm.Data = map[string]string{}
	}
	cm.Data[projectIDsKey] = strconv.FormatInt(next+1, 10)
	if cm.ResourceVersion == "" {
		err = p.Client.Create(ctx, &cm)
	} else {
		err = p.Client.Update(ctx, &cm)
	}
	if err != nil {
		return err
	}
	return write(next)
}

const maxProjectID = 4294967294

// QuotaReconciler gives the directory of a share with a quota a project ID and keeps the
// share's limits set on that project. The ID is set on the directory when the preparer creates
// it; this reconciler then runs a quota pod, which mounts only the directory, to set the limits,
// and again whenever the share's limits change. A directory that existed before is not tagged
// and reports DirectoryNotTagged.
type QuotaReconciler struct {
	client.Client
	Quota      QuotaConfig
	ProjectIDs *ProjectIDs
}

// +kubebuilder:rbac:groups=sharedfs.littlemex.github.io,resources=filesystemsharedirectories,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups="",namespace=system,resources=configmaps,verbs=get;list;watch;create;update

func (r *QuotaReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var dir sharedfsv1alpha1.FilesystemShareDirectory
	if err := r.Get(ctx, req.NamespacedName, &dir); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !dir.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	var share sharedfsv1alpha1.FilesystemShare
	if err := r.Get(ctx, types.NamespacedName{Name: dir.Spec.ShareRef.Name}, &share); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	t := r.task(&dir)
	before := dir.DeepCopy()
	noQuota := share.UID != dir.Spec.ShareRef.UID || share.Spec.Provision == nil || share.Spec.Provision.Quota == nil
	if noQuota && (dir.Status.Quota == nil || dir.Spec.ProjectID == 0 || share.UID != dir.Spec.ShareRef.UID) {
		meta.RemoveStatusCondition(&dir.Status.Conditions, sharedfsv1alpha1.ConditionQuotaApplied)
		dir.Status.Quota = nil
		if err := updateDirStatus(ctx, r.Client, &dir, before); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, r.cleanupIfConfigured(ctx, t)
	}
	if !noQuota && dir.Spec.ProjectID == 0 {
		// Quota added to a share that already has records: give them IDs; directories already
		// created report DirectoryNotTagged below.
		return requeueSoon, r.ProjectIDs.assign(ctx, func(id int64) error {
			patch := client.MergeFrom(dir.DeepCopy())
			dir.Spec.ProjectID = id
			return r.Patch(ctx, &dir, patch)
		})
	}
	if !meta.IsStatusConditionTrue(dir.Status.Conditions, sharedfsv1alpha1.ConditionPrepared) || dir.Status.Observed == nil {
		return ctrl.Result{}, nil
	}
	// A quota removed from the share clears the limits it set; the project ID stays.
	want := &sharedfsv1alpha1.AppliedQuota{}
	if !noQuota {
		want = applied(share.Spec.Provision.Quota)
	}
	cond := func(ok bool, reason, msg string) {
		setCondition(&dir.Status.Conditions, dir.Generation, sharedfsv1alpha1.ConditionQuotaApplied, ok, reason, msg)
	}
	switch {
	case r.Quota.Namespace == "" || r.Quota.Image == "":
		cond(false, sharedfsv1alpha1.ReasonQuotaNotConfigured, "the controller runs without --quota-namespace and --quota-image")
		return ctrl.Result{}, updateDirStatus(ctx, r.Client, &dir, before)
	case sameLimits(dir.Status.Quota, want) &&
		meta.IsStatusConditionTrue(dir.Status.Conditions, sharedfsv1alpha1.ConditionQuotaApplied):
		return ctrl.Result{}, t.cleanup(ctx)
	}
	if !sameLimits(dir.Status.Quota, want) && meta.IsStatusConditionTrue(dir.Status.Conditions, sharedfsv1alpha1.ConditionQuotaApplied) {
		cond(false, sharedfsv1alpha1.ReasonQuotaPending, "the share's limits changed")
	}
	firstApply := dir.Status.Quota == nil
	res, state, err := t.run(ctx, &dir, isReady(&share), func() (string, *corev1.Pod, bool, error) {
		// A change of limits on a large share would otherwise start a pod per directory at once.
		var pods corev1.PodList
		if err := r.List(ctx, &pods, client.InNamespace(r.Quota.Namespace), client.HasLabels{sharedfsv1alpha1.LabelInternal}); err != nil {
			return "", nil, false, err
		}
		if len(pods.Items) >= max(r.Quota.MaxPods, 1) {
			return "", nil, true, nil
		}
		args := []string{
			"set-quota",
			"--path=" + quotaMountPath,
			"--project-id=" + strconv.FormatInt(dir.Spec.ProjectID, 10),
			"--storage-kib=" + strconv.FormatInt(kib(want.Storage), 10),
			"--inodes=" + strconv.FormatInt(deref(want.Inodes), 10),
		}
		if firstApply {
			args = append(args, "--first-apply")
		}
		cfg := PreparerConfig{ServiceAccount: r.Quota.ServiceAccount, NodeSelector: r.Quota.NodeSelector,
			Tolerations: r.Quota.Tolerations, ImagePullSecrets: r.Quota.ImagePullSecrets}
		// SYS_ADMIN for the quotactl, which Lustre allows only to it, and DAC_READ_SEARCH to open a
		// directory whatever its owner and mode. The pod mounts the directory itself, read-write
		// because Lustre refuses quotactl on a read-only mount, and has no token.
		return dir.Spec.Path, taskPod(cfg, r.Quota.Image, []corev1.Capability{"SYS_ADMIN", "DAC_READ_SEARCH"}, quotaMountPath, args), false, nil
	}, func(report taskReport, raw string) bool {
		var out quota.Result
		_ = json.Unmarshal([]byte(raw), &out)
		ok := out.Outcome == quota.Applied
		switch {
		case ok && out.StorageKiB == 0 && out.Inodes == 0:
			// Cleared because the share no longer has a quota.
			dir.Status.Quota = nil
			meta.RemoveStatusCondition(&dir.Status.Conditions, sharedfsv1alpha1.ConditionQuotaApplied)
			return true
		case ok:
			// What the pod read back, which may differ from the share if it changed meanwhile.
			dir.Status.Quota = fromKiB(out.StorageKiB, out.Inodes)
		}
		cond(ok, out.Outcome, truncate(out.Message))
		return ok
	})
	if err != nil {
		return res, err
	}
	switch state {
	case taskStarted:
		cond(false, sharedfsv1alpha1.ReasonQuotaPending, fmt.Sprintf("quota pod %s/%s started", t.namespace, t.name))
	case taskRecorded:
		if err := updateDirStatus(ctx, r.Client, &dir, before); err != nil {
			return ctrl.Result{}, err
		}
		return res, t.cleanup(ctx)
	}
	return res, updateDirStatus(ctx, r.Client, &dir, before)
}

func (r *QuotaReconciler) task(dir *sharedfsv1alpha1.FilesystemShareDirectory) *task {
	return &task{
		Client: r.Client, what: "quota pod", condition: sharedfsv1alpha1.ConditionQuotaApplied,
		name: volume.QuotaName(dir.UID), namespace: r.Quota.Namespace, history: &dir.Status.QuotaSetter,
		outcomes: quota.Outcomes, failed: quota.Failed,
	}
}

func (r *QuotaReconciler) cleanupIfConfigured(ctx context.Context, t *task) error {
	if r.Quota.Namespace == "" {
		return nil
	}
	return t.cleanup(ctx)
}

func applied(q *sharedfsv1alpha1.Quota) *sharedfsv1alpha1.AppliedQuota {
	a := &sharedfsv1alpha1.AppliedQuota{Inodes: q.Inodes}
	if q.Storage != nil {
		s := q.Storage.DeepCopy()
		a.Storage = &s
	}
	return a
}

// sameLimits compares limits as the quota pod sets them, in KiB and inodes.
func sameLimits(a, b *sharedfsv1alpha1.AppliedQuota) bool {
	if a == nil || b == nil {
		return a == b
	}
	return kib(a.Storage) == kib(b.Storage) && deref(a.Inodes) == deref(b.Inodes)
}

func fromKiB(storageKiB, inodes int64) *sharedfsv1alpha1.AppliedQuota {
	a := &sharedfsv1alpha1.AppliedQuota{}
	if storageKiB > 0 {
		a.Storage = resource.NewQuantity(storageKiB*1024, resource.BinarySI)
	}
	if inodes > 0 {
		a.Inodes = &inodes
	}
	return a
}

// kib rounds a quantity up to KiB, the unit lfs takes; nil is no limit.
func kib(q *resource.Quantity) int64 {
	if q == nil {
		return 0
	}
	return (q.Value() + 1023) / 1024
}

func deref(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// SetupWithManager sets up the controller with the Manager.
func (r *QuotaReconciler) SetupWithManager(mgr ctrl.Manager) error {
	b := ctrl.NewControllerManagedBy(mgr).
		For(&sharedfsv1alpha1.FilesystemShareDirectory{}).
		// A change of the share's limits re-applies them.
		Watches(&sharedfsv1alpha1.FilesystemShare{}, handler.EnqueueRequestsFromMapFunc(directoriesOfShare(r.Client)),
			builder.WithPredicates(shareChanged)).
		Named("quota")
	if r.Quota.Namespace != "" {
		b = b.Owns(&corev1.Pod{})
	}
	return b.Complete(r)
}
