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

	"github.com/lenaxia/llmsafespaces/pkg/fsaudit"
)

// fakeFsAuditEngine records the last request and replies scripted.
type fakeFsAuditEngine struct {
	gotRequest fsaudit.Request
	calls      int
	report     fsaudit.Report
	err        error
}

func (f *fakeFsAuditEngine) Audit(_ context.Context, req fsaudit.Request) (fsaudit.Report, error) {
	f.calls++
	f.gotRequest = req
	return f.report, f.err
}

func doFsAudit(h http.HandlerFunc, method, auth string, body any) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, "/v1/fs-audit", &buf)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

// PIN (§D1 gate): no credentials and wrong credentials are rejected;
// BOTH the workspace password and the control-plane password are
// accepted (identical to every other API-driven user-mux route).
func TestFsAuditHandlerAuthGate(t *testing.T) {
	eng := &fakeFsAuditEngine{report: fsaudit.Report{NulFilled: []fsaudit.FileReport{}}}
	h := fsAuditHandler("ws-pw", "cp-pw", eng)

	if rec := doFsAudit(h, http.MethodPost, "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no auth: want 401, got %d", rec.Code)
	}
	if rec := doFsAudit(h, http.MethodPost, basicHdr("wrong"), nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password: want 401, got %d", rec.Code)
	}
	if rec := doFsAudit(h, http.MethodPost, basicHdr("ws-pw"), nil); rec.Code != http.StatusOK {
		t.Fatalf("workspace password: want 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	if eng.calls != 1 {
		t.Fatalf("engine calls after workspace-password hit: want 1, got %d", eng.calls)
	}
	if rec := doFsAudit(h, http.MethodPost, basicHdr("cp-pw"), nil); rec.Code != http.StatusOK {
		t.Fatalf("control-plane password: want 200, got %d", rec.Code)
	}
}

// PIN: the engine seam is only reached on POST — every other method is
// a 405 before any scan work.
func TestFsAuditHandlerMethodGate(t *testing.T) {
	eng := &fakeFsAuditEngine{}
	h := fsAuditHandler("ws-pw", "cp-pw", eng)
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		if rec := doFsAudit(h, method, basicHdr("ws-pw"), nil); rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s: want 405, got %d", method, rec.Code)
		}
	}
	if eng.calls != 0 {
		t.Fatalf("engine must not run for non-POST methods, got %d calls", eng.calls)
	}
}

// PIN (never a silent no-op): an unwired engine answers 503.
func TestFsAuditHandlerNilEngine(t *testing.T) {
	h := fsAuditHandler("ws-pw", "cp-pw", nil)
	if rec := doFsAudit(h, http.MethodPost, basicHdr("ws-pw"), nil); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil engine: want 503, got %d", rec.Code)
	}
}

// PIN (bounded-read convention #1561): an oversized body is a 400, and
// a body that is not one JSON object is a 400 — never a silent default
// sweep over attacker-shaped input.
func TestFsAuditHandlerBodyBounds(t *testing.T) {
	eng := &fakeFsAuditEngine{report: fsaudit.Report{NulFilled: []fsaudit.FileReport{}}}
	h := fsAuditHandler("ws-pw", "cp-pw", eng)

	if rec := doFsAudit(h, http.MethodPost, basicHdr("ws-pw"), json.RawMessage(`{"root": 42}`)); rec.Code != http.StatusBadRequest {
		t.Fatalf("type-mismatched body: want 400, got %d", rec.Code)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/fs-audit", bytes.NewReader(bytes.Repeat([]byte("x"), 4096)))
	req.Header.Set("Authorization", basicHdr("ws-pw"))
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("oversized body: want 400, got %d", rec.Code)
	}
	if eng.calls != 0 {
		t.Fatalf("engine must not run for a bad body, got %d calls", eng.calls)
	}
}

