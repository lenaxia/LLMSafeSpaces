// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package board

import (
	"testing"
	"time"
)

// TestFailureBackoffBoundaries (#1813, the review's unit-tier gate):
// doubling per consecutive failure from the configured base, the
// zero/negative misconfiguration floor, and the 1h cap.
func TestFailureBackoffBoundaries(t *testing.T) {
	base := 30 * time.Second
	cfg := EngineConfig{FailureBackoff: base}
	cases := []struct {
		n    int
		want time.Duration
	}{
		{1, 30 * time.Second},
		{2, time.Minute},
		{3, 2 * time.Minute},
		{8, 1 * time.Hour}, // 30s<<7 = 64m caps at 1h
		{100, 1 * time.Hour},
	}
	for _, c := range cases {
		if got := cfg.failureBackoff(c.n); got != c.want {
			t.Errorf("failureBackoff(%d) with base %s = %s, want %s", c.n, base, got, c.want)
		}
	}
	// Zero/negative base: the documented default floor (1m), doubling
	// and the cap still apply.
	zero := EngineConfig{FailureBackoff: 0}
	if got := zero.failureBackoff(1); got != time.Minute {
		t.Errorf("failureBackoff(1) zero base = %s, want 1m", got)
	}
	if got := zero.failureBackoff(2); got != 2*time.Minute {
		t.Errorf("failureBackoff(2) zero base = %s, want 2m", got)
	}
	neg := EngineConfig{FailureBackoff: -5 * time.Second}
	if got := neg.failureBackoff(1); got != time.Minute {
		t.Errorf("failureBackoff(1) negative base = %s, want 1m", got)
	}
	// n<1 clamps to 1.
	if got := cfg.failureBackoff(0); got != base {
		t.Errorf("failureBackoff(0) = %s, want base %s", got, base)
	}
}
