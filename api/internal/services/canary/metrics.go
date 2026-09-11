// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package canary

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	probeOutcomes = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "llmsafespaces_canary_probe_outcomes_total",
		Help: "Canary probe outcomes per workspace class and leg (epic-71 / 0c, #1312 metrics-only). resolve/not_found_error = S6 violation (absence did not resolve); */timeout = wedge signal; no_target/pick_error/unresolved_target = targeting failures, not pod verdicts. Alerts stay gated until L3/L4 are green.",
	}, []string{"class", "leg", "outcome"})

	probeDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "llmsafespaces_canary_probe_duration_seconds",
		Help:    "Canary probe attempt duration per class and leg, success and failure alike (the #1312 convergence-time surface; L-bound assertions arrive with the alerts wave).",
		Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5},
	}, []string{"class", "leg"})
)
