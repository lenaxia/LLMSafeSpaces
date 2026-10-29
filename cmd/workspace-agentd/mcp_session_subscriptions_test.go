package main

// mcp_session_subscriptions_test.go — idle-subscriptions v1, the MCP
// surface pins (design /workspace/reports/session-subscription-design.md
// §5): every case drives the REAL mcpHandler HTTP surface (the
// callMCPEnvelope pattern, mcp_phrase_parity_test.go) so the catalog
// entries, dispatch cases, AND handlers are exercised together — a
// dispatch typo 32601s at the wire while unit tests still pass.
//
// Coverage map (charter acceptance criteria → tests):
//   - catalog + dispatch + ordered schema + annotations + phrase-first
//     emission: TestSubscriptionTools_CatalogShape,
//     TestCreateSession_SchemaPromptLastPlusSubscribe,
//     TestSubscribeSession_PhraseParityAndDegradedEmission
//   - target exists (did-you-mean), events subset {idle,error},
//     self/cycle/subtask/cap refusals, idempotent re-subscribe:
//     TestSubscribeSession_* below
//   - create_session subscribe bool (prompt LAST), subscribed:true,
//     subagent-caller loud warning: TestCreateSession_Subscribe*
//   - caller identity via lsp_injected_session only (#1849 posture;
//     the resolveCallerSession degraded fallback deliberately NOT
//     used): TestSubscribeSession_NoInjectionRefused,
//     TestUnsubscribeSession_NoInjectionRefused,
//     TestListSubscriptions_NoInjectionRefused
//   - list round-trip: TestListSubscriptions_RoundTrip

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// withSubscriptionNotifier swaps the process-wide subscription engine
// for one with a temp-file store and empty ListSessions facts (the
// withShortIDStore precedent); tests register edges against it and
// assert on its store directly.
func withSubscriptionNotifier(t *testing.T) (*sessionNotifier, *sessionSubscriptionStore) {
	t.Helper()
	old := currentSessionNotifier()
	store := newSessionSubscriptionStore(t.TempDir() + "/session-subscriptions.json")
	n := newSessionNotifier(sessionNotifierConfig{
		Store:  store,
		Facts:  newHarnessSessionFacts(),
		Logger: zap.NewNop(),
	})
	setSessionNotifier(n)
	t.Cleanup(func() { setSessionNotifier(old) })
	return n, store
}

// callMCPExpectError drives one tools/call that must refuse and
// returns the error text block.
func callMCPExpectError(t *testing.T, tool string, args map[string]any) string {
	t.Helper()
	result := callMCPEnvelope(t, mcpTestPassword, tool, args)
	isErr, ok := result["isError"].(bool)
	require.True(t, ok && isErr, "expected an error envelope, got: %#v", result)
	blocks := envelopeTextBlocks(t, result)
	require.NotEmpty(t, blocks)
	return blocks[0]
}

// callMCPExpectOK drives one tools/call that must succeed and returns
// the decoded JSON object of the first text block.
func callMCPExpectOK(t *testing.T, tool string, args map[string]any) map[string]any {
	t.Helper()
	result := callMCPEnvelope(t, mcpTestPassword, tool, args)
	isErr, _ := result["isError"].(bool)
	require.False(t, isErr, "unexpected error envelope")
	blocks := envelopeTextBlocks(t, result)
	require.NotEmpty(t, blocks)
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(blocks[0]), &out), "block: %s", blocks[0])
	return out
}

// ---- catalog shape (tools/list is the catalog, verbatim) ----

