package prepare

import (
	"os"
	"path/filepath"
	"testing"
)

// These run as an ordinary user: the parent is then not root-owned, so the root-owner checks are
// exercised through InsecureParent, and creation is exercised with Run's internals.

func TestRejectsBadNames(t *testing.T) {
	for _, n := range []string{"", "a/b", "..", ".x", "-x", "{namespace}", "/"} {
		if r := Run(Request{Root: t.TempDir(), Name: n, Mode: "2770"}); r.Outcome != Failed {
			t.Errorf("%q: %+v", n, r)
		}
	}
}

func TestRejectsBadModes(t *testing.T) {
	for _, m := range []string{"4755", "1777", "9", "abc"} {
		if r := Run(Request{Root: t.TempDir(), Name: "x", Mode: m}); r.Outcome != Failed {
			t.Errorf("%q: %+v", m, r)
		}
	}
}

func TestInsecureParent(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root owns the temp dir")
	}
	if r := Run(Request{Root: t.TempDir(), Name: "x", Mode: "2770"}); r.Outcome != InsecureParent {
		t.Fatalf("a parent not owned by root must be refused: %+v", r)
	}
}

func TestSymlinkParentRefused(t *testing.T) {
	d := t.TempDir()
	link := filepath.Join(d, "link")
	if err := os.Symlink(d, link); err != nil {
		t.Fatal(err)
	}
	if r := Run(Request{Root: link, Name: "x", Mode: "2770"}); r.Outcome == Created || r.Outcome == Exists {
		t.Fatalf("a symlinked parent must not be used: %+v", r)
	}
}

func TestResultJSON(t *testing.T) {
	if got := (Result{Outcome: Created, UID: 1, GID: 2, Mode: "2770"}).JSON(); got != `{"outcome":"DirectoryCreated","uid":1,"gid":2,"mode":"2770"}` {
		t.Error(got)
	}
}
