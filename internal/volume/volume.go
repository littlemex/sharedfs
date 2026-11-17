// Package volume builds the PersistentVolumes sharedfs provisions, and answers which existing
// objects belong to it. It is pure: no client calls.
package volume

import (
	"maps"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	sharedfsv1alpha1 "github.com/littlemex/sharedfs/api/v1alpha1"
	"github.com/littlemex/sharedfs/internal/sharepath"
)

const (
	// AnnotationProvisionedBy is the annotation the PV controller and every external
	// provisioner use to tell whose volume a PV is. With it set to sharedfs, the PV controller
	// leaves a Released Delete volume alone and the driver's own provisioner ignores it, so
	// deleting it is sharedfs's job and never reaches the driver.
	AnnotationProvisionedBy = "pv.kubernetes.io/provisioned-by"
	// AnnotationStorageProvisioner is set on PVCs by the PV controller to the provisioner of
	// their class.
	AnnotationStorageProvisioner     = "volume.kubernetes.io/storage-provisioner"
	AnnotationBetaStorageProvisioner = "volume.beta.kubernetes.io/storage-provisioner"

	mountOptionReadOnly  = "ro"
	mountOptionReadWrite = "rw"
	mountOptionFlock     = "flock"
	mountOptionLocalLock = "localflock"

	pvNamePrefix       = "sfs-"
	preparerNamePrefix = "sfs-prep-"
	handlePrefix       = "sharedfs:"
)

// PVName is the name of the PersistentVolume provisioned for a claim.
func PVName(pvcUID types.UID) string { return pvNamePrefix + string(pvcUID) }

// IsTenantPVName reports whether a volume name is one sharedfs gives a tenant's claim.
func IsTenantPVName(name string) bool {
	return strings.HasPrefix(name, pvNamePrefix) && !strings.HasPrefix(name, preparerNamePrefix)
}

// PreparerName is the name of the internal PersistentVolume, claim and pod that prepare a
// directory record.
func PreparerName(dirUID types.UID) string { return preparerNamePrefix + string(dirUID) }

// IsGenerated reports whether an object was created by sharedfs.
func IsGenerated(o metav1.Object) bool {
	_, share := o.GetLabels()[sharedfsv1alpha1.LabelShareUID]
	_, internal := o.GetLabels()[sharedfsv1alpha1.LabelInternal]
	return share || internal || o.GetAnnotations()[AnnotationProvisionedBy] == sharedfsv1alpha1.ProvisionerName
}

// IsInternal reports whether an object is one of the controller's own preparation objects.
func IsInternal(o metav1.Object) bool {
	_, ok := o.GetLabels()[sharedfsv1alpha1.LabelInternal]
	return ok
}

// ProvisionedBySharedfs reports whether a PersistentVolume is a tenant volume sharedfs
// provisioned (not an internal one).
func ProvisionedBySharedfs(pv *corev1.PersistentVolume) bool {
	return pv.Annotations[AnnotationProvisionedBy] == sharedfsv1alpha1.ProvisionerName && !IsInternal(pv)
}

// ClaimedProvisioner is the provisioner the PV controller asked to provision a PVC, if any.
func ClaimedProvisioner(pvc *corev1.PersistentVolumeClaim) string {
	if v := pvc.Annotations[AnnotationStorageProvisioner]; v != "" {
		return v
	}
	return pvc.Annotations[AnnotationBetaStorageProvisioner]
}

// MountOptions are the source's options for an access level. Read-only adds ro and turns flock
// into localflock, so a reader cannot hold cluster-wide locks against everyone else.
func MountOptions(src *Source, access sharedfsv1alpha1.Access) []string {
	opts := slices.Clone(src.MountOptions)
	if access == sharedfsv1alpha1.AccessReadWrite {
		return opts
	}
	for i, o := range opts {
		if o == mountOptionFlock {
			opts[i] = mountOptionLocalLock
		}
	}
	return append(opts, mountOptionReadOnly)
}

// Attributes are the source's volume attributes pointing at an expanded path.
func Attributes(src *Source, expanded string) map[string]string {
	attrs := maps.Clone(src.VolumeAttributes)
	if expanded != sharepath.Root && src.Profile.SubPath != nil {
		attrs[src.Profile.SubPath.Attribute] = sharepath.Join(src.subPathBase(), expanded)
	}
	return attrs
}

// AccessModes lists the driver's mode first: kubelet passes the first one to the driver, and the
// FSx drivers accept multi-node-multi-writer only. The claim's own modes follow so the PV
// controller, which requires the PV to include every requested mode, binds them.
func AccessModes(src *Source, requested []corev1.PersistentVolumeAccessMode) []corev1.PersistentVolumeAccessMode {
	modes := []corev1.PersistentVolumeAccessMode{src.Profile.AccessMode}
	for _, m := range requested {
		if !slices.Contains(modes, m) {
			modes = append(modes, m)
		}
	}
	return modes
}

