package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
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
	Image          string
	ServiceAccount string
	NodeSelector   map[string]string
	Tolerations    []corev1.Toleration
}

// ProjectIDs hands out Lustre project IDs, unique among directory records. Only the leader
// reconciles, and the lock covers the read of every record and the write of the new one.
type ProjectIDs struct {
	mu     sync.Mutex
	Reader client.Reader
	// Base is the first ID handed out; IDs below it are left to the admin.
	Base int64
}

// assign calls write with the next free ID, under the lock.
func (p *ProjectIDs) assign(ctx context.Context, write func(id int64) error) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	var list sharedfsv1alpha1.FilesystemShareDirectoryList
	if err := p.Reader.List(ctx, &list); err != nil {
		return err
	}
	next := p.Base
	for _, d := range list.Items {
		if d.Spec.ProjectID >= next {
			next = d.Spec.ProjectID + 1
		}
	}
	return write(next)
}

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
	if share.UID != dir.Spec.ShareRef.UID || share.Spec.Provision == nil || share.Spec.Provision.Quota == nil {
		meta.RemoveStatusCondition(&dir.Status.Conditions, sharedfsv1alpha1.ConditionQuotaApplied)
		dir.Status.Quota = nil
		if err := updateDirStatus(ctx, r.Client, &dir, before); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, r.cleanupIfConfigured(ctx, t)
	}
	if dir.Spec.ProjectID == 0 {
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
	want := applied(share.Spec.Provision.Quota)
	cond := func(ok bool, reason, msg string) {
		setCondition(&dir.Status.Conditions, dir.Generation, sharedfsv1alpha1.ConditionQuotaApplied, ok, reason, msg)
	}
	switch {
	case r.Quota.Namespace == "" || r.Quota.Image == "":
		cond(false, sharedfsv1alpha1.ReasonQuotaNotConfigured, "the controller runs without --quota-namespace and --quota-image")
		return ctrl.Result{}, updateDirStatus(ctx, r.Client, &dir, before)
	case equality.Semantic.DeepEqual(dir.Status.Quota, want) &&
		meta.IsStatusConditionTrue(dir.Status.Conditions, sharedfsv1alpha1.ConditionQuotaApplied):
		return ctrl.Result{}, t.cleanup(ctx)
	}
	if !equality.Semantic.DeepEqual(dir.Status.Quota, want) && meta.IsStatusConditionTrue(dir.Status.Conditions, sharedfsv1alpha1.ConditionQuotaApplied) {
		cond(false, sharedfsv1alpha1.ReasonQuotaPending, "the share's limits changed")
	}
	res, state, err := t.run(ctx, &dir, isReady(&share), func() (string, *corev1.Pod, bool, error) {
		args := []string{
			"set-quota",
			"--path=" + quotaMountPath,
			"--project-id=" + strconv.FormatInt(dir.Spec.ProjectID, 10),
			"--storage-kib=" + strconv.FormatInt(kib(want.Storage), 10),
			"--inodes=" + strconv.FormatInt(deref(want.Inodes), 10),
		}
		cfg := PreparerConfig{ServiceAccount: r.Quota.ServiceAccount, NodeSelector: r.Quota.NodeSelector, Tolerations: r.Quota.Tolerations}
		// SYS_ADMIN only: setting a quota is a quotactl, which Lustre allows only to it. The pod
		// mounts the directory itself, read-write because Lustre refuses quotactl on a read-only
		// mount, and has no token.
		return dir.Spec.Path, taskPod(cfg, r.Quota.Image, []corev1.Capability{"SYS_ADMIN"}, quotaMountPath, args), false, nil
	}, func(report taskReport, raw string) bool {
		var out quota.Result
		_ = json.Unmarshal([]byte(raw), &out)
		ok := out.Outcome == quota.Applied
		if ok {
			dir.Status.Quota = want
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
