package controller

import (
	"slices"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/rand"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sharedfsv1alpha1 "github.com/littlemex/sharedfs/api/v1alpha1"
	"github.com/littlemex/sharedfs/internal/prepare"
	"github.com/littlemex/sharedfs/internal/sharepath"
	"github.com/littlemex/sharedfs/internal/volume"
)

// envtest runs an API server and etcd only: no PV binder, no garbage collector, no kubelet. These
// tests check what sharedfs writes and how it reacts to states they set by hand (a claim's
// provisioner annotation, binding, a finished preparer pod). Binding, mounting and garbage
// collection are covered on a real cluster.

const (
	timeout             = 20 * time.Second
	interval            = 200 * time.Millisecond
	gracePeriod         = 3 * time.Second
	grantKey            = "sharedfs-test/granted"
	controllerNamespace = "sharedfs-system"
	preparerImage       = "example.com/sharedfs:test"
	lustre              = "fsx.csi.aws.com"
	openzfs             = "fsx.openzfs.csi.aws.com"
)

var protectionFinalizers = []string{"kubernetes.io/pvc-protection", "kubernetes.io/pv-protection"}

func ensureDrivers() {
	withFSType := storagev1.ReadWriteOnceWithFSTypeFSGroupPolicy
	file := storagev1.FileFSGroupPolicy
	for _, d := range []*storagev1.CSIDriver{
		{ObjectMeta: metav1.ObjectMeta{Name: lustre}, Spec: storagev1.CSIDriverSpec{
			FSGroupPolicy:        &withFSType,
			VolumeLifecycleModes: []storagev1.VolumeLifecycleMode{storagev1.VolumeLifecyclePersistent},
		}},
		// OpenZFS stands in for a driver whose CSIDriver policy would undo the guarantees.
		{ObjectMeta: metav1.ObjectMeta{Name: openzfs}, Spec: storagev1.CSIDriverSpec{FSGroupPolicy: &file}},
	} {
		if err := k8sClient.Create(ctx, d); err != nil && !apierrors.IsAlreadyExists(err) {
			Expect(err).NotTo(HaveOccurred())
		}
	}
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "sharedfs-preparer", Namespace: controllerNamespace}}
	if err := k8sClient.Create(ctx, sa); err != nil && !apierrors.IsAlreadyExists(err) {
		Expect(err).NotTo(HaveOccurred())
	}
}

// sourcePV is a Lustre file system reserved for an admin claim. Each test gets its own file
// system (dnsname), so overlap checks of different tests never meet.
func sourcePV(name string) *corev1.PersistentVolume {
	return &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: corev1.PersistentVolumeSpec{
			Capacity:                      corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("4800Gi")},
			AccessModes:                   []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
			MountOptions:                  []string{"flock"},
			ClaimRef:                      &corev1.ObjectReference{Namespace: "fs-admin", Name: "hold-" + name},
			PersistentVolumeSource: corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{
				Driver: lustre, VolumeHandle: "fs-" + name,
				VolumeAttributes: map[string]string{"dnsname": name + ".fsx.example", "mountname": "abc"},
			}},
		},
	}
}

func uniq(prefix string) string { return prefix + "-" + rand.String(6) }

func namespace(granted string) *corev1.Namespace {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: uniq("t"), Labels: map[string]string{}}}
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

func share(name, source, path string, grants ...sharedfsv1alpha1.Grant) *sharedfsv1alpha1.FilesystemShare {
	return &sharedfsv1alpha1.FilesystemShare{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: sharedfsv1alpha1.FilesystemShareSpec{
			PersistentVolumeRef: sharedfsv1alpha1.PersistentVolumeReference{Name: source},
			Path:                path,
			Grants:              grants,
		},
	}
}

// claim is a PVC as the PV controller leaves it for an external provisioner: annotated with the
// provisioner of its class.
func claim(ns, class string, modes ...corev1.PersistentVolumeAccessMode) *corev1.PersistentVolumeClaim {
	if len(modes) == 0 {
		modes = []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany}
	}
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: uniq("c"), Namespace: ns,
			Annotations: map[string]string{volume.AnnotationStorageProvisioner: sharedfsv1alpha1.ProvisionerName},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      modes,
			StorageClassName: &class,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
			},
		},
	}
}

