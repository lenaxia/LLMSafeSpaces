// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// send_message_file_test.go — the #2030b pins: messageFile (the #1837 /
// design-0070 payload-file pattern applied to the send_message body).
//
//   - (a) byte-identical round-trip: the SAME body delivered via
//     messageFile and via the direct message param produces identical
//     delivered text (sentinel included) — the file is a transport for
//     the body, not a second message semantic (one shared trim, one
//     shared compose);
//   - (b) the message param is unaffected at the DISPATCH level (the
//     inline arm still delivers through the new code path; the
//     pre-existing mcpSendMessage suites cover the rest);
//   - (c) the mixed case refuses loudly — exactly-one-of, documented
//     precedence, no silent winner, file not consumed;
//   - (d) edge pins: missing file (the consumed-after-success hint),
//     oversize (the 256 KiB cap), nested exchange path (the flat
//     contract), non-UTF-8, whitespace-only body, placeholder-class
//     body (the #1814 guard applies to file-sourced bodies too), the
//     consumed file SURVIVES a failed send (retryable), and a delete
//     failure folds cleanup_warning into the succeeded send;
//   - (e) harness parity (owner ruling 2026-11-15): the file-mediated
//     artifact crosses BOTH in-tree harness adapters byte-identically
//     and the send path carries ZERO harness predicates.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/agent"
	"github.com/lenaxia/llmsafespaces/pkg/agent/pi"
	"github.com/lenaxia/llmsafespaces/pkg/session"
)

// withExchangeRoot pins the payload-file anchoring to a temp dir for
// one test (the board tests' override pattern — boardFileExchangeRoot
// is a var precisely for this).
func withExchangeRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	orig := boardFileExchangeRoot
	boardFileExchangeRoot = root
	t.Cleanup(func() { boardFileExchangeRoot = orig })
	return root
}

// hostileBody is the round-trip pin's payload: the shapes that make
// escaped-JSON long-string emission fragile — quotes, backslashes,
// newlines, JSON-lookalike fragments (including the injection-stamp
// key), unicode, and surrounding whitespace (the shared trim
// discipline must land both spellings on the same bytes).
const hostileBody = "\n\nStatus update for the reviewer — quotes \"like this\", backslashes \\n literal,\n" +
	"a JSON-lookalike {\"message\":\"probe\",\"lsp_injected_session\":\"ses_decoy\"},\n" +
	"unicode ✓ 日本語 🜛 and a tail of dashes --- \n  `code with \"quotes\" inside`  \n\n"

func fileDeliveredText(t *testing.T, f *fakeAgent, id string, n int) string {
	t.Helper()
	require.Eventually(t, func() bool { return len(f.sentFor(id)) == n },
		7*time.Second, 25*time.Millisecond, "detached delivery must land")
	f.awaitMsgDone(t, id, n)
	parts := f.sentFor(id)[n-1]["parts"].([]any)
	return parts[0].(map[string]any)["text"].(string)
}

// (a) THE round-trip pin: the same body, both spellings, identical
// delivered bytes — sentinel line included (same origin, same target),
// so full-string equality is the byte-identity claim.
func TestMCPSendMessage_FileBodyByteIdenticalRoundTrip(t *testing.T) {
	f := newFakeAgent()
	target := f.newSession("target")
	caller := f.newSession("caller")
	withAgentServer(t, f.handler(t))
	root := withExchangeRoot(t)

	// Direct spelling first (the pre-existing surface).
	_, err := mcpSendMessage(context.Background(), mcpTestPassword, target, hostileBody, caller)
	require.NoError(t, err)
	direct := fileDeliveredText(t, f, target, 1)

	// File spelling: the body written under the exchange root, sent by
	// PATH through the dispatch surface.
	require.NoError(t, os.WriteFile(filepath.Join(root, "body.md"), []byte(hostileBody), 0o600))
	out, err := callMCPTool(context.Background(), mcpTestPassword, "send_message", map[string]any{
		"destination":          target,
		"messageFile":          "body.md", // RELATIVE: resolves against the exchange root
		"lsp_injected_session": caller,
	})
	require.NoError(t, err)
	assert.Contains(t, out, `"message_source":"messageFile"`, "the result names the body's source")
	viaFile := fileDeliveredText(t, f, target, 2)

	assert.Equal(t, direct, viaFile,
		"file-mediated and direct message of the same body must deliver byte-identical text (sentinel included)")
	assert.True(t, strings.HasSuffix(viaFile, "\n"+strings.TrimSpace(hostileBody)),
		"the delivered payload carries the (shared-trim) body after the sentinel line")

	// Consumed after success: the path is gone.
	_, statErr := os.Stat(filepath.Join(root, "body.md"))
	require.True(t, os.IsNotExist(statErr), "messageFile must be deleted after a successful send")
}

