// Package volume builds the PersistentVolume and PersistentVolumeClaim that one
// SharedFilesystemClaim stands for, and answers which existing objects belong to which claim.
//
// It is pure: no client calls, so every rule here is unit-testable.
package volume

import (
	"fmt"
	"maps"
	"slices"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	"k8s.io/utils/ptr"

	sharedfsv1alpha1 "github.com/littlemex/sharedfs/api/v1alpha1"
	"github.com/littlemex/sharedfs/internal/driver"
)

const (
	group = "sharedfs.littlemex.github.io"

	// FieldOwner is the server-side apply field manager for generated PersistentVolumes.
	FieldOwner = "sharedfs"

	// LabelManagedBy marks every object sharedfs generates.
	LabelManagedBy = "app.kubernetes.io/managed-by"
	// ManagedByValue is the value of LabelManagedBy.
	ManagedByValue = "sharedfs"

	// Labels carry UIDs and the namespace only: names can be up to 253 characters and do not
	// fit in a label value (63).
	LabelClaimUID       = group + "/claim-uid"
	LabelClaimNamespace = group + "/claim-namespace"
	LabelFilesystemUID  = group + "/filesystem-uid"

	AnnotationClaimName      = group + "/claim-name"
	AnnotationFilesystemName = group + "/filesystem-name"

	// Finalizer is used on both kinds.
	Finalizer = group + "/cleanup"

	// MaxVolumeHandleBytes is the CSI limit on a volume ID.
	MaxVolumeHandleBytes = 128
)

// RequestedStorage is what the generated PVC asks for. A static PersistentVolume binds when its
// capacity is at least the request, so a small request binds to any file system while keeping
// the tenant's storage ResourceQuota from being charged the whole file system size.
var RequestedStorage = resource.MustParse("1Gi")

// PVName is the generated PersistentVolume name. It is derived from the claim UID so that two
// claims can never map to the same name ("a-b"+"c" and "a"+"b-c" would with a joined name).
func PVName(claimUID types.UID) string { return "sfc-" + string(claimUID) }

// VolumeHandle gives each generated PersistentVolume its own handle: kubelet identifies a CSI
// volume by driver and handle, so two PVs with the same handle collide on one node.
func VolumeHandle(sourceHandle string, claimUID types.UID) (string, error) {
	h := sourceHandle + "-sfc-" + string(claimUID)
	if len(h) > MaxVolumeHandleBytes {
		return "", fmt.Errorf("volume handle %q is %d bytes, over the CSI limit of %d", h, len(h), MaxVolumeHandleBytes)
	}
	return h, nil
}

// PV builds the server-side apply configuration of the PersistentVolume for a claim.
//
// claimRef carries the namespace and name only. The PV controller owns uid and resourceVersion;
// leaving them out lets the claimRef reserve the volume for exactly this PVC without fighting the
// binder over the fields it writes.
func PV(claim *sharedfsv1alpha1.SharedFilesystemClaim, fs *sharedfsv1alpha1.SharedFilesystem, profile driver.Profile) (*corev1ac.PersistentVolumeApplyConfiguration, error) {
	src := fs.Status.Resolved
	if src == nil {
		return nil, fmt.Errorf("filesystem %s has no resolved source", fs.Name)
	}
	handle, err := VolumeHandle(src.VolumeHandle, claim.UID)
	if err != nil {
		return nil, err
	}
	capacity, err := resource.ParseQuantity(src.Capacity)
	if err != nil {
		return nil, fmt.Errorf("filesystem %s: capacity %q: %w", fs.Name, src.Capacity, err)
	}
	opts := slices.Clone(src.MountOptions)
	if claim.Spec.Access != sharedfsv1alpha1.AccessReadWrite {
		opts = append(opts, profile.ReadOnlyMountOption)
	}

	pv := corev1ac.PersistentVolume(PVName(claim.UID)).
		WithLabels(map[string]string{
			LabelManagedBy:      ManagedByValue,
			LabelClaimUID:       string(claim.UID),
			LabelClaimNamespace: claim.Namespace,
			LabelFilesystemUID:  string(fs.UID),
		}).
		WithAnnotations(map[string]string{
			AnnotationClaimName:      claim.Name,
			AnnotationFilesystemName: fs.Name,
		}).
		WithSpec(corev1ac.PersistentVolumeSpec().
			WithCapacity(corev1.ResourceList{corev1.ResourceStorage: capacity}).
			// The FSx drivers accept only single-node-writer and multi-node-multi-writer, so
			// read-only is the ro mount option above, not ReadOnlyMany.
			WithAccessModes(corev1.ReadWriteMany).
			WithPersistentVolumeReclaimPolicy(corev1.PersistentVolumeReclaimRetain).
			WithStorageClassName("").
			WithVolumeMode(corev1.PersistentVolumeFilesystem).
			WithMountOptions(opts...).
			WithCSI(corev1ac.CSIPersistentVolumeSource().
				WithDriver(src.Driver).
				WithVolumeHandle(handle).
				WithVolumeAttributes(maps.Clone(src.VolumeAttributes))).
			WithClaimRef(corev1ac.ObjectReference().
				WithKind("PersistentVolumeClaim").
				WithAPIVersion("v1").
				WithNamespace(claim.Namespace).
				WithName(PVCName(claim))))
	return pv, nil
}

