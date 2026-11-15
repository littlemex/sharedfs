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
)

const claimUID = types.UID("0a975dbf-1969-4108-8d3a-ef37d27cc913")

func lustreSource() *corev1.PersistentVolume {
	return &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "fsx-training",
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

func resolved(t *testing.T, pv *corev1.PersistentVolume) *Source {
	t.Helper()
	s, err := Resolve(pv)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func claimFor(access sharedfsv1alpha1.Access) *sharedfsv1alpha1.SharedFilesystemClaim {
	return &sharedfsv1alpha1.SharedFilesystemClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "data", Namespace: "team-a", UID: claimUID},
		Spec:       sharedfsv1alpha1.SharedFilesystemClaimSpec{FilesystemRef: sharedfsv1alpha1.FilesystemReference{Name: "lustre"}, Access: access},
	}
}

var fs = &sharedfsv1alpha1.SharedFilesystem{ObjectMeta: metav1.ObjectMeta{Name: "lustre", UID: "fs-uid"}}

func TestResolveCopiesOnlyWhatTheDriverReads(t *testing.T) {
	s := resolved(t, lustreSource())
	if len(s.VolumeAttributes) != 2 || s.VolumeAttributes["mountname"] != "abcdefgh" {
		t.Errorf("want dnsname and mountname only, got %v", s.VolumeAttributes)
	}
	if !slices.Equal(s.MountOptions, []string{"flock"}) {
		t.Errorf("rw must be stripped, got %v", s.MountOptions)
	}
	if s.Capacity.String() != "4800Gi" || s.Profile.AccessMode != corev1.ReadWriteMany {
		t.Errorf("capacity %s, mode %s", s.Capacity.String(), s.Profile.AccessMode)
	}
}