func create(objs ...client.Object) {
	for _, o := range objs {
		Expect(k8sClient.Create(ctx, o)).To(Succeed())
	}
}

func readyOf(s *sharedfsv1alpha1.FilesystemShare) func(g Gomega) *metav1.Condition {
	return func(g Gomega) *metav1.Condition {
		g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(s), s)).To(Succeed())
		c := meta.FindStatusCondition(s.Status.Conditions, sharedfsv1alpha1.ConditionReady)
		if c != nil && c.ObservedGeneration != s.Generation {
			return nil
		}
		return c
	}
}

func haveCondition(status metav1.ConditionStatus, reason string) OmegaMatcher {
	return And(Not(BeNil()),
		WithTransform(func(c *metav1.Condition) metav1.ConditionStatus { return c.Status }, Equal(status)),
		WithTransform(func(c *metav1.Condition) string { return c.Reason }, Equal(reason)))
}

func readyShare(s *sharedfsv1alpha1.FilesystemShare) {
	Eventually(readyOf(s), timeout, interval).Should(haveCondition(metav1.ConditionTrue, sharedfsv1alpha1.ReasonResolved))
}

func pvOf(pvc *corev1.PersistentVolumeClaim) func() (*corev1.PersistentVolume, error) {
	return func() (*corev1.PersistentVolume, error) {
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(pvc), pvc)).To(Succeed())
		var pv corev1.PersistentVolume
		err := k8sClient.Get(ctx, client.ObjectKey{Name: volume.PVName(pvc.UID)}, &pv)
		return &pv, err
	}
}

func provisioned(pvc *corev1.PersistentVolumeClaim) *corev1.PersistentVolume {
	var pv *corev1.PersistentVolume
	Eventually(func() error {
		var err error
		pv, err = pvOf(pvc)()
		return err
	}, timeout, interval).Should(Succeed())
	return pv
}

func neverProvisioned(pvc *corev1.PersistentVolumeClaim) {
	Consistently(func() bool {
		_, err := pvOf(pvc)()
		return apierrors.IsNotFound(err)
	}, 2*time.Second, interval).Should(BeTrue())
}

func eventReasons(pvc *corev1.PersistentVolumeClaim) func() []string {
	return func() []string {
		var list corev1.EventList
		Expect(k8sClient.List(ctx, &list, client.InNamespace(pvc.Namespace))).To(Succeed())
		var out []string
		for _, e := range list.Items {
			if e.InvolvedObject.UID == pvc.UID {
				out = append(out, e.Reason)
			}
		}
		return out
	}
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
func gone(obj client.Object) {
	Eventually(func() bool {
		removeFinalizers(obj, protectionFinalizers...)
		return apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKeyFromObject(obj), obj))
	}, timeout, interval).Should(BeTrue())
}

// bind does the PV controller's part: the claim takes the volume's name and the volume is Bound.
func bind(pvc *corev1.PersistentVolumeClaim, pv *corev1.PersistentVolume) {
	Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(pvc), pvc)).To(Succeed())
	pvc.Spec.VolumeName = pv.Name
	Expect(k8sClient.Update(ctx, pvc)).To(Succeed())
	Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(pv), pv)).To(Succeed())
	pv.Status.Phase = corev1.VolumeBound
	Expect(k8sClient.Status().Update(ctx, pv)).To(Succeed())
}

