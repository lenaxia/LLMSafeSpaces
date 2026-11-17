// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package agent

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// transcripthygiene_test.go — the #2030a contract pins: the key set,
// the strip helper, and the LOUD construction-seam failure for an
// adapter that does not provide the contract (pin (e): no silent
// per-harness gaps).

func TestIdentityStampKeys_ExactSet(t *testing.T) {
	// The lockstep set: platform-namespace stamp + both legacy
	// spellings — mirrored accept-side by boardWireBody
	// (cmd/workspace-agentd/board_manager_tools.go) and
	// boardIdentityInlineKeys. A key added here must land there too.
	assert.Equal(t, []string{"lsp_injected_session", "sessionId", "session_id"}, IdentityStampKeys)
}

func TestIsIdentityStampKey(t *testing.T) {
	for _, k := range IdentityStampKeys {
		assert.True(t, IsIdentityStampKey(k), k)
	}
	for _, k := range []string{"", "session", "sessionID", "lsp_injected", "origin", "destination"} {
		assert.False(t, IsIdentityStampKey(k), k)
	}
}

func TestStripIdentityStampKeys(t *testing.T) {
	args := map[string]any{
		"lsp_injected_session": "ses_origin",
		"sessionId":            "ses_legacy_camel",
		"session_id":           "ses_legacy_snake",
		"destination":          "sacred-yacht-dines",
		"message":              "payload text",
	}
	changed := StripIdentityStampKeys(args)
	assert.True(t, changed, "stamped args must report changed=true")
	assert.Equal(t, map[string]any{"destination": "sacred-yacht-dines", "message": "payload text"}, args,
		"exactly the three identity keys disappear; semantic params survive")

	clean := map[string]any{"projectId": "prj_1", "bodyFile": "plan.json"}
	assert.False(t, StripIdentityStampKeys(clean), "clean args must report changed=false")
	assert.Len(t, clean, 2)

	assert.False(t, StripIdentityStampKeys(nil), "nil args are a clean no-op")
	assert.False(t, StripIdentityStampKeys(map[string]any{}), "empty args are a clean no-op")
}

func TestTranscriptHygieneFor_CompliantAdapter(t *testing.T) {
	h, err := TranscriptHygieneFor(&fakeAdapter{})
	require.NoError(t, err)
	assert.NotNil(t, h)
}

func TestTranscriptHygieneFor_NilAdapterFailsLoud(t *testing.T) {
	_, err := TranscriptHygieneFor(nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nil adapter")
}

// nonHygienicAdapter satisfies Adapter but NOT TranscriptHygiene —
// the third-harness-without-the-contract shape pin (e) exercises.
type nonHygienicAdapter struct {
	fakeAdapter
}

func (a *nonHygienicAdapter) StripIdentityStamps() {} // deliberately wrong signature — not the contract

func TestTranscriptHygieneFor_NonCompliantAdapterFailsLoud(t *testing.T) {
	_, err := TranscriptHygieneFor(&nonHygienicAdapter{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "*agent.nonHygienicAdapter",
		"the loud error names the concrete adapter type")
	assert.Contains(t, err.Error(), "TranscriptHygiene")
}

func TestNewAdapterForType_NonHygienicFactoryProductFailsLoud(t *testing.T) {
	// Pin (e) at the registry seam: a registered harness whose factory
	// returns an adapter lacking the contract must FAIL CONSTRUCTION —
	// startup-fatal for a default harness, a per-call error for a lazy
	// one — never a silent per-harness gap.
	RegisterAdapter(testFactoryAgentType, func(deps AdapterFactoryDeps) (Adapter, error) {
		return &nonHygienicAdapter{}, nil
	})
	t.Cleanup(func() { UnregisterAdapter(testFactoryAgentType) })

	_, err := NewAdapterForType(testFactoryAgentType, AdapterFactoryDeps{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "transcript hygiene")
	assert.Contains(t, err.Error(), "*agent.nonHygienicAdapter")
}
