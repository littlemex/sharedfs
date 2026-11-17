package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	sharedfsv1alpha1 "github.com/littlemex/sharedfs/api/v1alpha1"
	"github.com/littlemex/sharedfs/internal/metrics"
	"github.com/littlemex/sharedfs/internal/volume"
)

const (
	// taskDeadline bounds one task pod, including the time its volume takes to mount: a missing
	// directory leaves the pod in ContainerCreating until the deadline.
	taskDeadline = 3 * time.Minute
	// taskTimeout is when a pod that has not finished is given up on, which also covers a pod
	// that is never scheduled, where activeDeadlineSeconds does not run.
	taskTimeout = taskDeadline + time.Minute
	maxBackoff  = time.Hour
)

// task is one kind of short-lived pod a directory record needs: the preparer, which creates the
// directory, or the quota pod, which sets its limits. Each run is an internal claim, a volume
// reserved for it by UID that mounts one path read-write, and a pod, all owned by the record and
// all named after it. The pod reports a JSON result in its termination message; the exit code is
// not trusted.
type task struct {
	client.Client
	// what names the task in messages, for example "preparer".
	what string
	// condition is the record's condition the task drives.
	condition string
	name      string
	namespace string
	history   *sharedfsv1alpha1.TaskStatus
	// outcomes are the outcomes the pod may report; failed is reported for anything else.
	outcomes []string
	failed   string
}

// taskState is what one step of a task did.
type taskState int

const (
	taskIdle     taskState = iota // nothing to do yet, or waiting
	taskStarted                   // a pod was started
	taskRecorded                  // an outcome was recorded on the record's status
)

// taskReport is the part of every task result the controller reads first.
type taskReport struct {
	Outcome string `json:"outcome"`
	Message string `json:"message,omitempty"`
}

// run advances the task by one step. With no pod, it starts one when start allows (after the
// backoff); with a finished pod, it hands the raw report to finish, which records the outcome
// and reports whether it succeeded. The caller writes the status and cleans up.
func (t *task) run(ctx context.Context, dir *sharedfsv1alpha1.FilesystemShareDirectory, canStart bool,
	start func() (path string, pod *corev1.Pod, wait bool, err error),
	finish func(report taskReport, raw string) (ok bool)) (ctrl.Result, taskState, error) {
	var pod corev1.Pod
	err := t.Get(ctx, types.NamespacedName{Namespace: t.namespace, Name: t.name}, &pod)
	if apierrors.IsNotFound(err) {
		if !canStart {
			return ctrl.Result{}, taskIdle, nil
		}
		if wait := backoff(t.history); wait > 0 {
			return ctrl.Result{RequeueAfter: wait}, taskIdle, nil
		}
		waiting, err := t.start(ctx, dir, start)
		switch {
		case err == nil && waiting:
			return ctrl.Result{RequeueAfter: 5 * time.Second}, taskIdle, nil
		case err != nil && refused(err):
			// Refused by admission (Pod Security, a policy engine, a quota): say so, remove the
			// internal claim and volume, which mount a path read-write, and retry later.
			t.fail()
			metrics.TaskOutcomes.WithLabelValues(t.what, sharedfsv1alpha1.ReasonPreparerNotCreated).Inc()
			setCondition(&dir.Status.Conditions, dir.Generation, t.condition, false,
				sharedfsv1alpha1.ReasonPreparerNotCreated, truncate(fmt.Sprintf("the %s pod was refused: %v", t.what, err)))
			return ctrl.Result{RequeueAfter: backoff(t.history)}, taskRecorded, t.cleanup(ctx)
		case err != nil:
			return ctrl.Result{}, taskIdle, err
		}
		return ctrl.Result{RequeueAfter: taskTimeout}, taskStarted, nil
	}
	if err != nil {
		return ctrl.Result{}, taskIdle, err
	}
	if !metav1.IsControlledBy(&pod, dir) {
		// Not ours: never take a result from a pod someone else created under this name.
		return ctrl.Result{RequeueAfter: time.Minute}, taskIdle, fmt.Errorf("pod %s/%s is not controlled by %s", pod.Namespace, pod.Name, dir.Name)
	}
	if !pod.DeletionTimestamp.IsZero() || pod.UID == t.history.LastPod {
		// A finished pod whose outcome is recorded, on its way out.
		return ctrl.Result{RequeueAfter: 5 * time.Second}, taskIdle, t.cleanup(ctx)
	}
	report, raw, finished := t.report(&pod)
	if !finished {
		if age := time.Since(pod.CreationTimestamp.Time); age < taskTimeout {
			return ctrl.Result{RequeueAfter: taskTimeout - age}, taskIdle, nil
		}
		report = taskReport{Outcome: sharedfsv1alpha1.ReasonPreparerTimedOut, Message: t.stuck(&pod)}
	}
	t.history.LastPod = pod.UID
	metrics.TaskOutcomes.WithLabelValues(t.what, report.Outcome).Inc()
	ok := false
	if slices.Contains(t.outcomes, report.Outcome) {
		ok = finish(report, raw)
	} else {
		setCondition(&dir.Status.Conditions, dir.Generation, t.condition, false, report.Outcome, truncate(report.Message))
	}
	if !ok {
		t.fail()
	}
	res := ctrl.Result{}
	if !ok {
		res.RequeueAfter = max(backoff(t.history), 5*time.Second)
	}
	return res, taskRecorded, nil
}

