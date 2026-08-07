// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workflows

// Epic 64: SSE events + Prometheus metrics for workflow runs.
//
// Publishes workflow.run_started/node_finished/run_finished + trigger.fired
// events via the existing eventbroker. Registers Prometheus counters/histograms
// for workflow execution observability.

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	workflowRunsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "workflow_runs_total",
		Help: "Total workflow runs by status and error_code.",
	}, []string{"status", "error_code", "owner_type"})

	_ = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "workflow_run_duration_seconds",
		Help:    "Workflow run duration in seconds.",
		Buckets: prometheus.DefBuckets,
	}, []string{"workflow_id"})

	workflowNodeDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "workflow_node_duration_seconds",
		Help:    "Per-node execution duration in seconds.",
		Buckets: prometheus.DefBuckets,
	}, []string{"node_type"})

	workflowConcurrentRuns = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "workflow_concurrent_runs",
		Help: "Number of in-flight workflow runs.",
	})

	triggerFiresTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "trigger_fires_total",
		Help: "Total trigger fires by source and status.",
	}, []string{"source", "status"})

	webhookDeliveriesTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "webhook_deliveries_total",
		Help: "Total webhook deliveries by webhook_id and status.",
	}, []string{"webhook_id", "status"})

	schedulerTickDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "workflow_scheduler_tick_duration_seconds",
		Help:    "Scheduler tick duration in seconds.",
		Buckets: prometheus.DefBuckets,
	})
)

// RecordRunStarted increments the concurrent gauge.
func RecordRunStarted() {
	workflowConcurrentRuns.Inc()
}

// RecordRunFinished records a run's terminal status + duration.
func RecordRunFinished(status, errorCode, ownerType string, durationSeconds float64) {
	if errorCode == "" {
		errorCode = "none"
	}
	workflowRunsTotal.WithLabelValues(status, errorCode, ownerType).Inc()
	workflowConcurrentRuns.Dec()
}

// RecordNodeDuration records per-node execution timing.
func RecordNodeDuration(nodeType string, durationSeconds float64) {
	workflowNodeDuration.WithLabelValues(nodeType).Observe(durationSeconds)
}

// RecordTriggerFire records a trigger fire event.
func RecordTriggerFire(source, status string) {
	triggerFiresTotal.WithLabelValues(source, status).Inc()
}

// RecordWebhookDelivery records a webhook delivery.
func RecordWebhookDelivery(webhookID, status string) {
	webhookDeliveriesTotal.WithLabelValues(webhookID, status).Inc()
}

// RecordSchedulerTick records scheduler tick timing.
func RecordSchedulerTick(durationSeconds float64) {
	schedulerTickDuration.Observe(durationSeconds)
}
