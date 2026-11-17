// Package prepare creates one directory inside a parent directory with a given owner and mode,
// without following symlinks, without changing anything that already exists, and without ever
// exposing a half-made directory under its final name. It runs inside the preparer pod, which
// mounts the parent and runs as UID 0 with CAP_CHOWN only, and reports a Result as JSON.
//
// The directory is made under a temporary name derived from the final one (".sharedfs-tmp-<hash>",
// which no share path can name because path segments may not start with "."), handed to root:root,
// given its mode and then its owner through a file descriptor, checked, and renamed to the final
// name without replacing anything. A crash leaves only that empty temporary directory, which the
// next run for the same name removes; preparers of other names in the same parent leave it alone. The parent must be owned by root
// and not writable by group or others, so no tenant can create, swap or remove entries in it.
//
// Every step works with CAP_CHOWN alone: the process is the owner of what it creates (no
// CAP_DAC_OVERRIDE needed to open it, mode 0700), sets the mode while it still owns the directory
// (no CAP_FOWNER), and only then hands it over.
package prepare

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"golang.org/x/sys/unix"

	sharedfsv1alpha1 "github.com/littlemex/sharedfs/api/v1alpha1"
	"github.com/littlemex/sharedfs/internal/sharepath"
)

// Outcomes are FilesystemShareDirectory Prepared reasons.
const (
	Created          = sharedfsv1alpha1.ReasonDirectoryCreated
	Exists           = sharedfsv1alpha1.ReasonDirectoryExists
	NotADirectory    = sharedfsv1alpha1.ReasonNotADirectory
	InsecureParent   = sharedfsv1alpha1.ReasonInsecureParent
	PermissionDenied = sharedfsv1alpha1.ReasonPermissionDenied
	Failed           = sharedfsv1alpha1.ReasonPreparationFailed
)

// Outcomes lists every outcome a preparer may report; anything else is not trusted.
var Outcomes = []string{Created, Exists, NotADirectory, InsecureParent, PermissionDenied, Failed}

// TempPrefix starts the name of a directory being prepared.
const TempPrefix = ".sharedfs-tmp-"

// Request is one directory to prepare.
type Request struct {
	// Root is the mounted parent directory.
	Root string
	// Name is the directory to create in Root: one path segment.
	Name string
	UID  int
	GID  int
	// Mode is the octal permission string, for example "2770".
	Mode string
	// ProjectID, if not 0, is given to a created directory and inherited below it.
	ProjectID uint32
}

// Result is reported back to the controller.
type Result struct {
	Outcome string `json:"outcome"`
	Message string `json:"message,omitempty"`
	UID     int64  `json:"uid,omitempty"`
	GID     int64  `json:"gid,omitempty"`
	Mode    string `json:"mode,omitempty"`
	// ProjectID is the directory's project ID, 0 for none or unknown.
	ProjectID uint32 `json:"projectID,omitempty"`
}

// JSON encodes a result; it never fails for this type.
func (r Result) JSON() string {
	b, _ := json.Marshal(r)
	return string(b)
}

const permMask = 0o7777

// Run prepares the directory.
func Run(req Request) Result {
	mode, err := ParseMode(req.Mode)
	if err != nil {
		return Result{Outcome: Failed, Message: err.Error()}
	}
	if err := sharepath.CheckSegment(req.Name); err != nil {
		return Result{Outcome: Failed, Message: fmt.Sprintf("invalid directory name %q: %v", req.Name, err)}
	}
	if req.UID < 0 || req.GID < 0 {
		return Result{Outcome: Failed, Message: "uid and gid must not be negative"}
	}

	parent, err := unix.Open(req.Root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fromErrno("opening the parent", err)
	}
	defer unix.Close(parent)
	var pst unix.Stat_t
	if err := unix.Fstat(parent, &pst); err != nil {
		return fromErrno("checking the parent", err)
	}
	if pst.Uid != 0 || uint32(pst.Mode)&0o022 != 0 {
		return Result{Outcome: InsecureParent, Message: fmt.Sprintf(
			"the parent directory must be owned by root and not writable by group or others (uid %d, mode %04o)", pst.Uid, uint32(pst.Mode)&permMask)}
	}
	tmp := tempName(req.Name)
	// An interrupted run for this name; rmdir removes only an empty directory.
	_ = unix.Unlinkat(parent, tmp, unix.AT_REMOVEDIR)

	if res, ok := existing(parent, req.Name); ok {
		return res
	}
	if err := unix.Mkdirat(parent, tmp, 0o700); err != nil {
		return fromErrno("creating the directory", err)
	}
	res := finish(parent, tmp, req, mode)
	if res.Outcome != Created {
		_ = unix.Unlinkat(parent, tmp, unix.AT_REMOVEDIR)
	}
	return res
}

