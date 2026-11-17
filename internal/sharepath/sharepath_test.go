package sharepath

import (
	"testing"
)

func TestParseVectors(t *testing.T) {
	for _, p := range ValidExamples {
		if _, err := Parse(p); err != nil {
			t.Errorf("%q must be valid: %v", p, err)
		}
	}
	for _, p := range InvalidExamples {
		if _, err := Parse(p); err == nil {
			t.Errorf("%q must be invalid", p)
		}
	}
}

func TestExpand(t *testing.T) {
	tp, _ := Parse("checkpoints/{namespace}")
	got, err := tp.Expand("team-a")
	if err != nil || got != "checkpoints/team-a" {
		t.Fatalf("got %q %v", got, err)
	}
	if !tp.PerNamespace() {
		t.Error("PerNamespace")
	}
	// Namespace names are DNS labels, but expansion re-validates anyway.
	if _, err := tp.Expand("-x"); err == nil {
		t.Error("an expansion that breaks the segment rules must fail")
	}
	root, _ := Parse("/")
	if got, _ := root.Expand("team-a"); got != "/" || root.PerNamespace() {
		t.Error("root")
	}
}

func TestParentAndJoin(t *testing.T) {
	if p, l := Parent("checkpoints/team-a"); p != "checkpoints" || l != "team-a" {
		t.Errorf("%s %s", p, l)
	}
	if p, l := Parent("datasets"); p != Root || l != "datasets" {
		t.Errorf("%s %s", p, l)
	}
	if Join("abcdefgh", "/") != "abcdefgh" || Join("abcdefgh", "a/b") != "abcdefgh/a/b" {
		t.Error("join")
	}
}

func TestMayContain(t *testing.T) {
	p := func(s string) Template { t, _ := Parse(s); return t }
	cases := []struct {
		a, b string
		want bool
	}{
		{"/", "datasets", true},
		{"checkpoints", "checkpoints/{namespace}", true},
		{"checkpoints/{namespace}", "checkpoints/team-a", true},
		{"checkpoints/team-a", "checkpoints/{namespace}", true}, // namespace team-a expands to it
		{"scratch/common", "scratch/{namespace}", true},         // namespace "common"
		{"datasets", "models", false},
		{"checkpoints/{namespace}", "checkpoints", false},
		{"checkpoints/{namespace}", "datasets/team-a", false},
	}
	for _, c := range cases {
		if got := MayContain(p(c.a), p(c.b)); got != c.want {
			t.Errorf("MayContain(%q, %q) = %v", c.a, c.b, got)
		}
	}
}
