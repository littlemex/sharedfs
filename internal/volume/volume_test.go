package volume

import (
	"errors"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	sharedfsv1alpha1 "github.com/littlemex/sharedfs/api/v1alpha1"
	"github.com/littlemex/sharedfs/internal/driver"
)

const claimUID = types.UID("0a975dbf-1969-4108-8d3a-ef37d27cc913")

func lustreSource() *corev1.PersistentVolume {
	return &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: "fsx-training", UID: "src-uid",
			Annotations: map[string]string{"pv.kubernetes.io/provisioned-by": "fsx.csi.aws.com"},
		},
		Spec: corev1.PersistentVolumeSpec{
			Capacity:                      corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("4800Gi")},
			AccessModes:                   []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimDelete,
			MountOptions:                  []string{"flock", "rw"},
			PersistentVolumeSource: corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{
				Driver:       "fsx.csi.aws.com",
				VolumeHandle: "fs-0123456789abcdef0",
				VolumeAttributes: map[string]string{
					"dnsname": "fs-0123456789abcdef0.fsx.us-east-1.amazonaws.com", "mountname": "abcdefgh",
					"storage.kubernetes.io/csiProvisionerIdentity": "123-fsx",
				},
			}},
		},
	}
}

func claimFor(access sharedfsv1alpha1.Access) *sharedfsv1alpha1.SharedFilesystemClaim {
	return &sharedfsv1alpha1.SharedFilesystemClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "data", Namespace: "team-a", UID: claimUID},
		Spec:       sharedfsv1alpha1.SharedFilesystemClaimSpec{FilesystemRef: sharedfsv1alpha1.FilesystemReference{Name: "lustre"}, Access: access},
	}
}

func filesystem(t *testing.T) *sharedfsv1alpha1.SharedFilesystem {
	t.Helper()
	r, err := Resolve(lustreSource())
	if err != nil {
		t.Fatal(err)
	}
	return &sharedfsv1alpha1.SharedFilesystem{
		ObjectMeta: metav1.ObjectMeta{Name: "lustre", UID: "fs-uid"},
		Status:     sharedfsv1alpha1.SharedFilesystemStatus{Resolved: r},
	}
}

func TestResolveCopiesOnlyAllowListedFields(t *testing.T) {
	r, err := Resolve(lustreSource())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := r.VolumeAttributes["storage.kubernetes.io/csiProvisionerIdentity"]; ok {
		t.Errorf("a key the driver does not read was copied: %v", r.VolumeAttributes)
	}
	if len(r.VolumeAttributes) != 2 {
		t.Errorf("want dnsname and mountname, got %v", r.VolumeAttributes)
	}
	if !slices.Equal(r.MountOptions, []string{"flock"}) {
		t.Errorf("rw must be stripped from the source mount options, got %v", r.MountOptions)
	}
	if r.Capacity != "4800Gi" || r.UID != "src-uid" {
		t.Errorf("capacity/uid: %+v", r)
	}
}

func TestResolveRejects(t *testing.T) {
	cases := map[string]struct {
		mutate func(*corev1.PersistentVolume)
		reason string
	}{
		"not csi":     {func(pv *corev1.PersistentVolume) { pv.Spec.CSI = nil }, sharedfsv1alpha1.ReasonSourceNotCSI},
		"efs":         {func(pv *corev1.PersistentVolume) { pv.Spec.CSI.Driver = "efs.csi.aws.com" }, sharedfsv1alpha1.ReasonUnsupportedDriver},
		"generated":   {func(pv *corev1.PersistentVolume) { pv.Labels = map[string]string{LabelManagedBy: ManagedByValue} }, sharedfsv1alpha1.ReasonSourceIsGenerated},
		"long handle": {func(pv *corev1.PersistentVolume) { pv.Spec.CSI.VolumeHandle = strings.Repeat("x", 90) }, sharedfsv1alpha1.ReasonInvalidSource},
		"no capacity": {func(pv *corev1.PersistentVolume) { pv.Spec.Capacity = nil }, sharedfsv1alpha1.ReasonInvalidSource},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			pv := lustreSource()
			c.mutate(pv)
			_, err := Resolve(pv)
			var se *SourceError
			if !errors.As(err, &se) || se.Reason != c.reason {
				t.Fatalf("want %s, got %v", c.reason, err)
			}
		})
	}
}

func TestPVReadOnlyAndReadWrite(t *testing.T) {
	fs := filesystem(t)
	p, _ := driver.Lookup("fsx.csi.aws.com")
	for _, c := range []struct {
		access sharedfsv1alpha1.Access
		opts   []string
	}{
		{sharedfsv1alpha1.AccessReadOnly, []string{"flock", "ro"}},
		{sharedfsv1alpha1.AccessReadWrite, []string{"flock"}},
		{"", []string{"flock", "ro"}}, // an unset access is read-only, never read-write
	} {
		pv, err := PV(claimFor(c.access), fs, p)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(pv.Spec.MountOptions, c.opts) {
			t.Errorf("%q: mount options %v, want %v", c.access, pv.Spec.MountOptions, c.opts)
		}
		if !slices.Equal(pv.Spec.AccessModes, []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany}) {
			t.Errorf("%q: the FSx drivers reject ReadOnlyMany; got %v", c.access, pv.Spec.AccessModes)
		}
	}
}

