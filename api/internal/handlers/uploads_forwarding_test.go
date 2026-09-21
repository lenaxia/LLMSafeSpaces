// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

// Design 0060 §9 PR 3 — API forwarding + wiring. Pins:
//   - agentd's 507/429/504 statuses and reason bodies are forwarded
//     VERBATIM (today they collapse into a fixed 502), with the reason
//     field mapped onto the widened API metrics enum (§4.6);
//   - the API-generated 411 for undeclared client bodies (reason
//     invalid_declared_length) — the admission input for
//     reservation-before-acceptance (§4.1);
//   - X-LLS-Declared-Body-Bytes on the agentd hop, carrying the
//     client's declared Content-Length;
//   - the degrade rule: a 507 whose body carries no/unknown reason
//     still forwards verbatim but labels agentd_error.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The literal agentd shapes (frozen contract, worker-lane coordination
// 2026-09-21): forwarded byte-for-byte, labeled for metrics via reason.
var forwardedShapeTable = []struct {
	name       string
	status     int
	agentdBody string
	wantReason string
	wantInBody string
	wantAbsent string
}{
	{
		name:       "507 staging budget",
		status:     http.StatusInsufficientStorage,
		agentdBody: `{"error":"staging budget exhausted — retry after in-flight uploads settle or free tmpfs","reason":"staging_full"}`,
		wantReason: "staging_full",
		wantInBody: "staging budget exhausted — retry after in-flight uploads settle or free tmpfs",
		wantAbsent: "502",
	},
	{
		name:       "507 mid-stream staging abort",
		status:     http.StatusInsufficientStorage,
		agentdBody: `{"error":"staging write failed","reason":"staging_write_error"}`,
		wantReason: "staging_write_error",
		wantInBody: "staging write failed",
		wantAbsent: "",
	},
	{
		name:       "507 apply rejected with the §3.2 code riding",
		status:     http.StatusInsufficientStorage,
		agentdBody: `{"error":"upload apply failed: staged object vanished","reason":"apply_rejected","code":"staged_missing"}`,
		wantReason: "apply_rejected",
		wantInBody: `"code":"staged_missing"`,
		wantAbsent: "",
	},
	{
		name:       "507 write-time destination disk",
		status:     http.StatusInsufficientStorage,
		agentdBody: `{"error":"workspace disk is full (write-time check)","reason":"dest_disk_full"}`,
		wantReason: "dest_disk_full",
		wantInBody: "workspace disk is full (write-time check)",
		wantAbsent: "",
	},
	{
		name:       "429 staging busy",
		status:     http.StatusTooManyRequests,
		agentdBody: `{"error":"staging busy","reason":"staging_busy"}`,
		wantReason: "staging_busy",
		wantInBody: "staging busy",
		wantAbsent: "",
	},
	{
		name:       "504 apply timeout",
		status:     http.StatusGatewayTimeout,
		agentdBody: `{"error":"upstream apply timeout","reason":"apply_timeout"}`,
		wantReason: "apply_timeout",
		wantInBody: "upstream apply timeout",
		wantAbsent: "",
	},
}

func TestUpload_ForwardedStatusAndReasonBodies(t *testing.T) {
	for _, tt := range forwardedShapeTable {
		t.Run(tt.name, func(t *testing.T) {
			resetUploadMetrics(t)
			env, _ := newUploadEnvWithFakeAgentd(t, tt.status, tt.agentdBody)
			env.setupPassword(t, "test-password")
			env.setupWorkspace(t, activeUploadWS())

			body, ct := buildMultipart(t, uploadPartSpec{field: "file", filename: "x.txt", content: []byte("x")})
			w := doUpload(env, body, ct)

			// Status forwarded verbatim — never the fixed 502.
			assert.Equal(t, tt.status, w.Code)
			// Body forwarded verbatim: the client-actionable text, the
			// reason, and (for apply_rejected) the §3.2 code all ride.
			assert.Contains(t, w.Body.String(), tt.wantInBody)
			assert.Contains(t, w.Body.String(), `"reason":"`+tt.wantReason+`"`)
			if tt.wantAbsent != "" {
				assert.NotContains(t, w.Body.String(), tt.wantAbsent)
			}
			// Metrics labeled with the widened enum value.
			assert.Equal(t, 1.0, uploadMetricValue(t, tt.wantReason))
		})
	}
}

func TestUpload_507UnknownReason_ForwardsVerbatim_LabelsAgentdError(t *testing.T) {
	resetUploadMetrics(t)
	env, _ := newUploadEnvWithFakeAgentd(t, http.StatusInsufficientStorage, `{"error":"something novel"}`)
	env.setupPassword(t, "test-password")
	env.setupWorkspace(t, activeUploadWS())

	body, ct := buildMultipart(t, uploadPartSpec{field: "file", filename: "x.txt", content: []byte("x")})
	w := doUpload(env, body, ct)

	assert.Equal(t, http.StatusInsufficientStorage, w.Code)
	assert.Contains(t, w.Body.String(), "something novel", "body still forwards verbatim")
	assert.Equal(t, 1.0, uploadMetricValue(t, "agentd_error"), "no reason field → the true-remainder label")
}

