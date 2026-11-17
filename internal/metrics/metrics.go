// Package metrics holds the controller's own Prometheus metrics, served with controller-runtime's
// on the manager's metrics endpoint. Every failure a tenant or an admin would otherwise notice
// only by a Pending claim has a series here.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	// ShareReady is 1 for the current Ready reason of each share and absent otherwise.
	ShareReady = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "sharedfs_share_ready",
		Help: "1 for each share's current Ready status and reason.",
	}, []string{"share", "status", "reason"})

	// ClaimsPending counts reports on claims sharedfs does not provision, by event reason.
	ClaimsPending = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "sharedfs_claims_pending_total",
		Help: "Reports on claims left Pending, by reason.",
	}, []string{"reason"})

	// Provisioned counts volumes created for claims.
	Provisioned = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "sharedfs_volumes_provisioned_total",
		Help: "PersistentVolumes created for claims.",
	})

	// ProvisionLatency is the time from a claim's creation to its volume's creation.
	ProvisionLatency = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "sharedfs_provision_latency_seconds",
		Help:    "Seconds from a claim's creation to the creation of its PersistentVolume.",
		Buckets: []float64{0.5, 1, 2, 5, 10, 20, 30, 60, 120, 300, 600},
	})

	// Revocations counts revocation steps: scheduled, cancelled, executed, held (no share to
	// judge by, or paused) and rate-limited.
	Revocations = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "sharedfs_revocations_total",
		Help: "Revocation steps by outcome.",
	}, []string{"outcome"})

	// TaskOutcomes counts results of preparer and quota pods by reason.
	TaskOutcomes = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "sharedfs_task_outcomes_total",
		Help: "Preparer and quota pod results by task and reason.",
	}, []string{"task", "reason"})

	// QuotaUnrestricted is the number of granted namespaces not at Pod Security restricted, per
	// read-write share of a file system that has quotas; claims there are refused, and claims
	// made before are not withdrawn.
	QuotaUnrestricted = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "sharedfs_quota_unrestricted_namespaces",
		Help: "Granted namespaces not at Pod Security restricted on file systems with quotas, per share.",
	}, []string{"share"})

	// Reclaimed counts Released volumes deleted.
	Reclaimed = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "sharedfs_volumes_reclaimed_total",
		Help: "Released PersistentVolumes deleted.",
	})
)

func init() {
	ctrlmetrics.Registry.MustRegister(ShareReady, ClaimsPending, Provisioned, ProvisionLatency,
		Revocations, TaskOutcomes, Reclaimed, QuotaUnrestricted)
}

// SetShareReady records a share's current Ready status and reason, replacing the earlier one.
func SetShareReady(share, status, reason string) {
	ShareReady.DeletePartialMatch(prometheus.Labels{"share": share})
	ShareReady.WithLabelValues(share, status, reason).Set(1)
}

// ForgetShare removes a deleted share's series.
func ForgetShare(share string) {
	ShareReady.DeletePartialMatch(prometheus.Labels{"share": share})
	QuotaUnrestricted.DeleteLabelValues(share)
}