var _ = Describe("FilesystemShare API", func() {
	It("accepts exactly the paths sharepath accepts", func() {
		ensureDrivers()
		for _, p := range sharepath.ValidExamples {
			s := share(uniq("cel"), "none", p, grant("x", sharedfsv1alpha1.AccessReadOnly))
			Expect(k8sClient.Create(ctx, s)).To(Succeed(), "path %q", p)
			Expect(k8sClient.Delete(ctx, s)).To(Succeed())
		}
		for _, p := range sharepath.InvalidExamples {
			s := share(uniq("cel"), "none", p, grant("x", sharedfsv1alpha1.AccessReadOnly))
			Expect(k8sClient.Create(ctx, s)).NotTo(Succeed(), "path %q", p)
		}
	})

	It("refuses provision on the root", func() {
		s := share(uniq("root"), "none", "/", grant("x", sharedfsv1alpha1.AccessReadOnly))
		s.Spec.Provision = &sharedfsv1alpha1.Provision{GID: 1, Mode: "2770"}
		Expect(apierrors.IsInvalid(k8sClient.Create(ctx, s))).To(BeTrue())
	})

	It("keeps everything but the grants immutable and refuses empty selectors", func() {
		s := share(uniq("imm"), "none", "datasets", grant("x", sharedfsv1alpha1.AccessReadOnly))
		create(s)
		// Wait for the controller's finalizer, so the updates below cannot fail on a conflict.
		Eventually(func(g Gomega) []string {
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(s), s)).To(Succeed())
			return s.Finalizers
		}, timeout, interval).Should(ContainElement(sharedfsv1alpha1.Finalizer))
		s.Spec.Path = "models"
		Expect(apierrors.IsInvalid(k8sClient.Update(ctx, s))).To(BeTrue())
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(s), s)).To(Succeed())
		s.Spec.Grants = []sharedfsv1alpha1.Grant{{Access: sharedfsv1alpha1.AccessReadWrite}}
		Expect(apierrors.IsInvalid(k8sClient.Update(ctx, s))).To(BeTrue())
		// The controller adds its finalizer meanwhile, so retry on a conflict.
		Eventually(func() error {
			if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(s), s); err != nil {
				return err
			}
			s.Spec.Grants = []sharedfsv1alpha1.Grant{grant("y", sharedfsv1alpha1.AccessReadWrite)}
			return k8sClient.Update(ctx, s)
		}, timeout, interval).Should(Succeed())
		Expect(k8sClient.Delete(ctx, s)).To(Succeed())
	})
})

