package main

// mcp_phrase_parity_test.go — #1816 platform-MCP phrase parity pins
// (red-first): the §2 notice closure on the tools the #1806 round left
// shape-constrained (session_read's array output, session_metadata,
// send_message's origin ref), the harness-id leaks in error/warning
// strings, and the full-chain phrase-targeted send_message + abort
// through the REAL agentd MCP HTTP surface (mcpHandler tools/call).

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- §2 notice parity (the #1756 one-release window) ------------------------

// callMCPEnvelope drives one tools/call through the REAL mcpHandler
// (auth + strict params decode + dispatch) and returns the decoded
// JSON-RPC result object.
func callMCPEnvelope(t *testing.T, password, tool string, args map[string]any) map[string]any {
	t.Helper()
	params, err := json.Marshal(map[string]any{"name": tool, "arguments": args})
	require.NoError(t, err)
	req := mcpRequest{JSONRPC: "2.0", ID: 42, Method: "tools/call", Params: params}
	body, err := json.Marshal(req)
	require.NoError(t, err)

	w := httptest.NewRecorder()
	r := mcpAuthedRequest(body)
	mcpHandler(password)(w, r)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	var resp mcpResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	result, ok := resp.Result.(map[string]any)
	require.True(t, ok, "result must be an object, got %T", resp.Result)
	return result
}

// envelopeTextBlocks returns the text content blocks of one envelope.
func envelopeTextBlocks(t *testing.T, result map[string]any) []string {
	t.Helper()
	content, ok := result["content"].([]any)
	require.True(t, ok, "content must be an array, got %T", result["content"])
	out := make([]string, 0, len(content))
	for _, c := range content {
		m, ok := c.(map[string]any)
		require.True(t, ok)
		txt, _ := m["text"].(string)
		out = append(out, txt)
	}
	return out
}

// TestMCPEnvelope_DeprecatedSessionRefCarriesNotice: a full-harness-id
// session_id input to ANY session tool attaches the #1756 §2 deprecation
// notice as a SECOND content block on the tools/call result — the
// envelope-level carrier covers array-shaped outputs (session_read)
// and object-shaped tools uniformly (#1816: notice parity with the
// REST Deprecation/Sunset headers).
func TestMCPEnvelope_DeprecatedSessionRefCarriesNotice(t *testing.T) {
	f := newFakeAgent()
	s1 := f.newSession("readme")
	s2 := f.newSession("meta")
	withAgentServer(t, f.handler(t))

	for _, tc := range []struct {
		tool string
		args map[string]any
	}{
		{"session_read", map[string]any{"session_id": s1, "limit": 5}},
		{"session_metadata", map[string]any{"session_id": s2}},
		{"abort_session", map[string]any{"session_id": s1}},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			result := callMCPEnvelope(t, mcpTestPassword, tc.tool, tc.args)
			blocks := envelopeTextBlocks(t, result)
			require.NotEmpty(t, blocks)
			assert.NotContains(t, blocks[0], "Error:", "the call itself must succeed: %s", blocks[0])
			require.Len(t, blocks, 2,
				"a deprecated-form session ref must carry the §2 notice as a second content block")
			assert.Contains(t, blocks[1], "DEPRECATED")
			assert.Contains(t, blocks[1], "phrase", "the notice must point at the canonical phrase form")
		})
	}
}

// TestMCPEnvelope_PhraseRefCarriesNoNotice: the phrase form (the
// canonical address) never attaches the notice block.
func TestMCPEnvelope_PhraseRefCarriesNoNotice(t *testing.T) {
	f := newFakeAgent()
	s1 := f.newSession("warm")
	withAgentServer(t, f.handler(t))
	store := withShortIDStore(t)
	store.put("amber-falcon-naps", s1)

	result := callMCPEnvelope(t, mcpTestPassword, "session_read", map[string]any{
		"session_id": "amber-falcon-naps", "limit": 2,
	})
	blocks := envelopeTextBlocks(t, result)
	require.NotEmpty(t, blocks)
	assert.NotContains(t, blocks[0], "Error:", "the phrase-addressed read must succeed: %s", blocks[0])
	require.Len(t, blocks, 1, "a phrase ref carries NO notice block")
	assert.NotContains(t, blocks[0], "DEPRECATED")
}