// PIN: an EMPTY body is a valid audit of the whole workspace base (the
// frontend button's zero-config call), and a body's caps are passed
// through to the engine verbatim.
func TestFsAuditHandlerEmptyBodyAndPassthrough(t *testing.T) {
	eng := &fakeFsAuditEngine{report: fsaudit.Report{NulFilled: []fsaudit.FileReport{}}}
	h := fsAuditHandler("ws-pw", "cp-pw", eng)

	rec := doFsAudit(h, http.MethodPost, basicHdr("ws-pw"), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("empty body: want 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	if eng.gotRequest != (fsaudit.Request{}) {
		t.Fatalf("empty body must default the request, got %+v", eng.gotRequest)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type: want application/json, got %q", ct)
	}

	rec = doFsAudit(h, http.MethodPost, basicHdr("ws-pw"), map[string]any{"root": "repo", "maxFileBytes": 4096, "maxTotalBytes": 8192})
	if rec.Code != http.StatusOK {
		t.Fatalf("shaped body: want 200, got %d", rec.Code)
	}
	if eng.gotRequest.Root != "repo" || eng.gotRequest.MaxFileBytes != 4096 || eng.gotRequest.MaxTotalBytes != 8192 {
		t.Fatalf("caps not passed verbatim: %+v", eng.gotRequest)
	}
	var decoded fsaudit.Report
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("200 body must be the report JSON: %v (%s)", err, rec.Body.String())
	}
}

// PIN (sentinel mapping): ErrBusy→409, deadline→504, anything
// else→500 — identical classes to the #1601 route.
func TestFsAuditHandlerErrorMapping(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{fsaudit.ErrBusy, http.StatusConflict},
		{context.DeadlineExceeded, http.StatusGatewayTimeout},
		{errors.New("boom"), http.StatusInternalServerError},
	}
	for _, tc := range cases {
		eng := &fakeFsAuditEngine{err: tc.err}
		h := fsAuditHandler("ws-pw", "cp-pw", eng)
		rec := doFsAudit(h, http.MethodPost, basicHdr("ws-pw"), nil)
		if rec.Code != tc.want {
			t.Fatalf("err %v: want %d, got %d", tc.err, tc.want, rec.Code)
		}
	}
}

// --- sidecar forwarding leg ---------------------------------------------

type fakeFsAuditClient struct {
	calls int
	req   fsaudit.Request
	rep   fsaudit.Report
	err   error
}

func (f *fakeFsAuditClient) FsAudit(_ context.Context, req fsaudit.Request) (fsaudit.Report, error) {
	f.calls++
	f.req = req
	return f.rep, f.err
}

