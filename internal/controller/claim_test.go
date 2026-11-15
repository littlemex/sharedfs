package controller

import (
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
	"k8s.io/apimachinery/pkg/util/uuid"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sharedfsv1alpha1 "github.com/littlemex/sharedfs/api/v1alpha1"
	"github.com/littlemex/sharedfs/internal/volume"
)

var _ = Describe("SharedFilesystemClaim", func() {
	var (
		src *corev1.PersistentVolume
		fs  *sharedfsv1alpha1.SharedFilesystem
		ns  *corev1.Namespace
	)

	// setup makes a source, a filesystem with the given grants, and a namespace labelled label.
	setup := func(label string, grants ...sharedfsv1alpha1.Grant) {
		id := rand.String(6)
		src = sourcePV("src-"+id, corev1.PersistentVolumeReclaimRetain)
		Expect(k8sClient.Create(ctx, src)).To(Succeed())
		fs = sharedFS("fs-"+id, src.Name, grants...)
		Expect(k8sClient.Create(ctx, fs)).To(Succeed())
		ns = namespace(label)
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		Eventually(conditionOf(fs, sharedfsv1alpha1.ConditionReady), timeout, interval).
			Should(haveCondition(metav1.ConditionTrue, sharedfsv1alpha1.ReasonResolved))
	}
	accepted := func(name string, access sharedfsv1alpha1.Access) *sharedfsv1alpha1.SharedFilesystemClaim {
		c := claim(ns.Name, name, fs.Name, access)
		Expect(k8sClient.Create(ctx, c)).To(Succeed())
		Eventually(exists(pvOf(c)), timeout, interval).Should(Succeed())
		Eventually(exists(pvcOf(c)), timeout, interval).Should(Succeed())
		return c
	}

	It("creates a reserved Retain PV and a PVC of the claim's name", func() {
		setup("rw", grant("rw", sharedfsv1alpha1.AccessReadWrite))
		c := claim(ns.Name, "data", fs.Name, "")
		Expect(k8sClient.Create(ctx, c)).To(Succeed())
		Expect(c.Spec.Access).To(Equal(sharedfsv1alpha1.AccessReadOnly), "the API defaults access to ReadOnly")

		pv, pvc := pvOf(c), pvcOf(c)
		Eventually(exists(pv), timeout, interval).Should(Succeed())
		Expect(pv.Spec.PersistentVolumeReclaimPolicy).To(Equal(corev1.PersistentVolumeReclaimRetain))
		Expect(pv.Spec.MountOptions).To(Equal([]string{"flock", "ro"}))
		Expect(pv.Spec.AccessModes).To(Equal([]corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany}))
		Expect(pv.Spec.ClaimRef.Namespace).To(Equal(ns.Name))
		Expect(pv.Spec.ClaimRef.Name).To(Equal("data"))
		Expect(pv.Spec.ClaimRef.UID).To(BeEmpty())
		Expect(pv.Spec.CSI.VolumeHandle).To(Equal("fs-" + src.Name + "-sfc-" + string(c.UID)))

		Eventually(exists(pvc), timeout, interval).Should(Succeed())
		Expect(pvc.Spec.VolumeName).To(Equal(pv.Name))
		Expect(*pvc.Spec.StorageClassName).To(Equal(""))
		Expect(metav1.IsControlledBy(pvc, c)).To(BeTrue())

		Eventually(conditionOf(c, sharedfsv1alpha1.ConditionBound), timeout, interval).
			Should(haveCondition(metav1.ConditionFalse, sharedfsv1alpha1.ReasonPVCPending))
		Expect(meta.IsStatusConditionTrue(c.Status.Conditions, sharedfsv1alpha1.ConditionAccepted)).To(BeTrue())

		By("reporting Ready once the PVC is Bound")
		setPhase(pvc, corev1.ClaimBound)
		Eventually(conditionOf(c, sharedfsv1alpha1.ConditionReady), timeout, interval).
			Should(haveCondition(metav1.ConditionTrue, sharedfsv1alpha1.ReasonReady))
		Expect(c.Status.PersistentVolumeName).To(Equal(pv.Name))

		By("refusing a spec change")
		c.Spec.Access = sharedfsv1alpha1.AccessReadWrite
		Expect(apierrors.IsInvalid(k8sClient.Update(ctx, c))).To(BeTrue())

		By("counting the volume on the filesystem")
		Eventually(func(g Gomega) int32 {
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(fs), fs)).To(Succeed())
			return fs.Status.Volumes
		}, timeout, interval).Should(Equal(int32(1)))
	})

	It("gives a namespace the strongest of the grants that select it", func() {
		setup("rw", grant("rw", sharedfsv1alpha1.AccessReadOnly), grant("rw", sharedfsv1alpha1.AccessReadWrite))
		c := accepted("data", sharedfsv1alpha1.AccessReadWrite)
		pv := pvOf(c)
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(pv), pv)).To(Succeed())
		Expect(pv.Spec.MountOptions).To(Equal([]string{"flock"}), "read-write has no ro option")
	})

	It("rejects a namespace no grant selects, without creating anything", func() {
		setup("other", grant("rw", sharedfsv1alpha1.AccessReadWrite))
		c := claim(ns.Name, "data", fs.Name, sharedfsv1alpha1.AccessReadOnly)
		Expect(k8sClient.Create(ctx, c)).To(Succeed())
		Eventually(conditionOf(c, sharedfsv1alpha1.ConditionReady), timeout, interval).
			Should(haveCondition(metav1.ConditionFalse, sharedfsv1alpha1.ReasonNamespaceNotGranted))
		Consistently(exists(pvOf(c)), 2*time.Second, interval).ShouldNot(Succeed())
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(pvcOf(c)), pvcOf(c))).NotTo(Succeed())
		Expect(meta.FindStatusCondition(c.Status.Conditions, sharedfsv1alpha1.ConditionBound).Reason).
			To(Equal(sharedfsv1alpha1.ReasonNoVolume))
	})

	It("rejects ReadWrite where only ReadOnly is granted", func() {
		setup("ro", grant("ro", sharedfsv1alpha1.AccessReadOnly))
		c := claim(ns.Name, "data", fs.Name, sharedfsv1alpha1.AccessReadWrite)
		Expect(k8sClient.Create(ctx, c)).To(Succeed())
		Eventually(conditionOf(c, sharedfsv1alpha1.ConditionReady), timeout, interval).
			Should(haveCondition(metav1.ConditionFalse, sharedfsv1alpha1.ReasonAccessNotGranted))
		Consistently(exists(pvOf(c)), 2*time.Second, interval).ShouldNot(Succeed())
	})

	It("withdraws access once the grant has been gone for the grace period", func() {
		setup("ro", grant("ro", sharedfsv1alpha1.AccessReadOnly))
		c := accepted("data", sharedfsv1alpha1.AccessReadOnly)
		pvc := pvcOf(c)
		setPhase(pvc, corev1.ClaimBound)

		delete(ns.Labels, grantKey)
		Expect(k8sClient.Update(ctx, ns)).To(Succeed())
		Eventually(conditionOf(c, sharedfsv1alpha1.ConditionAccepted), timeout, interval).
			Should(haveCondition(metav1.ConditionFalse, sharedfsv1alpha1.ReasonNamespaceNotGranted))
		By("keeping the PVC during the grace period")
		// lastTransitionTime is stored with one-second resolution, so the grace period can end up
		// to a second early; check the part that is guaranteed.
		Consistently(notDeleting(pvc), gracePeriod-1500*time.Millisecond, interval).Should(Succeed())
		By("then deleting PVC and PV")
		Eventually(gone(pvc), timeout, interval).Should(BeTrue())
		Eventually(gone(pvOf(c)), timeout, interval).Should(BeTrue())
		Eventually(conditionOf(c, sharedfsv1alpha1.ConditionBound), timeout, interval).
			Should(haveCondition(metav1.ConditionFalse, sharedfsv1alpha1.ReasonNoVolume), "Bound is not left over")
	})

	It("does not withdraw access for a grant that flaps back within the grace period", func() {
		setup("ro", grant("ro", sharedfsv1alpha1.AccessReadOnly))
		c := accepted("data", sharedfsv1alpha1.AccessReadOnly)
		delete(ns.Labels, grantKey)
		Expect(k8sClient.Update(ctx, ns)).To(Succeed())
		Eventually(conditionOf(c, sharedfsv1alpha1.ConditionAccepted), timeout, interval).
			Should(haveCondition(metav1.ConditionFalse, sharedfsv1alpha1.ReasonNamespaceNotGranted))
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(ns), ns)).To(Succeed())
		ns.Labels[grantKey] = "ro"
		Expect(k8sClient.Update(ctx, ns)).To(Succeed())
		Consistently(notDeleting(pvcOf(c)), gracePeriod+2*time.Second, interval).Should(Succeed())
		Eventually(conditionOf(c, sharedfsv1alpha1.ConditionAccepted), timeout, interval).
			Should(haveCondition(metav1.ConditionTrue, sharedfsv1alpha1.ReasonAccepted))
	})

	It("keeps the volumes when the source is only temporarily unreadable", func() {
		setup("ro", grant("ro", sharedfsv1alpha1.AccessReadOnly))
		c := accepted("data", sharedfsv1alpha1.AccessReadOnly)
		Expect(k8sClient.Delete(ctx, src)).To(Succeed())
		Eventually(gone(src), timeout, interval).Should(BeTrue())
		Eventually(conditionOf(c, sharedfsv1alpha1.ConditionAccepted), timeout, interval).
			Should(haveCondition(metav1.ConditionFalse, sharedfsv1alpha1.ReasonSourceNotFound))
		Consistently(notDeleting(pvcOf(c)), gracePeriod+2*time.Second, interval).Should(Succeed())

		By("then losing the grant still gets the full grace period, counted from the loss")
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(ns), ns)).To(Succeed())
		delete(ns.Labels, grantKey)
		Expect(k8sClient.Update(ctx, ns)).To(Succeed())
		Eventually(conditionOf(c, sharedfsv1alpha1.ConditionAccepted), timeout, interval).
			Should(haveCondition(metav1.ConditionFalse, sharedfsv1alpha1.ReasonNamespaceNotGranted))
		Expect(c.Status.WithdrawAfter).NotTo(BeNil())
		Consistently(notDeleting(pvcOf(c)), gracePeriod-1500*time.Millisecond, interval).Should(Succeed())
		Eventually(gone(pvcOf(c)), timeout, interval).Should(BeTrue())
	})

	It("does not adopt someone else's PVC, and recovers once it is deleted", func() {
		setup("ro", grant("ro", sharedfsv1alpha1.AccessReadOnly))
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
		Eventually(conditionOf(c, sharedfsv1alpha1.ConditionBound), timeout, interval).
			Should(haveCondition(metav1.ConditionFalse, sharedfsv1alpha1.ReasonPVCConflict))
		Consistently(exists(pvOf(c)), 2*time.Second, interval).ShouldNot(Succeed(), "no PV is reserved for a name someone else holds")
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(foreign), foreign)).To(Succeed())
		Expect(foreign.OwnerReferences).To(BeEmpty())

		Expect(k8sClient.Delete(ctx, foreign)).To(Succeed())
		removeFinalizers(foreign, protectionFinalizers...)
		// The controller recreates a PVC of the same name at once, so wait for that, not for
		// the name to disappear.
		Eventually(exists(pvOf(c)), timeout, interval).Should(Succeed())
		pvc := pvcOf(c)
		Eventually(func(g Gomega) string {
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(pvc), pvc)).To(Succeed())
			return pvc.Spec.VolumeName
		}, timeout, interval).Should(Equal(volume.PVName(c.UID)))
	})

	It("puts back an ownerReference the tenant removed", func() {
		setup("ro", grant("ro", sharedfsv1alpha1.AccessReadOnly))
		c := accepted("data", sharedfsv1alpha1.AccessReadOnly)
		pvc := pvcOf(c)
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(pvc), pvc)).To(Succeed())
		pvc.OwnerReferences = nil
		Expect(k8sClient.Update(ctx, pvc)).To(Succeed())
		Eventually(func(g Gomega) bool {
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(pvc), pvc)).To(Succeed())
			return metav1.IsControlledBy(pvc, c)
		}, timeout, interval).Should(BeTrue())
	})

	It("clears a stale claim UID on a Released PV, and never on a Bound one", func() {
		setup("ro", grant("ro", sharedfsv1alpha1.AccessReadOnly))
		c := accepted("data", sharedfsv1alpha1.AccessReadOnly)
		pv := pvOf(c)
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(pv), pv)).To(Succeed())

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

	It("deletes the PVC first and the PV only after the PVC is gone", func() {
		setup("ro", grant("ro", sharedfsv1alpha1.AccessReadOnly))
		c := accepted("data", sharedfsv1alpha1.AccessReadOnly)
		pv, pvc := pvOf(c), pvcOf(c)
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(pvc), pvc)).To(Succeed())
		pvc.Finalizers = append(pvc.Finalizers, holdFinalizer)
		Expect(k8sClient.Update(ctx, pvc)).To(Succeed())

		Expect(k8sClient.Delete(ctx, c)).To(Succeed())
		Eventually(func(g Gomega) bool {
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(pvc), pvc)).To(Succeed())
			return pvc.DeletionTimestamp != nil
		}, timeout, interval).Should(BeTrue())
		Consistently(notDeleting(pv), 2*time.Second, interval).Should(Succeed(), "the PV waits while the PVC is in use")
		Eventually(conditionOf(c, sharedfsv1alpha1.ConditionReady), timeout, interval).
			Should(haveCondition(metav1.ConditionFalse, sharedfsv1alpha1.ReasonDeleting))

		removeFinalizers(pvc, holdFinalizer)
		Eventually(gone(pvc), timeout, interval).Should(BeTrue())
		Eventually(gone(pv), timeout, interval).Should(BeTrue())
		Eventually(gone(c), timeout, interval).Should(BeTrue())
	})
})

