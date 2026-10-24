// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// #1919 red pins — the injected-origin sentinel must carry the
// PLATFORM phrase, never the raw harness id, when the replica knows
// the mapping. The send_message result fields already phrase-emit via
// emitID/emitIDFields (#1756 §1); the sentinel stamped at the Compose
// call still carried the resolved CANONICAL raw id — the last
// un-migrated id emission in the injected-origin flow, delivered into
// the recipient's transcript and rendered verbatim by the frontend
// AgentOriginBadge.
//
// These pins are born RED on main: mcpSendMessage stamps
// FromSession: origin (canonical ses_…) regardless of replica warmth,
// so the phrase-shape assertion fails while the raw form succeeds.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/session/agentmessage"
	"github.com/lenaxia/llmsafespaces/pkg/shortid"
)

// deliveredSentinelText waits for the detached delivery to land on the
// target and returns the first text part (the sentinel-stamped
// payload).
func deliveredSentinelText(t *testing.T, f *fakeAgent, target string) string {
	t.Helper()
	var delivered string
	require.Eventually(t, func() bool {
		bodies := f.sentFor(target)
		if len(bodies) == 0 {
			return false
		}
		parts, ok := bodies[0]["parts"].([]any)
		if !ok || len(parts) == 0 {
			return false
		}
		text, ok := parts[0].(map[string]any)["text"].(string)
		if !ok {
			return false
		}
		delivered = text
		return strings.HasPrefix(delivered, "<!-- lsp:agent-message-v1 ")
	}, 5*time.Second, 25*time.Millisecond, "the sentineled delivery must land")
	return delivered
}

// TestSendMessage_OriginSentinelCarriesPhrase_WarmReplica — the #1919
// pin: with the replica WARM (the caller's phrase known), the delivered
// sentinel's fromSession is the PHRASE form. The raw ses_ shape FAILS
// this pin by construction (the second assertion).
func TestSendMessage_OriginSentinelCarriesPhrase_WarmReplica(t *testing.T) {
	f := newFakeAgent()
	target := f.newSession("target")
	caller := f.newSession("caller")
	store := withShortIDStore(t)
	const phrase = "amber-falcon-glides"
	store.put(phrase, caller)
	withAgentServer(t, f.handler(t))

	out, err := mcpSendMessage(context.Background(), mcpTestPassword, target, "status report", caller, "")
	require.NoError(t, err)
	assert.Contains(t, out, `"origin":"`+phrase+`"`,
		"the result's origin field already phrase-emits (#1756); the sentinel must match")

	delivered := deliveredSentinelText(t, f, target)
	origin, found, _ := agentmessage.Parse(delivered)
	require.True(t, found, "the delivery must open with a parseable v1 sentinel: %q", delivered)

	assert.True(t, shortid.IsShortIDForm(origin.FromSession),
		"#1919: the sentinel's fromSession must carry the short-id (phrase) shape, got %q", origin.FromSession)
	assert.False(t, shortid.IsLegacyIDForm(origin.FromSession),
		"#1919: the sentinel's fromSession must NOT be the raw harness id (ses_…) — the phrase machinery retires that emission")
	assert.Equal(t, phrase, origin.FromSession,
		"the sentinel's fromSession is the replica-known phrase for the caller")
	assert.Equal(t, agentmessage.ModeInjected, origin.Mode)
}

// TestSendMessage_OriginSentinelRawOnlyWhenReplicaCold — the degraded
// window contract (#1756 §1 parity): replica COLD (no mapping known)
// keeps today's raw fallback so the origin stays attributable —
// identical to every other id emission. Born green on main AND on the
// fix; pins that the #1919 migration never loses attribution.
func TestSendMessage_OriginSentinelRawOnlyWhenReplicaCold(t *testing.T) {
	f := newFakeAgent()
	target := f.newSession("target")
	caller := f.newSession("caller")
	withShortIDStore(t)
	withAgentServer(t, f.handler(t))

	_, err := mcpSendMessage(context.Background(), mcpTestPassword, target, "status report", caller, "")
	require.NoError(t, err)

	delivered := deliveredSentinelText(t, f, target)
	origin, found, _ := agentmessage.Parse(delivered)
	require.True(t, found, "cold-replica delivery still carries the sentinel: %q", delivered)
	assert.Equal(t, caller, origin.FromSession,
		"degraded window: the raw id is the only addressable form — the fallback is the #1756 §1 contract")
}
