package prepare

import (
	"errors"

	"golang.org/x/sys/unix"
)

// renameNoReplace renames without replacing an existing entry. Where the file system does not
// support RENAME_NOREPLACE it checks that the target is absent first; the parent is writable only
// by root, so the only other writer is another run of this preparer.
func renameNoReplace(dirfd int, from, to string) error {
	err := unix.Renameat2(dirfd, from, dirfd, to, unix.RENAME_NOREPLACE)
	if !errors.Is(err, unix.EINVAL) && !errors.Is(err, unix.ENOSYS) {
		return err
	}
	var st unix.Stat_t
	if err := unix.Fstatat(dirfd, to, &st, unix.AT_SYMLINK_NOFOLLOW); err == nil {
		return unix.EEXIST
	} else if !errors.Is(err, unix.ENOENT) {
		return err
	}
	return unix.Renameat(dirfd, from, dirfd, to)
}
