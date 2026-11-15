package controller

import (
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sharedfsv1alpha1 "github.com/littlemex/sharedfs/api/v1alpha1"
	"github.com/littlemex/sharedfs/internal/volume"
)

// envtest runs an API server and etcd only. There is no PV binder, no garbage collector and no
// pvc-protection controller, so these tests check what sharedfs writes and how it reacts to
// states the tests set up by hand. Binding, GC and mounting are covered on a real cluster.

const (
	timeout  = 20 * time.Second
	interval = 200 * time.Millisecond
	allowKey = "sharedfs-test/allowed"
)

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

func namespace(allowed bool) *corev1.Namespace {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "t-" + rand.String(6), Labels: map[string]string{}}}
	if allowed {
		ns.Labels[allowKey] = "yes"
	}
	return ns
}

func sharedFS(name, source string, max sharedfsv1alpha1.Access) *sharedfsv1alpha1.SharedFilesystem {
	return &sharedfsv1alpha1.SharedFilesystem{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: sharedfsv1alpha1.SharedFilesystemSpec{
			Source:            sharedfsv1alpha1.FilesystemSource{PersistentVolumeName: source},
			AllowedNamespaces: &sharedfsv1alpha1.AllowedNamespaces{Selector: metav1.LabelSelector{MatchLabels: map[string]string{allowKey: "yes"}}},
			MaxAccess:         max,
		},
	}
}

func claim(ns, name, fs string, access sharedfsv1alpha1.Access) *sharedfsv1alpha1.SharedFilesystemClaim {
	return &sharedfsv1alpha1.SharedFilesystemClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       sharedfsv1alpha1.SharedFilesystemClaimSpec{FilesystemRef: sharedfsv1alpha1.FilesystemReference{Name: fs}, Access: access},
	}
}

func condition(obj client.Object, conds func() []metav1.Condition, typ string) func(g Gomega) *metav1.Condition {
	return func(g Gomega) *metav1.Condition {
		g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(obj), obj)).To(Succeed())
		return meta.FindStatusCondition(conds(), typ)
	}
}

func haveCondition(status metav1.ConditionStatus, reason string) OmegaMatcher {
	return And(Not(BeNil()), WithTransform(func(c *metav1.Condition) metav1.ConditionStatus { return c.Status }, Equal(status)),
		WithTransform(func(c *metav1.Condition) string { return c.Reason }, Equal(reason)))
}

// stripFinalizers stands in for controllers envtest does not run (pvc-protection, pv-protection).
func stripFinalizers(obj client.Object) {
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
		return
	}
	if len(obj.GetFinalizers()) == 0 {
		return
	}
	obj.SetFinalizers(nil)
	_ = k8sClient.Update(ctx, obj)
}

func gone(obj client.Object) func() bool {
	return func() bool {
		err := k8sClient.Get(ctx, client.ObjectKeyFromObject(obj), obj)
		if err == nil && obj.GetDeletionTimestamp() != nil {
			// envtest has no protection controllers: let deletion finish.
			stripFinalizers(obj)
			return false
		}
		return apierrors.IsNotFound(err)
	}
}

