package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	sharedfsv1alpha1 "github.com/littlemex/sharedfs/api/v1alpha1"
	"github.com/littlemex/sharedfs/internal/prepare"
	"github.com/littlemex/sharedfs/internal/sharepath"
	"github.com/littlemex/sharedfs/internal/volume"
)

const (
	// preparerDeadline bounds one preparer pod, including the time its volume takes to mount: a
	// missing parent directory leaves the pod in ContainerCreating until the deadline.
	preparerDeadline = 3 * time.Minute
	// preparerMountPath is where the preparer sees the parent of the directory.
	// preparerTimeout is when a pod that has not finished is given up on, which also covers a pod
	// that is never scheduled, where activeDeadlineSeconds does not run.
	preparerTimeout   = preparerDeadline + time.Minute
	preparerMountPath = "/parent"
	maxBackoff        = time.Hour
)

// DirectoryReconciler prepares the directory a FilesystemShareDirectory records, when its share
// asks for preparation. It runs one short-lived pod per directory that mounts only the parent,
// creates the directory with the share's owner and mode, and reports the result as JSON. An
// existing directory is never changed.
type DirectoryReconciler struct {
	client.Client
	Preparer PreparerConfig
}

// +kubebuilder:rbac:groups=sharedfs.littlemex.github.io,resources=filesystemsharedirectories,verbs=get;list;watch;delete
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
	if share.UID != dir.Spec.ShareRef.UID || share.Spec.Provision == nil ||
		meta.IsStatusConditionTrue(dir.Status.Conditions, sharedfsv1alpha1.ConditionPrepared) {
		return ctrl.Result{}, r.cleanup(ctx, &dir)
	}
	before := dir.DeepCopy()

	var pod corev1.Pod
	err := r.Get(ctx, types.NamespacedName{Namespace: r.Preparer.Namespace, Name: volume.PreparerName(dir.UID)}, &pod)
	if apierrors.IsNotFound(err) {
		// A share that is not Ready (a changed source, an unsafe driver) prepares nothing.
		if !isReady(&share) {
			return ctrl.Result{}, nil
		}
		if wait := backoff(&dir); wait > 0 {
			return ctrl.Result{RequeueAfter: wait}, nil
		}
		if waiting, err := r.start(ctx, &dir, &share); err != nil || waiting {
			if err == nil {
				return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
			}
			if apierrors.IsInvalid(err) || apierrors.IsForbidden(err) {
				// Refused by admission (Pod Security, a policy engine, a quota): say so and retry later.
				dir.Status.Failures++
				dir.Status.LastAttemptTime = &metav1.Time{Time: time.Now()}
				setCondition(&dir.Status.Conditions, dir.Generation, sharedfsv1alpha1.ConditionPrepared, false,
					sharedfsv1alpha1.ReasonPreparerNotCreated, truncate(err.Error()))
				return ctrl.Result{RequeueAfter: backoff(&dir)}, r.updateStatus(ctx, &dir, before)
			}
			return ctrl.Result{}, err
		}
		dir.Status.LastAttemptTime = &metav1.Time{Time: time.Now()}
		setCondition(&dir.Status.Conditions, dir.Generation, sharedfsv1alpha1.ConditionPrepared, false,
			sharedfsv1alpha1.ReasonPreparing, fmt.Sprintf("preparer pod %s/%s started", r.Preparer.Namespace, volume.PreparerName(dir.UID)))
		return ctrl.Result{RequeueAfter: preparerTimeout}, r.updateStatus(ctx, &dir, before)
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	if !metav1.IsControlledBy(&pod, &dir) {
		// Not ours: never take a result from a pod someone else created under this name.
		return ctrl.Result{RequeueAfter: time.Minute}, fmt.Errorf("pod %s/%s is not controlled by %s", pod.Namespace, pod.Name, dir.Name)
	}
	if !pod.DeletionTimestamp.IsZero() {
		// A finished pod is being removed; its result is already recorded.
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
	res, done := result(&pod)
	if !done {
		// Also covers a pod that is never scheduled, where activeDeadlineSeconds never starts.
		if age := time.Since(pod.CreationTimestamp.Time); age < preparerTimeout {
			return ctrl.Result{RequeueAfter: preparerTimeout - age}, nil
		}
		res = prepare.Result{Outcome: sharedfsv1alpha1.ReasonPreparerTimedOut, Message: stuck(&pod)}
	}
	ok := res.Outcome == prepare.Created || res.Outcome == prepare.Exists
	msg := res.Message
	if ok {
		dir.Status.Observed = &sharedfsv1alpha1.ObservedDirectory{UID: res.UID, GID: res.GID, Mode: res.Mode}
		if d := differs(share.Spec.Provision, res); d != "" {
			msg = "existing directory left unchanged; it differs from provision:" + d
		}
	} else {
		dir.Status.Failures++
	}
	setCondition(&dir.Status.Conditions, dir.Generation, sharedfsv1alpha1.ConditionPrepared, ok, res.Outcome, truncate(msg))
	if err := r.updateStatus(ctx, &dir, before); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.cleanup(ctx, &dir); err != nil {
		return ctrl.Result{}, err
	}
	if ok {
		return ctrl.Result{}, nil
	}
	return ctrl.Result{RequeueAfter: max(backoff(&dir), 5*time.Second)}, nil
}

// backoff is the time left before the next attempt: after n failures, 30s doubled n-1 times, at
// most an hour, counted from the latest attempt.
func backoff(dir *sharedfsv1alpha1.FilesystemShareDirectory) time.Duration {
	if dir.Status.Failures == 0 || dir.Status.LastAttemptTime == nil {
		return 0
	}
	d := min(30*time.Second<<min(dir.Status.Failures-1, 7), maxBackoff)
	return max(time.Until(dir.Status.LastAttemptTime.Add(d)), 0)
}

// result reads the preparer's report. The exit code is not trusted: only the JSON in the
// termination message decides, its outcome must be one a preparer reports, and a pod that ended
// without a valid report failed.
func result(pod *corev1.Pod) (prepare.Result, bool) {
	if pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed {
		return prepare.Result{}, false
	}
	for _, s := range pod.Status.ContainerStatuses {
		if t := s.State.Terminated; t != nil && t.Message != "" {
			var res prepare.Result
			if err := json.Unmarshal([]byte(t.Message), &res); err == nil && slices.Contains(prepare.Outcomes, res.Outcome) {
				return res, true
			}
			return prepare.Result{Outcome: prepare.Failed, Message: "the preparer reported something that is not a result"}, true
		}
	}
	if pod.Status.Reason == "DeadlineExceeded" {
		return prepare.Result{Outcome: sharedfsv1alpha1.ReasonPreparerTimedOut, Message: stuck(pod)}, true
	}
	return prepare.Result{Outcome: prepare.Failed, Message: fmt.Sprintf(
		"the preparer pod ended without a result: %s %s", pod.Status.Reason, pod.Status.Message)}, true
}

// stuck says where a preparer pod that never reported got stuck.
func stuck(pod *corev1.Pod) string {
	if len(pod.Spec.SchedulingGates) > 0 {
		return fmt.Sprintf("the preparer pod is held by scheduling gate %s; exclude the controller namespace from whatever adds it (Kueue manages plain pods when configured to)", pod.Spec.SchedulingGates[0].Name)
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Status != corev1.ConditionTrue {
			return "the preparer pod was not scheduled: " + c.Message + " (see --preparer-node-selector and --preparer-tolerations)"
		}
	}
	for _, s := range pod.Status.ContainerStatuses {
		if w := s.State.Waiting; w != nil && w.Reason != "ContainerCreating" {
			return fmt.Sprintf("the preparer container is waiting: %s %s", w.Reason, w.Message)
		}
	}
	return fmt.Sprintf("the preparer did not start within %s, usually because its volume did not mount: "+
		"does the parent directory exist? (see the events of pod %s/%s)", preparerDeadline, pod.Namespace, pod.Name)
}

// truncate keeps a condition message within the API's limit.
func truncate(s string) string {
	const limit = 32768
	if len(s) > limit {
		return s[:limit-3] + "..."
	}
	return s
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

// start creates the internal claim, the volume reserved for it by UID, and the pod. The volume
// mounts the parent of the directory read-write, so all three live in the controller's
// namespace, which nothing else runs in, and are owned by the record for garbage collection.
func (r *DirectoryReconciler) start(ctx context.Context, dir *sharedfsv1alpha1.FilesystemShareDirectory, share *sharedfsv1alpha1.FilesystemShare) (bool, error) {
	name := volume.PreparerName(dir.UID)
	// The previous attempt's claim and volume may still be going away; wait rather than reuse them.
	for _, o := range []client.Object{
		&corev1.PersistentVolumeClaim{}, &corev1.PersistentVolume{},
	} {
		key := types.NamespacedName{Name: name}
		if _, ok := o.(*corev1.PersistentVolumeClaim); ok {
			key.Namespace = r.Preparer.Namespace
		}
		if err := r.Get(ctx, key, o); err == nil && !o.GetDeletionTimestamp().IsZero() {
			return true, nil
		} else if client.IgnoreNotFound(err) != nil {
			return false, err
		}
	}
	var sourcePV corev1.PersistentVolume
	if err := r.Get(ctx, types.NamespacedName{Name: share.Spec.PersistentVolumeRef.Name}, &sourcePV); err != nil {
		return false, err
	}
	src, err := volume.Resolve(&sourcePV)
	if err != nil {
		return true, nil
	}
	parent, leaf := sharepath.Parent(dir.Spec.Path)

	pvc := volume.PreparerPVC(dir, r.Preparer.Namespace)
	if err := controllerutil.SetControllerReference(dir, pvc, r.Scheme()); err != nil {
		return false, err
	}
	if err := r.Create(ctx, pvc); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return false, err
		}
		if err := r.Get(ctx, client.ObjectKeyFromObject(pvc), pvc); err != nil {
			return false, err
		}
	}
	pv := volume.PreparerPV(dir, src, parent, pvc)
	if err := controllerutil.SetControllerReference(dir, pv, r.Scheme()); err != nil {
		return false, err
	}
	if err := r.Create(ctx, pv); client.IgnoreAlreadyExists(err) != nil {
		return false, err
	}
	pod := r.pod(dir, share.Spec.Provision, leaf)
	if err := controllerutil.SetControllerReference(dir, pod, r.Scheme()); err != nil {
		return false, err
	}
	return false, client.IgnoreAlreadyExists(r.Create(ctx, pod))
}