// TestSubscriptionTools_CatalogShape: the three tools are registered
// with annotations and load-bearing description prose (the catalog's
// voice: what for, what NOT for, suppression/debounce/loss semantics
// stated plainly — scope item 5).
func TestSubscriptionTools_CatalogShape(t *testing.T) {
	t.Parallel()

	byName := map[string]mcpTool{}
	for _, tl := range mcpToolCatalog() {
		byName[tl.Name] = tl
	}
	sub, ok := byName["subscribe_session"]
	require.True(t, ok, "subscribe_session in catalog")
	unsub, ok := byName["unsubscribe_session"]
	require.True(t, ok, "unsubscribe_session in catalog")
	list, ok := byName["list_subscriptions"]
	require.True(t, ok, "list_subscriptions in catalog")

	// Annotations: mutating-but-idempotent for the edge tools,
	// read-only for the introspection one.
	require.NotNil(t, sub.Annotations)
	assert.False(t, *sub.Annotations.ReadOnlyHint)
	assert.True(t, *sub.Annotations.IdempotentHint)
	require.NotNil(t, unsub.Annotations)
	assert.True(t, *unsub.Annotations.IdempotentHint)
	require.NotNil(t, list.Annotations)
	assert.True(t, *list.Annotations.ReadOnlyHint)

	// Description guidance pins (the ToolDescriptionGuidance
	// discipline: load-bearing elements are contract, not prose).
	for _, want := range []string{
		"woken when its turn completes",   // what it's for
		"NOT a message channel",           // what it's NOT for
		"send_message",                    // the channel pointer
		"task tool",                       // not-for: blocking subtasks
		"SUPPRESSED",                      // suppression semantics stated
		"debounce",                        // debounce semantics stated
		"at-least-once",                   // loss semantics stated
		"duplicates are possible",         // duplicates after restart
		"watching yourself",               // refusal named
		"watch cycles",                    // refusal named
		"subtask sessions on EITHER side", // refusal named
		"did-you-mean",                    // refusal named
	} {
		assert.Contains(t, sub.Description, want)
	}
	assert.Contains(t, unsub.Description, "not_subscribed", "idempotence stated")
	assert.Contains(t, list.Description, "outbound", "scope stated")

	// Schema: sessionId required on the edge tools; events carries
	// the closed vocabulary. One camelCase catalog (#1849 casing
	// pass): the snake_case spelling resolves at dispatch for one
	// release but is never advertised.
	subSchema := sub.InputSchema.(map[string]any)
	props := subSchema["properties"].(map[string]any)
	require.Contains(t, props, "sessionId")
	assert.NotContains(t, props, "session_id", "one camelCase catalog — the legacy spelling is never advertised")
	events := props["events"].(map[string]any)
	items := events["items"].(map[string]any)
	assert.Equal(t, []string{"idle", "error"}, items["enum"])
	require.Contains(t, subSchema["required"], "sessionId")
	// No long freeform string anywhere: the map flavor is safe (the
	// #1849 long-string-last discipline binds only when one exists).
}

// TestCreateSession_SchemaPromptLastPlusSubscribe: the ordered
// create_session schema keeps prompt (the long freeform string) LAST
// with the new discrete subscribe bool before it (#1849 emission
// discipline — the property order IS the wire order).
func TestCreateSession_SchemaPromptLastPlusSubscribe(t *testing.T) {
	t.Parallel()

	var create mcpTool
	for _, tl := range mcpToolCatalog() {
		if tl.Name == "create_session" {
			create = tl
			break
		}
	}
	require.NotEmpty(t, create.Description, "create_session in catalog")
	schema, ok := create.InputSchema.(json.RawMessage)
	require.True(t, ok, "create_session schema stays ordered json.RawMessage")
	order := objectPropertyOrder(t, schema)
	require.Equal(t, []string{"title", "subscribe", "prompt"}, order,
		"discrete keys first, the long freeform prompt LAST")
}

// objectPropertyOrder decodes an object schema and returns the key
// order of its "properties" member AS ENCODED (map re-decoding would
// alphabetize; json.RawMessage + a token walk preserves the wire
// order).
func objectPropertyOrder(t *testing.T, schema []byte) []string {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(schema))
	var order []string
	depth := 0      // current nesting depth
	propsDepth := 0 // depth of the properties object; 0 = not inside it
	expectKey := false
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		if d, ok := tok.(json.Delim); ok {
			switch d {
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if propsDepth > 0 && depth < propsDepth {
					propsDepth = 0 // left the properties object
					continue
				}
			}
			if propsDepth > 0 && depth == propsDepth {
				expectKey = true // back at the properties boundary: the next string is a key
			}
			continue
		}
		s, ok := tok.(string)
		if !ok {
			continue // numbers/bools — none matter here
		}
		if propsDepth == 0 && depth == 1 && s == "properties" {
			nt, nerr := dec.Token()
			require.NoError(t, nerr)
			d, ok := nt.(json.Delim)
			require.True(t, ok && d == '{', "properties must be an object")
			depth++
			propsDepth = depth
			expectKey = true
			continue
		}
		if propsDepth > 0 && depth == propsDepth && expectKey {
			order = append(order, s)
			expectKey = false
		}
	}
	return order
}

