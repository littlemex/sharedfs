package driver

import (
	"slices"
	"testing"
)

func TestLookup(t *testing.T) {
	for _, name := range []string{"fsx.csi.aws.com", "fsx.openzfs.csi.aws.com"} {
		if _, ok := Lookup(name); !ok {
			t.Errorf("%s must be supported", name)
		}
	}
	// efs.csi.aws.com mounts from volumeHandle, so a per-claim handle would break its mount.
	if _, ok := Lookup("efs.csi.aws.com"); ok {
		t.Error("efs.csi.aws.com must not be supported")
	}
}

func TestAttributes(t *testing.T) {
	p, _ := Lookup("fsx.openzfs.csi.aws.com")
	got, missing := p.Attributes(map[string]string{"DNSName": "a", "ResourceType": "filesystem", "Other": "x"})
	if len(missing) != 0 || len(got) != 2 {
		t.Errorf("got %v missing %v", got, missing)
	}
	_, missing = p.Attributes(map[string]string{"DNSName": "a"})
	if !slices.Equal(missing, []string{"ResourceType"}) {
		t.Errorf("missing %v", missing)
	}
}
