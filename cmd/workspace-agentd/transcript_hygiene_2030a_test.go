// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

// transcript_hygiene_2030a_test.go — the #2030a born-red pin at the
// MODEL-FACING serving seam: mcpSessionRead (the session_read MCP
// tool's emission — the surface whose tool results persist into
// history and replay into model context every turn) must serve ZERO
// identity stamps in tool-call args, while the harness's stored
// record (the fake opencode's fixture — the session DB stand-in)
// keeps them for attribution. The replay-side mirror of the
// accept-side strip (boardWireBody); the #1307 image-strip sibling.

// stampedServeBody is the flat 1.18.x V1 message list the fake
// opencode serves from storage — stamps present (the plugin's
// persisted injection), exactly what the next turn would replay.
const stampedServeBody = `[{"info":{"id":"msg_1","role":"assistant"},"parts":[
	{"type":"tool","tool":"llmsafespaces_board_import_plan","callID":"call_1",
	 "state":{"status":"completed","input":{"projectId":"prj_9","planFile":"plan.json","lsp_injected_session":"ses_ORIGIN"},"output":"ok"}},
	{"type":"tool","tool":"llmsafespaces_send_message","callID":"call_2",
	 "state":{"status":"completed","input":{"destination":"sacred-yacht-dines","message":"hi","sessionId":"ses_CAMEL","session_id":"ses_SNAKE"},"output":"delivered"}}
]}]`

func TestMCPSessionRead_StripsIdentityStamps(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(stampedServeBody))
	}))
	defer srv.Close()

	old := agentAddrAtomic.Load().(string)
	agentAddrAtomic.Store(srv.URL)
	defer agentAddrAtomic.Store(old)

	body, err := mcpSessionRead(context.Background(), "test-pw", "ses_1", 50)
	assert.NoError(t, err)

	// Pin (a): the served view carries zero identity keys in tool args.
	assert.NotContains(t, body, "lsp_injected_session")
	assert.NotContains(t, body, "sessionId")
	assert.NotContains(t, body, "session_id")
	assert.NotContains(t, body, "ses_ORIGIN")
	assert.NotContains(t, body, "ses_CAMEL")
	assert.NotContains(t, body, "ses_SNAKE")

	// Semantic params survive (the bleed variant's collateral rule).
	assert.Contains(t, body, `"projectId":"prj_9"`)
	assert.Contains(t, body, `"planFile":"plan.json"`)
	assert.Contains(t, body, `"destination":"sacred-yacht-dines"`)
	assert.Contains(t, body, `"message":"hi"`)

	// Pin (b): the STORED record keeps the stamps (attribution) — the
	// fixture is the session DB stand-in; the strip is serve-side only.
	assert.Contains(t, stampedServeBody, `"lsp_injected_session":"ses_ORIGIN"`)
	assert.Contains(t, stampedServeBody, `"sessionId":"ses_CAMEL"`)
}

// TestMCPSessionRead_HygieneComposesWithImageStrip pins the serving
// transforms' order-independence: stamps and image bytes strip in one
// pass without breaking each other's markers.
func TestMCPSessionRead_HygieneComposesWithImageStrip(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`[{"info":{"id":"m1","role":"user"},"parts":[
			{"type":"file","mime":"image/png","filename":"shot.png","url":"data:image/png;base64,aVBORw0KGgo="},
			{"type":"tool","tool":"llmsafespaces_board_validate_file","callID":"c1",
			 "state":{"status":"completed","input":{"op":"import_plan","file":"plan.json","lsp_injected_session":"ses_O"}}}
		]}]`))
	}))
	defer srv.Close()

	old := agentAddrAtomic.Load().(string)
	agentAddrAtomic.Store(srv.URL)
	defer agentAddrAtomic.Store(old)

	body, err := mcpSessionRead(context.Background(), "test-pw", "ses_1", 50)
	assert.NoError(t, err)
	assert.NotContains(t, body, "base64", "the #1307 image strip still fires")
	assert.Contains(t, body, "shot.png")
	assert.NotContains(t, body, "lsp_injected_session", "the #2030a stamp strip composes with it")
	assert.Contains(t, body, `"op":"import_plan"`)
}
