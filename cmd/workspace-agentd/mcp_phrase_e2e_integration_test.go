//go:build integration

// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// #1816 e2e legs: phrase-targeted send_message + abort through the
// REAL production chain — pinned opencode + the origin-injection
// plugin + agentd's REAL callMCPTool dispatch (the bootOriginE2E1627
// skeleton; one scripted tool call per boot, one leg per boot). The
// phrase replica is warmed the way the boot/on-miss registry pulls
// warm it (store.put); the model then addresses the target by its
// 3-word PHRASE and the chain must deliver/abort the harness session
// behind it, emitting the phrase back.
//
// Run: OPENCODE_BINARY=/opencode/usr/local/bin/opencode \
//      go test -tags=integration -run TestOriginE2E1816 ./cmd/workspace-agentd/ -timeout 600s

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const e2e1816Phrase = "amber-falcon-glides"

// The #1816 acceptance, send half: a send_message addressed by PHRASE
// (through the real catalog, real plugin injection, real dispatch)
// delivers to the harness session behind the phrase and emits the
// phrase back — the harness id never surfaces.
func TestOriginE2E1816_PhraseTargetedSendMessage(t *testing.T) {
	client, toolResults, provider := bootOriginE2E1627(t)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	callerID := createWithBootstrapRetry(t, client, "e21816-caller")
	targetID := createWithBootstrapRetry(t, client, "e21816-phrase-target")

	// Warm the phrase replica exactly as the boot/on-miss registry
	// pulls do (the in-process store callMCPTool resolves against).
	store := withShortIDStore(t)
	store.put(e2e1816Phrase, targetID)

	provider.mu.Lock()
	provider.tool = "llmsafespaces_send_message"
	provider.args = `{"session_id":"` + e2e1816Phrase + `","message":"phrase-addressed hello"}`
	provider.mu.Unlock()

	_, err := client.SessionSend(ctx, callerID, "send to the phrase session", "", nil)
	require.NoError(t, err, "the caller's turn must complete")

	require.NotEmpty(t, *toolResults)
	if strings.HasPrefix((*toolResults)[0], "ERROR: ") {
		t.Fatalf("phrase-targeted send_message failed inside the real dispatch: %s", (*toolResults)[0])
	}
	var sendResult map[string]any
	require.NoError(t, json.Unmarshal([]byte((*toolResults)[0]), &sendResult))
	assert.Equal(t, e2e1816Phrase, sendResult["session_id"],
		"the send result emits the PHRASE, never the harness id")
	assert.NotContains(t, (*toolResults)[0], targetID,
		"the harness id must not leak anywhere in the phrase-addressed result")

	// The message actually ARRIVED at the harness session behind the
	// phrase (poll the real session's transcript).
	require.Eventually(t, func() bool {
		raw, _, err := client.SessionMessagesRaw(ctx, targetID, 10, "")
		return err == nil && strings.Contains(string(raw), "phrase-addressed hello")
	}, 30*time.Second, 500*time.Millisecond, "the phrase-addressed message must arrive at the target session")
}

// The #1816 acceptance, abort half: an abort_session addressed by
// PHRASE reaches the real seam (the harness session behind the phrase)
// and emits the phrase back.
func TestOriginE2E1816_PhraseTargetedAbort(t *testing.T) {
	client, toolResults, provider := bootOriginE2E1627(t)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	callerID := createWithBootstrapRetry(t, client, "e21816-abort-caller")
	targetID := createWithBootstrapRetry(t, client, "e21816-abort-target")

	store := withShortIDStore(t)
	store.put(e2e1816Phrase, targetID)

	provider.mu.Lock()
	provider.tool = "llmsafespaces_abort_session"
	provider.args = `{"session_id":"` + e2e1816Phrase + `"}`

	// (provider: one scripted tool call per boot — this is it.)
	provider.mu.Unlock()

	_, err := client.SessionSend(ctx, callerID, "abort the phrase session", "", nil)
	require.NoError(t, err, "the caller's turn must complete")

	require.NotEmpty(t, *toolResults)
	if strings.HasPrefix((*toolResults)[0], "ERROR: ") {
		t.Fatalf("phrase-targeted abort failed inside the real dispatch: %s", (*toolResults)[0])
	}
	var abortResult map[string]any
	require.NoError(t, json.Unmarshal([]byte((*toolResults)[0]), &abortResult))
	assert.Equal(t, "aborted", abortResult["status"])
	assert.Equal(t, e2e1816Phrase, abortResult["session_id"],
		"the abort result emits the PHRASE")
	assert.NotContains(t, (*toolResults)[0], targetID)
}

