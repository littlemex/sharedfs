// Package quota sets the Lustre project quota of one directory. It runs in the quota pod, which
// mounts only that directory read-write and has only CAP_SYS_ADMIN (for the quotactl), runs as
// root with the share's group, and calls lfs, which the quota image carries. The directory must
// be readable by root as its owner or by that group. The directory must already carry its project ID: a directory sharedfs did
// not create is never tagged here, because tagging what is already in it would change files the
// controller did not create.
package quota

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	sharedfsv1alpha1 "github.com/littlemex/sharedfs/api/v1alpha1"
	"github.com/littlemex/sharedfs/internal/prepare"
)

// Outcomes, mirrored by FilesystemShareDirectory QuotaApplied reasons.
const (
	Applied   = sharedfsv1alpha1.ReasonQuotaApplied
	NotTagged = sharedfsv1alpha1.ReasonDirectoryNotTagged
	InUse     = sharedfsv1alpha1.ReasonProjectInUse
	Failed    = sharedfsv1alpha1.ReasonQuotaFailed
)

// Outcomes lists every outcome a quota pod may report.
var Outcomes = []string{Applied, NotTagged, InUse, Failed}

// Request is one directory's limits. Zero means no limit; both zero clears the limits.
type Request struct {
	Path       string
	ProjectID  uint32
	StorageKiB int64
	Inodes     int64
	// FirstApply asks for a check that the project counts nothing but the directory itself, so an
	// ID that some other directory already uses is never taken over.
	FirstApply bool
}

// Result is reported back to the controller, with the limits read back after setting them.
type Result struct {
	Outcome    string `json:"outcome"`
	Message    string `json:"message,omitempty"`
	StorageKiB int64  `json:"storageKiB,omitempty"`
	Inodes     int64  `json:"inodes,omitempty"`
}

// JSON encodes a result.
func (r Result) JSON() string {
	b, _ := json.Marshal(r)
	return string(b)
}

// Run checks the directory's project ID and sets its limits.
func Run(ctx context.Context, req Request) Result {
	if req.ProjectID == 0 || req.StorageKiB < 0 || req.Inodes < 0 {
		return Result{Outcome: Failed, Message: "a project ID and non-negative limits are required"}
	}
	fd, err := unix.Open(req.Path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return Result{Outcome: Failed, Message: fmt.Sprintf(
			"opening the directory: %v (the quota pod runs as root with the share's group: the directory must be owned by root or give that group read and search)", err)}
	}
	id, inherit, err := prepare.ProjectOf(fd)
	unix.Close(fd)
	if err != nil {
		return Result{Outcome: Failed, Message: fmt.Sprintf("reading the project ID: %v", err)}
	}
	clearing := req.StorageKiB == 0 && req.Inodes == 0
	if !clearing && (id != req.ProjectID || !inherit) {
		return Result{Outcome: NotTagged, Message: fmt.Sprintf(
			"the directory has project ID %d (inherit %t), not %d: it existed before sharedfs; an admin can tag it with "+
				"`lfs project -p %d -r -s <dir>` and then the limit applies", id, inherit, req.ProjectID, req.ProjectID)}
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	p := strconv.FormatUint(uint64(req.ProjectID), 10)
	if req.FirstApply {
		out, err := exec.CommandContext(ctx, "lfs", "quota", "-p", p, req.Path).CombinedOutput()
		if err != nil {
			return Result{Outcome: Failed, Message: fmt.Sprintf("lfs quota: %v: %s", err, strings.TrimSpace(string(out)))}
		}
		if files, ok := usedFiles(string(out)); !ok || files > 1 {
			return Result{Outcome: InUse, Message: fmt.Sprintf(
				"project %d already counts %d files: another directory uses this ID (another cluster on the same file system, or a record that was deleted); give each cluster its own --project-id-base", req.ProjectID, files)}
		}
	}
	out, err := exec.CommandContext(ctx, "lfs", "setquota", "-p", p,
		"-B", strconv.FormatInt(req.StorageKiB, 10)+"k", "-I", strconv.FormatInt(req.Inodes, 10), req.Path).CombinedOutput()
	if err != nil {
		return Result{Outcome: Failed, Message: fmt.Sprintf("lfs setquota: %v: %s", err, strings.TrimSpace(string(out)))}
	}
	out, err = exec.CommandContext(ctx, "lfs", "quota", "-p", p, req.Path).CombinedOutput()
	if err != nil {
		return Result{Outcome: Failed, Message: fmt.Sprintf("lfs quota: %v: %s", err, strings.TrimSpace(string(out)))}
	}
	got, ok := limits(string(out))
	if !ok || got[0] != req.StorageKiB || got[1] != req.Inodes {
		return Result{Outcome: Failed, Message: fmt.Sprintf("limits read back as %v, wanted [%d %d]: %s", got, req.StorageKiB, req.Inodes, strings.TrimSpace(string(out)))}
	}
	return Result{Outcome: Applied, StorageKiB: got[0], Inodes: got[1]}
}

// usedFiles parses the files in use from `lfs quota -p` output.
func usedFiles(out string) (int64, bool) {
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 9 || !strings.HasPrefix(f[0], "/") {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimSuffix(f[5], "*"), 10, 64)
		return n, err == nil
	}
	return 0, false
}

// limits parses the block and inode hard limits from `lfs quota -p` output: the data line has
// path, kbytes, quota, limit, grace, files, quota, limit, grace.
func limits(out string) ([2]int64, bool) {
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 9 || !strings.HasPrefix(f[0], "/") {
			continue
		}
		b, err1 := strconv.ParseInt(strings.TrimSuffix(f[3], "*"), 10, 64)
		i, err2 := strconv.ParseInt(strings.TrimSuffix(f[7], "*"), 10, 64)
		if err1 == nil && err2 == nil {
			return [2]int64{b, i}, true
		}
	}
	return [2]int64{}, false
}
