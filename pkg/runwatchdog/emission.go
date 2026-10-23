package runwatchdog

import "time"

// WorkflowEmission is the JSON document .github/workflows/run-watchdog.yml
// builds with jq -n and pipes to cmd/runwatchdog: PRState plus the
// ergonomic min_age_minutes field (nanosecond durations are natural in
// Go and hostile in jq). It lives here — not in package main — so the
// workflow↔binary contract is an importable, testable type
// (tests/ghaworkflows TestRunWatchdogWiringContract reflects over it).
//
// Contract notes pinned by tests:
//   - min_age_minutes is the ONLY key outside PRState's tags;
//   - PRState.min_age is NEVER emitted (in-process override only —
//     the workflow speaks minutes); its zero value is safe (Evaluate
//     falls back to DefaultMinAge);
//   - now may be omitted by hand-driven probes; Normalize defaults it.
type WorkflowEmission struct {
	PRState
	MinAgeMinutes int `json:"min_age_minutes,omitempty"`
}

// Normalize applies the ergonomic fields onto the embedded PRState and
// returns it ready for Evaluate: min_age_minutes (when > 0) overrides
// MinAge; a zero Now defaults to the current UTC time.
func (e WorkflowEmission) Normalize() PRState {
	s := e.PRState
	if e.MinAgeMinutes > 0 {
		s.MinAge = time.Duration(e.MinAgeMinutes) * time.Minute
	}
	if s.Now.IsZero() {
		s.Now = time.Now().UTC()
	}
	return s
}
