package handlers

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/agent/systemnotices"
)

// TestComposedAdapterSatisfiesDeliveryVerifier is the wiring-level guard
// for the 2026-08-29 outage: production adapters are COMPOSED
// (systemnotices.Wrap at app wiring), and the handlers' deliveryVerifier
// type assertion failed silently against the wrapper for hours — every
// verification returned inconclusive without touching the network, and
// all V2 delivery parked as "delivery unverifiable". Every unit test
// passed because the test environment wired the RAW adapter.
//
// This test composes the same wrapper stack and asserts the seam the
// production code actually asserts. If VerifyDelivery leaves the
// agent.Adapter interface, or the wrapper stops embedding it, this fails
// at exactly the point production would break.
func TestComposedAdapterSatisfiesDeliveryVerifier(t *testing.T) {
	composed := systemnotices.Wrap(&mockAdapter{}, nil) // agent.Adapter-typed by Wrap's signature
	v, ok := composed.(deliveryVerifier)
	require.True(t, ok,
		"the PRODUCTION adapter composition must satisfy deliveryVerifier — this exact assertion failed silently on 2026-08-29 and broke all V2 delivery")
	// Inconclusive-shape call must succeed (nil usage is never touched by
	// verification; the wrapper must pass through untouched).
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	delivered, definitive, err := v.VerifyDelivery(ctx, "u", "ws", "ses", "x", time.Now())
	require.NoError(t, err)
	assert.False(t, delivered)
	assert.False(t, definitive)

	// Discipline (2026-08-29 postmortem): EVERY method the handlers call
	// through seam assertions or with wrapping-sensitive semantics gets a
	// pass-through assertion HERE, on the composed production stack —
	// unit tests see the raw adapter and cannot catch wrapper gaps.
	// SetSessionModel (R4): nil is a documented no-op; the wrapper must
	// pass it through untouched rather than panic or swallow.
	require.NoError(t, composed.SetSessionModel(ctx, "u", "ws", "ses", nil))
}
