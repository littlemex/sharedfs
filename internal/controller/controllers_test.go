package controller

import (
	"fmt"
	"slices"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/rand"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sharedfsv1alpha1 "github.com/littlemex/sharedfs/api/v1alpha1"
	"github.com/littlemex/sharedfs/internal/volume"
)

// envtest runs an API server and etcd only. There is no PV binder, no garbage collector and no
// pvc-protection controller, so these tests check what sharedfs writes and how it reacts to
// states the tests set by hand. Binding, GC and mounting are covered on a real cluster.

const (
	timeout     = 20 * time.Second
	interval    = 200 * time.Millisecond
	gracePeriod = 3 * time.Second
	grantKey    = "sharedfs-test/granted"
	// holdFinalizer stands in for a pod that keeps a PVC in use.
	holdFinalizer = "sharedfs-test/hold"
)

// protectionFinalizers are the ones the controllers envtest does not run would remove. The
// tests remove only these, never sharedfs's own.
var protectionFinalizers = []string{"kubernetes.io/pvc-protection", "kubernetes.io/pv-protection"}

func sourcePV(name string, reclaim corev1.PersistentVolumeReclaimPolicy) *corev1.PersistentVolume {
	return &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: corev1.PersistentVolumeSpec{
			Capacity:                      corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("4800Gi")},
			AccessModes:                   []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			PersistentVolumeReclaimPolicy: reclaim,
			MountOptions:                  []string{"flock"},
			PersistentVolumeSource: corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{
				Driver: "fsx.csi.aws.com", VolumeHandle: "fs-" + name,
				VolumeAttributes: map[string]string{"dnsname": name + ".fsx.example", "mountname": "abc"},
			}},
		},
	}
}

func namespace(granted string) *corev1.Namespace {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "t-" + rand.String(6), Labels: map[string]string{}}}
	if granted != "" {
		ns.Labels[grantKey] = granted
	}
	return ns
}

func grant(value string, access sharedfsv1alpha1.Access) sharedfsv1alpha1.Grant {
	return sharedfsv1alpha1.Grant{
		NamespaceSelector: metav1.LabelSelector{MatchLabels: map[string]string{grantKey: value}},
		Access:            access,
	}
}

func sharedFS(name, source string, grants ...sharedfsv1alpha1.Grant) *sharedfsv1alpha1.SharedFilesystem {
	return &sharedfsv1alpha1.SharedFilesystem{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: sharedfsv1alpha1.SharedFilesystemSpec{
			PersistentVolumeRef: sharedfsv1alpha1.PersistentVolumeReference{Name: source},
			Grants:              grants,
		},
	}
}

func claim(ns, name, fs string, access sharedfsv1alpha1.Access) *sharedfsv1alpha1.SharedFilesystemClaim {
	return &sharedfsv1alpha1.SharedFilesystemClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       sharedfsv1alpha1.SharedFilesystemClaimSpec{FilesystemRef: sharedfsv1alpha1.FilesystemReference{Name: fs}, Access: access},
	}
}

func conditionOf(obj client.Object, typ string) func(g Gomega) *metav1.Condition {
	return func(g Gomega) *metav1.Condition {
		g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(obj), obj)).To(Succeed())
		var conds []metav1.Condition
		switch o := obj.(type) {
		case *sharedfsv1alpha1.SharedFilesystem:
			conds = o.Status.Conditions
		case *sharedfsv1alpha1.SharedFilesystemClaim:
			conds = o.Status.Conditions
		}
		return meta.FindStatusCondition(conds, typ)
	}
}

func haveCondition(status metav1.ConditionStatus, reason string) OmegaMatcher {
	return And(Not(BeNil()),
		WithTransform(func(c *metav1.Condition) metav1.ConditionStatus { return c.Status }, Equal(status)),
		WithTransform(func(c *metav1.Condition) string { return c.Reason }, Equal(reason)))
}

func removeFinalizers(obj client.Object, which ...string) {
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
		return
	}
	kept := slices.DeleteFunc(slices.Clone(obj.GetFinalizers()), func(f string) bool { return slices.Contains(which, f) })
	if len(kept) == len(obj.GetFinalizers()) {
		return
	}
	obj.SetFinalizers(kept)
	_ = k8sClient.Update(ctx, obj)
}

// gone waits for an object to disappear, doing the protection controllers' part only.
func gone(obj client.Object) func() bool {
	return func() bool {
		err := k8sClient.Get(ctx, client.ObjectKeyFromObject(obj), obj)
		if err == nil && obj.GetDeletionTimestamp() != nil {
			removeFinalizers(obj, protectionFinalizers...)
			return false
		}
		return apierrors.IsNotFound(err)
	}
}

func exists(obj client.Object) func() error {
	return func() error { return k8sClient.Get(ctx, client.ObjectKeyFromObject(obj), obj) }
}

