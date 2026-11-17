// Package quota sets the Lustre project quota of one directory. It runs in the quota pod, which
// mounts only that directory read-write and has only CAP_SYS_ADMIN, and calls lfs, which the
// quota image carries. The directory must already carry its project ID: a directory sharedfs did
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
	Failed    = sharedfsv1alpha1.ReasonQuotaFailed
)

// Outcomes lists every outcome a quota pod may report.
var Outcomes = []string{Applied, NotTagged, Failed}

// Request is one directory's limits. Zero means no limit.
type Request struct {
	Path       string
	ProjectID  uint32
	StorageKiB int64
	Inodes     int64
}

// Result is reported back to the controller.
type Result struct {
	Outcome string `json:"outcome"`
	Message string `json:"message,omitempty"`
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
		return Result{Outcome: Failed, Message: fmt.Sprintf("opening the directory: %v", err)}
	}
	id, inherit, err := prepare.ProjectOf(fd)
	unix.Close(fd)
	if err != nil {
		return Result{Outcome: Failed, Message: fmt.Sprintf("reading the project ID: %v", err)}
	}
	if id != req.ProjectID || !inherit {
		return Result{Outcome: NotTagged, Message: fmt.Sprintf(
			"the directory has project ID %d (inherit %t), not %d: it existed before sharedfs; an admin can tag it with "+
				"`lfs project -p %d -r -s <dir>` and then the limit applies", id, inherit, req.ProjectID, req.ProjectID)}
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	p := strconv.FormatUint(uint64(req.ProjectID), 10)
	out, err := exec.CommandContext(ctx, "lfs", "setquota", "-p", p,
		"-B", strconv.FormatInt(req.StorageKiB, 10)+"k", "-I", strconv.FormatInt(req.Inodes, 10), req.Path).CombinedOutput()
	if err != nil {
		return Result{Outcome: Failed, Message: fmt.Sprintf("lfs setquota: %v: %s", err, strings.TrimSpace(string(out)))}
	}
	out, err = exec.CommandContext(ctx, "lfs", "quota", "-p", p, req.Path).CombinedOutput()
	if err != nil {
		return Result{Outcome: Failed, Message: fmt.Sprintf("lfs quota: %v: %s", err, strings.TrimSpace(string(out)))}
	}
	if got, ok := limits(string(out)); !ok || got[0] != req.StorageKiB || got[1] != req.Inodes {
		return Result{Outcome: Failed, Message: fmt.Sprintf("limits read back as %v, wanted [%d %d]: %s", got, req.StorageKiB, req.Inodes, strings.TrimSpace(string(out)))}
	}
	return Result{Outcome: Applied}
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