// existing reports on a directory already under the final name. It is never changed.
func existing(parent int, name string) (Result, bool) {
	fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	switch {
	case err == nil:
		defer unix.Close(fd)
		var st unix.Stat_t
		if err := unix.Fstat(fd, &st); err != nil {
			return fromErrno("checking the directory", err), true
		}
		res := observed(Exists, &st, "")
		if id, _, err := ProjectOf(fd); err == nil {
			res.ProjectID = id
		}
		return res, true
	case errors.Is(err, unix.ENOENT):
		return Result{}, false
	case errors.Is(err, unix.ELOOP), errors.Is(err, unix.ENOTDIR):
		return Result{Outcome: NotADirectory, Message: fmt.Sprintf("%s exists and is not a directory (a symlink or a file)", name)}, true
	case errors.Is(err, unix.EACCES):
		// Exists but this process may not open it (for example mode 0700 for another owner).
		var st unix.Stat_t
		if err := unix.Fstatat(parent, name, &st, unix.AT_SYMLINK_NOFOLLOW); err == nil && st.Mode&unix.S_IFMT == unix.S_IFDIR {
			return observed(Exists, &st, ""), true
		}
		return fromErrno("opening the directory", err), true
	default:
		return fromErrno("opening the directory", err), true
	}
}

// finish gives the temporary directory its mode and owner, checks them, and renames it into
// place without replacing anything.
func finish(parent int, tmp string, req Request, mode uint32) Result {
	fd, err := unix.Openat(parent, tmp, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fromErrno("opening the new directory", err)
	}
	defer unix.Close(fd)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return fromErrno("checking the new directory", err)
	}
	if st.Uid != 0 {
		return Result{Outcome: Failed, Message: fmt.Sprintf("the new directory is owned by uid %d, not root: is root squash on for this node?", st.Uid)}
	}
	// Root's own group first: in a setgid parent the directory inherits the parent's group, and
	// without CAP_FSETID setting setgid on a directory of a group root is not in is silently
	// dropped. Then the mode while root still owns it (no CAP_FOWNER needed), then the owner.
	if err := unix.Fchown(fd, 0, 0); err != nil {
		return fromErrno("setting the owner", err)
	}
	if req.ProjectID != 0 {
		// While root owns it: setting a project ID needs the owner (or CAP_FOWNER).
		if err := setProject(fd, req.ProjectID); err != nil {
			return fromErrno("setting the project ID (does the file system support project quotas?)", err)
		}
	}
	if err := unix.Fchmod(fd, mode); err != nil {
		return fromErrno("setting the mode", err)
	}
	if err := unix.Fchown(fd, req.UID, req.GID); err != nil {
		return fromErrno("setting the owner", err)
	}
	if err := unix.Fstat(fd, &st); err != nil {
		return fromErrno("checking the new directory", err)
	}
	if int(st.Uid) != req.UID || int(st.Gid) != req.GID || uint32(st.Mode)&permMask != mode {
		return observed(Failed, &st, fmt.Sprintf("after chown the directory is %d:%d %04o, wanted %d:%d %04o",
			st.Uid, st.Gid, uint32(st.Mode)&permMask, req.UID, req.GID, mode))
	}
	var projectID uint32
	if req.ProjectID != 0 {
		id, inherit, err := ProjectOf(fd)
		if err != nil || id != req.ProjectID || !inherit {
			return observed(Failed, &st, fmt.Sprintf("project ID is %d (inherit %t), wanted %d: %v", id, inherit, req.ProjectID, err))
		}
		projectID = id
	}
	if err := renameNoReplace(parent, tmp, req.Name); err != nil {
		if errors.Is(err, unix.EEXIST) || errors.Is(err, unix.ENOTEMPTY) {
			// Created by someone else meanwhile: report theirs, discard ours.
			if res, ok := existing(parent, req.Name); ok {
				return res
			}
		}
		return fromErrno("moving the directory into place", err)
	}
	res := observed(Created, &st, "")
	res.ProjectID = projectID
	return res
}

// tempName is the temporary name of a directory being prepared. It is derived from the final name,
// so concurrent preparers of different names in one parent never touch each other's.
func tempName(name string) string {
	sum := sha256.Sum256([]byte(name))
	return TempPrefix + hex.EncodeToString(sum[:8])
}

func observed(outcome string, st *unix.Stat_t, msg string) Result {
	return Result{Outcome: outcome, Message: msg, UID: int64(st.Uid), GID: int64(st.Gid),
		Mode: fmt.Sprintf("%04o", uint32(st.Mode)&permMask)}
}

// ParseMode parses an octal mode of permission bits and optionally setgid.
func ParseMode(s string) (uint32, error) {
	m, err := strconv.ParseUint(s, 8, 32)
	if err != nil {
		return 0, fmt.Errorf("mode %q is not octal", s)
	}
	if m&^0o2777 != 0 {
		return 0, fmt.Errorf("mode %q: only permission bits and setgid are allowed", s)
	}
	return uint32(m), nil
}

func fromErrno(what string, err error) Result {
	if errors.Is(err, unix.EACCES) || errors.Is(err, unix.EPERM) {
		return Result{Outcome: PermissionDenied, Message: fmt.Sprintf(
			"%s: %v (root squash on the preparer's node, or permissions of the parent)", what, err)}
	}
	return Result{Outcome: Failed, Message: fmt.Sprintf("%s: %v", what, err)}
}