var _ = Describe("FilesystemShare", func() {
	BeforeEach(ensureDrivers)

	It("becomes Ready on a reserved source and owns both StorageClasses", func() {
		src := sourcePV(uniq("src"))
		s := share(uniq("ds"), src.Name, "datasets", grant("a", sharedfsv1alpha1.AccessReadOnly))
		create(src, s)
		readyShare(s)
		Expect(s.Status.StorageClasses).To(Equal(&sharedfsv1alpha1.StorageClassNames{ReadWrite: s.Name + "-rw", ReadOnly: s.Name + "-ro"}))
		for _, name := range []string{s.Name + "-rw", s.Name + "-ro"} {
			var sc storagev1.StorageClass
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: name}, &sc)).To(Succeed())
			Expect(sc.Provisioner).To(Equal(sharedfsv1alpha1.ProvisionerName))
			Expect(metav1.IsControlledBy(&sc, s)).To(BeTrue())
			Expect(*sc.AllowVolumeExpansion).To(BeFalse())
			Expect(*sc.ReclaimPolicy).To(Equal(corev1.PersistentVolumeReclaimDelete))
			Expect(sc.Parameters[sharedfsv1alpha1.ParameterShareUID]).To(Equal(string(s.UID)))
		}
	})

	It("is not Ready on an unreserved source", func() {
		src := sourcePV(uniq("src"))
		src.Spec.ClaimRef = nil
		s := share(uniq("ds"), src.Name, "datasets", grant("a", sharedfsv1alpha1.AccessReadOnly))
		create(src, s)
		Eventually(readyOf(s), timeout, interval).Should(haveCondition(metav1.ConditionFalse, sharedfsv1alpha1.ReasonSourceUnreserved))
	})

	It("refuses a sub-path on a driver that cannot mount one, and an unsafe CSIDriver", func() {
		src := sourcePV(uniq("zfs"))
		src.Spec.CSI.Driver = openzfs
		src.Spec.CSI.VolumeAttributes = map[string]string{"DNSName": "zfs.example", "ResourceType": "filesystem"}
		sub := share(uniq("zsub"), src.Name, "datasets", grant("a", sharedfsv1alpha1.AccessReadOnly))
		root := share(uniq("zroot"), src.Name, "/", grant("a", sharedfsv1alpha1.AccessReadOnly))
		create(src, sub, root)
		Eventually(readyOf(sub), timeout, interval).Should(haveCondition(metav1.ConditionFalse, sharedfsv1alpha1.ReasonSubPathUnsupported))
		Eventually(readyOf(root), timeout, interval).Should(haveCondition(metav1.ConditionFalse, sharedfsv1alpha1.ReasonUnsafeDriverPolicy))
	})

	It("blocks the newer of two overlapping shares when the outer one grants ReadWrite", func() {
		src := sourcePV(uniq("src"))
		outer := share(uniq("outer"), src.Name, "teams", grant("a", sharedfsv1alpha1.AccessReadWrite))
		create(src, outer)
		readyShare(outer)
		// Creation times have a resolution of one second; within one second the UID decides.
		time.Sleep(1100 * time.Millisecond)
		inner := share(uniq("inner"), src.Name, "teams/{namespace}", grant("b", sharedfsv1alpha1.AccessReadWrite))
		create(inner)
		Eventually(readyOf(inner), timeout, interval).Should(haveCondition(metav1.ConditionFalse, sharedfsv1alpha1.ReasonOverlappingShare))
		Consistently(readyOf(outer), 2*time.Second, interval).Should(haveCondition(metav1.ConditionTrue, sharedfsv1alpha1.ReasonResolved))

		By("a disjoint path on the same file system is fine")
		other := share(uniq("other"), src.Name, "datasets", grant("b", sharedfsv1alpha1.AccessReadWrite))
		create(other)
		readyShare(other)

		By("an inner share already in use is not stopped by an outer share that gains ReadWrite later")
		src2 := sourcePV(uniq("src"))
		outer2 := share(uniq("outer"), src2.Name, "teams", grant("in2", sharedfsv1alpha1.AccessReadOnly))
		inner2 := share(uniq("inner"), src2.Name, "teams/x", grant("in2", sharedfsv1alpha1.AccessReadWrite))
		ns2 := namespace("in2")
		create(src2, outer2, inner2, ns2)
		readyShare(outer2)
		readyShare(inner2)
		create(claim(ns2.Name, inner2.Name+"-rw"))
		Eventually(func() int32 {
			_ = k8sClient.Get(ctx, client.ObjectKeyFromObject(inner2), inner2)
			return inner2.Status.Volumes
		}, timeout, interval).Should(BeNumerically(">", 0))
		By("even when the outer share is in use too")
		create(claim(ns2.Name, outer2.Name+"-ro"))
		Eventually(func() int32 {
			_ = k8sClient.Get(ctx, client.ObjectKeyFromObject(outer2), outer2)
			return outer2.Status.Volumes
		}, timeout, interval).Should(BeNumerically(">", 0))
		// The grant arrives in a later second than the inner share's creation.
		time.Sleep(1100 * time.Millisecond)
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(outer2), outer2)).To(Succeed())
		outer2.Spec.Grants = []sharedfsv1alpha1.Grant{grant("in2", sharedfsv1alpha1.AccessReadWrite)}
		Expect(k8sClient.Update(ctx, outer2)).To(Succeed())
		Eventually(readyOf(outer2), timeout, interval).Should(haveCondition(metav1.ConditionFalse, sharedfsv1alpha1.ReasonOverlappingShare))
		Consistently(readyOf(inner2), 2*time.Second, interval).Should(haveCondition(metav1.ConditionTrue, sharedfsv1alpha1.ReasonResolved))

		By("exempting the outer share lifts the block")
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(outer), outer)).To(Succeed())
		outer.Annotations = map[string]string{sharedfsv1alpha1.AnnotationAllowOverlap: "true"}
		Expect(k8sClient.Update(ctx, outer)).To(Succeed())
		readyShare(inner)
	})

	It("does not take over a StorageClass it does not own", func() {
		name := uniq("taken")
		reclaim := corev1.PersistentVolumeReclaimDelete
		create(&storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: name + "-ro"}, Provisioner: "example.com/other", ReclaimPolicy: &reclaim})
		src := sourcePV(uniq("src"))
		s := share(name, src.Name, "datasets", grant("a", sharedfsv1alpha1.AccessReadOnly))
		create(src, s)
		Eventually(readyOf(s), timeout, interval).Should(haveCondition(metav1.ConditionFalse, sharedfsv1alpha1.ReasonStorageClassTaken))
	})

	It("stays while a provisioned volume remains", func() {
		src := sourcePV(uniq("src"))
		s := share(uniq("ds"), src.Name, "datasets", grant("fin", sharedfsv1alpha1.AccessReadOnly))
		ns := namespace("fin")
		create(src, s, ns)
		readyShare(s)
		pvc := claim(ns.Name, s.Name+"-ro")
		create(pvc)
		pv := provisioned(pvc)

		Expect(k8sClient.Delete(ctx, s)).To(Succeed())
		Eventually(func(g Gomega) string {
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(s), s)).To(Succeed())
			c := meta.FindStatusCondition(s.Status.Conditions, sharedfsv1alpha1.ConditionReady)
			g.Expect(c).NotTo(BeNil())
			return c.Reason
		}, timeout, interval).Should(Equal(sharedfsv1alpha1.ReasonVolumesRemaining))

		Expect(k8sClient.Delete(ctx, pvc)).To(Succeed())
		gone(pvc)
		Expect(k8sClient.Delete(ctx, pv)).To(Succeed())
		gone(pv)
		gone(s)
	})
})