// TestMCPEnvelope_SendMessageDeprecatedOriginNotice: send_message's
// from_session_id (the declared origin fallback) in deprecated form
// attaches the notice even when the TARGET is a clean phrase — the
// origin is the id the recipient re-uses to reply (#1816 A3).
func TestMCPEnvelope_SendMessageDeprecatedOriginNotice(t *testing.T) {
	f := newFakeAgent()
	target := f.newSession("target")
	origin := f.newSession("origin")
	withAgentServer(t, f.handler(t))
	store := withShortIDStore(t)
	store.put("calm-otter-naps", target)

	result := callMCPEnvelope(t, mcpTestPassword, "send_message", map[string]any{
		"session_id":      "calm-otter-naps",
		"message":         "hello",
		"from_session_id": origin,
	})
	blocks := envelopeTextBlocks(t, result)
	require.NotEmpty(t, blocks)
	assert.NotContains(t, blocks[0], "Error:", "send must succeed: %s", blocks[0])
	require.Len(t, blocks, 2, "the deprecated ORIGIN ref must carry the notice")
	assert.Contains(t, blocks[1], "DEPRECATED")
}

// --- emission closure: no raw harness ids in strings when a phrase is known --

// TestMCPSendMessage_RecoveryWarningEmitsPhrase: the #1530 duplicated-JSON
// recovery warning embeds session ids — with a warm replica those must
// be PHRASES, matching the result fields beside them (#1816 A4).
func TestMCPSendMessage_RecoveryWarningEmitsPhrase(t *testing.T) {
	f := newFakeAgent()
	target := f.newSession("target")
	other := f.newSession("other")
	withAgentServer(t, f.handler(t))
	store := withShortIDStore(t)
	store.put("amber-falcon-naps", target)

	// The #1530 duplicated-argument shape: TARGET + the injected-session
	// fragment mark + ORIGIN + trailing quote/brace.
	mangled := target + `","lsp_injected_session":"` + other + `"}`
	out, err := mcpSendMessage(context.Background(), mcpTestPassword, mangled, "hi", other, "")
	require.NoError(t, err)
	assert.Contains(t, out, "amber-falcon-naps", "the recovered target ships as the phrase")
	assert.NotContains(t, out, target, "the TARGET harness id must not leak once the phrase is known")
	f.awaitMsgDone(t, target, 1)
}

// TestMCPSendMessage_PhraseTargetDuplicatedFragmentRecovers: the #1530
// recovery accepts a 3-word-phrase leading id too — phrases are the
// canonical address now, and the emission slip follows the address
// form the caller sees in context (#1816).
func TestMCPSendMessage_PhraseTargetDuplicatedFragmentRecovers(t *testing.T) {
	f := newFakeAgent()
	target := f.newSession("target")
	other := f.newSession("other")
	withAgentServer(t, f.handler(t))
	store := withShortIDStore(t)
	store.put("amber-falcon-naps", target)
	store.put("calm-otter-naps", other)

	mangled := "amber-falcon-naps" + `","lsp_injected_session":"` + "calm-otter-naps" + `"}`
	out, err := mcpSendMessage(context.Background(), mcpTestPassword, mangled, "hi", other, "")
	require.NoError(t, err)
	assert.Contains(t, out, `"session_id":"amber-falcon-naps"`, "the recovered phrase target ships as the phrase")
	assert.Contains(t, out, "duplicated-argument fragment", "the recovery warning must be present")
	f.awaitMsgDone(t, target, 1)
}