func TestResolveRejects(t *testing.T) {
	cases := map[string]struct {
		mutate func(*corev1.PersistentVolume)
		reason string
	}{
		"not csi":   {func(pv *corev1.PersistentVolume) { pv.Spec.CSI = nil }, sharedfsv1alpha1.ReasonSourceNotCSI},
		"efs":       {func(pv *corev1.PersistentVolume) { pv.Spec.CSI.Driver = "efs.csi.aws.com" }, sharedfsv1alpha1.ReasonUnsupportedDriver},
		"generated": {func(pv *corev1.PersistentVolume) { pv.Labels = map[string]string{sharedfsv1alpha1.LabelClaimUID: "x"} }, sharedfsv1alpha1.ReasonSourceIsGenerated},
		"long handle": {func(pv *corev1.PersistentVolume) {
			pv.Spec.CSI.VolumeHandle = strings.Repeat("x", MaxSourceHandleBytes+1)
		}, sharedfsv1alpha1.ReasonInvalidSource},
		"no dnsname":  {func(pv *corev1.PersistentVolume) { delete(pv.Spec.CSI.VolumeAttributes, "dnsname") }, sharedfsv1alpha1.ReasonInvalidSource},
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

func TestLongestSourceHandleFitsTheCSILimit(t *testing.T) {
	pv := lustreSource()
	pv.Spec.CSI.VolumeHandle = strings.Repeat("x", MaxSourceHandleBytes)
	got := *PV(claimFor(""), fs, resolved(t, pv)).Spec.CSI.VolumeHandle
	if len(got) != maxCSIVolumeIDBytes {
		t.Errorf("longest generated handle is %d bytes, want exactly %d", len(got), maxCSIVolumeIDBytes)
	}
}

func TestPVAccess(t *testing.T) {
	s := resolved(t, lustreSource())
	for _, c := range []struct {
		access sharedfsv1alpha1.Access
		opts   []string
	}{
		{sharedfsv1alpha1.AccessReadOnly, []string{"flock", "ro"}},
		{sharedfsv1alpha1.AccessReadWrite, []string{"flock"}},
		{"", []string{"flock", "ro"}}, // unset is read-only, never read-write
	} {
		pv := PV(claimFor(c.access), fs, s)
		if !slices.Equal(pv.Spec.MountOptions, c.opts) {
			t.Errorf("%q: mount options %v, want %v", c.access, pv.Spec.MountOptions, c.opts)
		}
		if !slices.Equal(pv.Spec.AccessModes, []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany}) {
			t.Errorf("%q: access modes %v", c.access, pv.Spec.AccessModes)
		}
	}
}

func TestPVShape(t *testing.T) {
	pv := PV(claimFor(sharedfsv1alpha1.AccessReadOnly), fs, resolved(t, lustreSource()))
	if *pv.Name != "sfc-"+string(claimUID) {
		t.Errorf("name %s", *pv.Name)
	}
	if got := *pv.Spec.PersistentVolumeReclaimPolicy; got != corev1.PersistentVolumeReclaimRetain {
		t.Errorf("a Delete source must still produce Retain, got %s", got)
	}
	if *pv.Spec.CSI.VolumeHandle != "fs-0123456789abcdef0-sfc-"+string(claimUID) {
		t.Errorf("handle %s", *pv.Spec.CSI.VolumeHandle)
	}
	if *pv.Spec.StorageClassName != "" {
		t.Errorf("storageClassName %q", *pv.Spec.StorageClassName)
	}
	cr := pv.Spec.ClaimRef
	if *cr.Namespace != "team-a" || *cr.Name != "data" || cr.UID != nil || cr.ResourceVersion != nil {
		t.Errorf("claimRef must be namespace and name only, got %+v", cr)
	}
	if _, leaked := pv.Annotations["pv.kubernetes.io/provisioned-by"]; leaked {
		t.Errorf("source annotations leaked: %v", pv.Annotations)
	}
	for k, v := range pv.Labels {
		if len(v) > 63 {
			t.Errorf("label %s value is %d chars", k, len(v))
		}
	}
}

func TestPVCShapeAndRequest(t *testing.T) {
	pvc := PVC(claimFor(sharedfsv1alpha1.AccessReadWrite), resolved(t, lustreSource()))
	if pvc.Name != "data" || pvc.Namespace != "team-a" || pvc.Spec.VolumeName != PVName(claimUID) {
		t.Errorf("identity %s/%s -> %s", pvc.Namespace, pvc.Name, pvc.Spec.VolumeName)
	}
	if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName != "" {
		t.Error("storageClassName must be the empty string, not nil")
	}
	if q := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; q.Cmp(requestedStorage) != 0 {
		t.Errorf("request %s", q.String())
	}

	small := lustreSource()
	small.Spec.Capacity[corev1.ResourceStorage] = resource.MustParse("100Mi")
	pvc = PVC(claimFor(""), resolved(t, small))
	if q := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; q.String() != "100Mi" {
		t.Errorf("request must not exceed a small source's capacity, got %s", q.String())
	}
}

func TestOwnsPVCIgnoresOwnerReferences(t *testing.T) {
	pvc := PVC(claimFor(""), resolved(t, lustreSource()))
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

func TestClaimUIDRoundTrip(t *testing.T) {
	if uid, ok := ClaimUIDOf(PVName(claimUID)); !ok || uid != claimUID {
		t.Errorf("round trip: %s %v", uid, ok)
	}
	for _, name := range []string{"fsx-training", "sfc-short", "pvc-" + string(claimUID)} {
		if _, ok := ClaimUIDOf(name); ok {
			t.Errorf("%s is not a generated name", name)
		}
	}
}

func TestStaleClaimUID(t *testing.T) {
	live := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{UID: "new"}}
	released := &corev1.PersistentVolume{
		Spec:   corev1.PersistentVolumeSpec{ClaimRef: &corev1.ObjectReference{UID: "old"}},
		Status: corev1.PersistentVolumeStatus{Phase: corev1.VolumeReleased},
	}
	if !StaleClaimUID(released, live) {
		t.Error("a Released PV holding an old UID is healed")
	}
	bound := released.DeepCopy()
	bound.Status.Phase = corev1.VolumeBound
	if StaleClaimUID(bound, live) {
		t.Error("a Bound PV is never touched")
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

func TestCSIDriftAndKeepCSI(t *testing.T) {
	desired := PV(claimFor(""), fs, resolved(t, lustreSource()))
	existing := &corev1.PersistentVolume{Spec: corev1.PersistentVolumeSpec{PersistentVolumeSource: corev1.PersistentVolumeSource{
		CSI: &corev1.CSIPersistentVolumeSource{
			Driver: *desired.Spec.CSI.Driver, VolumeHandle: *desired.Spec.CSI.VolumeHandle,
			VolumeAttributes: map[string]string{"dnsname": "old.example"},
		},
	}}}
	if d := CSIDrift(existing, desired); !slices.Equal(d, []string{"csi.volumeAttributes"}) {
		t.Fatalf("got %v", d)
	}
	KeepCSI(desired, existing)
	if d := CSIDrift(existing, desired); len(d) != 0 {
		t.Errorf("after KeepCSI there is nothing left to drift, got %v", d)
	}
	if !slices.Equal(desired.Spec.MountOptions, []string{"flock", "ro"}) {
		t.Errorf("the mutable fields are still the desired ones, got %v", desired.Spec.MountOptions)
	}
}

func TestAccessAllows(t *testing.T) {
	ro, rw := sharedfsv1alpha1.AccessReadOnly, sharedfsv1alpha1.AccessReadWrite
	if !ro.Allows(ro) || ro.Allows(rw) || !rw.Allows(ro) || !rw.Allows(rw) || !ro.Allows("") {
		t.Error("Allows table")
	}
}
