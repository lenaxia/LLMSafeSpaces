package workspace

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestCalculateBackoff_FirstAttempt(t *testing.T) {
	policy := recoveryPolicies[FailureClassInfrastructure]
	backoff := calculateBackoff(1, policy)
	assert.Equal(t, 5*time.Second, backoff)
}

func TestCalculateBackoff_SecondAttempt(t *testing.T) {
	policy := recoveryPolicies[FailureClassInfrastructure]
	backoff := calculateBackoff(2, policy)
	assert.Equal(t, 10*time.Second, backoff)
}

func TestCalculateBackoff_CapsAtMax(t *testing.T) {
	policy := recoveryPolicies[FailureClassInfrastructure]
	backoff := calculateBackoff(10, policy)
	assert.Equal(t, policy.BackoffMax, backoff)
}

func TestCalculateBackoff_ZeroFailures(t *testing.T) {
	policy := recoveryPolicies[FailureClassInfrastructure]
	backoff := calculateBackoff(0, policy)
	assert.Equal(t, time.Duration(0), backoff)
}

func TestCalculateBackoff_HighFailureCount_NoOverflow(t *testing.T) {
	// F39: ConsecutiveFailures=100 → BackoffMax, no negative duration
	policy := recoveryPolicies[FailureClassProcess]
	backoff := calculateBackoff(100, policy)
	assert.Equal(t, policy.BackoffMax, backoff)
	assert.True(t, backoff > 0, "backoff must be positive")
}

func TestCalculateBackoff_ShiftCappedAt30(t *testing.T) {
	policy := RecoveryPolicy{
		BackoffBase: 1 * time.Second,
		BackoffMax:  1 * time.Hour,
	}
	// failures=32 → shift=31 → capped at 30
	backoff := calculateBackoff(32, policy)
	assert.True(t, backoff > 0)
	assert.True(t, backoff <= policy.BackoffMax)
}

// TestRecoveryPolicy_EveryClassHasNonZeroExhaustionThreshold is the
// regression pin for the #760 Longhorn silent-loop: the one class that
// most needs the escalation signal (Infrastructure — kubelet/PVC/CSI
// failures that never self-heal) previously had threshold 0 and could
// never trip it. Zero thresholds are a silent-loop bug, not a feature.
func TestRecoveryPolicy_EveryClassHasNonZeroExhaustionThreshold(t *testing.T) {
	for class, policy := range recoveryPolicies {
		assert.Greater(t, policy.ExhaustionAfter, int32(0),
			"class %s must have a non-zero exhaustion threshold (#760)", class)
	}
}

func TestRecoveryPolicy_ExhaustionThresholds(t *testing.T) {
	// Resource/Process/Configuration reuse the pre-#760 SafeModeAfter
	// defaults; Infrastructure=10 is the new non-zero value (the original
	// fix-branch proposal: infra backoff 5s→2m means 10 failures span
	// ~12 minutes of retries before escalating).
	assert.Equal(t, int32(10), recoveryPolicies[FailureClassInfrastructure].ExhaustionAfter)
	assert.Equal(t, int32(6), recoveryPolicies[FailureClassResource].ExhaustionAfter)
	assert.Equal(t, int32(6), recoveryPolicies[FailureClassProcess].ExhaustionAfter)
	assert.Equal(t, int32(3), recoveryPolicies[FailureClassConfiguration].ExhaustionAfter)
}

func TestRecoveryExhausted_ThresholdBoundaries(t *testing.T) {
	tests := []struct {
		class     FailureClass
		below     int32
		threshold int32
	}{
		{FailureClassInfrastructure, 9, 10},
		{FailureClassResource, 5, 6},
		{FailureClassProcess, 5, 6},
		{FailureClassConfiguration, 2, 3},
	}
	for _, tt := range tests {
		t.Run(string(tt.class), func(t *testing.T) {
			policy := recoveryPolicies[tt.class]
			assert.False(t, recoveryExhausted(tt.below, policy),
				"%s at %d (below threshold %d) must not be exhausted", tt.class, tt.below, tt.threshold)
			assert.True(t, recoveryExhausted(tt.threshold, policy),
				"%s at %d (== threshold %d) must be exhausted", tt.class, tt.threshold, tt.threshold)
			assert.True(t, recoveryExhausted(tt.threshold+5, policy),
				"%s beyond threshold must stay exhausted", tt.class)
		})
	}
}

// TestRecoveryExhausted_InfrastructureEscalates is the #760 incident
// pin: an infrastructure failure loop (the Longhorn silent-loop class)
// must eventually cross the exhaustion threshold and produce a signal.
func TestRecoveryExhausted_InfrastructureEscalates(t *testing.T) {
	policy := recoveryPolicies[FailureClassInfrastructure]
	for n := int32(1); n < policy.ExhaustionAfter; n++ {
		assert.False(t, recoveryExhausted(n, policy), "infra failure %d below threshold", n)
	}
	assert.True(t, recoveryExhausted(policy.ExhaustionAfter, policy),
		"infra failures at %d must trip exhaustion — the Longhorn silent-loop fix", policy.ExhaustionAfter)
}

func TestRecoveryExhausted_ZeroThresholdDisabled(t *testing.T) {
	policy := RecoveryPolicy{ExhaustionAfter: 0}
	assert.False(t, recoveryExhausted(1000, policy),
		"a zero threshold must disable escalation for that class")
}
