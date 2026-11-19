// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

import (
	"github.com/prometheus/client_golang/prometheus"
)

// bind_metrics.go — the API tier's CRD-skew observability (#2092
// ruling 2): the bind-not-persisted verdict (previously a named error
// returned to one caller at a time) promoted to a counter the
// LLMSafeSpacesHarnessBindNotPersisted alert can ride. Registered on
// the DEFAULT registry — the API's authenticated /metrics endpoint
// (server/router.go) serves it.
//
// Fatigue posture (the #2047 alert-lane discipline): the counter ticks
// once per failed-bind EPISODE-ATTEMPT (one per bind call that verified
// un-persisted), the alert gates on increase() over a 15m window held
// for 5m, and it self-resolves the moment binds stop failing — after
// the CRD lands the .16 verify passes and the rate drops to zero. It is
// the user-impact echo of the controller's llmsafespaces_crd_skew
// gauge (which pages); this one explains WHO is being blocked.
var harnessBindNotPersistedTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "llmsafespaces_harness_bind_not_persisted_total",
		Help: "Harness bind attempts whose verified persistence FAILED (spec.harness read back empty after an accepted write — the cluster's Workspace CRD predates 0.44.15; issue #2092), by surface. The bind API + first-session bind paths increment bind_verify; the create-time capability probe's fail-loud refusal increments create_probe",
	},
	[]string{"surface"},
)

// The label values (exported for tests).
const (
	bindNotPersistedSurfaceBindVerify  = "bind_verify"
	bindNotPersistedSurfaceCreateProbe = "create_probe"
)

func init() {
	prometheus.MustRegister(harnessBindNotPersistedTotal)
}

// countBindNotPersisted increments the counter by surface.
func countBindNotPersisted(surface string) {
	harnessBindNotPersistedTotal.WithLabelValues(surface).Inc()
}