var _ = Describe("Provisioner", func() {
	BeforeEach(ensureDrivers)

	It("provisions a read-only volume of the share's directory for a granted namespace", func() {
		src := sourcePV(uniq("src"))
		s := share(uniq("ds"), src.Name, "datasets", grant("ro", sharedfsv1alpha1.AccessReadOnly))
		ns := namespace("ro")
		create(src, s, ns)
		readyShare(s)
		pvc := claim(ns.Name, s.Name+"-ro", corev1.ReadWriteOnce)
		create(pvc)
		pv := provisioned(pvc)

		Expect(pv.Annotations[volume.AnnotationProvisionedBy]).To(Equal(sharedfsv1alpha1.ProvisionerName))
		Expect(pv.Spec.ClaimRef.UID).To(Equal(pvc.UID))
		Expect(pv.Spec.PersistentVolumeReclaimPolicy).To(Equal(corev1.PersistentVolumeReclaimDelete))
		Expect(pv.Spec.StorageClassName).To(Equal(s.Name + "-ro"))
		Expect(pv.Spec.AccessModes).To(Equal([]corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany, corev1.ReadWriteOnce}))
		Expect(pv.Spec.MountOptions).To(Equal([]string{"localflock", "ro"}))
		Expect(pv.Spec.CSI.VolumeHandle).To(Equal("sharedfs:" + string(pvc.UID)))
		Expect(pv.Spec.CSI.VolumeAttributes).To(Equal(map[string]string{"dnsname": src.Name + ".fsx.example", "mountname": "abc/datasets"}))
		Expect(pv.Spec.CSI.FSType).To(BeEmpty())
		Eventually(eventReasons(pvc), timeout, interval).Should(ContainElement(sharedfsv1alpha1.EventProvisioned))
	})

	It("leaves a claim Pending with a reason when it is not granted or not supported", func() {
		src := sourcePV(uniq("src"))
		s := share(uniq("ds"), src.Name, "datasets", grant("ro2", sharedfsv1alpha1.AccessReadOnly))
		granted, other := namespace("ro2"), namespace("")
		create(src, s, granted, other)
		readyShare(s)

		denied := claim(other.Name, s.Name+"-ro")
		rwOnRO := claim(granted.Name, s.Name+"-rw")
		selector := claim(granted.Name, s.Name+"-ro")
		selector.Spec.Selector = &metav1.LabelSelector{MatchLabels: map[string]string{"a": "b"}}
		rwop := claim(granted.Name, s.Name+"-ro", corev1.ReadWriteOncePod)
		create(denied, rwOnRO, selector, rwop)
		for _, c := range []*corev1.PersistentVolumeClaim{denied, rwOnRO} {
			neverProvisioned(c)
			Eventually(eventReasons(c), timeout, interval).Should(ContainElement(sharedfsv1alpha1.EventNotGranted))
		}
		for _, c := range []*corev1.PersistentVolumeClaim{selector, rwop} {
			neverProvisioned(c)
			Eventually(eventReasons(c), timeout, interval).Should(ContainElement(sharedfsv1alpha1.EventUnsupportedClaim))
		}

		By("granting the namespace provisions the waiting claim")
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(other), other)).To(Succeed())
		other.Labels[grantKey] = "ro2"
		Expect(k8sClient.Update(ctx, other)).To(Succeed())
		provisioned(denied)
	})

	It("ignores claims addressed to another provisioner and classes it does not own", func() {
		src := sourcePV(uniq("src"))
		s := share(uniq("ds"), src.Name, "datasets", grant("ig", sharedfsv1alpha1.AccessReadOnly))
		ns := namespace("ig")
		create(src, s, ns)
		readyShare(s)
		foreign := claim(ns.Name, s.Name+"-ro")
		foreign.Annotations[volume.AnnotationStorageProvisioner] = "example.com/other"
		reclaim := corev1.PersistentVolumeReclaimDelete
		fake := &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: uniq("fake")}, Provisioner: sharedfsv1alpha1.ProvisionerName,
			ReclaimPolicy: &reclaim, Parameters: map[string]string{
				sharedfsv1alpha1.ParameterShare: s.Name, sharedfsv1alpha1.ParameterShareUID: string(s.UID),
				sharedfsv1alpha1.ParameterAccess: string(sharedfsv1alpha1.AccessReadWrite),
			}}
		create(fake)
		forged := claim(ns.Name, fake.Name)
		create(foreign, forged)
		neverProvisioned(foreign)
		neverProvisioned(forged)
	})

	It("withdraws a bound claim after its namespace loses the grant for the grace period", func() {
		src := sourcePV(uniq("src"))
		s := share(uniq("ds"), src.Name, "datasets", grant("rv", sharedfsv1alpha1.AccessReadWrite))
		ns := namespace("rv")
		create(src, s, ns)
		readyShare(s)
		pvc := claim(ns.Name, s.Name+"-rw")
		create(pvc)
		pv := provisioned(pvc)
		bind(pvc, pv)
		By("labels on the claim are the tenant's and change nothing")
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(pvc), pvc)).To(Succeed())
		pvc.Labels = map[string]string{sharedfsv1alpha1.LabelInternal: "x"}
		Expect(k8sClient.Update(ctx, pvc)).To(Succeed())

		By("a grant restored within the grace period cancels the withdrawal")
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(ns), ns)).To(Succeed())
		ns.Labels[grantKey] = "nope"
		Expect(k8sClient.Update(ctx, ns)).To(Succeed())
		Eventually(func(g Gomega) string {
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(pv), pv)).To(Succeed())
			return pv.Annotations[sharedfsv1alpha1.AnnotationWithdrawAfter]
		}, timeout, interval).ShouldNot(BeEmpty())
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(ns), ns)).To(Succeed())
		ns.Labels[grantKey] = "rv"
		Expect(k8sClient.Update(ctx, ns)).To(Succeed())
		Eventually(func(g Gomega) string {
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(pv), pv)).To(Succeed())
			return pv.Annotations[sharedfsv1alpha1.AnnotationWithdrawAfter]
		}, timeout, interval).Should(BeEmpty())
		Consistently(func() error { return k8sClient.Get(ctx, client.ObjectKeyFromObject(pvc), pvc) }, gracePeriod+time.Second, interval).Should(Succeed())

		By("a grant still missing after the grace period deletes the claim")
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(ns), ns)).To(Succeed())
		ns.Labels[grantKey] = "nope"
		Expect(k8sClient.Update(ctx, ns)).To(Succeed())
		Eventually(func() bool {
			err := k8sClient.Get(ctx, client.ObjectKeyFromObject(pvc), pvc)
			return apierrors.IsNotFound(err) || !pvc.DeletionTimestamp.IsZero()
		}, timeout, interval).Should(BeTrue())
	})

	It("deletes a Released volume and nothing else", func() {
		src := sourcePV(uniq("src"))
		s := share(uniq("ds"), src.Name, "datasets", grant("rel", sharedfsv1alpha1.AccessReadOnly))
		ns := namespace("rel")
		create(src, s, ns)
		readyShare(s)
		pvc := claim(ns.Name, s.Name+"-ro")
		create(pvc)
		pv := provisioned(pvc)
		bind(pvc, pv)
		Consistently(func() error { return k8sClient.Get(ctx, client.ObjectKeyFromObject(pv), pv) }, 2*time.Second, interval).Should(Succeed())

		Expect(k8sClient.Delete(ctx, pvc)).To(Succeed())
		gone(pvc)
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(pv), pv)).To(Succeed())
		pv.Status.Phase = corev1.VolumeReleased
		Expect(k8sClient.Status().Update(ctx, pv)).To(Succeed())
		gone(pv)

		By("the source volume is never touched")
		Consistently(func() error { return k8sClient.Get(ctx, client.ObjectKeyFromObject(src), src) }, 2*time.Second, interval).Should(Succeed())
	})
})

