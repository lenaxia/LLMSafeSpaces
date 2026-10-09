// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lenaxia/llmsafespaces/pkg/agentd"
	"github.com/lenaxia/llmsafespaces/pkg/diskrecovery"
)

// fakeDiskRecoverEngine records the last request and replies scripted.
type fakeDiskRecoverEngine struct {
	gotRequest diskrecovery.Request
	calls      int
	report     diskrecovery.Report
	err        error
}

func (f *fakeDiskRecoverEngine) Recover(_ context.Context, req diskrecovery.Request) (diskrecovery.Report, error) {
	f.calls++
	f.gotRequest = req
	return f.report, f.err
}

func basicHdr(pw string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(agentd.AuthUsername+":"+pw))
}

func doDiskRecover(h http.HandlerFunc, method, auth string, body any) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, "/v1/disk-recover", &buf)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

// PIN (§D1 gate): no credentials and wrong credentials are rejected;
// BOTH the workspace password and the control-plane password are
// accepted (the API drives this route with either per the carve-out).
func TestDiskRecoverHandlerAuthGate(t *testing.T) {
	eng := &fakeDiskRecoverEngine{}
	h := diskRecoverHandler("ws-pw", "cp-pw", eng)

	if rec := doDiskRecover(h, http.MethodPost, "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no auth: want 401, got %d", rec.Code)
	}
	if rec := doDiskRecover(h, http.MethodPost, basicHdr("wrong"), nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong auth: want 401, got %d", rec.Code)
	}
	if rec := doDiskRecover(h, http.MethodPost, basicHdr("ws-pw"), map[string]any{}); rec.Code != http.StatusOK {
		t.Fatalf("workspace password: want 200, got %d", rec.Code)
	}
	if rec := doDiskRecover(h, http.MethodPost, basicHdr("cp-pw"), map[string]any{}); rec.Code != http.StatusOK {
		t.Fatalf("control-plane password: want 200, got %d", rec.Code)
	}
	if eng.calls != 2 {
		t.Fatalf("engine must run exactly for the two authorized calls, got %d", eng.calls)
	}
}

// PIN (method gate + unwired engine): GET is 405; a nil engine answers
// 503 loudly — never a silent no-op.
func TestDiskRecoverHandlerMethodAndWiringGates(t *testing.T) {
	h := diskRecoverHandler("ws-pw", "", nil)
	if rec := doDiskRecover(h, http.MethodGet, basicHdr("ws-pw"), nil); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET: want 405, got %d", rec.Code)
	}
	if rec := doDiskRecover(h, http.MethodPost, basicHdr("ws-pw"), map[string]any{}); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unwired engine: want 503, got %d", rec.Code)
	}
}

// PIN (body decode): dryRun and targetRatio cross the wire into the
// engine request; malformed JSON is a 400.
func TestDiskRecoverHandlerBodyDecodes(t *testing.T) {
	eng := &fakeDiskRecoverEngine{report: diskrecovery.Report{BytesFreed: 42, AlreadyBelowTarget: false}}
	h := diskRecoverHandler("ws-pw", "", eng)

	rec := doDiskRecover(h, http.MethodPost, basicHdr("ws-pw"), map[string]any{"dryRun": true, "targetRatio": 0.7})
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !eng.gotRequest.DryRun || eng.gotRequest.TargetRatio != 0.7 {
		t.Fatalf("engine request must mirror the body, got %+v", eng.gotRequest)
	}
	var out diskrecovery.Report
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.BytesFreed != 42 {
		t.Fatalf("report must relay to the response, got %s err=%v", rec.Body.String(), err)
	}

	rec = doDiskRecover(h, http.MethodPost, basicHdr("ws-pw"), nil) // nil body → decode error
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty body: want 400, got %d", rec.Code)
	}
}

// PIN (sentinel mapping): ErrBusy → 409, ErrNoUsage → 503, generic →
// 500. The two fail-closed engine states must stay distinguishable to
// the API facade.
func TestDiskRecoverHandlerSentinelMapping(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{diskrecovery.ErrBusy, http.StatusConflict},
		{diskrecovery.ErrNoUsage, http.StatusServiceUnavailable},
		{errors.New("boom"), http.StatusInternalServerError},
	}
	for _, c := range cases {
		eng := &fakeDiskRecoverEngine{err: c.err}
		h := diskRecoverHandler("ws-pw", "", eng)
		if rec := doDiskRecover(h, http.MethodPost, basicHdr("ws-pw"), map[string]any{}); rec.Code != c.want {
			t.Errorf("%v: want %d, got %d", c.err, c.want, rec.Code)
		}
	}
}

// PIN (bounded body): an oversized body is rejected, not truncated.
func TestDiskRecoverHandlerBoundedBody(t *testing.T) {
	eng := &fakeDiskRecoverEngine{}
	h := diskRecoverHandler("ws-pw", "", eng)
	req := httptest.NewRequest(http.MethodPost, "/v1/disk-recover", bytes.NewReader(bytes.Repeat([]byte("x"), 4096)))
	req.Header.Set("Authorization", basicHdr("ws-pw"))
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("oversized body: want 400, got %d", rec.Code)
	}
}

// PIN (engine construction): the production engine builds from the
// default runtime base and rejects an unknown base with an empty
// manifest (fail-safe, deletions impossible) rather than erroring —
// and the manifest self-validation must pass for the real base.
func TestNewDiskRecoverEngineDefaultBase(t *testing.T) {
	eng, err := newDiskRecoverEngine()
	if err != nil {
		t.Fatalf("default base must build: %v", err)
	}
	if eng == nil {
		t.Fatal("engine must be non-nil")
	}
}
