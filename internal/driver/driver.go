// Package driver holds what sharedfs knows about each CSI driver it can distribute.
//
// Every entry is a safety claim checked against the driver's source and on a real cluster, so
// the table lives in code and is not configurable: a driver whose mount depends on volumeHandle
// (efs.csi.aws.com parses it as the mount target) would break silently if added by
// configuration. When a driver is upgraded, rerun the e2e mount test rather than editing here.
package driver

import (
	"slices"

	corev1 "k8s.io/api/core/v1"
)

// Profile describes one CSI driver.
type Profile struct {
	// Name is the CSI driver name.
	Name string
	// RequiredAttributes are the volumeAttributes the mount source is built from; a source
	// without them would only fail at mount time.
	RequiredAttributes []string
	// OptionalAttributes are the other volumeAttributes the driver reads.
	OptionalAttributes []string
	// AccessMode is the access mode generated volumes use. Read-only is enforced with the ro
	// mount option instead, since neither driver accepts ReadOnlyMany.
	AccessMode corev1.PersistentVolumeAccessMode
}

// For every profile the volumeHandle is an identifier only and the mount source comes from
// volumeAttributes. That is what lets each generated PersistentVolume carry its own handle.
var profiles = []Profile{
	{
		// kubernetes-sigs/aws-fsx-csi-driver@5ede23e pkg/driver/node.go NodePublishVolume:
		// source = "<dnsname>@tcp:/<mountname>" (mountname defaults to "fsx").
		// controller.go volumeCaps: SINGLE_NODE_WRITER, MULTI_NODE_MULTI_WRITER.
		Name:               "fsx.csi.aws.com",
		RequiredAttributes: []string{"dnsname"},
		OptionalAttributes: []string{"mountname"},
		AccessMode:         corev1.ReadWriteMany,
	},
	{
		// kubernetes-sigs/aws-fsx-openzfs-csi-driver@93c640d pkg/driver/node.go NodePublishVolume:
		// source = "<DNSName>:<VolumePath>" (VolumePath defaults to "fsx" for a file system).
		// controller.go volumeCaps: SINGLE_NODE_WRITER, MULTI_NODE_MULTI_WRITER.
		Name:               "fsx.openzfs.csi.aws.com",
		RequiredAttributes: []string{"DNSName", "ResourceType"},
		OptionalAttributes: []string{"VolumePath"},
		AccessMode:         corev1.ReadWriteMany,
	},
}

// Lookup returns the profile for a CSI driver name.
func Lookup(name string) (Profile, bool) {
	i := slices.IndexFunc(profiles, func(p Profile) bool { return p.Name == name })
	if i < 0 {
		return Profile{}, false
	}
	return profiles[i], true
}

// Names lists the supported driver names.
func Names() []string {
	names := make([]string, len(profiles))
	for i, p := range profiles {
		names[i] = p.Name
	}
	return names
}

// Attributes returns the attributes of attrs that the driver reads, and the required ones
// that are missing.
func (p Profile) Attributes(attrs map[string]string) (map[string]string, []string) {
	out := map[string]string{}
	var missing []string
	for _, k := range p.RequiredAttributes {
		if v, ok := attrs[k]; ok && v != "" {
			out[k] = v
		} else {
			missing = append(missing, k)
		}
	}
	for _, k := range p.OptionalAttributes {
		if v, ok := attrs[k]; ok {
			out[k] = v
		}
	}
	return out, missing
}