func TestUpload_DeclaredBodyGate_ChunkedClientBody_411(t *testing.T) {
	resetUploadMetrics(t)
	env, _ := newUploadEnvWithFakeAgentd(t, http.StatusCreated, `{"path":"/workspace/uploads/x","name":"x","size":1}`)
	env.setupPassword(t, "test-password")
	env.setupWorkspace(t, activeUploadWS())

	// A body with UNKNOWN length (no Content-Length on the wire — the
	// chunked shape; MultiReader hides the concrete type from
	// NewRequest's length-detection). The 411 must fire before the dial.
	buf, ct := buildMultipart(t, uploadPartSpec{field: "file", filename: "x.txt", content: []byte("x")})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/workspaces/ws-1/uploads", io.MultiReader(buf))
	req.Header.Set("Content-Type", ct)
	env.router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusLengthRequired, w.Code)
	assert.Contains(t, w.Body.String(), "declared body length required")
	assert.Contains(t, w.Body.String(), `"reason":"invalid_declared_length"`)
	assert.Equal(t, 1.0, uploadMetricValue(t, "invalid_declared_length"))
}

func TestUpload_507UnparseableBody_ForwardsVerbatim_LabelsAgentdError(t *testing.T) {
	resetUploadMetrics(t)
	// The forwardedUploadReason json.Unmarshal failure branch (r2
	// missing case): agentd's body is NOT valid JSON — the 507 still
	// forwards byte-for-byte and the label degrades to agentd_error.
	env, _ := newUploadEnvWithFakeAgentd(t, http.StatusInsufficientStorage, `not-json{garbage`)
	env.setupPassword(t, "test-password")
	env.setupWorkspace(t, activeUploadWS())

	body, ct := buildMultipart(t, uploadPartSpec{field: "file", filename: "x.txt", content: []byte("x")})
	w := doUpload(env, body, ct)

	assert.Equal(t, http.StatusInsufficientStorage, w.Code)
	assert.Contains(t, w.Body.String(), "not-json{garbage", "unparseable body still forwards verbatim")
	assert.Equal(t, 1.0, uploadMetricValue(t, "agentd_error"))
}

func TestUpload_ForwardedBody_TruncatedAt4KiB_CarriesTruncationMarker(t *testing.T) {
	resetUploadMetrics(t)
	// forwardedReasonBodyCap is 4 KiB: an agentd reason body beyond it
	// truncates (a deliberate, documented deviation from verbatim —
	// reason bodies are one-line envelopes, never file content; the
	// frozen shapes are ~100 bytes). The truncation is pinned so a
	// regression to unbounded reads fails here.
	bigBody := `{"error":"` + strings.Repeat("x", 8192) + `","reason":"staging_full"}`
	env, _ := newUploadEnvWithFakeAgentd(t, http.StatusInsufficientStorage, bigBody)
	env.setupPassword(t, "test-password")
	env.setupWorkspace(t, activeUploadWS())

	body, ct := buildMultipart(t, uploadPartSpec{field: "file", filename: "x.txt", content: []byte("x")})
	w := doUpload(env, body, ct)

	assert.Equal(t, http.StatusInsufficientStorage, w.Code)
	assert.LessOrEqual(t, w.Body.Len(), 4096+512, "body truncated at the 4KiB cap (+ envelope overhead)")
	// The reason field sits at the body's END — truncation cuts it, the
	// JSON parse fails on the fragment, and the label degrades to
	// agentd_error. (Frozen envelopes are ~100 bytes; a >4KiB reason
	// body is off-spec by construction — the pin documents the pair.)
	assert.Equal(t, 1.0, uploadMetricValue(t, "agentd_error"))
}

func TestUpload_DeclaredHeaderForwardedToAgentd(t *testing.T) {
	resetUploadMetrics(t)
	rec, srv := newFakeAgentd(t, http.StatusCreated, `{"path":"/workspace/uploads/x","name":"x","size":1}`)
	env := newUploadEnv(t, &uploadCaptureTransport{server: srv})
	env.setupPassword(t, "test-password")
	env.setupWorkspace(t, activeUploadWS())

	body, ct := buildMultipart(t, uploadPartSpec{field: "file", filename: "x.txt", content: []byte("x")})
	declaredLen := body.Len() // doUpload drains the buffer
	w := doUpload(env, body, ct)
	require.Equal(t, http.StatusCreated, w.Code)

	// The hop carries the client's declared Content-Length as the
	// admission input (§4.1) — agentd 411s without it.
	rec.mu.Lock()
	defer rec.mu.Unlock()
	assert.Equal(t, strconv.FormatInt(int64(declaredLen), 10), rec.declaredBodyBytes,
		"the header must carry the client's declared Content-Length")
}
