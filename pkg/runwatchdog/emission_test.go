package runwatchdog

import (
	"testing"
	"time"
)

// TestWorkflowEmissionNormalize pins the ergonomic-field conversion:
// minutes override MinAge, zero minutes preserve an explicit MinAge
// (in-process callers), zero Now defaults to now-UTC.
func TestWorkflowEmissionNormalize(t *testing.T) {
	e := WorkflowEmission{MinAgeMinutes: 15}
	s := e.Normalize()
	if s.MinAge != 15*time.Minute {
		t.Errorf("min_age_minutes=15 must become MinAge=15m, got %s", s.MinAge)
	}
	if s.Now.IsZero() {
		t.Error("zero Now must default to now-UTC (hand-driven probes omit it)")
	}

	e = WorkflowEmission{PRState: PRState{MinAge: 7 * time.Minute, Now: now}}
	if got := e.Normalize().MinAge; got != 7*time.Minute {
		t.Errorf("zero min_age_minutes must preserve an explicit MinAge (in-process override), got %s", got)
	}
}
