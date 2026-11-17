package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	sharedfsv1alpha1 "github.com/littlemex/sharedfs/api/v1alpha1"
	"github.com/littlemex/sharedfs/internal/prepare"
	"github.com/littlemex/sharedfs/internal/sharepath"
	"github.com/littlemex/sharedfs/internal/volume"
)

// preparerMountPath is where the preparer sees the parent of the directory.
const preparerMountPath = "/parent"

var taskResources = corev1.ResourceRequirements{
	Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: resource.MustParse("32Mi")},
	Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("64Mi")},
}

// DirectoryReconciler prepares the directory a FilesystemShareDirectory records, when its share
// asks for preparation. It runs one short-lived pod per directory that mounts only the parent,
// creates the directory with the share's owner, mode and project ID, and reports the result as
// JSON. An existing directory is never changed.
type DirectoryReconciler struct {
	client.Client
	Preparer PreparerConfig
}

// +kubebuilder:rbac:groups=sharedfs.littlemex.github.io,resources=filesystemsharedirectories,verbs=get;list;watch;patch
// Owner references with blockOwnerDeletion need update on the owner's finalizers.
// +kubebuilder:rbac:groups=sharedfs.littlemex.github.io,resources=filesystemsharedirectories/finalizers,verbs=update
// +kubebuilder:rbac:groups=sharedfs.littlemex.github.io,resources=filesystemsharedirectories/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=persistentvolumes,verbs=get;list;watch;create;delete
// Pods and internal claims are created only in the controller's namespace.
// +kubebuilder:rbac:groups="",namespace=system,resources=persistentvolumeclaims,verbs=create
// +kubebuilder:rbac:groups="",namespace=system,resources=pods,verbs=get;list;watch;create;delete

func (r *DirectoryReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
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
	if share.UID != dir.Spec.ShareRef.UID || share.Spec.Provision == nil ||
		meta.IsStatusConditionTrue(dir.Status.Conditions, sharedfsv1alpha1.ConditionPrepared) {
		return ctrl.Result{}, t.cleanup(ctx)
	}
	p := share.Spec.Provision
	before := dir.DeepCopy()
	// A share that is not Ready (a changed source, an unsafe driver) prepares nothing, and a
	// directory that will have a quota waits for its project ID.
	canStart := isReady(&share) && (p.Quota == nil || dir.Spec.ProjectID != 0)
	res, state, err := t.run(ctx, &dir, canStart, func() (string, *corev1.Pod, bool, error) {
		parent, leaf := sharepath.Parent(dir.Spec.Path)
		args := []string{
			"prepare-directory",
			"--root=" + preparerMountPath,
			"--name=" + leaf,
			"--uid=" + strconv.FormatInt(p.UID, 10),
			"--gid=" + strconv.FormatInt(p.GID, 10),
			"--mode=" + p.Mode,
		}
		if dir.Spec.ProjectID != 0 {
			args = append(args, "--project-id="+strconv.FormatInt(dir.Spec.ProjectID, 10))
		}
		// CHOWN only: with chmod before chown that is all creating a directory for another
		// owner takes, and it fits Pod Security baseline.
		return parent, taskPod(r.Preparer, r.Preparer.Image, []corev1.Capability{"CHOWN"}, preparerMountPath, args), false, nil
	}, func(report taskReport, raw string) bool {
		var out prepare.Result
		_ = json.Unmarshal([]byte(raw), &out)
		ok := out.Outcome == prepare.Created || out.Outcome == prepare.Exists
		msg := out.Message
		if ok {
			dir.Status.Observed = &sharedfsv1alpha1.ObservedDirectory{UID: out.UID, GID: out.GID, Mode: out.Mode, ProjectID: int64(out.ProjectID)}
			if d := differs(p, out); d != "" {
				msg = "existing directory left unchanged; it differs from provision:" + d
			}
		}
		setCondition(&dir.Status.Conditions, dir.Generation, sharedfsv1alpha1.ConditionPrepared, ok, out.Outcome, truncate(msg))
		return ok
	})
	if err != nil {
		return res, err
	}
	switch state {
	case taskStarted:
		setCondition(&dir.Status.Conditions, dir.Generation, sharedfsv1alpha1.ConditionPrepared, false,
			sharedfsv1alpha1.ReasonPreparing, fmt.Sprintf("preparer pod %s/%s started", t.namespace, t.name))
	case taskRecorded:
		if err := r.updateStatus(ctx, &dir, before); err != nil {
			return ctrl.Result{}, err
		}
		return res, t.cleanup(ctx)
	}
	return res, r.updateStatus(ctx, &dir, before)
}

func (r *DirectoryReconciler) task(dir *sharedfsv1alpha1.FilesystemShareDirectory) *task {
	return &task{
		Client: r.Client, what: "preparer", condition: sharedfsv1alpha1.ConditionPrepared,
		name: volume.PreparerName(dir.UID), namespace: r.Preparer.Namespace, history: &dir.Status.Preparer,
		outcomes: prepare.Outcomes, failed: prepare.Failed,
	}
}

func differs(p *sharedfsv1alpha1.Provision, res prepare.Result) string {
	var d string
	if res.UID != p.UID {
		d += fmt.Sprintf(" uid %d (wanted %d)", res.UID, p.UID)
	}
	if res.GID != p.GID {
		d += fmt.Sprintf(" gid %d (wanted %d)", res.GID, p.GID)
	}
	want, _ := strconv.ParseUint(p.Mode, 8, 32)
	got, _ := strconv.ParseUint(res.Mode, 8, 32)
	if want != got {
		d += fmt.Sprintf(" mode %s (wanted %04o)", res.Mode, want)
	}
	return d
}

func updateDirStatus(ctx context.Context, c client.Client, dir, before *sharedfsv1alpha1.FilesystemShareDirectory) error {
	if equality.Semantic.DeepEqual(&before.Status, &dir.Status) {
		return nil
	}
	return c.Status().Patch(ctx, dir, client.MergeFrom(before))
}

func (r *DirectoryReconciler) updateStatus(ctx context.Context, dir, before *sharedfsv1alpha1.FilesystemShareDirectory) error {
	return updateDirStatus(ctx, r.Client, dir, before)
}

// SetupWithManager sets up the controller with the Manager.
func (r *DirectoryReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&sharedfsv1alpha1.FilesystemShareDirectory{}).
		Owns(&corev1.Pod{}).
		// A share turning Ready lets its directories be prepared.
		Watches(&sharedfsv1alpha1.FilesystemShare{}, handler.EnqueueRequestsFromMapFunc(directoriesOfShare(r.Client)),
			builder.WithPredicates(shareChanged)).
		Named("directory").
		Complete(r)
}

func directoriesOfShare(c client.Client) handler.MapFunc {
	return func(ctx context.Context, o client.Object) []reconcile.Request {
		var list sharedfsv1alpha1.FilesystemShareDirectoryList
		if err := c.List(ctx, &list, client.MatchingFields{indexDirShare: string(o.GetUID())}); err != nil {
			logf.FromContext(ctx).Error(err, "listing directories", "share", o.GetName())
			return nil
		}
		keys := make([]types.NamespacedName, 0, len(list.Items))
		for _, d := range list.Items {
			keys = append(keys, types.NamespacedName{Name: d.Name})
		}
		return toRequests(keys)
	}
}

// requeueSoon is for a step that waits on another controller's write.
var requeueSoon = ctrl.Result{RequeueAfter: 2 * time.Second}
