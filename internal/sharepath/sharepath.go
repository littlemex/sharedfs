// Package sharepath defines the paths a FilesystemShare may expose, in one place: validation,
// namespace expansion, and the ancestor relation used to detect overlapping shares.
//
// A path is "/" (the file system root) or a relative, slash-separated list of segments. A
// segment is 1 to 255 bytes of [A-Za-z0-9._-] that does not start with "." or "-", or the
// placeholder "{namespace}" taking a whole segment. The CRD carries an equivalent CEL rule; the
// shared test vectors in sharepath_test.go keep the two in step.
package sharepath

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

const (
	// Root is the path of the whole file system.
	Root = "/"
	// NamespacePlaceholder expands to the claiming PVC's namespace.
	NamespacePlaceholder = "{namespace}"

	MaxBytes        = 1024
	MaxSegments     = 32
	MaxSegmentBytes = 255
)

var segment = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]*$`)

// Template is a validated share path, possibly containing the namespace placeholder.
type Template struct{ raw string }

// Parse validates a share path.
func Parse(p string) (Template, error) {
	if p == Root {
		return Template{raw: p}, nil
	}
	if p == "" {
		return Template{}, errors.New(`path is empty; use "/" for the file system root`)
	}
	if len(p) > MaxBytes {
		return Template{}, fmt.Errorf("path is %d bytes, more than %d", len(p), MaxBytes)
	}
	segs := strings.Split(p, "/")
	if len(segs) > MaxSegments {
		return Template{}, fmt.Errorf("path has %d segments, more than %d", len(segs), MaxSegments)
	}
	for i, s := range segs {
		if err := checkSegment(s, true); err != nil {
			return Template{}, fmt.Errorf("segment %d %q: %w", i+1, s, err)
		}
	}
	return Template{raw: p}, nil
}

func checkSegment(s string, placeholderOK bool) error {
	switch {
	case s == "":
		return errors.New("empty segment (leading, trailing or doubled slash)")
	case s == NamespacePlaceholder:
		if !placeholderOK {
			return errors.New("placeholder not allowed here")
		}
		return nil
	case len(s) > MaxSegmentBytes:
		return fmt.Errorf("%d bytes, more than %d", len(s), MaxSegmentBytes)
	case !segment.MatchString(s):
		return errors.New(`only [A-Za-z0-9._-], not starting with "." or "-"`)
	}
	return nil
}

func (t Template) String() string { return t.raw }

// IsRoot reports whether the template is the file system root.
func (t Template) IsRoot() bool { return t.raw == Root }

// PerNamespace reports whether the template contains the namespace placeholder.
func (t Template) PerNamespace() bool {
	return !t.IsRoot() && contains(t.segments(), NamespacePlaceholder)
}

func (t Template) segments() []string {
	if t.IsRoot() {
		return nil
	}
	return strings.Split(t.raw, "/")
}

// Expand substitutes the namespace and validates the result, so a namespace name can never
// produce a path the template rules would reject.
func (t Template) Expand(namespace string) (string, error) {
	if t.IsRoot() {
		return Root, nil
	}
	segs := t.segments()
	out := make([]string, len(segs))
	for i, s := range segs {
		if s == NamespacePlaceholder {
			s = namespace
		}
		if err := checkSegment(s, false); err != nil {
			return "", fmt.Errorf("expanded segment %d %q: %w", i+1, s, err)
		}
		out[i] = s
	}
	p := strings.Join(out, "/")
	if len(p) > MaxBytes {
		return "", fmt.Errorf("expanded path is %d bytes, more than %d", len(p), MaxBytes)
	}
	return p, nil
}

// Parent returns the expanded parent of an expanded path and the last segment. The parent of a
// one-segment path is Root.
func Parent(expanded string) (parent, leaf string) {
	i := strings.LastIndex(expanded, "/")
	if i < 0 {
		return Root, expanded
	}
	return expanded[:i], expanded[i+1:]
}

// Join appends an expanded path to a base that a driver mounts from (for example a Lustre
// mountname). Root leaves the base unchanged.
func Join(base, expanded string) string {
	if expanded == Root {
		return base
	}
	return strings.TrimRight(base, "/") + "/" + expanded
}

// MayContain reports whether some expansion of a can be equal to, or an ancestor of, some
// expansion of b. The placeholder matches any single segment. It is the relation behind the
// overlap guard: an ancestor share with write access can rename or plant files in what b exposes.
func MayContain(a, b Template) bool {
	as, bs := a.segments(), b.segments()
	if len(as) > len(bs) {
		return false
	}
	for i := range as {
		if as[i] != bs[i] && as[i] != NamespacePlaceholder && bs[i] != NamespacePlaceholder {
			return false
		}
	}
	return true
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