// The §2 deprecation window, end to end: addressing a DIFFERENT
// session by its full harness id still works (one-release window) and
// the tool result carries the deprecation note.
func TestOriginE2E1816_DeprecatedFullIDCarriesNotice(t *testing.T) {
	client, toolResults, provider := bootOriginE2E1627(t)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	callerID := createWithBootstrapRetry(t, client, "e21816-dep-caller")
	targetID := createWithBootstrapRetry(t, client, "e21816-dep-target")

	provider.mu.Lock()
	provider.tool = "llmsafespaces_abort_session"
	provider.args = `{"session_id":"` + targetID + `"}` // full id — deprecated form
	provider.mu.Unlock()

	_, err := client.SessionSend(ctx, callerID, "abort that one by its full id", "", nil)
	require.NoError(t, err, "the caller's turn must complete (the window still accepts the id)")

	require.NotEmpty(t, *toolResults)
	if strings.HasPrefix((*toolResults)[0], "ERROR: ") {
		t.Fatalf("full-id abort failed inside the real dispatch: %s", (*toolResults)[0])
	}
	assert.Contains(t, (*toolResults)[0], "DEPRECATED",
		"a full-id input must carry the §2 note through the real chain")
	assert.Contains(t, (*toolResults)[0], `"status":"aborted"`,
		"the window still ACCEPTS the deprecated form")
}

// TestOriginE2E1816_UnresolvablePhraseRefusesCleanly (r2 missing-test
// 1, the unhappy leg): a 3-word phrase that resolves to NO session is
// an id-shaped miss — it passes through to the seam untouched and the
// REAL opencode's verdict is the refusal: no delivery, the caller's
// phrase echoed, no harness-id leak. (The did-you-mean clause rides
// the resolver-side ALIAS leg — covered by the twin subtest — because
// the pinned seam answers id-shaped misses with its own 500-class
// verdict, not a 404.)
func TestOriginE2E1816_UnresolvablePhraseRefusesCleanly(t *testing.T) {
	client, toolResults, provider := bootOriginE2E1627(t)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	callerID := createWithBootstrapRetry(t, client, "e21816-unresolvable-caller")
	// The replica is COLD for this phrase (never warmed): the miss
	// passes through untouched and the seam arbitrates.
	_ = withShortIDStore(t)

	provider.mu.Lock()
	provider.tool = "llmsafespaces_send_message"
	provider.args = `{"session_id":"quiet-marmot-glints","message":"anyone?"}`
	provider.mu.Unlock()

	_, err := client.SessionSend(ctx, callerID, "send to a phrase that does not exist", "", nil)
	require.NoError(t, err, "the caller's turn must complete (the refusal is a tool error)")

	require.NotEmpty(t, *toolResults)
	first := (*toolResults)[0]
	assert.True(t, strings.HasPrefix(first, "ERROR: "),
		"an unresolvable phrase must REFUSE, not deliver: %s", first)
	assert.Contains(t, first, "quiet-marmot-glints",
		"the refusal names the requested phrase (the caller's own input, echoed back)")

	// NO DELIVERY (r3): the message body appears in exactly ONE
	// transcript — the caller's own (its tool-call arguments echo the
	// body). Any SECOND session containing it is a leaked delivery.
	require.Eventually(t, func() bool {
		sessions, err := client.SessionList(ctx)
		if err != nil {
			return false
		}
		hits := 0
		for _, s := range sessions {
			raw, _, err := client.SessionMessagesRaw(ctx, s.ID, 5, "")
			if err == nil && strings.Contains(string(raw), "anyone?") {
				hits++
			}
		}
		return hits == 1
	}, 10*time.Second, 500*time.Millisecond,
		"the refused message must appear only in the caller's own transcript (its tool args), never delivered to a second session")
}

// TestOriginE2E1816_AliasMissRefusesWithDidYouMean (the resolver-side
// unhappy leg): an alias-form target that matches nothing fails at the
// RESOLVER with the #1663-class did-you-mean clause — no seam call, no
// delivery — through the real dispatch. The miss shares tokens with
// the caller's real title so the ranking actually produces a
// suggestion (r3: the first draft only asserted "not found").
func TestOriginE2E1816_AliasMissRefusesWithDidYouMean(t *testing.T) {
	client, toolResults, provider := bootOriginE2E1627(t)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	callerID := createWithBootstrapRetry(t, client, "gamma relay worker nine")
	_ = callerID

	provider.mu.Lock()
	provider.tool = "llmsafespaces_send_message"
	provider.args = `{"session_id":"gamma relay nowhere","message":"x"}`
	provider.mu.Unlock()

	_, err := client.SessionSend(ctx, callerID, "send by a near-miss title", "", nil)
	require.NoError(t, err, "the caller's turn must complete (the refusal is a tool error)")

	require.NotEmpty(t, *toolResults)
	first := (*toolResults)[0]
	assert.True(t, strings.HasPrefix(first, "ERROR: "), "an alias miss must refuse: %s", first)
	assert.Contains(t, first, "not found", "the refusal names the miss")
	assert.Contains(t, first, "did you mean",
		"the token-overlap miss must produce a did-you-mean suggestion through the real chain")
}