// ---- subscribe_session ----

// TestSubscribeSession_PhraseParityAndDegradedEmission: the happy
// path over the full HTTP surface — phrase-addressed target emits the
// PHRASE (#1756 §1: the identifier output IS the phrase,
// case-insensitive input, no degraded flag, no notice); a canonical-
// id-addressed target with a cold replica emits the harness id
// FLAGGED with the deprecation parity.
func TestSubscribeSession_PhraseParityAndDegradedEmission(t *testing.T) {
	f := newFakeAgent()
	caller := f.newSession("manager")
	target := f.newSession("worker")
	withAgentServer(t, f.handler(t))
	withSubscriptionNotifier(t)
	store := withShortIDStore(t)
	store.put("amber-falcon-naps", target)

	// Phrase form, mixed case — resolves, emits the phrase verbatim.
	out := callMCPExpectOK(t, "subscribe_session", map[string]any{
		"sessionId": "Amber-Falcon-Naps",
		"lsp_injected_session": caller,
	})
	assert.Equal(t, "subscribed", out["status"])
	assert.Equal(t, "amber-falcon-naps", out["session_id"])
	assert.Equal(t, []any{"idle", "error"}, out["events"], "absent events default to BOTH kinds (ruling 1)")
	assert.NotContains(t, out, "session_id_deprecated")
	assert.NotContains(t, out, "deprecation")

	// Canonical form, cold replica — flagged degraded emission.
	out = callMCPExpectOK(t, "subscribe_session", map[string]any{
		"sessionId":           f.newSession("worker-2"),
		"lsp_injected_session": caller,
	})
	assert.Equal(t, "subscribed", out["status"])
	assert.NotEmpty(t, out["session_id"])
	assert.Equal(t, "true", out["session_id_deprecated"], "harness-id emission is flagged, never silent")
	assert.Contains(t, out["deprecation"], "DEPRECATED")

	// Both rows are canonical-keyed in the registry (phrases never
	// reach the store — §7.8).
	rows := currentSessionNotifier().store.rowsForSubscriber(caller)
	require.Len(t, rows, 2)
	assert.Equal(t, target, rows[0].Broadcaster)
}

// TestSubscribeSession_ExplicitEventsAndIdempotentUpdate: an explicit
// event list stores exactly that set; re-subscribing updates the set
// in place (idempotent upsert — one row, the new events).
func TestSubscribeSession_ExplicitEventsAndIdempotentUpdate(t *testing.T) {
	f := newFakeAgent()
	caller := f.newSession("manager")
	target := f.newSession("worker")
	withAgentServer(t, f.handler(t))
	_, store := withSubscriptionNotifier(t)

	out := callMCPExpectOK(t, "subscribe_session", map[string]any{
		"sessionId":           target,
		"events":               []any{"idle"},
		"lsp_injected_session": caller,
	})
	assert.Equal(t, []any{"idle"}, out["events"])
	rows := store.rowsForSubscriber(caller)
	require.Len(t, rows, 1)
	assert.Equal(t, []string{"idle"}, rows[0].Events)

	// Re-subscribe narrows to error-only: same edge, updated set.
	out = callMCPExpectOK(t, "subscribe_session", map[string]any{
		"sessionId":           target,
		"events":               []any{"error", "error"},
		"lsp_injected_session": caller,
	})
	assert.Equal(t, []any{"error"}, out["events"], "deduplicated in stable order")
	assert.Len(t, store.rowsForSubscriber(caller), 1, "re-subscribe upserts, never duplicates")
}

