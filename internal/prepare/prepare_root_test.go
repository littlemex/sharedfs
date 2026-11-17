package prepare

import (
	"os"
	"path/filepath"
	"testing"
)

// These run only as root, the way the preparer pod runs: UID 0 with every capability dropped but
// CHOWN (`make test-preparer` runs them so in a container).
func rootParent(t *testing.T, mode os.FileMode) string {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	d := filepath.Join(t.TempDir(), "parent")
	if err := os.Mkdir(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(d, mode); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestCreateThenExists(t *testing.T) {
	d := rootParent(t, 0o755)
	r := Run(Request{Root: d, Name: "team-a", UID: 0, GID: 1000, Mode: "2770"})
	if r.Outcome != Created || r.GID != 1000 || r.Mode != "2770" {
		t.Fatalf("%+v", r)
	}
	if r := Run(Request{Root: d, Name: "team-a", UID: 1234, GID: 5, Mode: "0700"}); r.Outcome != Exists || r.GID != 1000 || r.Mode != "2770" {
		t.Fatalf("an existing directory must be reported unchanged: %+v", r)
	}
	entries, _ := os.ReadDir(d)
	if len(entries) != 1 {
		t.Fatalf("only the directory must remain: %v", entries)
	}
}

func TestOtherOwner(t *testing.T) {
	d := rootParent(t, 0o755)
	r := Run(Request{Root: d, Name: "u", UID: 1234, GID: 1234, Mode: "0700"})
	if r.Outcome != Created || r.UID != 1234 || r.Mode != "0700" {
		t.Fatalf("%+v", r)
	}
	// Mode 0700 for another owner: root without DAC_OVERRIDE cannot open it, but must still see it.
	if r := Run(Request{Root: d, Name: "u", UID: 1234, GID: 1234, Mode: "0700"}); r.Outcome != Exists || r.UID != 1234 {
		t.Fatalf("%+v", r)
	}
}

func TestSetgidParentDoesNotLeakIntoMode(t *testing.T) {
	d := rootParent(t, 0o755|os.ModeSetgid)
	if r := Run(Request{Root: d, Name: "x", GID: 7, Mode: "0750"}); r.Outcome != Created || r.Mode != "0750" {
		t.Fatalf("%+v", r)
	}
}

func TestLeftoversRemoved(t *testing.T) {
	d := rootParent(t, 0o755)
	if err := os.Mkdir(filepath.Join(d, tempName("x")), 0o700); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(d, tempName("y"))
	if err := os.Mkdir(other, 0o700); err != nil {
		t.Fatal(err)
	}
	if r := Run(Request{Root: d, Name: "x", GID: 7, Mode: "0750"}); r.Outcome != Created {
		t.Fatalf("%+v", r)
	}
	if _, err := os.Stat(filepath.Join(d, tempName("x"))); !os.IsNotExist(err) {
		t.Fatal("the interrupted run's temporary directory must be removed")
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatal("another name's temporary directory is not ours to remove")
	}
}

func TestSetgidParentOfAnotherGroup(t *testing.T) {
	d := rootParent(t, 0o755|os.ModeSetgid)
	if err := os.Chown(d, 0, 7); err != nil {
		t.Fatal(err)
	}
	if r := Run(Request{Root: d, Name: "x", GID: 1000, Mode: "2770"}); r.Outcome != Created || r.Mode != "2770" || r.GID != 1000 {
		t.Fatalf("%+v", r)
	}
}

func TestSymlinkAndFileRefused(t *testing.T) {
	d := rootParent(t, 0o755)
	if err := os.Symlink("/etc", filepath.Join(d, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "file"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"link", "file"} {
		if r := Run(Request{Root: d, Name: n, GID: 7, Mode: "0750"}); r.Outcome != NotADirectory {
			t.Errorf("%s: %+v", n, r)
		}
	}
}

func TestWritableParentRefused(t *testing.T) {
	d := rootParent(t, 0o775)
	if r := Run(Request{Root: d, Name: "x", GID: 7, Mode: "0750"}); r.Outcome != InsecureParent {
		t.Fatalf("%+v", r)
	}
}