// Claim describes the PersistentVolumeClaim a volume is provisioned for.
type Claim struct {
	PVC          *corev1.PersistentVolumeClaim
	Share        *sharedfsv1alpha1.FilesystemShare
	Source       *Source
	Access       sharedfsv1alpha1.Access
	ExpandedPath string
	ClassName    string
}

// PV builds the PersistentVolume for a claim. It is bound to the claim by UID like any
// dynamically provisioned volume. Its handle cannot be taken for a file system ID, and it
// carries no fsType, so the CSIDriver's fsGroup handling never applies a recursive chown.
func PV(c Claim) *corev1.PersistentVolume {
	pvc := c.PVC
	request := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
	if request.IsZero() {
		request = resource.MustParse("1Gi")
	}
	return &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: PVName(pvc.UID),
			Labels: map[string]string{
				sharedfsv1alpha1.LabelShareUID: string(c.Share.UID),
				sharedfsv1alpha1.LabelPVCUID:   string(pvc.UID),
			},
			Annotations: map[string]string{
				AnnotationProvisionedBy:              sharedfsv1alpha1.ProvisionerName,
				sharedfsv1alpha1.AnnotationShareName: c.Share.Name,
				sharedfsv1alpha1.AnnotationAccess:    string(c.Access),
			},
		},
		Spec: corev1.PersistentVolumeSpec{
			Capacity:                      corev1.ResourceList{corev1.ResourceStorage: request},
			AccessModes:                   AccessModes(c.Source, pvc.Spec.AccessModes),
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimDelete,
			StorageClassName:              c.ClassName,
			VolumeMode:                    ptrTo(corev1.PersistentVolumeFilesystem),
			MountOptions:                  MountOptions(c.Source, c.Access),
			NodeAffinity:                  c.Source.NodeAffinity.DeepCopy(),
			ClaimRef: &corev1.ObjectReference{
				Kind: "PersistentVolumeClaim", APIVersion: "v1",
				Namespace: pvc.Namespace, Name: pvc.Name, UID: pvc.UID,
			},
			PersistentVolumeSource: corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{
				Driver:           c.Source.Profile.Name,
				VolumeHandle:     handlePrefix + string(pvc.UID),
				VolumeAttributes: Attributes(c.Source, c.ExpandedPath),
			}},
		},
	}
}

// PreparerPV builds the internal PersistentVolume through which the preparer mounts the parent
// of a directory it creates. It is read-write, reserved for the internal claim by UID, and
// Retain: deleting it never asks anything of the driver.
func PreparerPV(dir *sharedfsv1alpha1.FilesystemShareDirectory, src *Source, parent string, claim *corev1.PersistentVolumeClaim) *corev1.PersistentVolume {
	return &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name:   PreparerName(dir.UID),
			Labels: map[string]string{sharedfsv1alpha1.LabelInternal: string(dir.UID)},
			Annotations: map[string]string{
				AnnotationProvisionedBy: sharedfsv1alpha1.ProvisionerName,
			},
		},
		Spec: corev1.PersistentVolumeSpec{
			Capacity:                      corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
			AccessModes:                   []corev1.PersistentVolumeAccessMode{src.Profile.AccessMode},
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
			StorageClassName:              "",
			VolumeMode:                    ptrTo(corev1.PersistentVolumeFilesystem),
			MountOptions:                  MountOptions(src, sharedfsv1alpha1.AccessReadWrite),
			NodeAffinity:                  src.NodeAffinity.DeepCopy(),
			ClaimRef: &corev1.ObjectReference{
				Kind: "PersistentVolumeClaim", APIVersion: "v1",
				Namespace: claim.Namespace, Name: claim.Name, UID: claim.UID,
			},
			PersistentVolumeSource: corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{
				Driver:           src.Profile.Name,
				VolumeHandle:     handlePrefix + "prepare:" + string(dir.UID),
				VolumeAttributes: Attributes(src, parent),
			}},
		},
	}
}

// PreparerPVC builds the internal claim of a preparer.
func PreparerPVC(dir *sharedfsv1alpha1.FilesystemShareDirectory, namespace string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: PreparerName(dir.UID), Namespace: namespace,
			Labels: map[string]string{sharedfsv1alpha1.LabelInternal: string(dir.UID)},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			StorageClassName: ptrTo(""),
			VolumeName:       PreparerName(dir.UID),
			VolumeMode:       ptrTo(corev1.PersistentVolumeFilesystem),
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
			},
		},
	}
}

func ptrTo[T any](v T) *T { return &v }