// TestSubscribeSession_EventVocabularyIsClosed: unsupported values,
// non-string entries, and non-array shapes all refuse loudly BEFORE
// anything registers.
func TestSubscribeSession_EventVocabularyIsClosed(t *testing.T) {
	f := newFakeAgent()
	caller := f.newSession("manager")
	target := f.newSession("worker")
	withAgentServer(t, f.handler(t))
	_, store := withSubscriptionNotifier(t)

	errText := callMCPExpectError(t, "subscribe_session", map[string]any{
		"sessionId":           target,
		"events":               []any{"busy"},
		"lsp_injected_session": caller,
	})
	assert.Contains(t, errText, `unsupported subscription event "busy"`)
	assert.Contains(t, errText, `"idle"`)
	assert.Contains(t, errText, `"error"`)

	errText = callMCPExpectError(t, "subscribe_session", map[string]any{
		"sessionId":           target,
		"events":               []any{1},
		"lsp_injected_session": caller,
	})
	assert.Contains(t, errText, "events entries must be strings")

	errText = callMCPExpectError(t, "subscribe_session", map[string]any{
		"sessionId":           target,
		"events":               "idle",
		"lsp_injected_session": caller,
	})
	assert.Contains(t, errText, "events must be an array")

	assert.Empty(t, store.rowsForSubscriber(caller), "nothing registered on refused vocabulary")
}

// TestSubscribeSession_TargetNotFoundDidYouMean: an unknown target
// refuses with the #1663 near-miss clause — suggest-only, never an
// auto-correct.
func TestSubscribeSession_TargetNotFoundDidYouMean(t *testing.T) {
	f := newFakeAgent()
	caller := f.newSession("manager")
	target := f.newSession("worker") // ses_3-shaped
	withAgentServer(t, f.handler(t))
	withSubscriptionNotifier(t)

	errText := callMCPExpectError(t, "subscribe_session", map[string]any{
		"sessionId":           target + "9", // one dropped-char-class near miss
		"lsp_injected_session": caller,
	})
	assert.Contains(t, errText, "not found in this workspace")
	assert.Contains(t, errText, target, "the near-miss suggestion names the live session")
}

// TestSubscribeSession_SelfRefused: watching yourself is the infinite
// wake loop — refused with its own sentinel, never registered.
func TestSubscribeSession_SelfRefused(t *testing.T) {
	f := newFakeAgent()
	caller := f.newSession("manager")
	withAgentServer(t, f.handler(t))
	_, store := withSubscriptionNotifier(t)

	errText := callMCPExpectError(t, "subscribe_session", map[string]any{
		"sessionId":           caller,
		"lsp_injected_session": caller,
	})
	assert.Contains(t, errText, "self-subscription refused")
	assert.Empty(t, store.rowsForSubscriber(caller))
}

// TestSubscribeSession_CycleRefused: A watching B is fine; B then
// watching A closes the wake loop and refuses (the DFS guard).
func TestSubscribeSession_CycleRefused(t *testing.T) {
	f := newFakeAgent()
	a := f.newSession("manager-a")
	b := f.newSession("manager-b")
	withAgentServer(t, f.handler(t))
	withSubscriptionNotifier(t)

	callMCPExpectOK(t, "subscribe_session", map[string]any{
		"sessionId": b, "lsp_injected_session": a,
	})
	errText := callMCPExpectError(t, "subscribe_session", map[string]any{
		"sessionId": a, "lsp_injected_session": b,
	})
	assert.Contains(t, errText, "subscription cycle refused")
	assert.Empty(t, currentSessionNotifier().store.rowsForSubscriber(b),
		"the cycle-closing edge is never registered")
}