func TestPVShape(t *testing.T) {
	fs := filesystem(t)
	p, _ := driver.Lookup("fsx.csi.aws.com")
	pv, err := PV(claimFor(sharedfsv1alpha1.AccessReadOnly), fs, p)
	if err != nil {
		t.Fatal(err)
	}
	if *pv.Name != "sfc-"+string(claimUID) {
		t.Errorf("name %s", *pv.Name)
	}
	if got := *pv.Spec.PersistentVolumeReclaimPolicy; got != corev1.PersistentVolumeReclaimRetain {
		t.Errorf("a source with Delete must still produce Retain, got %s", got)
	}
	if *pv.Spec.CSI.VolumeHandle != "fs-0123456789abcdef0-sfc-"+string(claimUID) {
		t.Errorf("handle %s", *pv.Spec.CSI.VolumeHandle)
	}
	if *pv.Spec.StorageClassName != "" {
		t.Errorf("storageClassName %q", *pv.Spec.StorageClassName)
	}
	cr := pv.Spec.ClaimRef
	if *cr.Namespace != "team-a" || *cr.Name != "data" || cr.UID != nil || cr.ResourceVersion != nil {
		t.Errorf("claimRef must be namespace+name only, got %+v", cr)
	}
	if len(pv.Annotations) != 2 || pv.Annotations["pv.kubernetes.io/provisioned-by"] != "" {
		t.Errorf("source annotations leaked: %v", pv.Annotations)
	}
	for k, v := range pv.Labels {
		if len(v) > 63 {
			t.Errorf("label %s value is %d chars", k, len(v))
		}
	}
}

func TestPVCShape(t *testing.T) {
	pvc := PVC(claimFor(sharedfsv1alpha1.AccessReadWrite))
	if pvc.Name != "data" || pvc.Namespace != "team-a" || pvc.Spec.VolumeName != PVName(claimUID) {
		t.Errorf("identity %s/%s -> %s", pvc.Namespace, pvc.Name, pvc.Spec.VolumeName)
	}
	if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName != "" {
		t.Errorf("storageClassName must be the empty string, not nil")
	}
	if q := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; q.Cmp(RequestedStorage) != 0 {
		t.Errorf("request %s", q.String())
	}
}

func TestOwnsPVCIgnoresOwnerReferences(t *testing.T) {
	pvc := PVC(claimFor(sharedfsv1alpha1.AccessReadOnly))
	if !OwnsPVC(claimUID, pvc) {
		t.Fatal("own PVC not recognised")
	}
	foreign := pvc.DeepCopy()
	foreign.Spec.VolumeName = ""
	foreign.OwnerReferences = []metav1.OwnerReference{{UID: claimUID}}
	if OwnsPVC(claimUID, foreign) {
		t.Fatal("an ownerReference alone must not make a PVC ours")
	}
}

func TestStaleClaimUID(t *testing.T) {
	live := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{UID: "new"}}
	released := &corev1.PersistentVolume{
		Spec:   corev1.PersistentVolumeSpec{ClaimRef: &corev1.ObjectReference{UID: "old"}},
		Status: corev1.PersistentVolumeStatus{Phase: corev1.VolumeReleased},
	}
	if !StaleClaimUID(released, live) {
		t.Error("released PV holding an old UID must be healed")
	}
	bound := released.DeepCopy()
	bound.Status.Phase = corev1.VolumeBound
	if StaleClaimUID(bound, live) {
		t.Error("a Bound PV is never touched, even if the cached PVC disagrees")
	}
	same := released.DeepCopy()
	same.Spec.ClaimRef.UID = "new"
	if StaleClaimUID(same, live) {
		t.Error("matching UID")
	}
	deleting := live.DeepCopy()
	deleting.DeletionTimestamp = &metav1.Time{}
	if StaleClaimUID(released, deleting) {
		t.Error("a PVC being deleted is not something to bind to")
	}
}

func TestImmutableDrift(t *testing.T) {
	fs := filesystem(t)
	p, _ := driver.Lookup("fsx.csi.aws.com")
	desired, _ := PV(claimFor(sharedfsv1alpha1.AccessReadOnly), fs, p)
	existing := &corev1.PersistentVolume{Spec: corev1.PersistentVolumeSpec{PersistentVolumeSource: corev1.PersistentVolumeSource{
		CSI: &corev1.CSIPersistentVolumeSource{
			Driver: *desired.Spec.CSI.Driver, VolumeHandle: *desired.Spec.CSI.VolumeHandle,
			VolumeAttributes: desired.Spec.CSI.VolumeAttributes,
		},
	}}}
	if d := ImmutableDrift(existing, desired); len(d) != 0 {
		t.Errorf("no drift expected, got %v", d)
	}
	existing.Spec.CSI.VolumeAttributes = map[string]string{"dnsname": "other"}
	if d := ImmutableDrift(existing, desired); !slices.Equal(d, []string{"csi.volumeAttributes"}) {
		t.Errorf("got %v", d)
	}
}

func TestVolumeHandleLimit(t *testing.T) {
	if _, err := VolumeHandle(strings.Repeat("x", 88), claimUID); err == nil {
		t.Error("over 128 bytes must fail")
	}
	if _, err := VolumeHandle(strings.Repeat("x", 87), claimUID); err != nil {
		t.Errorf("exactly 128 bytes must pass: %v", err)
	}
}

func TestAccessAllows(t *testing.T) {
	ro, rw := sharedfsv1alpha1.AccessReadOnly, sharedfsv1alpha1.AccessReadWrite
	if !ro.Allows(ro) || ro.Allows(rw) || !rw.Allows(ro) || !rw.Allows(rw) {
		t.Error("Allows table")
	}
}