func (t *task) fail() {
	t.history.Failures++
	t.history.LastFailureTime = &metav1.Time{Time: time.Now()}
}

// report reads a finished pod's result. A report that is not JSON, or names an outcome the task
// does not report, is a failure; so is a pod that ended without one.
func (t *task) report(pod *corev1.Pod) (taskReport, string, bool) {
	if pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed {
		return taskReport{}, "", false
	}
	for _, s := range pod.Status.ContainerStatuses {
		if term := s.State.Terminated; term != nil && term.Message != "" {
			var r taskReport
			if err := json.Unmarshal([]byte(term.Message), &r); err == nil && slices.Contains(t.outcomes, r.Outcome) {
				return r, term.Message, true
			}
			return taskReport{Outcome: t.failed, Message: fmt.Sprintf("the %s reported something that is not a result", t.what)}, "", true
		}
	}
	if pod.Status.Reason == "DeadlineExceeded" {
		return taskReport{Outcome: sharedfsv1alpha1.ReasonPreparerTimedOut, Message: t.stuck(pod)}, "", true
	}
	return taskReport{Outcome: t.failed, Message: fmt.Sprintf(
		"the %s pod ended without a result: %s %s", t.what, pod.Status.Reason, pod.Status.Message)}, "", true
}

// stuck says where a pod that never reported got stuck.
func (t *task) stuck(pod *corev1.Pod) string {
	if len(pod.Spec.SchedulingGates) > 0 {
		return fmt.Sprintf("the %s pod is held by scheduling gate %s; exclude its namespace from whatever adds it (Kueue manages plain pods when configured to)", t.what, pod.Spec.SchedulingGates[0].Name)
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Status != corev1.ConditionTrue {
			return fmt.Sprintf("the %s pod was not scheduled: %s (see its node selector and tolerations)", t.what, c.Message)
		}
	}
	for _, s := range pod.Status.ContainerStatuses {
		if w := s.State.Waiting; w != nil && w.Reason != "ContainerCreating" {
			return fmt.Sprintf("the %s container is waiting: %s %s", t.what, w.Reason, w.Message)
		}
	}
	return fmt.Sprintf("the %s did not start within %s, usually because its volume did not mount: "+
		"does the directory it mounts exist? (see the events of pod %s/%s)", t.what, taskDeadline, pod.Namespace, pod.Name)
}

// start creates the claim, the volume and the pod. It waits while the previous run's claim or
// volume is still going away, rather than reuse it.
func (t *task) start(ctx context.Context, dir *sharedfsv1alpha1.FilesystemShareDirectory,
	build func() (string, *corev1.Pod, bool, error)) (bool, error) {
	for _, o := range []client.Object{
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Namespace: t.namespace, Name: t.name}},
		&corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: t.name}},
	} {
		if err := t.Get(ctx, client.ObjectKeyFromObject(o), o); err == nil && !o.GetDeletionTimestamp().IsZero() {
			return true, nil
		} else if client.IgnoreNotFound(err) != nil {
			return false, err
		}
	}
	path, pod, wait, err := build()
	if err != nil || wait {
		return wait, err
	}
	src, err := t.source(ctx, dir)
	if err != nil || src == nil {
		return src == nil, err
	}
	pvc := volume.InternalPVC(t.name, dir, t.namespace)
	if err := controllerutil.SetControllerReference(dir, pvc, t.Scheme()); err != nil {
		return false, err
	}
	if err := t.Create(ctx, pvc); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return false, err
		}
		if err := t.Get(ctx, client.ObjectKeyFromObject(pvc), pvc); err != nil {
			return false, err
		}
	}
	pv := volume.InternalPV(t.name, dir, src, path, pvc)
	if err := controllerutil.SetControllerReference(dir, pv, t.Scheme()); err != nil {
		return false, err
	}
	if err := t.Create(ctx, pv); client.IgnoreAlreadyExists(err) != nil {
		return false, err
	}
	pod.Name, pod.Namespace = t.name, t.namespace
	if pod.Labels == nil {
		pod.Labels = map[string]string{}
	}
	pod.Labels[sharedfsv1alpha1.LabelInternal] = string(dir.UID)
	pod.Spec.Volumes = []corev1.Volume{{Name: "target", VolumeSource: corev1.VolumeSource{
		PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: t.name},
	}}}
	if err := controllerutil.SetControllerReference(dir, pod, t.Scheme()); err != nil {
		return false, err
	}
	return false, client.IgnoreAlreadyExists(t.Create(ctx, pod))
}