var _ = Describe("sharedfs", func() {
	var (
		src *corev1.PersistentVolume
		fs  *sharedfsv1alpha1.SharedFilesystem
		ns  *corev1.Namespace
	)

	setup := func(max sharedfsv1alpha1.Access, reclaim corev1.PersistentVolumeReclaimPolicy) {
		id := rand.String(6)
		src = sourcePV("src-"+id, reclaim)
		Expect(k8sClient.Create(ctx, src)).To(Succeed())
		fs = sharedFS("fs-"+id, src.Name, max)
		Expect(k8sClient.Create(ctx, fs)).To(Succeed())
		ns = namespace(true)
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		Eventually(condition(fs, func() []metav1.Condition { return fs.Status.Conditions }, sharedfsv1alpha1.ConditionSourceResolved),
			timeout, interval).Should(haveCondition(metav1.ConditionTrue, sharedfsv1alpha1.ReasonResolved))
	}

	pvOf := func(c *sharedfsv1alpha1.SharedFilesystemClaim) *corev1.PersistentVolume {
		return &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: volume.PVName(c.UID)}}
	}
	pvcOf := func(c *sharedfsv1alpha1.SharedFilesystemClaim) *corev1.PersistentVolumeClaim {
		return &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: c.Name, Namespace: c.Namespace}}
	}
	exists := func(obj client.Object) func() error {
		return func() error { return k8sClient.Get(ctx, client.ObjectKeyFromObject(obj), obj) }
	}

	Context("SharedFilesystem", func() {
		It("resolves only allow-listed fields and warns on a Delete source", func() {
			setup(sharedfsv1alpha1.AccessReadWrite, corev1.PersistentVolumeReclaimDelete)
			Expect(fs.Status.Resolved.VolumeHandle).To(Equal("fs-" + src.Name))
			Expect(fs.Status.Resolved.VolumeAttributes).To(HaveLen(2))
			Expect(fs.Status.ObservedGeneration).To(Equal(fs.Generation))
			Expect(meta.IsStatusConditionTrue(fs.Status.Conditions, sharedfsv1alpha1.ConditionSourceReclaimDelete)).To(BeTrue())
			Expect(meta.IsStatusConditionTrue(fs.Status.Conditions, sharedfsv1alpha1.ConditionReady)).To(BeTrue())
		})

		It("reports a missing source and an unsupported driver", func() {
			missing := sharedFS("fs-missing-"+rand.String(4), "does-not-exist", sharedfsv1alpha1.AccessReadOnly)
			Expect(k8sClient.Create(ctx, missing)).To(Succeed())
			Eventually(condition(missing, func() []metav1.Condition { return missing.Status.Conditions }, sharedfsv1alpha1.ConditionSourceResolved),
				timeout, interval).Should(haveCondition(metav1.ConditionFalse, sharedfsv1alpha1.ReasonSourceNotFound))

			efs := sourcePV("efs-"+rand.String(4), corev1.PersistentVolumeReclaimRetain)
			efs.Spec.CSI.Driver = "efs.csi.aws.com"
			Expect(k8sClient.Create(ctx, efs)).To(Succeed())
			bad := sharedFS("fs-efs-"+rand.String(4), efs.Name, sharedfsv1alpha1.AccessReadOnly)
			Expect(k8sClient.Create(ctx, bad)).To(Succeed())
			Eventually(condition(bad, func() []metav1.Condition { return bad.Status.Conditions }, sharedfsv1alpha1.ConditionSourceResolved),
				timeout, interval).Should(haveCondition(metav1.ConditionFalse, sharedfsv1alpha1.ReasonUnsupportedDriver))
		})

		It("refuses to change the source", func() {
			setup(sharedfsv1alpha1.AccessReadOnly, corev1.PersistentVolumeReclaimRetain)
			fs.Spec.Source.PersistentVolumeName = "other"
			err := k8sClient.Update(ctx, fs)
			Expect(apierrors.IsInvalid(err)).To(BeTrue(), fmt.Sprint(err))
		})
	})

	Context("SharedFilesystemClaim", func() {
		It("creates a reserved Retain PV and a PVC of the claim's name", func() {
			setup(sharedfsv1alpha1.AccessReadWrite, corev1.PersistentVolumeReclaimDelete)
			c := claim(ns.Name, "data", fs.Name, "")
			Expect(k8sClient.Create(ctx, c)).To(Succeed())
			Expect(c.Spec.Access).To(Equal(sharedfsv1alpha1.AccessReadOnly), "the API defaults access to ReadOnly")

			pv := pvOf(c)
			Eventually(exists(pv), timeout, interval).Should(Succeed())
			Expect(pv.Spec.PersistentVolumeReclaimPolicy).To(Equal(corev1.PersistentVolumeReclaimRetain))
			Expect(pv.Spec.MountOptions).To(Equal([]string{"flock", "ro"}))
			Expect(pv.Spec.AccessModes).To(Equal([]corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany}))
			Expect(pv.Spec.ClaimRef.Namespace).To(Equal(ns.Name))
			Expect(pv.Spec.ClaimRef.Name).To(Equal("data"))
			Expect(pv.Spec.ClaimRef.UID).To(BeEmpty())
			Expect(pv.Spec.CSI.VolumeHandle).To(Equal("fs-" + src.Name + "-sfc-" + string(c.UID)))

			pvc := pvcOf(c)
			Eventually(exists(pvc), timeout, interval).Should(Succeed())
			Expect(pvc.Spec.VolumeName).To(Equal(pv.Name))
			Expect(*pvc.Spec.StorageClassName).To(Equal(""))
			Expect(metav1.IsControlledBy(pvc, c)).To(BeTrue())

			// No binder in envtest: the claim is accepted and waits for the bind.
			Eventually(condition(c, func() []metav1.Condition { return c.Status.Conditions }, sharedfsv1alpha1.ConditionAccepted),
				timeout, interval).Should(haveCondition(metav1.ConditionTrue, sharedfsv1alpha1.ReasonAccepted))
			Expect(meta.IsStatusConditionTrue(c.Status.Conditions, sharedfsv1alpha1.ConditionReady)).To(BeFalse())

			By("reporting Ready once the PVC is Bound")
			pvc.Status.Phase = corev1.ClaimBound
			Expect(k8sClient.Status().Update(ctx, pvc)).To(Succeed())
			Eventually(condition(c, func() []metav1.Condition { return c.Status.Conditions }, sharedfsv1alpha1.ConditionReady),
				timeout, interval).Should(haveCondition(metav1.ConditionTrue, sharedfsv1alpha1.ReasonReady))

			By("refusing a spec change")
			c.Spec.Access = sharedfsv1alpha1.AccessReadWrite
			Expect(apierrors.IsInvalid(k8sClient.Update(ctx, c))).To(BeTrue())
		})

		It("rejects a namespace that is not selected, without creating anything", func() {
			setup(sharedfsv1alpha1.AccessReadOnly, corev1.PersistentVolumeReclaimRetain)
			other := namespace(false)
			Expect(k8sClient.Create(ctx, other)).To(Succeed())
			c := claim(other.Name, "data", fs.Name, sharedfsv1alpha1.AccessReadOnly)
			Expect(k8sClient.Create(ctx, c)).To(Succeed())
			Eventually(condition(c, func() []metav1.Condition { return c.Status.Conditions }, sharedfsv1alpha1.ConditionAccepted),
				timeout, interval).Should(haveCondition(metav1.ConditionFalse, sharedfsv1alpha1.ReasonNamespaceNotAllowed))
			Consistently(exists(pvOf(c)), 2*time.Second, interval).ShouldNot(Succeed())
			Consistently(exists(pvcOf(c)), time.Second, interval).ShouldNot(Succeed())
		})

		It("rejects ReadWrite on a ReadOnly filesystem", func() {
			setup(sharedfsv1alpha1.AccessReadOnly, corev1.PersistentVolumeReclaimRetain)
			c := claim(ns.Name, "data", fs.Name, sharedfsv1alpha1.AccessReadWrite)
			Expect(k8sClient.Create(ctx, c)).To(Succeed())
			Eventually(condition(c, func() []metav1.Condition { return c.Status.Conditions }, sharedfsv1alpha1.ConditionReady),
				timeout, interval).Should(haveCondition(metav1.ConditionFalse, sharedfsv1alpha1.ReasonAccessNotAllowed))
			Consistently(exists(pvOf(c)), 2*time.Second, interval).ShouldNot(Succeed())
		})

		It("revokes when the namespace stops being selected", func() {
			setup(sharedfsv1alpha1.AccessReadOnly, corev1.PersistentVolumeReclaimRetain)
			c := claim(ns.Name, "data", fs.Name, sharedfsv1alpha1.AccessReadOnly)
			Expect(k8sClient.Create(ctx, c)).To(Succeed())
			pv, pvc := pvOf(c), pvcOf(c)
			Eventually(exists(pvc), timeout, interval).Should(Succeed())

			delete(ns.Labels, allowKey)
			Expect(k8sClient.Update(ctx, ns)).To(Succeed())
			Eventually(gone(pvc), timeout, interval).Should(BeTrue())
			Eventually(gone(pv), timeout, interval).Should(BeTrue())
			Eventually(condition(c, func() []metav1.Condition { return c.Status.Conditions }, sharedfsv1alpha1.ConditionAccepted),
				timeout, interval).Should(haveCondition(metav1.ConditionFalse, sharedfsv1alpha1.ReasonNamespaceNotAllowed))
		})

		It("keeps the volumes when the source is only temporarily unreadable", func() {
			setup(sharedfsv1alpha1.AccessReadOnly, corev1.PersistentVolumeReclaimRetain)
			c := claim(ns.Name, "data", fs.Name, sharedfsv1alpha1.AccessReadOnly)
			Expect(k8sClient.Create(ctx, c)).To(Succeed())
			pvc := pvcOf(c)
			Eventually(exists(pvc), timeout, interval).Should(Succeed())

			Expect(k8sClient.Delete(ctx, src)).To(Succeed())
			Eventually(gone(src), timeout, interval).Should(BeTrue())
			Eventually(condition(c, func() []metav1.Condition { return c.Status.Conditions }, sharedfsv1alpha1.ConditionAccepted),
				timeout, interval).Should(haveCondition(metav1.ConditionFalse, sharedfsv1alpha1.ReasonFilesystemNotReady))
			Consistently(func() error {
				var live corev1.PersistentVolumeClaim
				if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(pvc), &live); err != nil {
					return err
				}
				if live.DeletionTimestamp != nil {
					return fmt.Errorf("PVC is being deleted")
				}
				return nil
			}, 2*time.Second, interval).Should(Succeed())
		})

		It("does not adopt someone else's PVC of the same name", func() {
			setup(sharedfsv1alpha1.AccessReadOnly, corev1.PersistentVolumeReclaimRetain)
			foreign := &corev1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{Name: "data", Namespace: ns.Name},
				Spec: corev1.PersistentVolumeClaimSpec{
					AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
					Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}},
				},
			}
			Expect(k8sClient.Create(ctx, foreign)).To(Succeed())
			c := claim(ns.Name, "data", fs.Name, sharedfsv1alpha1.AccessReadOnly)
			Expect(k8sClient.Create(ctx, c)).To(Succeed())
			Eventually(condition(c, func() []metav1.Condition { return c.Status.Conditions }, sharedfsv1alpha1.ConditionAccepted),
				timeout, interval).Should(haveCondition(metav1.ConditionFalse, sharedfsv1alpha1.ReasonPVCConflict))
			Consistently(exists(pvOf(c)), 2*time.Second, interval).ShouldNot(Succeed(), "no PV may be reserved for a name someone else holds")
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(foreign), foreign)).To(Succeed())
			Expect(foreign.OwnerReferences).To(BeEmpty())
		})

		It("puts back an ownerReference the tenant removed", func() {
			setup(sharedfsv1alpha1.AccessReadOnly, corev1.PersistentVolumeReclaimRetain)
			c := claim(ns.Name, "data", fs.Name, sharedfsv1alpha1.AccessReadOnly)
			Expect(k8sClient.Create(ctx, c)).To(Succeed())
			pvc := pvcOf(c)
			Eventually(exists(pvc), timeout, interval).Should(Succeed())
			pvc.OwnerReferences = nil
			Expect(k8sClient.Update(ctx, pvc)).To(Succeed())
			Eventually(func(g Gomega) bool {
				g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(pvc), pvc)).To(Succeed())
				return metav1.IsControlledBy(pvc, c)
			}, timeout, interval).Should(BeTrue())
		})

		It("clears a stale claim UID on a Released PV, and never on a Bound one", func() {
			setup(sharedfsv1alpha1.AccessReadOnly, corev1.PersistentVolumeReclaimRetain)
			c := claim(ns.Name, "data", fs.Name, sharedfsv1alpha1.AccessReadOnly)
			Expect(k8sClient.Create(ctx, c)).To(Succeed())
			pv, pvc := pvOf(c), pvcOf(c)
			Eventually(exists(pvc), timeout, interval).Should(Succeed())
			Eventually(exists(pv), timeout, interval).Should(Succeed())

			By("a Bound PV with a mismatching UID is left alone")
			pv.Spec.ClaimRef.UID = types.UID("previous-pvc")
			Expect(k8sClient.Update(ctx, pv)).To(Succeed())
			pv.Status.Phase = corev1.VolumeBound
			Expect(k8sClient.Status().Update(ctx, pv)).To(Succeed())
			Consistently(func(g Gomega) types.UID {
				g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(pv), pv)).To(Succeed())
				return pv.Spec.ClaimRef.UID
			}, 2*time.Second, interval).Should(Equal(types.UID("previous-pvc")))

			By("the same PV once Released is healed")
			pv.Status.Phase = corev1.VolumeReleased
			Expect(k8sClient.Status().Update(ctx, pv)).To(Succeed())
			Eventually(func(g Gomega) types.UID {
				g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(pv), pv)).To(Succeed())
				return pv.Spec.ClaimRef.UID
			}, timeout, interval).Should(BeEmpty())
			Expect(pv.Spec.ClaimRef.Name).To(Equal("data"), "only the uid goes; the reservation stays")
		})

		It("deletes PVC, then PV, then itself", func() {
			setup(sharedfsv1alpha1.AccessReadOnly, corev1.PersistentVolumeReclaimRetain)
			c := claim(ns.Name, "data", fs.Name, sharedfsv1alpha1.AccessReadOnly)
			Expect(k8sClient.Create(ctx, c)).To(Succeed())
			pv, pvc := pvOf(c), pvcOf(c)
			Eventually(exists(pv), timeout, interval).Should(Succeed())
			Eventually(exists(pvc), timeout, interval).Should(Succeed())

			Expect(k8sClient.Delete(ctx, c)).To(Succeed())
			Eventually(gone(pvc), timeout, interval).Should(BeTrue())
			Eventually(gone(pv), timeout, interval).Should(BeTrue())
			Eventually(gone(c), timeout, interval).Should(BeTrue())
		})
	})

	Context("orphans and deletion guards", func() {
		It("sweeps the volumes of a claim whose finalizer was removed by hand", func() {
			setup(sharedfsv1alpha1.AccessReadOnly, corev1.PersistentVolumeReclaimRetain)
			c := claim(ns.Name, "data", fs.Name, sharedfsv1alpha1.AccessReadOnly)
			Expect(k8sClient.Create(ctx, c)).To(Succeed())
			pv, pvc := pvOf(c), pvcOf(c)
			Eventually(exists(pv), timeout, interval).Should(Succeed())
			Eventually(exists(pvc), timeout, interval).Should(Succeed())

			// Forcibly remove the claim without letting its finalizer run, and detach the PVC
			// from it so no garbage collector would help either.
			pvc.OwnerReferences = nil
			Expect(k8sClient.Update(ctx, pvc)).To(Succeed())
			Expect(k8sClient.Delete(ctx, c)).To(Succeed())
			stripFinalizers(c)
			Eventually(gone(c), timeout, interval).Should(BeTrue())

			Eventually(gone(pvc), timeout, interval).Should(BeTrue())
			Eventually(gone(pv), timeout, interval).Should(BeTrue())
		})

		It("keeps a SharedFilesystem until its generated volumes are gone", func() {
			setup(sharedfsv1alpha1.AccessReadOnly, corev1.PersistentVolumeReclaimRetain)
			c := claim(ns.Name, "data", fs.Name, sharedfsv1alpha1.AccessReadOnly)
			Expect(k8sClient.Create(ctx, c)).To(Succeed())
			Eventually(exists(pvOf(c)), timeout, interval).Should(Succeed())

			Expect(k8sClient.Delete(ctx, fs)).To(Succeed())
			Eventually(condition(fs, func() []metav1.Condition { return fs.Status.Conditions }, sharedfsv1alpha1.ConditionDeleting),
				timeout, interval).Should(haveCondition(metav1.ConditionTrue, sharedfsv1alpha1.ReasonVolumesRemaining))
			Eventually(condition(c, func() []metav1.Condition { return c.Status.Conditions }, sharedfsv1alpha1.ConditionAccepted),
				timeout, interval).Should(haveCondition(metav1.ConditionFalse, sharedfsv1alpha1.ReasonFilesystemDeleting))

			Expect(k8sClient.Delete(ctx, c)).To(Succeed())
			Eventually(gone(pvcOf(c)), timeout, interval).Should(BeTrue())
			Eventually(gone(pvOf(c)), timeout, interval).Should(BeTrue())
			Eventually(gone(fs), timeout, interval).Should(BeTrue())
		})

		It("ignores a rejected claim when guarding filesystem deletion", func() {
			setup(sharedfsv1alpha1.AccessReadOnly, corev1.PersistentVolumeReclaimRetain)
			other := namespace(false)
			Expect(k8sClient.Create(ctx, other)).To(Succeed())
			c := claim(other.Name, "data", fs.Name, sharedfsv1alpha1.AccessReadOnly)
			Expect(k8sClient.Create(ctx, c)).To(Succeed())
			Eventually(condition(c, func() []metav1.Condition { return c.Status.Conditions }, sharedfsv1alpha1.ConditionAccepted),
				timeout, interval).Should(haveCondition(metav1.ConditionFalse, sharedfsv1alpha1.ReasonNamespaceNotAllowed))
			Expect(k8sClient.Delete(ctx, fs)).To(Succeed())
			Eventually(gone(fs), timeout, interval).Should(BeTrue())
		})
	})
})
