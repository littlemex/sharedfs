/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"crypto/tls"
	"flag"
	"os"
	"strings"
	"time"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"golang.org/x/time/rate"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	sharedfsv1alpha1 "github.com/littlemex/sharedfs/api/v1alpha1"
	"github.com/littlemex/sharedfs/internal/controller"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(sharedfsv1alpha1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

// nolint:gocyclo
func main() {
	if len(os.Args) > 1 && os.Args[1] == prepareCommand {
		os.Exit(prepareDirectory(os.Args[2:]))
	}
	if len(os.Args) > 1 && os.Args[1] == quotaCommand {
		os.Exit(setQuota(os.Args[2:]))
	}
	var metricsAddr string
	var metricsCertPath, metricsCertName, metricsCertKey string
	var enableLeaderElection bool
	var probeAddr string
	var secureMetrics bool
	var enableHTTP2 bool
	var tlsOpts []func(*tls.Config)
	var revocationGracePeriod time.Duration
	flag.DurationVar(&revocationGracePeriod, "revocation-grace-period", 15*time.Minute,
		"How long a claim must stay ungranted before its PersistentVolumeClaim is deleted. "+
			"Long enough to undo a mistaken label or grant change.")
	var revocationsPerMinute float64
	flag.Float64Var(&revocationsPerMinute, "revocations-per-minute", 30,
		"Most claims deleted by revocation per minute, cluster-wide (bursts of the same size).")
	var concurrency int
	flag.IntVar(&concurrency, "provisioner-concurrency", 4, "Claims provisioned or revoked at once.")
	var qps float64
	var burst int
	flag.Float64Var(&qps, "kube-api-qps", 50, "Queries per second to the API server.")
	flag.IntVar(&burst, "kube-api-burst", 100, "Burst of queries to the API server.")
	var quotaNamespace, quotaImage, quotaServiceAccount string
	var projectIDBase int64
	var quotaMaxPods, preparerMaxPods int
	flag.IntVar(&preparerMaxPods, "preparer-max-pods", 20, "Most preparer pods running at once.")
	var quotaRequiresRestricted bool
	flag.BoolVar(&quotaRequiresRestricted, "quota-requires-restricted", true,
		"Refuse read-write claims of shares with a quota from namespaces that do not enforce Pod Security restricted; "+
			"turn off only where root squash keeps pods from writing as root.")
	flag.IntVar(&quotaMaxPods, "quota-max-pods", 10, "Most quota pods running at once.")
	flag.StringVar(&quotaNamespace, "quota-namespace", "",
		"Namespace of quota pods, which need CAP_SYS_ADMIN; empty turns directory quotas off.")
	flag.StringVar(&quotaImage, "quota-image", "",
		"Image of quota pods: this controller's binary and lfs, pinned by digest (Dockerfile.quota).")
	flag.StringVar(&quotaServiceAccount, "quota-service-account", "sharedfs-quota",
		"Service account of quota pods; it is bound to nothing and its token is not mounted.")
	flag.Int64Var(&projectIDBase, "project-id-base", 1000000,
		"First Lustre project ID given to directories; IDs below it are left to the admin.")
	var preparerImage, preparerServiceAccount, preparerNodeSelector, preparerTolerations string
	flag.StringVar(&preparerImage, "preparer-image", "",
		"Image of preparer pods. Empty means this controller's own image, read from its pod.")
	flag.StringVar(&preparerServiceAccount, "preparer-service-account", "sharedfs-preparer",
		"Service account of preparer pods; it needs no permissions and its token is not mounted.")
	flag.StringVar(&preparerNodeSelector, "preparer-node-selector", "",
		"Comma-separated key=value node labels for preparer pods, for example nodes exempt from root squash.")
	flag.StringVar(&preparerTolerations, "preparer-tolerations", "",
		"JSON list of tolerations for preparer pods.")
	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", true,
		"Enable leader election for controller manager. "+
			"Two active managers would race on the same PersistentVolumes, so keep this on outside local runs.")
	flag.BoolVar(&secureMetrics, "metrics-secure", true,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	flag.StringVar(&metricsCertPath, "metrics-cert-path", "",
		"The directory that contains the metrics server certificate.")
	flag.StringVar(&metricsCertName, "metrics-cert-name", "tls.crt", "The name of the metrics server certificate file.")
	flag.StringVar(&metricsCertKey, "metrics-cert-key", "tls.key", "The name of the metrics server key file.")
	flag.BoolVar(&enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics server")
	opts := zap.Options{
		Development: false,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// if the enable-http2 flag is false (the default), http/2 should be disabled
	// due to its vulnerabilities. More specifically, disabling http/2 will
	// prevent from being vulnerable to the HTTP/2 Stream Cancellation and
	// Rapid Reset CVEs. For more information see:
	// - https://github.com/advisories/GHSA-qppj-fm5r-hxr3
	// - https://github.com/advisories/GHSA-4374-p667-p6c8
	disableHTTP2 := func(c *tls.Config) {
		setupLog.Info("Disabling HTTP/2")
		c.NextProtos = []string{"http/1.1"}
	}

	if !enableHTTP2 {
		tlsOpts = append(tlsOpts, disableHTTP2)
	}

	// Metrics endpoint is enabled in 'config/default/kustomization.yaml'. The Metrics options configure the server.
	// More info:
	// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.24.1/pkg/metrics/server
	// - https://book.kubebuilder.io/reference/metrics.html
	metricsServerOptions := metricsserver.Options{
		BindAddress:   metricsAddr,
		SecureServing: secureMetrics,
		TLSOpts:       tlsOpts,
	}

	if secureMetrics {
		// FilterProvider is used to protect the metrics endpoint with authn/authz.
		// These configurations ensure that only authorized users and service accounts
		// can access the metrics endpoint. The RBAC are configured in 'config/rbac/kustomization.yaml'. More info:
		// https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.24.1/pkg/metrics/filters#WithAuthenticationAndAuthorization
		metricsServerOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	// If the certificate is not specified, controller-runtime will automatically
	// generate self-signed certificates for the metrics server. While convenient for development and testing,
	// this setup is not recommended for production.
	//
	// TODO(user): If you enable certManager, uncomment the following lines:
	// - [METRICS-WITH-CERTS] at config/default/kustomization.yaml to generate and use certificates
	// managed by cert-manager for the metrics server.
	// - [PROMETHEUS-WITH-CERTS] at config/prometheus/kustomization.yaml for TLS certification.
	if len(metricsCertPath) > 0 {
		setupLog.Info("Initializing metrics certificate watcher using provided certificates",
			"metrics-cert-path", metricsCertPath, "metrics-cert-name", metricsCertName, "metrics-cert-key", metricsCertKey)

		metricsServerOptions.CertDir = metricsCertPath
		metricsServerOptions.CertName = metricsCertName
		metricsServerOptions.KeyName = metricsCertKey
	}

	// Preparer pods exist only in the controller's namespace, and the controller may only read
	// pods there.
	podNamespace := os.Getenv("POD_NAMESPACE")
	if podNamespace == "" {
		setupLog.Error(nil, "POD_NAMESPACE must name the controller's namespace")
		os.Exit(1)
	}
	restConfig := ctrl.GetConfigOrDie()
	restConfig.QPS, restConfig.Burst = float32(qps), burst
	mgr, err := ctrl.NewManager(restConfig, ctrl.Options{
		Scheme: scheme,
		Cache: cache.Options{
			ByObject: map[client.Object]cache.ByObject{
				&corev1.Pod{}: {Namespaces: podNamespaces(podNamespace, quotaNamespace)},
			},
			// Claims and volumes of the whole cluster are cached; their managed fields are not needed.
			DefaultTransform: cache.TransformStripManagedFields(),
		},
		Metrics:                metricsServerOptions,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "f5b32313.littlemex.github.io",
		// LeaderElectionReleaseOnCancel defines if the leader should step down voluntarily
		// when the Manager ends. This requires the binary to immediately end when the
		// Manager is stopped, otherwise, this setting is unsafe. Setting this significantly
		// speeds up voluntary leader transitions as the new leader don't have to wait
		// LeaseDuration time first.
		//
		// In the default scaffold provided, the program ends immediately after
		// the manager stops, so would be fine to enable this option. However,
		// if you are doing or is intended to do any operation such as perform cleanups
		// after the manager stops then its usage might be unsafe.
		// LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		setupLog.Error(err, "Failed to start manager")
		os.Exit(1)
	}

	ctx := ctrl.SetupSignalHandler()
	preparer, err := preparerConfig(ctx, mgr.GetAPIReader(), preparerImage, preparerServiceAccount, preparerNodeSelector, preparerTolerations)
	if err != nil {
		setupLog.Error(err, "Failed to configure the preparer")
		os.Exit(1)
	}
	setupLog.Info("Preparer configured", "image", preparer.Image, "namespace", preparer.Namespace)
	if err := controller.SetupIndexes(ctx, mgr); err != nil {
		setupLog.Error(err, "Failed to set up field indexes")
		os.Exit(1)
	}
	preparer.MaxPods = preparerMaxPods
	recorder := mgr.GetEventRecorder("sharedfs")
	projectIDs := &controller.ProjectIDs{Client: mgr.GetClient(), Reader: mgr.GetAPIReader(), Namespace: podNamespace, Base: projectIDBase}
	if quotaNamespace != "" && !strings.Contains(quotaImage, "@sha256:") {
		setupLog.Error(nil, "--quota-image must name an image by digest")
		os.Exit(1)
	}
	for name, r := range map[string]interface{ SetupWithManager(ctrl.Manager) error }{
		"filesystemshare": &controller.ShareReconciler{
			Client: mgr.GetClient(), APIReader: mgr.GetAPIReader(),
		},
		"provisioner": &controller.ProvisionerReconciler{
			Client: mgr.GetClient(), APIReader: mgr.GetAPIReader(), Recorder: recorder,
			RevocationGracePeriod:   revocationGracePeriod,
			RevocationLimiter:       rate.NewLimiter(rate.Limit(revocationsPerMinute/60), max(int(revocationsPerMinute), 1)),
			MaxConcurrentReconciles: concurrency,
			ProjectIDs:              projectIDs,
			QuotaRequiresRestricted: quotaRequiresRestricted,
		},
		"directory": &controller.DirectoryReconciler{Client: mgr.GetClient(), Preparer: preparer},
		"reclaim":   &controller.ReclaimReconciler{Client: mgr.GetClient()},
		"quota": &controller.QuotaReconciler{Client: mgr.GetClient(), ProjectIDs: projectIDs, Quota: controller.QuotaConfig{
			Namespace: quotaNamespace, Image: quotaImage, ServiceAccount: quotaServiceAccount,
			NodeSelector: preparer.NodeSelector, Tolerations: preparer.Tolerations,
			MaxPods: quotaMaxPods,
		}},
	} {
		if err := r.SetupWithManager(mgr); err != nil {
			setupLog.Error(err, "Failed to create controller", "controller", name)
			os.Exit(1)
		}
	}
	// +kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("Starting manager")
	if err := mgr.Start(ctx); err != nil {
		setupLog.Error(err, "Failed to run manager")
		os.Exit(1)
	}
}

// podNamespaces are the namespaces whose pods the controller reads: its own, for preparers, and
// the quota namespace.
func podNamespaces(namespaces ...string) map[string]cache.Config {
	out := map[string]cache.Config{}
	for _, n := range namespaces {
		if n != "" {
			out[n] = cache.Config{}
		}
	}
	return out
}
