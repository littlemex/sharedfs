// Package driver holds what sharedfs knows about each CSI driver it can distribute.
//
// Every entry is a safety claim that was checked against the driver's source and on a real
// cluster, so the table lives in code and is not configurable: adding a driver whose mount does
// depend on volumeHandle (efs.csi.aws.com parses it as the mount target) would silently break.
// When a driver is upgraded, rerun the e2e mount test rather than editing this table.
package driver

import "slices"

// Profile describes one CSI driver.
type Profile struct {
	// Name is the CSI driver name.
	Name string
	// AttributeKeys are the volumeAttributes keys the driver's NodePublishVolume reads to build
	// the mount source. Only these keys are copied from the source PersistentVolume.
	AttributeKeys []string
	// ReadOnlyMountOption is the mount option that makes the mount read-only. The drivers below
	// reject the ReadOnlyMany access mode, so read-only is enforced at mount time instead.
	ReadOnlyMountOption string
	// AccessModeOptions are mount options that set rw or ro and are stripped from the source, so
	// the generated PersistentVolume decides the access on its own.
	AccessModeOptions []string
}

// The volumeHandle of every profile below is an identifier only: the mount source is built from
// volumeAttributes. That is what lets each generated PersistentVolume carry its own handle.
var profiles = []Profile{
	{
		// kubernetes-sigs/aws-fsx-csi-driver@5ede23e pkg/driver/node.go NodePublishVolume:
		// source = "<dnsname>@tcp:/<mountname>"; controller.go volumeCaps has no MULTI_NODE_READER_ONLY.
		Name:                "fsx.csi.aws.com",
		AttributeKeys:       []string{"dnsname", "mountname"},
		ReadOnlyMountOption: "ro",
		AccessModeOptions:   []string{"ro", "rw"},
	},
	{
		// kubernetes-sigs/aws-fsx-openzfs-csi-driver@93c640d pkg/driver/node.go NodePublishVolume:
		// source = "<DNSName>:<VolumePath>"; controller.go volumeCaps has no MULTI_NODE_READER_ONLY.
		Name:                "fsx.openzfs.csi.aws.com",
		AttributeKeys:       []string{"DNSName", "ResourceType", "VolumePath"},
		ReadOnlyMountOption: "ro",
		AccessModeOptions:   []string{"ro", "rw"},
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

// Names lists the supported driver names, for messages.
func Names() []string {
	names := make([]string, len(profiles))
	for i, p := range profiles {
		names[i] = p.Name
	}
	return names
}

// CopyAttributes returns the subset of attrs that the driver reads.
func (p Profile) CopyAttributes(attrs map[string]string) map[string]string {
	out := map[string]string{}
	for _, k := range p.AttributeKeys {
		if v, ok := attrs[k]; ok {
			out[k] = v
		}
	}
	return out
}

// NormalizeMountOptions drops the rw/ro options from the source's mount options.
func (p Profile) NormalizeMountOptions(opts []string) []string {
	out := make([]string, 0, len(opts))
	for _, o := range opts {
		if !slices.Contains(p.AccessModeOptions, o) {
			out = append(out, o)
		}
	}
	return out
}
