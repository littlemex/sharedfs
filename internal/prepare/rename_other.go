//go:build !linux

package prepare

import (
	"errors"

	"golang.org/x/sys/unix"
)

// renameNoReplace has no RENAME_NOREPLACE outside Linux; the preparer only runs on Linux, and
// this exists so the package builds and its checks can be tested elsewhere.
func renameNoReplace(dirfd int, from, to string) error {
	var st unix.Stat_t
	if err := unix.Fstatat(dirfd, to, &st, unix.AT_SYMLINK_NOFOLLOW); err == nil {
		return unix.EEXIST
	} else if !errors.Is(err, unix.ENOENT) {
		return err
	}
	return unix.Renameat(dirfd, from, dirfd, to)
}
