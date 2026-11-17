package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/littlemex/sharedfs/internal/controller"
	"github.com/littlemex/sharedfs/internal/prepare"
	"github.com/littlemex/sharedfs/internal/quota"
)

const (
	prepareCommand = "prepare-directory"
	quotaCommand   = "set-quota"
	// terminationLog is where kubelet reads the container's termination message.
	terminationLog = "/dev/termination-log"
)

// prepareDirectory is the preparer pod's entry point. It always exits 0 once it has a result:
// the controller decides from the JSON report, never from the exit code.
func prepareDirectory(args []string) int {
	fs := flag.NewFlagSet(prepareCommand, flag.ContinueOnError)
	var req prepare.Request
	fs.StringVar(&req.Root, "root", "", "mounted parent directory")
	fs.StringVar(&req.Name, "name", "", "directory to create in root: one path segment")
	fs.IntVar(&req.UID, "uid", 0, "owner uid")
	fs.IntVar(&req.GID, "gid", -1, "owner gid")
	fs.StringVar(&req.Mode, "mode", "", "octal mode, for example 2770")
	projectID := fs.Uint("project-id", 0, "Lustre project ID for a created directory, 0 for none")
	out := fs.String("report", terminationLog, "file the JSON result is written to")
	var res prepare.Result
	if err := fs.Parse(args); err != nil || req.Root == "" || req.Name == "" || req.GID < 0 || req.UID < 0 || req.Mode == "" {
		res = prepare.Result{Outcome: prepare.Failed, Message: fmt.Sprintf("bad arguments %q", args)}
	} else {
		req.ProjectID = uint32(*projectID)
		res = prepare.Run(req)
	}
	report := res.JSON()
	fmt.Println(report)
	if err := os.WriteFile(*out, []byte(report), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "writing the report:", err)
		return 1
	}
	return 0
}

// ownImage is the manager container's image, pinned to the digest it runs when the runtime
// reports one, so a moving tag cannot hand the preparer different code.
func ownImage(pod *corev1.Pod) string {
	var image string
	for _, c := range pod.Spec.Containers {
		if c.Name == "manager" {
			image = c.Image
		}
	}
	if image == "" {
		return ""
	}
	for _, s := range pod.Status.ContainerStatuses {
		if s.Name != "manager" {
			continue
		}
		if _, digest, ok := strings.Cut(s.ImageID, "@sha256:"); ok {
			repo := image
			if i := strings.Index(repo, "@"); i >= 0 {
				repo = repo[:i]
			} else if i := strings.LastIndex(repo, ":"); i > strings.LastIndex(repo, "/") {
				repo = repo[:i]
			}
			return repo + "@sha256:" + digest
		}
	}
	return image
}

// setQuota is the quota pod's entry point; like the preparer it reports JSON and exits 0.
func setQuota(args []string) int {
	fs := flag.NewFlagSet(quotaCommand, flag.ContinueOnError)
	var req quota.Request
	fs.StringVar(&req.Path, "path", "", "the mounted directory")
	id := fs.Uint("project-id", 0, "the directory's project ID")
	fs.Int64Var(&req.StorageKiB, "storage-kib", 0, "hard limit of data in KiB, 0 for none")
	fs.Int64Var(&req.Inodes, "inodes", 0, "hard limit of inodes, 0 for none")
	out := fs.String("report", terminationLog, "file the JSON result is written to")
	var res quota.Result
	if err := fs.Parse(args); err != nil || req.Path == "" {
		res = quota.Result{Outcome: quota.Failed, Message: fmt.Sprintf("bad arguments %q", args)}
	} else {
		req.ProjectID = uint32(*id)
		res = quota.Run(context.Background(), req)
	}
	report := res.JSON()
	fmt.Println(report)
	if err := os.WriteFile(*out, []byte(report), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "writing the report:", err)
		return 1
	}
	return 0
}

// preparerConfig resolves where preparer pods run. The image defaults to the controller's own,
// read from its pod and pinned to the digest it runs: a preparer never runs code other than the
// controller's.
func preparerConfig(ctx context.Context, c client.Reader, image, sa, nodeSelector, tolerations string) (controller.PreparerConfig, error) {
	cfg := controller.PreparerConfig{
		Namespace:      os.Getenv("POD_NAMESPACE"),
		Image:          image,
		ServiceAccount: sa,
	}
	if cfg.Namespace == "" {
		return cfg, errors.New("POD_NAMESPACE is not set")
	}
	if cfg.Image == "" {
		name := os.Getenv("POD_NAME")
		if name == "" {
			return cfg, errors.New("set --preparer-image, or POD_NAME so the controller can read its own image")
		}
		// The kubelet reports the running image's digest shortly after the container starts.
		var pod corev1.Pod
		for i := 0; ; i++ {
			if err := c.Get(ctx, types.NamespacedName{Namespace: cfg.Namespace, Name: name}, &pod); err != nil {
				return cfg, fmt.Errorf("reading own pod for the preparer image: %w", err)
			}
			cfg.Image = ownImage(&pod)
			if strings.Contains(cfg.Image, "@sha256:") {
				break
			}
			if i == 60 {
				return cfg, errors.New("own pod reports no image digest; set --preparer-image to an image by digest")
			}
			select {
			case <-ctx.Done():
				return cfg, ctx.Err()
			case <-time.After(time.Second):
			}
		}
		if cfg.Image == "" {
			return cfg, errors.New("own pod has no container named manager")
		}
		cfg.ImagePullSecrets = pod.Spec.ImagePullSecrets
	}
	if nodeSelector != "" {
		cfg.NodeSelector = map[string]string{}
		for _, kv := range strings.Split(nodeSelector, ",") {
			k, v, ok := strings.Cut(kv, "=")
			if !ok || k == "" {
				return cfg, fmt.Errorf("--preparer-node-selector: %q is not key=value", kv)
			}
			cfg.NodeSelector[k] = v
		}
	}
	if tolerations != "" {
		if err := json.Unmarshal([]byte(tolerations), &cfg.Tolerations); err != nil {
			return cfg, fmt.Errorf("--preparer-tolerations: %w", err)
		}
	}
	return cfg, nil
}