// TestResolveSingleBusySession_MultipleBusyErrorEmitsPhrases: the
// ambiguity error joins session ids — warm-replica phrases, never raw
// harness ids (#1816 A5).
func TestResolveSingleBusySession_MultipleBusyErrorEmitsPhrases(t *testing.T) {
	f := newFakeAgent()
	s1 := f.newSession("busy-one")
	s2 := f.newSession("busy-two")
	f.busySet[s1] = true
	f.busySet[s2] = true
	withAgentServer(t, f.handler(t))
	store := withShortIDStore(t)
	store.put("amber-falcon-naps", s1)
	store.put("calm-otter-naps", s2)

	_, err := resolveSingleBusySession(context.Background(), seamClientWithPassword(mcpTestPassword))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "amber-falcon-naps")
	assert.Contains(t, err.Error(), "calm-otter-naps")
	assert.NotContains(t, err.Error(), s1, "harness ids never surface when phrases are known")
	assert.NotContains(t, err.Error(), s2, "harness ids never surface when phrases are known")
}

// --- full chain: phrase-targeted send_message + abort through the REAL MCP --

// TestRealMCP_PhraseTargetedSendMessageAndAbort is the #1816 acceptance
// e2e at the unit tier: the REAL mcpHandler HTTP dispatch (auth, strict
// params, tools/call routing) with a warm phrase replica — send_message
// addressed by 3-word phrase DELIVERS to the harness session, and
// abort_session addressed by phrase ABORTS it. The integration-tier
// twin (real opencode binary) lives in
// mcp_phrase_e2e_integration_test.go.
func TestRealMCP_PhraseTargetedSendMessageAndAbort(t *testing.T) {
	f := newFakeAgent()
	target := f.newSession("phrase-target")
	withAgentServer(t, f.handler(t))
	store := withShortIDStore(t)
	store.put("amber-falcon-glides", target)

	// send_message by phrase through the real envelope.
	result := callMCPEnvelope(t, mcpTestPassword, "send_message", map[string]any{
		"session_id":      "Amber-Falcon-Glides", // case-insensitive phrase form
		"message":         "phrase hello",
		"from_session_id": "amber-falcon-glides", // self-send: resolves, warns, still delivers
	})
	blocks := envelopeTextBlocks(t, result)
	require.NotEmpty(t, blocks)
	assert.NotContains(t, blocks[0], "Error:", "phrase-targeted send must deliver: %s", blocks[0])
	assert.Contains(t, blocks[0], `"status"`)
	assert.Contains(t, blocks[0], `"session_id":"amber-falcon-glides"`)
	assert.Len(t, blocks, 1, "phrase target + phrase origin carry no notice block")
	f.awaitMsgDone(t, target, 1)

	// abort by phrase through the real envelope.
	result2 := callMCPEnvelope(t, mcpTestPassword, "abort_session", map[string]any{
		"session_id": "amber-falcon-glides",
	})
	blocks2 := envelopeTextBlocks(t, result2)
	require.NotEmpty(t, blocks2)
	assert.NotContains(t, blocks2[0], "Error:", "phrase-targeted abort must succeed: %s", blocks2[0])
	assert.Contains(t, blocks2[0], "amber-falcon-glides")
	f.mu.Lock()
	aborted := append([]string{}, f.aborted...)
	f.mu.Unlock()
	assert.Contains(t, aborted, target, "abort must reach the harness session behind the phrase")
}

// TestMCPPhraseInput_CaseInsensitiveNormalization pins that the
// phrase input normalizes (the store keys are lowercase; mixed-case
// input still delivers).
func TestMCPPhraseInput_CaseInsensitiveNormalization(t *testing.T) {
	f := newFakeAgent()
	target := f.newSession("case-target")
	withAgentServer(t, f.handler(t))
	store := withShortIDStore(t)
	store.put("amber-falcon-glides", target)

	out, err := mcpSendMessage(context.Background(), mcpTestPassword, "AMBER-FALCON-GLIDES", "upper", target, "")
	require.NoError(t, err)
	assert.Contains(t, out, "amber-falcon-glides")
	f.awaitMsgDone(t, target, 1)
	f.mu.Lock()
	defer f.mu.Unlock()
	assert.Equal(t, 1, len(f.sentBodies[target]), "mixed-case phrase must resolve to the harness session")
}
