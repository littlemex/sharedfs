//go:build !linux

package prepare

import "golang.org/x/sys/unix"

// Project IDs exist on Linux only; the preparer only runs there.
func ProjectOf(int) (uint32, bool, error) { return 0, false, nil }

func setProject(int, uint32) error { return unix.ENOTSUP }
