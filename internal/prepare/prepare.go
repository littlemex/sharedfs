// Package prepare creates or checks one directory inside a parent directory, without following
// symlinks and without leaving a half-made directory behind. It runs inside the preparer pod,
// which mounts the parent and has only CAP_CHOWN, and reports a Result as JSON.
//
// Creation is crash-safe without rename: the directory is made with mode 0000 owned by root, so
// nobody but root can use it, then given its mode and owner through a file descriptor. A
// root-owned, mode 0000, empty directory is therefore recognised as an interrupted creation and
// completed on the next run. The parent must be owned by root and not writable by group or
// others, so no tenant can swap entries between the steps.
package prepare

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"

	"golang.org/x/sys/unix"

	"github.com/littlemex/sharedfs/internal/sharepath"
)

// Outcomes, mirrored by FilesystemShareDirectory Prepared reasons.
const (
	Created          = "DirectoryCreated"
	Exists           = "DirectoryExists"
	NotADirectory    = "NotADirectory"
	InsecureParent   = "InsecureParent"
	PermissionDenied = "PermissionDenied"
	Failed           = "PreparationFailed"
)

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
}

// Result is reported back to the controller.
type Result struct {
	Outcome string `json:"outcome"`
	Message string `json:"message,omitempty"`
	UID     int64  `json:"uid,omitempty"`
	GID     int64  `json:"gid,omitempty"`
	Mode    string `json:"mode,omitempty"`
}

// JSON encodes a result; it never fails for this type.
func (r Result) JSON() string {
	b, _ := json.Marshal(r)
	return string(b)
}

const permMask = 0o7777

// Run prepares the directory.
func Run(req Request) Result {
	mode, err := parseMode(req.Mode)
	if err != nil {
		return Result{Outcome: Failed, Message: err.Error()}
	}
	if t, err := sharepath.Parse(req.Name); err != nil || t.IsRoot() || t.PerNamespace() || t.String() != req.Name || containsSlash(req.Name) {
		return Result{Outcome: Failed, Message: fmt.Sprintf("invalid directory name %q", req.Name)}
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

	for attempt := 0; attempt < 2; attempt++ {
		fd, err := unix.Openat(parent, req.Name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		switch {
		case err == nil:
			res := finishOrReport(fd, req, mode)
			unix.Close(fd)
			return res
		case errors.Is(err, unix.ENOENT):
			// Mode 0000 and owned by root until it is complete.
			if err := unix.Mkdirat(parent, req.Name, 0); err != nil && !errors.Is(err, unix.EEXIST) {
				return fromErrno("creating the directory", err)
			}
		case errors.Is(err, unix.ELOOP), errors.Is(err, unix.ENOTDIR):
			return Result{Outcome: NotADirectory, Message: fmt.Sprintf("%s exists and is not a directory (a symlink or a file)", req.Name)}
		default:
			return fromErrno("opening the directory", err)
		}
	}
	return Result{Outcome: Failed, Message: "the directory appeared and disappeared while it was being created"}
}

// finishOrReport completes a directory this preparer created (or an interrupted earlier run
// did), or reports an existing one unchanged.
func finishOrReport(fd int, req Request, mode uint32) Result {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return fromErrno("checking the directory", err)
	}
	if !(st.Uid == 0 && uint32(st.Mode)&permMask == 0) {
		return observed(Exists, &st, "")
	}
	if empty, err := isEmpty(fd); err != nil {
		return fromErrno("listing the directory", err)
	} else if !empty {
		return observed(Exists, &st, "mode 0000 but not empty: left unchanged")
	}
	// Mode before owner: changing a root-owned directory's mode needs no capability, while
	// changing it after handing it to another uid would need CAP_FOWNER.
	if err := unix.Fchmod(fd, mode); err != nil {
		return fromErrno("setting the mode", err)
	}
	if err := unix.Fchown(fd, req.UID, req.GID); err != nil {
		return fromErrno("setting the owner", err)
	}
	if err := unix.Fstat(fd, &st); err != nil {
		return fromErrno("checking the directory", err)
	}
	if uint32(st.Mode)&permMask != mode {
		return observed(Failed, &st, fmt.Sprintf("mode is %04o after chown, wanted %04o", uint32(st.Mode)&permMask, mode))
	}
	return observed(Created, &st, "")
}

func observed(outcome string, st *unix.Stat_t, msg string) Result {
	return Result{Outcome: outcome, Message: msg, UID: int64(st.Uid), GID: int64(st.Gid),
		Mode: fmt.Sprintf("%04o", uint32(st.Mode)&permMask)}
}

func isEmpty(fd int) (bool, error) {
	dup, err := unix.Dup(fd)
	if err != nil {
		return false, err
	}
	f := os.NewFile(uintptr(dup), "dir")
	defer f.Close()
	names, err := f.Readdirnames(1)
	if errors.Is(err, io.EOF) {
		return true, nil
	}
	return len(names) == 0, err
}

func parseMode(s string) (uint32, error) {
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
			"%s: %v (is root squash on for this node? the preparer node must be exempt)", what, err)}
	}
	return Result{Outcome: Failed, Message: fmt.Sprintf("%s: %v", what, err)}
}

func containsSlash(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == '/' {
			return true
		}
	}
	return false
}