var _ = Describe("withdrawal and orphans", func() {
	It("withdraws access while the filesystem is being deleted when its grants are removed", func() {
		src := sourcePV("src-"+rand.String(6), corev1.PersistentVolumeReclaimRetain)
		Expect(k8sClient.Create(ctx, src)).To(Succeed())
		fs := sharedFS("fs-"+rand.String(6), src.Name, grant("ro", sharedfsv1alpha1.AccessReadOnly))
		Expect(k8sClient.Create(ctx, fs)).To(Succeed())
		ns := namespace("ro")
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		c := claim(ns.Name, "data", fs.Name, sharedfsv1alpha1.AccessReadOnly)
		Expect(k8sClient.Create(ctx, c)).To(Succeed())
		Eventually(exists(pvOf(c)), timeout, interval).Should(Succeed())

		Expect(k8sClient.Delete(ctx, fs)).To(Succeed())
		Eventually(conditionOf(fs, sharedfsv1alpha1.ConditionReady), timeout, interval).
			Should(haveCondition(metav1.ConditionFalse, sharedfsv1alpha1.ReasonVolumesRemaining))
		Eventually(conditionOf(c, sharedfsv1alpha1.ConditionAccepted), timeout, interval).
			Should(haveCondition(metav1.ConditionFalse, sharedfsv1alpha1.ReasonFilesystemDeleting))
		Consistently(notDeleting(pvcOf(c)), gracePeriod+time.Second, interval).Should(Succeed(),
			"deleting the filesystem alone does not withdraw access")

		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(fs), fs)).To(Succeed())
		fs.Spec.Grants = nil
		Expect(k8sClient.Update(ctx, fs)).To(Succeed())
		Eventually(gone(pvcOf(c)), timeout, interval).Should(BeTrue())
		Eventually(gone(pvOf(c)), timeout, interval).Should(BeTrue())
		Eventually(gone(fs), timeout, interval).Should(BeTrue(), "the guard lets go by itself once no volume is left")
	})

	It("withdraws access when the filesystem disappears without its finalizer", func() {
		src := sourcePV("src-"+rand.String(6), corev1.PersistentVolumeReclaimRetain)
		Expect(k8sClient.Create(ctx, src)).To(Succeed())
		fs := sharedFS("fs-"+rand.String(6), src.Name, grant("ro", sharedfsv1alpha1.AccessReadOnly))
		Expect(k8sClient.Create(ctx, fs)).To(Succeed())
		ns := namespace("ro")
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		c := claim(ns.Name, "data", fs.Name, sharedfsv1alpha1.AccessReadOnly)
		Expect(k8sClient.Create(ctx, c)).To(Succeed())
		Eventually(exists(pvcOf(c)), timeout, interval).Should(Succeed())

		Expect(k8sClient.Delete(ctx, fs)).To(Succeed())
		removeFinalizers(fs, sharedfsv1alpha1.Finalizer)
		Eventually(gone(fs), timeout, interval).Should(BeTrue())
		Eventually(conditionOf(c, sharedfsv1alpha1.ConditionAccepted), timeout, interval).
			Should(haveCondition(metav1.ConditionFalse, sharedfsv1alpha1.ReasonFilesystemNotFound))
		Eventually(gone(pvcOf(c)), timeout, interval).Should(BeTrue())
		Eventually(gone(pvOf(c)), timeout, interval).Should(BeTrue())
	})

	It("does not let a rejected claim block filesystem deletion", func() {
		src := sourcePV("src-"+rand.String(6), corev1.PersistentVolumeReclaimRetain)
		Expect(k8sClient.Create(ctx, src)).To(Succeed())
		fs := sharedFS("fs-"+rand.String(6), src.Name, grant("ro", sharedfsv1alpha1.AccessReadOnly))
		Expect(k8sClient.Create(ctx, fs)).To(Succeed())
		ns := namespace("")
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		c := claim(ns.Name, "data", fs.Name, sharedfsv1alpha1.AccessReadOnly)
		Expect(k8sClient.Create(ctx, c)).To(Succeed())
		Eventually(conditionOf(c, sharedfsv1alpha1.ConditionAccepted), timeout, interval).
			Should(haveCondition(metav1.ConditionFalse, sharedfsv1alpha1.ReasonNamespaceNotGranted))
		Expect(k8sClient.Delete(ctx, fs)).To(Succeed())
		Eventually(gone(fs), timeout, interval).Should(BeTrue())
	})

	It("sweeps a generated PV and PVC whose claim never existed", func() {
		ns := namespace("")
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		uid := uuid.NewUUID()
		pv := sourcePV(volume.PVName(uid), corev1.PersistentVolumeReclaimRetain)
		pv.Labels = map[string]string{sharedfsv1alpha1.LabelClaimUID: string(uid)}
		pv.Spec.ClaimRef = &corev1.ObjectReference{Kind: "PersistentVolumeClaim", Namespace: ns.Name, Name: "data"}
		pvc := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "data", Namespace: ns.Name},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
				StorageClassName: new(string),
				VolumeName:       pv.Name,
				Resources:        corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}},
			},
		}
		Expect(k8sClient.Create(ctx, pvc)).To(Succeed())
		Expect(k8sClient.Create(ctx, pv)).To(Succeed())
		Eventually(gone(pvc), timeout, interval).Should(BeTrue())
		Eventually(gone(pv), timeout, interval).Should(BeTrue())
	})
})