// pod is the preparer. It runs as UID 0 with every capability dropped except CHOWN, which with
// fchmod-before-fchown is all that creating a directory for another owner takes; it fits Pod
// Security baseline. It has no service account token, so the pod that holds a read-write mount
// of the parent cannot reach the API.
func (r *DirectoryReconciler) pod(dir *sharedfsv1alpha1.FilesystemShareDirectory, p *sharedfsv1alpha1.Provision, leaf string) *corev1.Pod {
	name := volume.PreparerName(dir.UID)
	deadline := int64(preparerDeadline.Seconds())
	root := int64(0)
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: r.Preparer.Namespace,
			Labels: map[string]string{sharedfsv1alpha1.LabelInternal: string(dir.UID)},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:                 corev1.RestartPolicyNever,
			ActiveDeadlineSeconds:         &deadline,
			ServiceAccountName:            r.Preparer.ServiceAccount,
			AutomountServiceAccountToken:  new(bool),
			EnableServiceLinks:            new(bool),
			NodeSelector:                  r.Preparer.NodeSelector,
			Tolerations:                   r.Preparer.Tolerations,
			ImagePullSecrets:              r.Preparer.ImagePullSecrets,
			TerminationGracePeriodSeconds: new(int64),
			SecurityContext: &corev1.PodSecurityContext{
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			Containers: []corev1.Container{{
				Name:  "prepare",
				Image: r.Preparer.Image,
				Args: []string{
					"prepare-directory",
					"--root=" + preparerMountPath,
					"--name=" + leaf,
					"--uid=" + strconv.FormatInt(p.UID, 10),
					"--gid=" + strconv.FormatInt(p.GID, 10),
					"--mode=" + p.Mode,
				},
				TerminationMessagePolicy: corev1.TerminationMessageReadFile,
				SecurityContext: &corev1.SecurityContext{
					RunAsUser:                &root,
					RunAsGroup:               &root,
					RunAsNonRoot:             new(bool),
					AllowPrivilegeEscalation: new(bool),
					ReadOnlyRootFilesystem:   boolPtr(true),
					Capabilities: &corev1.Capabilities{
						Drop: []corev1.Capability{"ALL"},
						Add:  []corev1.Capability{"CHOWN"},
					},
				},
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: resource.MustParse("32Mi")},
					Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("64Mi")},
				},
				VolumeMounts: []corev1.VolumeMount{{Name: "parent", MountPath: preparerMountPath}},
			}},
			Volumes: []corev1.Volume{{
				Name: "parent",
				VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: name,
				}},
			}},
		},
	}
}

