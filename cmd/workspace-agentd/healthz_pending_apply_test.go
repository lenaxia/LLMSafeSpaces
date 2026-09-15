// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// healthz_pending_apply_test.go — #1342 item 4: the deferred credential
// apply surfaces on /v1/healthz (process-only contract preserved — a
// cached snapshot, never a live call).

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/agentd"
)

func TestHealthzHandler_PendingApplySurfaces(t *testing.T) {
	pending := newPendingApplyTracker()
	pending.begin(2)

	handler := healthzHandler(time.Now(), "", nil, pending.snapshot)
	rec := httptest.NewRecorder()
	handler(rec, httptest.NewRequest(http.MethodGet, "/v1/healthz", nil))

	var resp agentd.HealthzResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.PendingApply, "a deferred credential apply must surface on healthz")
	assert.Equal(t, pendingApplyReasonCredentialChange, resp.PendingApply.Reason)
	assert.Equal(t, 2, resp.PendingApply.BusySessions)
	assert.True(t, resp.Healthy, "a pending apply is a maintenance window, not a health failure")
}

func TestHealthzHandler_PendingApplyAbsentWhenCleared(t *testing.T) {
	pending := newPendingApplyTracker()

	handler := healthzHandler(time.Now(), "", nil, pending.snapshot)
	rec := httptest.NewRecorder()
	handler(rec, httptest.NewRequest(http.MethodGet, "/v1/healthz", nil))

	var resp map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	_, present := resp["pendingApply"]
	assert.False(t, present, "no pending apply — the field must be omitted entirely")
}

func TestHealthzHandler_NilPendingApplySnapshotOmitted(t *testing.T) {
	handler := healthzHandler(time.Now(), "", nil, nil)
	rec := httptest.NewRecorder()
	handler(rec, httptest.NewRequest(http.MethodGet, "/v1/healthz", nil))

	var resp map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	_, present := resp["pendingApply"]
	assert.False(t, present, "nil snapshot func — the field must be omitted")
}