// PVCName is the name of the generated PersistentVolumeClaim: the claim's own name, so tenants
// mount what they asked for by the name they chose.
func PVCName(claim *sharedfsv1alpha1.SharedFilesystemClaim) string { return claim.Name }

// PVC builds the PersistentVolumeClaim for a claim. It is created once and never updated: its
// spec has nothing that changes after creation.
func PVC(claim *sharedfsv1alpha1.SharedFilesystemClaim) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      PVCName(claim),
			Namespace: claim.Namespace,
			Labels: map[string]string{
				LabelManagedBy: ManagedByValue,
				LabelClaimUID:  string(claim.UID),
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			// "" and not nil: nil would let the DefaultStorageClass admission plugin inject the
			// cluster default and the claim would provision a new volume instead.
			StorageClassName: ptr.To(""),
			VolumeName:       PVName(claim.UID),
			VolumeMode:       ptr.To(corev1.PersistentVolumeFilesystem),
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: RequestedStorage},
			},
		},
	}
}

// OwnsPVC reports whether pvc is the claim's. The test reads volumeName, which the tenant cannot
// point anywhere useful: the generated PV is reserved for this namespace and name by its
// claimRef. ownerReferences are not used for the test because the tenant can edit them.
func OwnsPVC(claimUID types.UID, pvc *corev1.PersistentVolumeClaim) bool {
	return pvc.Spec.VolumeName == PVName(claimUID)
}

// ImmutableDrift reports the CSI fields of an existing PV that differ from the desired ones. A
// PersistentVolume's volume source cannot be changed after creation, so a difference means the
// source file system changed under the claim.
func ImmutableDrift(existing *corev1.PersistentVolume, desired *corev1ac.PersistentVolumeApplyConfiguration) []string {
	var drift []string
	have := existing.Spec.CSI
	want := desired.Spec.CSI
	if have == nil {
		return []string{"csi"}
	}
	if have.Driver != *want.Driver {
		drift = append(drift, "csi.driver")
	}
	if have.VolumeHandle != *want.VolumeHandle {
		drift = append(drift, "csi.volumeHandle")
	}
	if !maps.Equal(have.VolumeAttributes, want.VolumeAttributes) {
		drift = append(drift, "csi.volumeAttributes")
	}
	return drift
}

// StaleClaimUID reports whether the PV holds the UID of a PVC that is not the live one: the
// "Released" trap. Only that case is healed, by removing claimRef.uid and resourceVersion.
func StaleClaimUID(pv *corev1.PersistentVolume, livePVC *corev1.PersistentVolumeClaim) bool {
	return pv.Status.Phase == corev1.VolumeReleased &&
		pv.Spec.ClaimRef != nil &&
		pv.Spec.ClaimRef.UID != "" &&
		livePVC != nil &&
		livePVC.DeletionTimestamp == nil &&
		pv.Spec.ClaimRef.UID != livePVC.UID
}
