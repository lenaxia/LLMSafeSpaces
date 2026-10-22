// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// #1814: send_message must auto-reject placeholder-class payloads at
// the delivery boundary — the emission-corruption mode replaces a
// drafted body with a literal placeholder token as the call fires.
// NARROW exact-match set only (trimmed, case-insensitive): short
// legitimate payloads ("ok", "x", probe traffic) and sentences merely
// CONTAINING a placeholder word must pass untouched.

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func placeholderGuardFixture(t *testing.T) (target, origin string, delivered func() int) {
	t.Helper()
	f := newFakeAgent()
	target = f.newSession("guard-target")
	origin = f.newSession("guard-origin")
	withAgentServer(t, f.handler(t))
	return target, origin, func() int { return len(f.sentFor(target)) }
}

func TestMCPSendMessage_PlaceholderClassRejected(t *testing.T) {
	for _, payload := range []string{
		"placeholder",
		"PLACEHOLDER",
		"Placeholder",
		"placeholder-removed",
		"PLACEHOLDER-REMOVED",
		"todo",
		"TODO",
		"TBD",
		"tbd",
		"<placeholder>",
		"<PLACEHOLDER>",
		"asdf",
		"ASDF",
		"qwerty",
	} {
		t.Run(payload, func(t *testing.T) {
			target, origin, delivered := placeholderGuardFixture(t)

			out, err := mcpSendMessage(context.Background(), mcpTestPassword, target, payload, origin, "")
			require.Error(t, err, "payload %q must be rejected", payload)
			assert.Empty(t, out)
			assert.Contains(t, err.Error(), "placeholder",
				"the error must name the placeholder class")
			assert.Contains(t, err.Error(), "re-compose",
				"the error must instruct re-composition, not just refuse")
			assert.Equal(t, 0, delivered(),
				"nothing may be delivered for a rejected payload")
		})
	}
}

func TestMCPSendMessage_ShortAndProbePayloadsPass(t *testing.T) {
	// The issue's explicit constraint: short legitimate payloads and
	// probe traffic exist — a length heuristic would block them. The
	// guard is EXACT-MATCH ONLY.
	for _, payload := range []string{
		"ok",
		"x",
		"probe",
		"ping",
		"test", // probe traffic, NOT keyboard-mash
		"k",
		"?",

		"placeholder text I meant to send", // contains the word, not exact
		"TODO: fix this later",             // contains todo, not exact
		"not-a-placeholder",
		"what happened to the placeholder-removed token?", // sentence, not exact
	} {
		t.Run(payload, func(t *testing.T) {
			target, origin, delivered := placeholderGuardFixture(t)

			out, err := mcpSendMessage(context.Background(), mcpTestPassword, target, payload, origin, "")
			require.NoError(t, err, "payload %q must pass", payload)
			assert.Contains(t, out, `"status"`)
			require.Eventually(t, func() bool { return delivered() == 1 },
				5e9, 5e7, "the message must actually deliver")
		})
	}
}

func TestMCPSendMessage_PlaceholderWhitespaceVariantsRejected(t *testing.T) {
	// The corruption may fire mid-write: surrounding whitespace rides
	// the token. Trimmed comparison catches it.
	for _, payload := range []string{"  placeholder  ", "\nplaceholder", "placeholder-removed\t"} {
		t.Run(payload, func(t *testing.T) {
			target, origin, delivered := placeholderGuardFixture(t)

			_, err := mcpSendMessage(context.Background(), mcpTestPassword, target, payload, origin, "")
			require.Error(t, err)
			assert.Equal(t, 0, delivered())
		})
	}
}