// TestSubscribeSession_SubtaskBothSidesRefused: a subtask target
// (broadcaster side) and a subtask caller (subscriber side) both
// refuse with the §4.6 sentinel — and the lineage pull feeds the
// engine's own facts (belt-and-suspenders for notify time).
func TestSubscribeSession_SubtaskBothSidesRefused(t *testing.T) {
	f := newFakeAgent()
	caller := f.newSession("manager")
	plain := f.newSession("worker")
	parent := f.newSession("orchestrator")
	child := f.newSubtaskSession(parent, "subtask worker")
	withAgentServer(t, f.handler(t))
	n, store := withSubscriptionNotifier(t)

	errText := callMCPExpectError(t, "subscribe_session", map[string]any{
		"sessionId":           child,
		"lsp_injected_session": caller,
	})
	assert.Contains(t, errText, "subtask sessions are excluded")
	assert.Contains(t, errText, "broadcaster")

	errText = callMCPExpectError(t, "subscribe_session", map[string]any{
		"sessionId":           plain,
		"lsp_injected_session": child,
	})
	assert.Contains(t, errText, "subtask sessions are excluded")
	assert.Contains(t, errText, "subscriber")
	assert.Empty(t, store.rowsForSubscriber(caller))

	hf, ok := n.facts.(*harnessSessionFacts)
	require.True(t, ok, "default engine carries ListSessions facts")
	assert.True(t, hf.IsSubtask(child), "the lineage pull fed the engine's facts")
	assert.False(t, hf.IsSubtask(plain))
}

// TestSubscribeSession_Cap32: the 33rd distinct watcher on one
// broadcaster refuses (the §4.1 storm backstop); the first 32 hold.
func TestSubscribeSession_Cap32(t *testing.T) {
	f := newFakeAgent()
	target := f.newSession("worker")
	withAgentServer(t, f.handler(t))
	_, store := withSubscriptionNotifier(t)

	for i := 0; i < maxSubscribersPerBroadcaster; i++ {
		watcher := f.newSession("watcher")
		callMCPExpectOK(t, "subscribe_session", map[string]any{
			"sessionId": target, "lsp_injected_session": watcher,
		})
	}
	assert.Equal(t, maxSubscribersPerBroadcaster, store.subscriberCount(target))

	extra := f.newSession("watcher-extra")
	errText := callMCPExpectError(t, "subscribe_session", map[string]any{
		"sessionId": target, "lsp_injected_session": extra,
	})
	assert.Contains(t, errText, "subscriber cap reached")
	assert.Equal(t, maxSubscribersPerBroadcaster, store.subscriberCount(target))
}

// TestSubscribeSession_NoInjectionRefused: absent injection refuses
// with the #1849 degraded-pod error — PINNED to stay that way even
// when the resolveCallerSession heuristic's precondition holds
// (exactly one busy session): the delete tools' single-busy fallback
// is deliberately NOT the subscription tools' posture, because a
// wrong guess registers a wrong durable edge.
func TestSubscribeSession_NoInjectionRefused(t *testing.T) {
	f := newFakeAgent()
	caller := f.newSession("the-only-busy-one")
	target := f.newSession("worker")
	f.busySet[caller] = true // resolveCallerSession would resolve THIS
	withAgentServer(t, f.handler(t))
	_, store := withSubscriptionNotifier(t)

	errText := callMCPExpectError(t, "subscribe_session", map[string]any{
		"sessionId": target, // no lsp_injected_session
	})
	assert.Contains(t, errText, "no caller identity")
	assert.Contains(t, errText, "degraded pod")
	assert.Empty(t, store.allRows(), "nothing registers without an attested origin")
}

// ---- unsubscribe_session ----

// TestUnsubscribeSession_Idempotent: removing a held edge reports
// unsubscribed; removing it again reports not_subscribed — never an
// error. The caller can only remove its OWN edge.
func TestUnsubscribeSession_Idempotent(t *testing.T) {
	f := newFakeAgent()
	a := f.newSession("manager-a")
	b := f.newSession("manager-b")
	target := f.newSession("worker")
	withAgentServer(t, f.handler(t))
	_, store := withSubscriptionNotifier(t)

	callMCPExpectOK(t, "subscribe_session", map[string]any{
		"sessionId": target, "lsp_injected_session": a,
	})
	callMCPExpectOK(t, "subscribe_session", map[string]any{
		"sessionId": target, "lsp_injected_session": b,
	})

	// B unsubscribes: only B's edge goes.
	out := callMCPExpectOK(t, "unsubscribe_session", map[string]any{
		"sessionId": target, "lsp_injected_session": b,
	})
	assert.Equal(t, "unsubscribed", out["status"])
	assert.Len(t, store.rowsForSubscriber(b), 0)
	assert.Len(t, store.rowsForSubscriber(a), 1, "A's edge survives B's unsubscribe")

	out = callMCPExpectOK(t, "unsubscribe_session", map[string]any{
		"sessionId": target, "lsp_injected_session": b,
	})
	assert.Equal(t, "not_subscribed", out["status"], "idempotent absent edge")
}