// source resolves the share's source volume, or nil while it does not resolve.
func (t *task) source(ctx context.Context, dir *sharedfsv1alpha1.FilesystemShareDirectory) (*volume.Source, error) {
	var share sharedfsv1alpha1.FilesystemShare
	if err := t.Get(ctx, types.NamespacedName{Name: dir.Spec.ShareRef.Name}, &share); err != nil {
		return nil, err
	}
	var pv corev1.PersistentVolume
	if err := t.Get(ctx, types.NamespacedName{Name: share.Spec.PersistentVolumeRef.Name}, &pv); err != nil {
		return nil, err
	}
	src, err := volume.Resolve(&pv)
	if err != nil {
		return nil, nil
	}
	return src, nil
}

// cleanup removes the pod, claim and volume of the task. The volume is Retain, so deleting it
// never reaches the driver. Objects not in the cache cost no API call.
func (t *task) cleanup(ctx context.Context) error {
	for _, o := range []client.Object{
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: t.namespace, Name: t.name}},
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Namespace: t.namespace, Name: t.name}},
		&corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: t.name}},
	} {
		if err := t.Get(ctx, client.ObjectKeyFromObject(o), o); apierrors.IsNotFound(err) || (err == nil && !o.GetDeletionTimestamp().IsZero()) {
			continue
		} else if err != nil {
			return err
		}
		if err := t.Delete(ctx, o, client.PropagationPolicy(metav1.DeletePropagationBackground)); client.IgnoreNotFound(err) != nil {
			return err
		}
	}
	return nil
}

// backoff is the time left before the next attempt: after n failures, 30s doubled n-1 times, at
// most an hour, counted from the latest failure.
func backoff(h *sharedfsv1alpha1.TaskStatus) time.Duration {
	if h.Failures == 0 || h.LastFailureTime == nil {
		return 0
	}
	d := min(30*time.Second<<min(h.Failures-1, 7), maxBackoff)
	return max(time.Until(h.LastFailureTime.Add(d)), 0)
}

// refused reports an error that retrying at once will not fix: admission refused the object.
func refused(err error) bool {
	return apierrors.IsInvalid(err) || apierrors.IsForbidden(err) || apierrors.IsBadRequest(err)
}

// truncate keeps a condition message within the API's limit.
func truncate(s string) string {
	const limit = 32768
	if len(s) > limit {
		return s[:limit-3] + "..."
	}
	return s
}

// taskPod is the common part of every task pod: UID 0 with only the given capabilities, no
// service account token, no service links, a read-only root, RuntimeDefault seccomp, and a
// deadline. The target volume is mounted at mountPath.
func taskPod(cfg PreparerConfig, image string, caps []corev1.Capability, mountPath string, args []string) *corev1.Pod {
	deadline := int64(taskDeadline.Seconds())
	root := int64(0)
	return &corev1.Pod{
		Spec: corev1.PodSpec{
			RestartPolicy:                 corev1.RestartPolicyNever,
			ActiveDeadlineSeconds:         &deadline,
			ServiceAccountName:            cfg.ServiceAccount,
			AutomountServiceAccountToken:  new(bool),
			EnableServiceLinks:            new(bool),
			NodeSelector:                  cfg.NodeSelector,
			Tolerations:                   cfg.Tolerations,
			ImagePullSecrets:              cfg.ImagePullSecrets,
			TerminationGracePeriodSeconds: new(int64),
			SecurityContext: &corev1.PodSecurityContext{
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			Containers: []corev1.Container{{
				Name:                     "task",
				Image:                    image,
				Args:                     args,
				TerminationMessagePolicy: corev1.TerminationMessageReadFile,
				SecurityContext: &corev1.SecurityContext{
					RunAsUser:                &root,
					RunAsGroup:               &root,
					RunAsNonRoot:             new(bool),
					AllowPrivilegeEscalation: new(bool),
					ReadOnlyRootFilesystem:   boolPtr(true),
					Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}, Add: caps},
				},
				Resources:    taskResources,
				VolumeMounts: []corev1.VolumeMount{{Name: "target", MountPath: mountPath}},
			}},
		},
	}
}

func boolPtr(v bool) *bool { return &v }
