package volume

import (
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	sharedfsv1alpha1 "github.com/littlemex/sharedfs/api/v1alpha1"
	"github.com/littlemex/sharedfs/internal/driver"
)

// Source is the allow-listed copy of a source PersistentVolume that generated volumes are built
// from.
type Source struct {
	Profile          driver.Profile
	VolumeHandle     string
	VolumeAttributes map[string]string
	// MountOptions are the source's options without rw or ro, so each generated volume decides
	// its own access.
	MountOptions []string
	Capacity     resource.Quantity
	// NodeAffinity of the source, so pods land only where the file system is reachable.
	NodeAffinity *corev1.VolumeNodeAffinity
	// Reserved reports whether the source is bound or reserved with claimRef.
	Reserved bool
	// ReservedFor is the namespace the source is bound or reserved to, if any.
	ReservedFor string
}

// SourceError is why a source PersistentVolume cannot be distributed. Reason is a
// FilesystemShare Ready reason.
type SourceError struct {
	Reason  string
	Message string
}

func (e *SourceError) Error() string { return e.Reason + ": " + e.Message }

// Resolve reads a source PersistentVolume. Annotations (for example
// pv.kubernetes.io/provisioned-by) and the reclaim policy are never copied.
func Resolve(pv *corev1.PersistentVolume) (*Source, error) {
	if IsGenerated(pv) {
		return nil, &SourceError{sharedfsv1alpha1.ReasonSourceIsGenerated,
			fmt.Sprintf("%s was created by sharedfs; name the original PersistentVolume", pv.Name)}
	}
	csi := pv.Spec.CSI
	if csi == nil {
		return nil, &SourceError{sharedfsv1alpha1.ReasonSourceNotCSI, fmt.Sprintf("%s is not a CSI volume", pv.Name)}
	}
	p, ok := driver.Lookup(csi.Driver)
	if !ok {
		return nil, &SourceError{sharedfsv1alpha1.ReasonUnsupportedDriver,
			fmt.Sprintf("driver %q is not supported; supported: %s", csi.Driver, strings.Join(driver.Names(), ", "))}
	}
	if csi.VolumeHandle == "" {
		return nil, &SourceError{sharedfsv1alpha1.ReasonInvalidSource, fmt.Sprintf("%s has no volumeHandle", pv.Name)}
	}
	attrs, missing := p.Attributes(csi.VolumeAttributes)
	if len(missing) > 0 {
		return nil, &SourceError{sharedfsv1alpha1.ReasonInvalidSource,
			fmt.Sprintf("%s lacks volumeAttributes %s that %s mounts from", pv.Name, strings.Join(missing, ", "), p.Name)}
	}
	capacity, ok := pv.Spec.Capacity[corev1.ResourceStorage]
	if !ok || capacity.IsZero() {
		return nil, &SourceError{sharedfsv1alpha1.ReasonInvalidSource, fmt.Sprintf("%s has no storage capacity", pv.Name)}
	}
	s := &Source{
		Profile:          p,
		VolumeHandle:     csi.VolumeHandle,
		VolumeAttributes: attrs,
		MountOptions:     slices.DeleteFunc(slices.Clone(pv.Spec.MountOptions), isAccessOption),
		Capacity:         capacity,
		NodeAffinity:     pv.Spec.NodeAffinity.DeepCopy(),
	}
	if ref := pv.Spec.ClaimRef; ref != nil && ref.Name != "" {
		s.Reserved, s.ReservedFor = true, ref.Namespace
	}
	return s, nil
}

// Status is the informational copy shown on the FilesystemShare.
func (s *Source) Status() *sharedfsv1alpha1.ResolvedSource {
	return &sharedfsv1alpha1.ResolvedSource{
		Driver:           s.Profile.Name,
		VolumeHandle:     s.VolumeHandle,
		VolumeAttributes: s.VolumeAttributes,
		MountOptions:     s.MountOptions,
		Capacity:         s.Capacity,
	}
}

// Identity names the file system the source mounts, so shares on the same file system can be
// compared whatever PersistentVolume they name: the driver, its attributes other than the
// sub-path one, and the sub-path attribute's value, which may itself point below the root.
func (s *Source) Identity() (fs string, base string) {
	keys := make([]string, 0, len(s.VolumeAttributes))
	for k := range s.VolumeAttributes {
		if s.Profile.SubPath == nil || k != s.Profile.SubPath.Attribute {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	var b strings.Builder
	b.WriteString(s.Profile.Name)
	for _, k := range keys {
		b.WriteString("|" + k + "=" + s.VolumeAttributes[k])
	}
	return b.String(), s.subPathBase()
}

func (s *Source) subPathBase() string {
	if s.Profile.SubPath == nil {
		return ""
	}
	if v, ok := s.VolumeAttributes[s.Profile.SubPath.Attribute]; ok && v != "" {
		return v
	}
	return s.Profile.SubPath.Default
}

func isAccessOption(o string) bool { return o == mountOptionReadOnly || o == mountOptionReadWrite }