var _ = Describe("Directories", func() {
	BeforeEach(ensureDrivers)

	finish := func(pod *corev1.Pod, res prepare.Result) {
		pod.Status.Phase = corev1.PodSucceeded
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name: "prepare", Image: preparerImage, ImageID: preparerImage,
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Message: res.JSON()}},
		}}
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
	}
	preparer := func(dirName string) (*sharedfsv1alpha1.FilesystemShareDirectory, *corev1.Pod) {
		var dir sharedfsv1alpha1.FilesystemShareDirectory
		var pod corev1.Pod
		Eventually(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: dirName}, &dir)).To(Succeed())
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: controllerNamespace, Name: volume.PreparerName(dir.UID)}, &pod)).To(Succeed())
		}, timeout, interval).Should(Succeed())
		return &dir, &pod
	}

	It("prepares a per-namespace directory with a locked-down pod before provisioning", func() {
		src := sourcePV(uniq("src"))
		s := share(uniq("ck"), src.Name, "teams/{namespace}", grant("dir", sharedfsv1alpha1.AccessReadWrite))
		s.Spec.Provision = &sharedfsv1alpha1.Provision{GID: 1000, Mode: "2770"}
		ns := namespace("dir")
		create(src, s, ns)
		readyShare(s)
		pvc := claim(ns.Name, s.Name+"-rw")
		create(pvc)

		dir, pod := preparer(DirectoryName(s.UID, ns.Name))
		Expect(dir.Spec.NamespaceUID).To(Equal(ns.UID))
		Expect(dir.Spec.Path).To(Equal("teams/" + ns.Name))
		neverProvisioned(pvc)

		By("the pod mounts only the parent, has CHOWN only and no token")
		ct := pod.Spec.Containers[0]
		Expect(ct.Image).To(Equal(preparerImage))
		Expect(ct.Args).To(Equal([]string{"prepare-directory", "--root=/parent", "--name=" + ns.Name, "--uid=0", "--gid=1000", "--mode=2770"}))
		Expect(*ct.SecurityContext.RunAsUser).To(BeZero())
		Expect(ct.SecurityContext.Capabilities.Drop).To(Equal([]corev1.Capability{"ALL"}))
		Expect(ct.SecurityContext.Capabilities.Add).To(Equal([]corev1.Capability{"CHOWN"}))
		Expect(*ct.SecurityContext.AllowPrivilegeEscalation).To(BeFalse())
		Expect(*pod.Spec.AutomountServiceAccountToken).To(BeFalse())
		var ipv corev1.PersistentVolume
		Expect(k8sClient.Get(ctx, client.ObjectKey{Name: volume.PreparerName(dir.UID)}, &ipv)).To(Succeed())
		Expect(ipv.Spec.CSI.VolumeAttributes["mountname"]).To(Equal("abc/teams"))
		Expect(ipv.Spec.PersistentVolumeReclaimPolicy).To(Equal(corev1.PersistentVolumeReclaimRetain))
		Expect(ipv.Spec.ClaimRef.Namespace).To(Equal(controllerNamespace))
		Expect(ipv.Spec.ClaimRef.UID).NotTo(BeEmpty())

		By("a created directory is recorded and the claim is provisioned")
		finish(pod, prepare.Result{Outcome: prepare.Created, UID: 0, GID: 1000, Mode: "2770"})
		pv := provisioned(pvc)
		Expect(pv.Spec.CSI.VolumeAttributes["mountname"]).To(Equal("abc/teams/" + ns.Name))
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(dir), dir)).To(Succeed())
		Expect(dir.Status.Observed).To(Equal(&sharedfsv1alpha1.ObservedDirectory{UID: 0, GID: 1000, Mode: "2770"}))
		gone(pod)
	})

	It("records a failure, retries with backoff, and keeps the claim Pending", func() {
		src := sourcePV(uniq("src"))
		s := share(uniq("ck"), src.Name, "{namespace}", grant("fail", sharedfsv1alpha1.AccessReadWrite))
		s.Spec.Provision = &sharedfsv1alpha1.Provision{GID: 1000, Mode: "2770"}
		ns := namespace("fail")
		create(src, s, ns)
		readyShare(s)
		pvc := claim(ns.Name, s.Name+"-rw")
		create(pvc)
		dir, pod := preparer(DirectoryName(s.UID, ns.Name))
		finish(pod, prepare.Result{Outcome: prepare.InsecureParent, Message: "the parent directory must be owned by root"})
		Eventually(func(g Gomega) *metav1.Condition {
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(dir), dir)).To(Succeed())
			return meta.FindStatusCondition(dir.Status.Conditions, sharedfsv1alpha1.ConditionPrepared)
		}, timeout, interval).Should(haveCondition(metav1.ConditionFalse, sharedfsv1alpha1.ReasonInsecureParent))
		Expect(dir.Status.Failures).To(Equal(int32(1)))
		Expect(dir.Status.LastPreparer).To(Equal(pod.UID))
		Consistently(func() int32 {
			_ = k8sClient.Get(ctx, client.ObjectKeyFromObject(dir), dir)
			return dir.Status.Failures
		}, 3*time.Second, interval).Should(Equal(int32(1)), "one pod is counted once")
		neverProvisioned(pvc)
		Eventually(eventReasons(pvc), timeout, interval).Should(ContainElement(sharedfsv1alpha1.EventDirectoryNotReady))
	})

	It("does not hand a recreated namespace the directory of the earlier one", func() {
		src := sourcePV(uniq("src"))
		s := share(uniq("ck"), src.Name, "teams/{namespace}", grant("rec", sharedfsv1alpha1.AccessReadWrite))
		ns := namespace("rec")
		create(src, s, ns)
		readyShare(s)
		first := claim(ns.Name, s.Name+"-rw")
		create(first)
		bind(first, provisioned(first))

		By("the record now names another namespace UID, as after a delete and re-create")
		var dir sharedfsv1alpha1.FilesystemShareDirectory
		Expect(k8sClient.Get(ctx, client.ObjectKey{Name: DirectoryName(s.UID, ns.Name)}, &dir)).To(Succeed())
		Expect(k8sClient.Delete(ctx, &dir)).To(Succeed())
		gone(&dir)
		stale := &sharedfsv1alpha1.FilesystemShareDirectory{
			ObjectMeta: metav1.ObjectMeta{Name: DirectoryName(s.UID, ns.Name)},
			Spec: sharedfsv1alpha1.FilesystemShareDirectorySpec{
				ShareRef:  sharedfsv1alpha1.ShareReference{Name: s.Name, UID: s.UID},
				Namespace: ns.Name, NamespaceUID: "earlier-uid", Path: "teams/" + ns.Name,
			},
		}
		create(stale)
		second := claim(ns.Name, s.Name+"-rw")
		create(second)
		neverProvisioned(second)
		Eventually(eventReasons(second), timeout, interval).Should(ContainElement(sharedfsv1alpha1.EventNamespaceRecreated))

		By("an admin deleting the record lets the namespace take the directory")
		Expect(k8sClient.Delete(ctx, stale)).To(Succeed())
		gone(stale)
		provisioned(second)
	})
})