// TestUnsubscribeSession_NoInjectionRefused: same #1849 posture.
func TestUnsubscribeSession_NoInjectionRefused(t *testing.T) {
	f := newFakeAgent()
	target := f.newSession("worker")
	withAgentServer(t, f.handler(t))

	errText := callMCPExpectError(t, "unsubscribe_session", map[string]any{
		"sessionId": target,
	})
	assert.Contains(t, errText, "no caller identity")
}

// ---- list_subscriptions ----

// TestListSubscriptions_RoundTrip: no params = the caller's outbound
// edges with PHRASES (#1756 §1); session_id = that session's edges;
// an unknown injected caller with no edges lists cleanly (count 0,
// empty array — never null).
func TestListSubscriptions_RoundTrip(t *testing.T) {
	f := newFakeAgent()
	caller := f.newSession("manager")
	other := f.newSession("other-manager")
	t1 := f.newSession("worker-1")
	t2 := f.newSession("worker-2")
	otherTarget := f.newSession("worker-3")
	withAgentServer(t, f.handler(t))
	withSubscriptionNotifier(t)
	store := withShortIDStore(t)
	store.put("amber-falcon-naps", t1)

	callMCPExpectOK(t, "subscribe_session", map[string]any{
		"sessionId": "amber-falcon-naps", "events": []any{"idle"}, "lsp_injected_session": caller,
	})
	// t2 stays OUT of the replica: the cold-replica emission path.
	callMCPExpectOK(t, "subscribe_session", map[string]any{
		"sessionId": t2, "lsp_injected_session": caller,
	})
	callMCPExpectOK(t, "subscribe_session", map[string]any{
		"sessionId": otherTarget, "lsp_injected_session": other,
	})

	// Own edges (no params): caller identity required, phrases emitted.
	out := callMCPExpectOK(t, "list_subscriptions", map[string]any{
		"lsp_injected_session": caller,
	})
	assert.Equal(t, float64(2), out["count"])
	rows := out["subscriptions"].([]any)
	got := map[string][]any{}
	for _, r := range rows {
		m := r.(map[string]any)
		got[m["session_id"].(string)] = m["events"].([]any)
	}
	assert.Equal(t, []any{"idle"}, got["amber-falcon-naps"], "phrase + event set round-trip")
	assert.Contains(t, got, t2, "cold-replica target emits the harness id (never worse)")

	// Explicit session_id: that session's edges, no caller identity needed.
	out = callMCPExpectOK(t, "list_subscriptions", map[string]any{
		"sessionId": other,
	})
	assert.Equal(t, float64(1), out["count"])
	rows = out["subscriptions"].([]any)
	assert.Equal(t, otherTarget, rows[0].(map[string]any)["session_id"])

	// Empty set: count 0 with [] not null.
	fresh := f.newSession("nobody")
	out = callMCPExpectOK(t, "list_subscriptions", map[string]any{
		"lsp_injected_session": fresh,
	})
	assert.Equal(t, float64(0), out["count"])
	assert.Empty(t, out["subscriptions"].([]any))
}

// TestListSubscriptions_NoInjectionRefused: listing OWN edges needs
// the caller identity (#1849 posture); the explicit-target form above
// deliberately does not.
func TestListSubscriptions_NoInjectionRefused(t *testing.T) {
	f := newFakeAgent()
	withAgentServer(t, f.handler(t))

	errText := callMCPExpectError(t, "list_subscriptions", map[string]any{})
	assert.Contains(t, errText, "no caller identity")
}

// ---- create_session + subscribe ----

