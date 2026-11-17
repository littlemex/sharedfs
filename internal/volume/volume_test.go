package volume

import (
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	sharedfsv1alpha1 "github.com/littlemex/sharedfs/api/v1alpha1"
)

func lustrePV(mountname string, opts ...string) *corev1.PersistentVolume {
	attrs := map[string]string{"dnsname": "fs.example", "extra": "dropped"}
	if mountname != "" {
		attrs["mountname"] = mountname
	}
	return &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "src", Annotations: map[string]string{AnnotationProvisionedBy: "fsx.csi.aws.com"}},
		Spec: corev1.PersistentVolumeSpec{
			Capacity:     corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1200Gi")},
			MountOptions: opts,
			ClaimRef:     &corev1.ObjectReference{Namespace: "admin", Name: "hold"},
			PersistentVolumeSource: corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{
				Driver: "fsx.csi.aws.com", VolumeHandle: "fs-1", VolumeAttributes: attrs, FSType: "lustre",
			}},
		},
	}
}

func TestResolveRefusals(t *testing.T) {
	generated := lustrePV("abc")
	generated.Annotations[AnnotationProvisionedBy] = sharedfsv1alpha1.ProvisionerName
	notCSI := lustrePV("abc")
	notCSI.Spec.CSI = nil
	efs := lustrePV("abc")
	efs.Spec.CSI.Driver = "efs.csi.aws.com"
	noDNS := lustrePV("abc")
	delete(noDNS.Spec.CSI.VolumeAttributes, "dnsname")
	for name, c := range map[string]struct {
		pv     *corev1.PersistentVolume
		reason string
	}{
		"generated": {generated, sharedfsv1alpha1.ReasonSourceIsGenerated},
		"not CSI":   {notCSI, sharedfsv1alpha1.ReasonSourceNotCSI},
		"efs":       {efs, sharedfsv1alpha1.ReasonUnsupportedDriver},
		"no dns":    {noDNS, sharedfsv1alpha1.ReasonInvalidSource},
	} {
		_, err := Resolve(c.pv)
		se, ok := err.(*SourceError)
		if !ok || se.Reason != c.reason {
			t.Errorf("%s: got %v, want %s", name, err, c.reason)
		}
	}
}

func TestResolveCopiesOnlyWhatTheDriverReads(t *testing.T) {
	src, err := Resolve(lustrePV("abc", "rw", "flock", "noatime"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := src.VolumeAttributes["extra"]; ok {
		t.Error("an attribute the driver does not read was copied")
	}
	if !slices.Equal(src.MountOptions, []string{"flock", "noatime"}) {
		t.Errorf("rw/ro must be dropped: %v", src.MountOptions)
	}
	if !src.Reserved || src.ReservedFor != "admin" {
		t.Errorf("reservation: %+v", src)
	}
}

func TestPV(t *testing.T) {
	src, _ := Resolve(lustrePV("abc", "flock"))
	share := &sharedfsv1alpha1.FilesystemShare{ObjectMeta: metav1.ObjectMeta{Name: "ds", UID: "share-uid"}}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "team", UID: "pvc-uid"},
		Spec:       corev1.PersistentVolumeClaimSpec{AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadOnlyMany}},
	}
	pv := PV(Claim{PVC: pvc, Share: share, Source: src, Access: sharedfsv1alpha1.AccessReadOnly,
		ExpandedPath: "datasets/v1", ClassName: "ds-ro"})

	if pv.Name != "sfs-pvc-uid" || pv.Spec.CSI.VolumeHandle != "sharedfs:pvc-uid" {
		t.Errorf("names: %s %s", pv.Name, pv.Spec.CSI.VolumeHandle)
	}
	if got := pv.Spec.CSI.VolumeAttributes["mountname"]; got != "abc/datasets/v1" {
		t.Errorf("mountname %q", got)
	}
	if pv.Spec.CSI.FSType != "" {
		t.Error("fsType must not be copied: with RWO in accessModes kubelet would apply fsGroup recursively")
	}
	if !slices.Equal(pv.Spec.AccessModes, []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany, corev1.ReadOnlyMany}) {
		t.Errorf("access modes %v", pv.Spec.AccessModes)
	}
	if !slices.Equal(pv.Spec.MountOptions, []string{"localflock", "ro"}) {
		t.Errorf("mount options %v", pv.Spec.MountOptions)
	}
	if pv.Spec.ClaimRef.UID != "pvc-uid" || pv.Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimDelete {
		t.Errorf("binding %+v", pv.Spec.ClaimRef)
	}
	if pv.Annotations[AnnotationProvisionedBy] != sharedfsv1alpha1.ProvisionerName || !ProvisionedBySharedfs(pv) {
		t.Error("must be marked provisioned by sharedfs")
	}
	if q := pv.Spec.Capacity[corev1.ResourceStorage]; q.String() != "1Gi" {
		t.Errorf("a claim without a request gets 1Gi, got %s", q.String())
	}
}

func TestDefaultMountname(t *testing.T) {
	src, _ := Resolve(lustrePV(""))
	if got := Attributes(src, "x")["mountname"]; got != "fsx/x" {
		t.Errorf("the driver's default mountname is fsx: got %q", got)
	}
	if _, ok := Attributes(src, "/")["mountname"]; ok {
		t.Error("the root keeps the source's attributes")
	}
}