// cleanup removes the preparer's pod, claim and volume. The volume is Retain, so deleting it
// never reaches the driver.
func (r *DirectoryReconciler) cleanup(ctx context.Context, dir *sharedfsv1alpha1.FilesystemShareDirectory) error {
	name := volume.PreparerName(dir.UID)
	for _, o := range []client.Object{
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: r.Preparer.Namespace, Name: name}},
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Namespace: r.Preparer.Namespace, Name: name}},
		&corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: name}},
	} {
		if err := r.Delete(ctx, o, client.PropagationPolicy(metav1.DeletePropagationBackground)); client.IgnoreNotFound(err) != nil {
			return err
		}
	}
	return nil
}

func (r *DirectoryReconciler) updateStatus(ctx context.Context, dir, before *sharedfsv1alpha1.FilesystemShareDirectory) error {
	if equality.Semantic.DeepEqual(&before.Status, &dir.Status) {
		return nil
	}
	return r.Status().Patch(ctx, dir, client.MergeFrom(before))
}

// SetupWithManager sets up the controller with the Manager.
func (r *DirectoryReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&sharedfsv1alpha1.FilesystemShareDirectory{}).
		Owns(&corev1.Pod{}).
		// A share turning Ready lets its directories be prepared.
		Watches(&sharedfsv1alpha1.FilesystemShare{}, handler.EnqueueRequestsFromMapFunc(r.directoriesOfShare)).
		Named("directory").
		Complete(r)
}

func (r *DirectoryReconciler) directoriesOfShare(ctx context.Context, o client.Object) []reconcile.Request {
	var list sharedfsv1alpha1.FilesystemShareDirectoryList
	if err := r.List(ctx, &list, client.MatchingFields{indexDirShare: string(o.GetUID())}); err != nil {
		logf.FromContext(ctx).Error(err, "listing directories", "share", o.GetName())
		return nil
	}
	keys := make([]types.NamespacedName, 0, len(list.Items))
	for _, d := range list.Items {
		keys = append(keys, types.NamespacedName{Name: d.Name})
	}
	return toRequests(keys)
}

func boolPtr(v bool) *bool { return &v }