// (b) The inline arm through the NEW dispatch code: an ordinary short
// message still delivers, and the result carries no file annotations.
func TestMCPSendMessage_MessageParamUnaffectedThroughDispatch(t *testing.T) {
	f := newFakeAgent()
	target := f.newSession("target")
	caller := f.newSession("caller")
	withAgentServer(t, f.handler(t))

	out, err := callMCPTool(context.Background(), mcpTestPassword, "send_message", map[string]any{
		"destination":          target,
		"message":              "short inline body still first-class",
		"lsp_injected_session": caller,
	})
	require.NoError(t, err)
	assert.Contains(t, out, `"delivering"`)
	assert.NotContains(t, out, "message_source", "the inline arm carries no file annotations")
	delivered := fileDeliveredText(t, f, target, 1)
	assert.True(t, strings.HasSuffix(delivered, "\nshort inline body still first-class"))
}

// (c) The mixed case refuses loudly per the documented precedence:
// exactly-one-of — there is no silent winner; the file is NOT consumed
// and nothing delivers.
func TestMCPSendMessage_BothBodyParamsRefuseLoudly(t *testing.T) {
	f := newFakeAgent()
	target := f.newSession("target")
	caller := f.newSession("caller")
	withAgentServer(t, f.handler(t))
	root := withExchangeRoot(t)
	bodyPath := filepath.Join(root, "both.md")
	require.NoError(t, os.WriteFile(bodyPath, []byte("file body"), 0o600))

	_, err := callMCPTool(context.Background(), mcpTestPassword, "send_message", map[string]any{
		"destination":          target,
		"message":              "inline body",
		"messageFile":          "both.md",
		"lsp_injected_session": caller,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mutually exclusive")
	assert.Contains(t, err.Error(), "exactly one", "the refusal teaches the precedence")

	// A null-valued message key is still a mention (the board stray-key
	// posture) — the refusal fires rather than a silent file win.
	_, err = callMCPTool(context.Background(), mcpTestPassword, "send_message", map[string]any{
		"destination":          target,
		"message":              nil,
		"messageFile":          "both.md",
		"lsp_injected_session": caller,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mutually exclusive")

	_, statErr := os.Stat(bodyPath)
	require.NoError(t, statErr, "a refused call must not consume the file")
	assert.Empty(t, f.sentBodies, "nothing delivered")
	assert.Empty(t, f.msgArrived, "no wire traffic at all")
}

// (d) Missing file: the not-found error carries the send-specific
// hint — consumed-after-success semantics, the session_read check, and
// WHERE message files live.
func TestMCPSendMessage_FileMissingLoudHint(t *testing.T) {
	f := newFakeAgent()
	target := f.newSession("target")
	caller := f.newSession("caller")
	withAgentServer(t, f.handler(t))
	withExchangeRoot(t)

	_, err := callMCPTool(context.Background(), mcpTestPassword, "send_message", map[string]any{
		"destination":          target,
		"messageFile":          "never-written.md",
		"lsp_injected_session": caller,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "messageFile")
	assert.Contains(t, err.Error(), "deleted after a successful send")
	assert.Contains(t, err.Error(), "session_read")
	assert.Contains(t, err.Error(), "/exchange", "the hint names the canonical dir")
	assert.Empty(t, f.msgArrived)
}

// (d) Oversize: the shared 256 KiB payload-file cap, refused before
// any delivery.
func TestMCPSendMessage_FileOversizeCap(t *testing.T) {
	f := newFakeAgent()
	target := f.newSession("target")
	caller := f.newSession("caller")
	withAgentServer(t, f.handler(t))
	root := withExchangeRoot(t)
	require.NoError(t, os.WriteFile(filepath.Join(root, "big.md"),
		[]byte(strings.Repeat("x", boardPayloadFileMaxBytes+1)), 0o600))

	_, err := callMCPTool(context.Background(), mcpTestPassword, "send_message", map[string]any{
		"destination":          target,
		"messageFile":          "big.md",
		"lsp_injected_session": caller,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exceeds the 256 KiB cap")
	assert.Empty(t, f.msgArrived)
}

// (d) The flat contract: a message file NESTED under the exchange root
// could be read but never auto-deleted — refused at the shared choke
// point.
func TestMCPSendMessage_FileNestedExchangePathRefused(t *testing.T) {
	f := newFakeAgent()
	target := f.newSession("target")
	caller := f.newSession("caller")
	withAgentServer(t, f.handler(t))
	root := withExchangeRoot(t)
	require.NoError(t, os.Mkdir(filepath.Join(root, "sub"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "sub", "body.md"), []byte("nested"), 0o600))

	_, err := callMCPTool(context.Background(), mcpTestPassword, "send_message", map[string]any{
		"destination":          target,
		"messageFile":          filepath.Join("sub", "body.md"),
		"lsp_injected_session": caller,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no subdirectories")
	assert.Empty(t, f.msgArrived)
}

// (d) Non-UTF-8: byte-identical delivery is impossible — refused
// loudly, never silently replaced.
func TestMCPSendMessage_FileNonUTF8Refused(t *testing.T) {
	f := newFakeAgent()
	target := f.newSession("target")
	caller := f.newSession("caller")
	withAgentServer(t, f.handler(t))
	root := withExchangeRoot(t)
	require.NoError(t, os.WriteFile(filepath.Join(root, "bin.md"),
		append([]byte("ok "), 0xff, 0xfe, 0xff), 0o600))

	_, err := callMCPTool(context.Background(), mcpTestPassword, "send_message", map[string]any{
		"destination":          target,
		"messageFile":          "bin.md",
		"lsp_injected_session": caller,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not valid UTF-8")
	assert.Empty(t, f.msgArrived)
}

// (d) A whitespace-only file is not a message — the ONE emptiness gate
// covers both body spellings.
func TestMCPSendMessage_FileWhitespaceOnlyIsNotAMessage(t *testing.T) {
	f := newFakeAgent()
	target := f.newSession("target")
	caller := f.newSession("caller")
	withAgentServer(t, f.handler(t))
	root := withExchangeRoot(t)
	require.NoError(t, os.WriteFile(filepath.Join(root, "blank.md"), []byte("  \n\t \n"), 0o600))

	_, err := callMCPTool(context.Background(), mcpTestPassword, "send_message", map[string]any{
		"destination":          target,
		"messageFile":          "blank.md",
		"lsp_injected_session": caller,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "message is required")
	assert.Empty(t, f.msgArrived)
}

// (d) The #1814 placeholder guard applies to file-sourced bodies too —
// and the refused call does not consume the file.
func TestMCPSendMessage_FilePlaceholderClassStillRejected(t *testing.T) {
	f := newFakeAgent()
	target := f.newSession("target")
	caller := f.newSession("caller")
	withAgentServer(t, f.handler(t))
	root := withExchangeRoot(t)
	bodyPath := filepath.Join(root, "todo.md")
	require.NoError(t, os.WriteFile(bodyPath, []byte("TODO"), 0o600))

	_, err := callMCPTool(context.Background(), mcpTestPassword, "send_message", map[string]any{
		"destination":          target,
		"messageFile":          "todo.md",
		"lsp_injected_session": caller,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "placeholder-class payload")
	_, statErr := os.Stat(bodyPath)
	require.NoError(t, statErr, "the placeholder refusal must leave the file for re-composition")
	assert.Empty(t, f.msgArrived)
}

// (d) The consumed file SURVIVES a failed send: every validation
// refusal (unknown destination here) leaves it on disk so the caller
// retries the same path without rewriting the body.
func TestMCPSendMessage_FileSurvivesFailedSend(t *testing.T) {
	f := newFakeAgent()
	caller := f.newSession("caller")
	withAgentServer(t, f.handler(t))
	root := withExchangeRoot(t)
	bodyPath := filepath.Join(root, "retry.md")
	require.NoError(t, os.WriteFile(bodyPath, []byte("body that must survive"), 0o600))

	_, err := callMCPTool(context.Background(), mcpTestPassword, "send_message", map[string]any{
		"destination":          "ses_absent",
		"messageFile":          "retry.md",
		"lsp_injected_session": caller,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
	_, statErr := os.Stat(bodyPath)
	require.NoError(t, statErr, "a failed send must not consume the body file")
}

// (d) A delete failure after a SUCCEEDED send folds cleanup_warning
// into the result — the send never fails for a cleanup chore (the
// board §2.4 semantics).
func TestMCPSendMessage_FileCleanupWarningWhenUndeletable(t *testing.T) {
	f := newFakeAgent()
	target := f.newSession("target")
	caller := f.newSession("caller")
	root := withExchangeRoot(t)
	bodyPath := filepath.Join(root, "stuck.md")
	require.NoError(t, os.WriteFile(bodyPath, []byte("body whose file outlives the send"), 0o600))
	// Root-proof refusal (review r1; the package precedent at
	// board_exchange_dir_test.go:114-125): between resolveMessageFile's
	// read and the post-success consume, swap the consumed file for a
	// NON-EMPTY directory, so os.Remove refuses ENOTEMPTY for EVERY
	// uid. (A chmod-0500 parent would not stop a root runner —
	// CAP_DAC_OVERRIDE — and the pin would false-fail the suite on
	// root CI.) The swap rides mcpSendMessage's FIRST synchronous wire
	// call (the resolve/exists/statuses probes all precede its return —
	// only the delivery POST is detached, and the consume follows the
	// return, so first-request timing is deterministic; later requests
	// no-op — the swap is idempotent).
	swapped := false
	var swapOnce sync.Mutex
	base := f.handler(t)
	withAgentServer(t, func(w http.ResponseWriter, r *http.Request) {
		swapOnce.Lock()
		if !swapped {
			swapped = true
			_ = os.Remove(bodyPath)
			_ = os.MkdirAll(filepath.Join(bodyPath, "child"), 0o755)
		}
		swapOnce.Unlock()
		base(w, r)
	})

	out, err := callMCPTool(context.Background(), mcpTestPassword, "send_message", map[string]any{
		"destination":          target,
		"messageFile":          "stuck.md",
		"lsp_injected_session": caller,
	})
	require.NoError(t, err, "the send itself succeeded — cleanup warns, never fails")
	assert.Contains(t, out, "cleanup_warning")
	assert.Contains(t, out, "message_source")

	delivered := fileDeliveredText(t, f, target, 1)
	assert.True(t, strings.HasSuffix(delivered, "\nbody whose file outlives the send"))
}

// A messageFile argument of the wrong type (or empty) is a loud
// admission error, not a silent fallthrough to "no body".
func TestMCPSendMessage_FileWrongTypeRefused(t *testing.T) {
	f := newFakeAgent()
	target := f.newSession("target")
	caller := f.newSession("caller")
	withAgentServer(t, f.handler(t))

	for _, tc := range []struct {
		name string
		val  any
	}{
		{"number", float64(42)},
		{"empty", "   "},
	} {
		_, err := callMCPTool(context.Background(), mcpTestPassword, "send_message", map[string]any{
			"destination":          target,
			"messageFile":          tc.val,
			"lsp_injected_session": caller,
		})
		require.Error(t, err, tc.name)
		assert.Contains(t, err.Error(), "non-empty workspace file path", tc.name)
	}
	assert.Empty(t, f.msgArrived)
}

// --- (e) harness parity (owner ruling 2026-11-15) -------------------------
//
// The file mediation is agentd's: ONE dispatch arm, ONE shared send
// path, ZERO harness predicates. The parity pin proves the claim from
// both ends:
//
//   - leg 1 (opencode, end-to-end): the file-mediated flow through the
//     real MCP dispatch against the opencode-shaped seam — the
//     delivered, sentinel-stamped artifact;
//   - leg 2 (pi, through the IN-TREE pkg/agent/pi adapter): that exact
//     artifact delivered via pi.Adapter.SendAsync — the recorded
//     `prompt` command must carry the text byte-for-byte;
//   - leg 3 (structural): a source scan proving the send path files
//     contain no harness-predicate vocabulary — the mitigation cannot
//     grow an #ifdef-harness branch without reddening this pin.

// harnessPredicateTokens is the vocabulary a harness branch in the
// send path would have to use (the agentd harness-selection seam).
var harnessPredicateTokens = []string{
	"harnessIsPi", "harnessFromEnv", "envAgentdHarness", "AGENTD_HARNESS", "AgentTypePi",
}

// parityPiTransport is the minimal in-tree pi.Transport: every command
// is recorded; responses are settle-free success envelopes (SendAsync
// never waits for the run).
type parityPiTransport struct {
	mu   sync.Mutex
	cmds []map[string]any
}

func (p *parityPiTransport) Session(_ context.Context, _, _, _ string) (pi.SessionClient, error) {
	return p, nil
}
func (p *parityPiTransport) CreateSession(_ context.Context, _, _, _ string) (pi.SessionClient, error) {
	return p, nil
}
func (p *parityPiTransport) Control(_ context.Context, _, _ string) (pi.SessionClient, error) {
	return p, nil
}
func (p *parityPiTransport) ListSessionFiles(_ context.Context, _, _ string) ([]pi.SessionFileMeta, error) {
	return nil, nil
}
func (p *parityPiTransport) OpenSessionFile(_ context.Context, _, _, _ string) (io.ReadCloser, error) {
	return nil, agent.ErrSessionNotFound
}
func (p *parityPiTransport) RemoveSessionFile(_ context.Context, _, _, _ string) error { return nil }

func (p *parityPiTransport) Call(_ context.Context, cmd map[string]any) (json.RawMessage, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cmds = append(p.cmds, cmd)
	ctype, _ := cmd["type"].(string)
	return json.RawMessage(fmt.Sprintf(`{"type":"response","command":%q,"success":true,"data":{}}`, ctype)), nil
}

func (p *parityPiTransport) Events(ctx context.Context) (<-chan pi.EventRecord, error) {
	ch := make(chan pi.EventRecord)
	go func() { <-ctx.Done() }() // held open until the caller's ctx ends
	return ch, nil
}

func TestMCPSendMessage_HarnessParity(t *testing.T) {
	f := newFakeAgent()
	target := f.newSession("target")
	caller := f.newSession("caller")
	withAgentServer(t, f.handler(t))
	root := withExchangeRoot(t)
	require.NoError(t, os.WriteFile(filepath.Join(root, "parity.md"), []byte(hostileBody), 0o600))

	// Leg 1 — opencode seam, end-to-end through the dispatch surface.
	_, err := callMCPTool(context.Background(), mcpTestPassword, "send_message", map[string]any{
		"destination":          target,
		"messageFile":          "parity.md",
		"lsp_injected_session": caller,
	})
	require.NoError(t, err)
	composed := fileDeliveredText(t, f, target, 1)
	require.True(t, strings.HasPrefix(composed, "<!-- lsp:agent-message-v1 "), "the artifact is the sentinel-stamped body")

	// Leg 2 — the pi in-tree adapter: the SAME composed artifact,
	// byte-identical, through pi's own delivery seam.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pt := &parityPiTransport{}
	pa := pi.NewAdapter(pt, nil)
	_, err = pa.SendAsync(ctx, "user-parity", "ws-parity", "ses_parity", composed, session.SendOpts{})
	require.NoError(t, err)
	pt.mu.Lock()
	defer pt.mu.Unlock()
	var promptText any
	for _, cmd := range pt.cmds {
		if cmd["type"] == "prompt" {
			promptText = cmd["message"]
		}
	}
	require.NotNil(t, promptText, "the pi adapter must issue a prompt command for the send")
	assert.Equal(t, composed, promptText,
		"the file-mediated artifact crosses the pi adapter byte-identically — no harness reshapes the body")

	// Leg 3 — structural: ZERO harness predicates in the send path.
	for _, name := range []string{"send_message_file.go", "mcp_tools.go", "mcp_server.go"} {
		src, rerr := os.ReadFile(name) // test cwd = the package dir
		require.NoError(t, rerr, "read %s", name)
		for _, tok := range harnessPredicateTokens {
			assert.NotContains(t, string(src), tok,
				"%s must not branch on the harness (%s) — the send surface is one path for every harness", name, tok)
		}
	}
}