// PIN: the sidecar forwarder refuses loudly when no control client is
// wired (never a silent zero-report), and relays verbatim otherwise.
func TestSocketFsAuditForwarding(t *testing.T) {
	s := socketFsAudit{}
	if _, err := s.Audit(context.Background(), fsaudit.Request{Root: "x"}); err == nil {
		t.Fatal("nil control client must error, not return a fake report")
	}

	rep := fsaudit.Report{FilesScanned: 7, NulFilledCount: 1, NulFilled: []fsaudit.FileReport{{Path: "dead", Size: 10, Verdict: fsaudit.VerdictNulFilled}}}
	fc := &fakeFsAuditClient{rep: rep}
	out, err := socketFsAudit{client: fc}.Audit(context.Background(), fsaudit.Request{Root: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if out.FilesScanned != 7 || out.NulFilledCount != 1 || len(out.NulFilled) != 1 {
		t.Fatalf("report not relayed verbatim: %+v", out)
	}
	if fc.req.Root != "x" {
		t.Fatalf("request not forwarded verbatim: %+v", fc.req)
	}
}

// --- control-socket dispatch leg -----------------------------------------

func controlReqFsAudit(params any) controlRequest {
	raw, _ := json.Marshal(params)
	var p map[string]any
	_ = json.Unmarshal(raw, &p)
	return controlRequest{V: controlProtocolVersion, Method: "fs_audit", Params: p}
}

// PIN: the supervisor control method decodes {root, max_file_bytes,
// max_total_bytes} explicitly-over-implicitly (an undecodable params
// shape is bad_request, never a silently-defaulted scan) and returns
// the full report as the result map.
func TestFsAuditControlMethod(t *testing.T) {
	srv := &controlSocketServer{}
	resp := srv.fsAuditControlMethod(context.Background(), nil, controlReqFsAudit(map[string]any{"root": "!not-a-string!"}))
	if resp.Error == nil || resp.Error.Code != "bad_request" {
		t.Fatalf("undecodable params: want bad_request, got %+v", resp.Error)
	}

	rep := fsaudit.Report{FilesScanned: 3, NulFilledCount: 2, NulFilled: []fsaudit.FileReport{{Path: "a"}, {Path: "b"}}}
	eng := &fakeFsAuditEngine{report: rep}
	srv = &controlSocketServer{fsAudit: eng}
	resp = srv.fsAuditControlMethod(context.Background(), nil, controlReqFsAudit(map[string]any{"root": "repo", "max_file_bytes": 4096, "max_total_bytes": 8192}))
	if resp.Error != nil {
		t.Fatalf("happy path errored: %+v", resp.Error)
	}
	if eng.calls != 1 || eng.gotRequest.Root != "repo" || eng.gotRequest.MaxFileBytes != 4096 || eng.gotRequest.MaxTotalBytes != 8192 {
		t.Fatalf("params not decoded into the engine request: calls=%d %+v", eng.calls, eng.gotRequest)
	}
	res, _ := json.Marshal(resp.Result)
	var back fsaudit.Report
	if err := json.Unmarshal(res, &back); err != nil {
		t.Fatalf("result map must round-trip the report: %v", err)
	}
	if back.FilesScanned != 3 || back.NulFilledCount != 2 {
		t.Fatalf("report lost in map round-trip: %+v", back)
	}
}

// PIN: nil engine on the supervisor answers internal (a wiring bug,
// never a silent no-op), and busy/timeout ride the closed error-code
// enum back as typed codes.
func TestFsAuditControlMethodErrorCodes(t *testing.T) {
	srv := &controlSocketServer{}
	if resp := srv.fsAuditControlMethod(context.Background(), nil, controlReqFsAudit(nil)); resp.Error == nil || resp.Error.Code != "internal" {
		t.Fatalf("nil engine: want internal, got %+v", resp.Error)
	}
	for _, tc := range []struct {
		err  error
		code string
	}{
		{fsaudit.ErrBusy, "busy"},
		{context.DeadlineExceeded, "timeout"},
		{errors.New("boom"), "internal"},
	} {
		srv := &controlSocketServer{fsAudit: &fakeFsAuditEngine{err: tc.err}}
		resp := srv.fsAuditControlMethod(context.Background(), nil, controlReqFsAudit(nil))
		if resp.Error == nil || resp.Error.Code != tc.code {
			t.Fatalf("err %v: want code %q, got %+v", tc.err, tc.code, resp.Error)
		}
	}
}

// PIN: the control client maps the closed error-code enum back to the
// engine's typed sentinel errors (busy), through a REAL control-socket
// server over loopback — the JSON framing both ways is exercised.
func TestControlClientFsAuditErrorMapping(t *testing.T) {
	srv := newControlSocketServerWithProc(t, "127.0.0.1:0", &fakeRestartProc{})
	srv.fsAudit = &fakeFsAuditEngine{err: fsaudit.ErrBusy}
	go func() { _ = srv.serve() }()

	c := newControlClient(srv.addr())
	if _, err := c.FsAudit(context.Background(), fsaudit.Request{}); !errors.Is(err, fsaudit.ErrBusy) {
		t.Fatalf("busy code must map to ErrBusy, got %v", err)
	}

	srv.fsAudit = &fakeFsAuditEngine{err: context.DeadlineExceeded}
	if _, err := c.FsAudit(context.Background(), fsaudit.Request{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout code must map to DeadlineExceeded, got %v", err)
	}
}