// TestCreateSession_SubscribeFlag: the happy path — the created child
// exists, the caller is subscribed to it (canonical broadcaster key),
// the return carries subscribed:true and the default event set.
func TestCreateSession_SubscribeFlag(t *testing.T) {
	f := newFakeAgent()
	caller := f.newSession("manager")
	withAgentServer(t, f.handler(t))
	_, store := withSubscriptionNotifier(t)

	out := callMCPExpectOK(t, "create_session", map[string]any{
		"prompt":               "do the delegated thing",
		"subscribe":            true,
		"lsp_injected_session": caller,
	})
	assert.Equal(t, "created", out["status"])
	child := out["session_id"].(string)
	assert.Equal(t, true, out["subscribed"])
	assert.NotContains(t, out, "warning")

	rows := store.rowsForSubscriber(caller)
	require.Len(t, rows, 1)
	assert.Equal(t, child, rows[0].Broadcaster, "canonical broadcaster key")
	assert.Equal(t, []string{"idle", "error"}, rows[0].Events)

	f.awaitMsgDone(t, child, 1) // join the detached prompt delivery
}

// TestCreateSession_SubscribeFlagFromSubagent: a subagent caller
// (parentID set) gets the flag IGNORED with a loud warning — the
// session is still created, nothing is registered.
func TestCreateSession_SubscribeFlagFromSubagent(t *testing.T) {
	f := newFakeAgent()
	parent := f.newSession("orchestrator")
	subagent := f.newSubtaskSession(parent, "spawned worker")
	withAgentServer(t, f.handler(t))
	_, store := withSubscriptionNotifier(t)

	out := callMCPExpectOK(t, "create_session", map[string]any{
		"prompt":               "child work",
		"subscribe":            true,
		"lsp_injected_session": subagent,
	})
	assert.Equal(t, "created", out["status"], "the flag never gates creation")
	assert.Equal(t, false, out["subscribed"])
	warning, _ := out["warning"].(string)
	assert.Contains(t, warning, "subagent")
	assert.Contains(t, warning, "ignored")
	assert.Empty(t, store.allRows())

	child := out["session_id"].(string)
	f.awaitMsgDone(t, child, 1)
}

// TestCreateSession_SubscribeNoInjection: a degraded pod (no injected
// identity) still creates; the flag degrades to subscribed:false with
// the loud warning — never a failed create.
func TestCreateSession_SubscribeNoInjection(t *testing.T) {
	f := newFakeAgent()
	withAgentServer(t, f.handler(t))
	_, store := withSubscriptionNotifier(t)

	out := callMCPExpectOK(t, "create_session", map[string]any{
		"prompt":    "still works",
		"subscribe": true, // no lsp_injected_session
	})
	assert.Equal(t, "created", out["status"])
	assert.Equal(t, false, out["subscribed"])
	warning, _ := out["warning"].(string)
	assert.Contains(t, warning, "no caller identity")
	assert.Empty(t, store.allRows())

	child := out["session_id"].(string)
	f.awaitMsgDone(t, child, 1)
}

// TestCreateSession_SubscribeOmitted: the flag defaults to false and
// the field stays ABSENT (the pre-subscription contract unchanged).
func TestCreateSession_SubscribeOmitted(t *testing.T) {
	f := newFakeAgent()
	caller := f.newSession("manager")
	withAgentServer(t, f.handler(t))
	withSubscriptionNotifier(t)

	out := callMCPExpectOK(t, "create_session", map[string]any{
		"prompt":               "plain creation",
		"lsp_injected_session": caller,
	})
	assert.Equal(t, "created", out["status"])
	assert.NotContains(t, out, "subscribed", "omitted flag ⇒ absent field")
	assert.NotContains(t, out, "warning")

	child := out["session_id"].(string)
	f.awaitMsgDone(t, child, 1)
}

// TestSubscribeSession_LegacySnakeCaseArgResolved: the advertised
// param is sessionId (one camelCase catalog); the legacy snake_case
// spelling still resolves at dispatch for one release (the argStr
// camelToSnake fallback shared with the rest of the catalog).
func TestSubscribeSession_LegacySnakeCaseArgResolved(t *testing.T) {
	f := newFakeAgent()
	caller := f.newSession("manager")
	target := f.newSession("worker")
	withAgentServer(t, f.handler(t))
	withSubscriptionNotifier(t)

	out := callMCPExpectOK(t, "subscribe_session", map[string]any{
		"session_id":           target, // legacy spelling, one release
		"events":               []any{"idle"},
		"lsp_injected_session": caller,
	})
	assert.Equal(t, "subscribed", out["status"])
}