func notDeleting(obj client.Object) func() error {
	return func() error {
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
			return err
		}
		if obj.GetDeletionTimestamp() != nil {
			return fmt.Errorf("%s is being deleted", obj.GetName())
		}
		return nil
	}
}

func pvOf(c *sharedfsv1alpha1.SharedFilesystemClaim) *corev1.PersistentVolume {
	return &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: volume.PVName(c.UID)}}
}

func pvcOf(c *sharedfsv1alpha1.SharedFilesystemClaim) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: c.Name, Namespace: c.Namespace}}
}

func setPhase(pvc *corev1.PersistentVolumeClaim, phase corev1.PersistentVolumeClaimPhase) {
	Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(pvc), pvc)).To(Succeed())
	pvc.Status.Phase = phase
	Expect(k8sClient.Status().Update(ctx, pvc)).To(Succeed())
}

var _ = Describe("SharedFilesystem", func() {
	It("resolves only what the driver reads, and warns on a Delete source", func() {
		src := sourcePV("src-"+rand.String(6), corev1.PersistentVolumeReclaimDelete)
		Expect(k8sClient.Create(ctx, src)).To(Succeed())
		fs := sharedFS("fs-"+rand.String(6), src.Name)
		Expect(k8sClient.Create(ctx, fs)).To(Succeed())
		Eventually(conditionOf(fs, sharedfsv1alpha1.ConditionReady), timeout, interval).
			Should(haveCondition(metav1.ConditionTrue, sharedfsv1alpha1.ReasonResolved))
		Expect(fs.Status.Resolved.VolumeHandle).To(Equal("fs-" + src.Name))
		Expect(fs.Status.Resolved.VolumeAttributes).To(HaveLen(2))
		Expect(fs.Status.ObservedGeneration).To(Equal(fs.Generation))
		Expect(meta.FindStatusCondition(fs.Status.Conditions, sharedfsv1alpha1.ConditionReady).Message).
			To(ContainSubstring("reclaimPolicy Delete"))
	})

	It("reports a missing source, an unsupported driver and an invalid grant", func() {
		missing := sharedFS("fs-"+rand.String(6), "does-not-exist")
		Expect(k8sClient.Create(ctx, missing)).To(Succeed())
		Eventually(conditionOf(missing, sharedfsv1alpha1.ConditionReady), timeout, interval).
			Should(haveCondition(metav1.ConditionFalse, sharedfsv1alpha1.ReasonSourceNotFound))

		efs := sourcePV("efs-"+rand.String(6), corev1.PersistentVolumeReclaimRetain)
		efs.Spec.CSI.Driver = "efs.csi.aws.com"
		Expect(k8sClient.Create(ctx, efs)).To(Succeed())
		bad := sharedFS("fs-"+rand.String(6), efs.Name)
		Expect(k8sClient.Create(ctx, bad)).To(Succeed())
		Eventually(conditionOf(bad, sharedfsv1alpha1.ConditionReady), timeout, interval).
			Should(haveCondition(metav1.ConditionFalse, sharedfsv1alpha1.ReasonUnsupportedDriver))

		src := sourcePV("src-"+rand.String(6), corev1.PersistentVolumeReclaimRetain)
		Expect(k8sClient.Create(ctx, src)).To(Succeed())
		g := sharedfsv1alpha1.Grant{NamespaceSelector: metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
			{Key: grantKey, Operator: "Bogus"},
		}}}
		invalid := sharedFS("fs-"+rand.String(6), src.Name, g)
		Expect(k8sClient.Create(ctx, invalid)).To(Succeed())
		Eventually(conditionOf(invalid, sharedfsv1alpha1.ConditionReady), timeout, interval).
			Should(haveCondition(metav1.ConditionFalse, sharedfsv1alpha1.ReasonInvalidGrant))
	})

	It("refuses a grant with an empty namespace selector", func() {
		fs := sharedFS("fs-"+rand.String(6), "a", sharedfsv1alpha1.Grant{Access: sharedfsv1alpha1.AccessReadWrite})
		err := k8sClient.Create(ctx, fs)
		Expect(apierrors.IsInvalid(err)).To(BeTrue(), fmt.Sprint(err))
		Expect(err.Error()).To(ContainSubstring("namespaceSelector must not be empty"))
	})

	It("refuses to change the source", func() {
		fs := sharedFS("fs-"+rand.String(6), "a")
		Expect(k8sClient.Create(ctx, fs)).To(Succeed())
		// Retry on Conflict: the controller adds its finalizer concurrently.
		Eventually(func() error {
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(fs), fs)).To(Succeed())
			fs.Spec.PersistentVolumeRef.Name = "b"
			return k8sClient.Update(ctx, fs)
		}, timeout, interval).Should(WithTransform(apierrors.IsInvalid, BeTrue()))
	})
})
