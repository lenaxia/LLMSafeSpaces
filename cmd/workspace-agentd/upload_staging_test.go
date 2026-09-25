// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// upload_staging_test.go — design 0060 §4/§6 pins for the sidecar
// staging leg: two-clause admission, declared-bytes gates
// (411/over-read), the unlink-before-release lifecycle, hygiene
// (boot scrub + TTL finalize), and the handler-level flow with a fake
// apply seam.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	agentdpkg "github.com/lenaxia/llmsafespaces/pkg/agentd"
)

// testUUIDSeq mints unique, uuid-shaped ids for the fixture's uuid seam.
var testUUIDCounter atomic.Int64

func testUUIDSeq() string {
	n := testUUIDCounter.Add(1)
	const hexdigits = "0123456789abcdef"
	b := make([]byte, 12)
	for i := 11; i >= 0; i-- {
		b[i] = hexdigits[n%16]
		n /= 16
	}
	return string(b)
}

type agentdFileUploadResponse = agentdpkg.FileUploadResponse

const agentdAuthUsername = agentdpkg.AuthUsername

// recordingStagingMetrics captures the §4.6 seam calls.
type recordingStagingMetrics struct {
	mu       sync.Mutex
	gauges   int
	inBytes  int64
	outBytes int64
}

func (m *recordingStagingMetrics) RecordStagingGauges(stagedBytes, reservedBytes, credentialBytes int64, files int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gauges++
}

// gaugeCount is the locked read for tests that poll from another
// goroutine (the sweep loop pushes under m.mu — #1532's -race proofs).
func (m *recordingStagingMetrics) gaugeCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.gauges
}

func (m *recordingStagingMetrics) RecordUploadBytes(direction string, n int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if direction == "staged_in" {
		m.inBytes += n
	} else {
		m.outBytes += n
	}
}

func (m *recordingStagingMetrics) RecordScrubbed(files int) {}

// fakeStatfs gives tests direct control of f_bavail (Bsize=1 so Bavail
// IS bytes).
type fakeStatfs struct {
	avail int64
	err   error
}

func (f fakeStatfs) statfs(string) (*statfsT, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &statfsT{Bavail: uint64(f.avail), Bsize: 1}, nil
}

func testStagingConfig(t *testing.T, budget, floor int64, maxConc int) stagingConfig {
	t.Helper()
	return stagingConfig{
		stagingDir:      t.TempDir() + "/staged-upload-files",
		budget:          budget,
		credentialFloor: floor,
		ttl:             15 * time.Minute,
		maxConcurrent:   maxConc,
		applyTimeout:    50 * time.Millisecond,
		uuid: func() string {
			return "00000000-0000-4000-8000-" + testUUIDSeq()
		},
	}
}

// --- Admission (0060 §4.1, clause by clause) ---

func TestStagingAdmission_TwoClauses(t *testing.T) {
	cfg := testStagingConfig(t, 100, 20, 4)
	cfg.statfs = fakeStatfs{avail: 120}.statfs
	s := newUploadStager(cfg, nil)

	// Clause A: reservations + new ≤ budget. A second 60 with 50 held → 110 > 100.
	if c := s.Admit("id-a", 50); c != "" {
		t.Fatalf("first admission should pass, got %q", c)
	}
	if c := s.Admit("id-b", 60); c != rejectStagingFull {
		t.Fatalf("clause A must reject 50+60>100, got %q", c)
	}
	s.Release("id-a")
	// Clause B at the exact boundary: 0 cred + 20 floor + 50 + 50 = 120 = avail.
	if c := s.Admit("id-c", 50); c != "" {
		t.Fatalf("clause B satisfied at the boundary should pass, got %q", c)
	}
	// id-c STAYS held: the +1-cred rejection below needs its 50 reserved.
	// Plant 1 credential byte: 1 + 20 + 50 + 50 = 121 > 120 → reject.
	credFile := filepath.Join(filepath.Dir(cfg.stagingDir), "secrets-env")
	if err := os.MkdirAll(filepath.Dir(credFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(credFile, make([]byte, 1), 0o600); err != nil {
		t.Fatal(err)
	}
	if c := s.Admit("id-d", 50); c != rejectStagingFull {
		t.Fatalf("clause B must reject with +1 credential byte over, got %q", c)
	}
}

func TestStagingAdmission_ClauseABinds(t *testing.T) {
	// Clause A must bind on its own: f_bavail far from binding, budget
	// exhausted by held reservations (the masked-mutation pin — clause
	// B alone would admit).
	cfg := testStagingConfig(t, 100, 0, 8)
	cfg.statfs = fakeStatfs{avail: 100000}.statfs
	s := newUploadStager(cfg, nil)
	if c := s.Admit("a", 80); c != "" {
		t.Fatalf("1st: %q", c)
	}
	if c := s.Admit("b", 30); c != rejectStagingFull {
		t.Fatalf("80+30 > 100 must reject on clause A alone, got %q", c)
	}
}

func TestStagingAdmission_ConcurrencyCap(t *testing.T) {
	cfg := testStagingConfig(t, 1000, 0, 2)
	cfg.statfs = fakeStatfs{avail: 100000}.statfs
	s := newUploadStager(cfg, nil)
	if c := s.Admit("a", 10); c != "" {
		t.Fatalf("1st: %q", c)
	}
	if c := s.Admit("b", 10); c != "" {
		t.Fatalf("2nd: %q", c)
	}
	if c := s.Admit("c", 10); c != rejectStagingBusy {
		t.Fatalf("3rd must be the 429 class even with budget free, got %q", c)
	}
}

func TestStagingAdmission_CountCapPreemptsBudgetAtTheBoundary(t *testing.T) {
	// The §6.6 double-violation point, on the shipped cap/budget
	// arithmetic the nightly's SR-6 storm exercises: cap=4,
	// budget=48MiB, 5×10MiB (clause B non-binding by construction:
	// floor=0 and a huge fake f_bavail, unlike the shipped 24MiB
	// floor — immaterial here, this pin is the A-vs-cap precedence).
	// The 5th Admit violates BOTH clauses (40+10 > 48 budget AND
	// len=4 ≥ cap) — §6.6 fixes the class: "the count cap's default IS
	// 4 — higher concurrency is unreachable by design … the 5th
	// concurrent upload's 429". A 507 here (budget-first order) tells
	// the client to wait for tmpfs when the binding constraint is
	// concurrency — the distinct-client-recovery contract §4.6 keeps
	// these classes apart to uphold. Regression pin for the nightly
	// SR-6 failure (runs 35872827066/36011237476/36026824047: the
	// pre-#1545 literal 429 in that row was the apply-busy TryLock
	// (#1539), not the cap; with the lock gone, budget-first ordering
	// left §6.6's boundary 507ing).
	const MiB = int64(1 << 20)
	cfg := testStagingConfig(t, 48*MiB, 0, 4) // the shipped defaults
	cfg.statfs = fakeStatfs{avail: 100000 * MiB}.statfs
	s := newUploadStager(cfg, nil)
	for i, id := range []string{"a", "b", "c", "d"} {
		if c := s.Admit(id, 10*MiB); c != "" {
			t.Fatalf("admission %d must pass (40MiB ≤ 48MiB budget, len %d < 4): %q", i+1, i, c)
		}
	}
	if c := s.Admit("e", 10*MiB); c != rejectStagingBusy {
		t.Fatalf("5th concurrent at the cap must be the 429 class even though 40+10MiB > 48MiB (§6.6 cap boundary), got %q", c)
	}
}

func TestStagingAdmission_StatfsFailureRejects(t *testing.T) {
	cfg := testStagingConfig(t, 100, 0, 4)
	cfg.statfs = fakeStatfs{err: errors.New("boom")}.statfs
	s := newUploadStager(cfg, nil)
	if c := s.Admit("a", 1); c != rejectStagingFull {
		t.Fatalf("unstatable tmpfs must reject (the safe direction), got %q", c)
	}
}

func TestStagingReconcileDown(t *testing.T) {
	cfg := testStagingConfig(t, 100, 0, 4)
	cfg.statfs = fakeStatfs{avail: 100000}.statfs
	s := newUploadStager(cfg, nil)
	_ = s.Admit("a", 80)
	s.ReconcileDown("a", 30)
	if got := s.ReservedBytes(); got != 30 {
		t.Fatalf("reservation must reconcile down to 30, got %d", got)
	}
	// Freed headroom admits what the declared bound would have refused.
	if c := s.Admit("b", 70); c != "" {
		t.Fatalf("post-reconcile admission should pass, got %q", c)
	}
}

// --- Lifecycle (0060 §4.1: unlink-before-release, §3.3 timeout-hold) ---

func TestStagingLifecycle_AbortUnlinksBeforeRelease(t *testing.T) {
	cfg := testStagingConfig(t, 100, 0, 4)
	cfg.statfs = fakeStatfs{avail: 100000}.statfs
	s := newUploadStager(cfg, nil)
	_ = s.Admit("a", 10)

	// Stage a real object, then abort: the file must be GONE and the
	// reservation released (the §6.1 no-crash walked bound rests on this).
	if _, _, err := s.stageStream(context.Background(), strings.NewReader("0123456789"), "a", 10); err != nil {
		t.Fatalf("stageStream: %v", err)
	}
	if _, files := s.StagedBytesAndFiles(); files != 1 {
		t.Fatalf("expected 1 staged object, got %d", files)
	}
	s.abortStaged("a")
	if _, files := s.StagedBytesAndFiles(); files != 0 {
		t.Fatalf("abort must unlink the staged object, %d remain", files)
	}
	if got := s.ReservedBytes(); got != 0 {
		t.Fatalf("abort must release the reservation, got %d", got)
	}
}

func TestStagingLifecycle_TimeoutHoldsReservation(t *testing.T) {
	cfg := testStagingConfig(t, 100, 0, 4)
	cfg.statfs = fakeStatfs{avail: 100000}.statfs
	s := newUploadStager(cfg, nil)
	_ = s.Admit("a", 10)
	if _, _, err := s.stageStream(context.Background(), strings.NewReader("0123456789"), "a", 10); err != nil {
		t.Fatalf("stageStream: %v", err)
	}
	// The §3.3 504 path: staged object stays AND reservation holds.
	if got := s.ReservedBytes(); got != 10 {
		t.Fatalf("reservation must hold at 10, got %d", got)
	}
	if _, files := s.StagedBytesAndFiles(); files != 1 {
		t.Fatalf("staged object must remain for the TTL tail, got %d files", files)
	}
}

// --- Hygiene (0060 §4.3) ---

func TestStagingScrub_BootAndTTL(t *testing.T) {
	cfg := testStagingConfig(t, 1000, 0, 4)
	s := newUploadStager(cfg, nil)

	old := filepath.Join(cfg.stagingDir, "old-id")
	fresh := filepath.Join(cfg.stagingDir, "fresh-id")
	if err := os.MkdirAll(cfg.stagingDir, 0o750); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{old, fresh, cfg.stagingDir + "/part-id.tmp"} {
		if err := os.WriteFile(p, []byte("x"), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().Add(-2 * cfg.ttl)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}

	// TTL scrub removes the AGED object only (the fresh object and the
	// fresh .tmp part both survive — age-gated, not name-gated).
	if n := s.scrubStagingDir(cfg.ttl, time.Now()); n != 1 {
		t.Fatalf("TTL scrub should remove only the aged object, got %d", n)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("fresh object must survive the TTL scrub: %v", err)
	}
	if _, err := os.Stat(cfg.stagingDir + "/part-id.tmp"); err != nil {
		t.Fatalf("fresh .tmp must survive the TTL scrub: %v", err)
	}

	// Boot scrub (ttl=0) removes everything — a fresh incarnation's
	// in-flight uploads died with the previous process (§4.3).
	if n := s.scrubStagingDir(0, time.Now()); n != 2 {
		t.Fatalf("boot scrub should remove the remainder (fresh + .tmp), got %d", n)
	}
}

func TestStagingScrub_FinalizesHeldReservation(t *testing.T) {
	cfg := testStagingConfig(t, 1000, 0, 4)
	cfg.statfs = fakeStatfs{avail: 100000}.statfs
	s := newUploadStager(cfg, nil)
	_ = s.Admit("held-id", 10)
	// A held 504 reservation: the scrub of its object finalizes the hold.
	if err := os.MkdirAll(cfg.stagingDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.stagingDir, "held-id"), []byte("x"), 0o640); err != nil {
		t.Fatal(err)
	}
	s.scrubStagingDir(0, time.Now())
	if got := s.ReservedBytes(); got != 0 {
		t.Fatalf("scrub must finalize the held reservation, got %d", got)
	}
}

// --- Staged stream (0060 §4.1 hard read-cap) ---

func TestStagingStream_OverReadRejected(t *testing.T) {
	cfg := testStagingConfig(t, 100, 0, 4)
	s := newUploadStager(cfg, nil)
	// Declares 5, sends 10 → errDeclaredExceeded, nothing staged.
	_, _, err := s.stageStream(context.Background(), strings.NewReader("0123456789"), "a", 5)
	if !errors.Is(err, errDeclaredExceeded) {
		t.Fatalf("expected errDeclaredExceeded, got %v", err)
	}
	if _, files := s.StagedBytesAndFiles(); files != 0 {
		t.Fatalf("over-read must leave nothing staged, got %d", files)
	}
	if _, err := os.Stat(s.partPath("a")); !os.IsNotExist(err) {
		t.Fatalf("the .part must be gone after the over-read abort: %v", err)
	}
}

func TestStagingStream_HashAndSize(t *testing.T) {
	cfg := testStagingConfig(t, 100, 0, 4)
	s := newUploadStager(cfg, nil)
	size, digest, err := s.stageStream(context.Background(), strings.NewReader("hello world"), "a", 64)
	if err != nil {
		t.Fatal(err)
	}
	if size != 11 {
		t.Fatalf("size: got %d", size)
	}
	if len(digest) != 64 {
		t.Fatalf("sha256 hex length: got %d", len(digest))
	}
	if _, err := os.Stat(s.stagedPath("a")); err != nil {
		t.Fatalf("completed staged object must exist post-rename: %v", err)
	}
}

// --- Handler-level flow (fake apply seam) ---

func stagingHandlerFixture(t *testing.T, budget int64, avail int64) (*uploadStager, *uploadApplyRecorder, http.HandlerFunc, *recordingStagingMetrics) {
	t.Helper()
	cfg := testStagingConfig(t, budget, 0, 4)
	cfg.statfs = fakeStatfs{avail: avail}.statfs
	m := &recordingStagingMetrics{}
	stager := newUploadStager(cfg, m)
	rec := &uploadApplyRecorder{}
	return stager, rec, uploadFilesHandler(nil, fileUploadConfig{
		uploadsDir:  t.TempDir(),
		maxBytes:    25 << 20,
		bodyTimeout: 5 * time.Second,
	}, "pw", stager, rec.apply), m
}

type uploadApplyRecorder struct {
	mu    sync.Mutex
	calls []uploadApplyRequest
	// Result/Err returned per call; defaults to a successful apply.
	Result *uploadApplyResult
	Err    *uploadApplyError
	// Delay, when set, sleeps inside the apply (timeout tests).
	Delay time.Duration
}

func (r *uploadApplyRecorder) apply(ctx context.Context, req uploadApplyRequest) (*uploadApplyResult, *uploadApplyError) {
	r.mu.Lock()
	r.calls = append(r.calls, req)
	res, err, delay := r.Result, r.Err, r.Delay
	r.mu.Unlock()
	if delay > 0 {
		select {
		case <-ctx.Done():
			return nil, &uploadApplyError{Code: "transport", Message: "timeout", cause: ctx.Err()}
		case <-time.After(delay):
		}
	}
	if err != nil {
		return nil, err
	}
	if res != nil {
		return res, nil
	}
	return &uploadApplyResult{Applied: true, Path: "/workspace/uploads/" + req.UploadID + "-" + req.TargetName, Size: req.Size}, nil
}

func stagingRequest(t *testing.T, body string, declared string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, "/v1/files?filename=notes.txt", strings.NewReader(body))
	req.SetBasicAuth(agentdAuthUsername, "pw")
	if declared != "" {
		req.Header.Set("X-LLS-Declared-Body-Bytes", declared)
	}
	return req
}

func decodeStagingBody(t *testing.T, w *httptest.ResponseRecorder) (int, uploadErrorResponse) {
	t.Helper()
	var resp uploadErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode body %q: %v", w.Body.String(), err)
	}
	return w.Code, resp
}

func TestStagedUpload_HappyPath(t *testing.T) {
	stager, rec, h, m := stagingHandlerFixture(t, 1000, 100000)
	w := httptest.NewRecorder()
	h(w, stagingRequest(t, "hello", "5"))

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var uploaded agentdFileUploadResponse
	if err := json.Unmarshal(w.Body.Bytes(), &uploaded); err != nil {
		t.Fatal(err)
	}
	if uploaded.Size != 5 || uploaded.Name != "notes.txt" {
		t.Fatalf("201 shape: %+v", uploaded)
	}
	if len(rec.calls) != 1 || rec.calls[0].Size != 5 || rec.calls[0].TargetName != "notes.txt" {
		t.Fatalf("apply call: %+v", rec.calls)
	}
	if len(rec.calls[0].SHA256) != 64 {
		t.Fatalf("apply must carry the sha256, got %q", rec.calls[0].SHA256)
	}
	// Ack lifecycle: staged bytes gone, reservation released.
	if _, files := stager.StagedBytesAndFiles(); files != 0 {
		t.Fatalf("post-ack staged files: %d", files)
	}
	if stager.ReservedBytes() != 0 {
		t.Fatalf("post-ack reservations: %d", stager.ReservedBytes())
	}
	// §4.6 bytes accounting: staged_in and copied_out both counted.
	if m.inBytes != 5 || m.outBytes != 5 {
		t.Fatalf("bytes accounting: in=%d out=%d (want 5/5)", m.inBytes, m.outBytes)
	}
}

func TestStagedUpload_MissingDeclaredHeader411(t *testing.T) {
	_, _, h, _ := stagingHandlerFixture(t, 1000, 100000)
	w := httptest.NewRecorder()
	h(w, stagingRequest(t, "hello", ""))
	code, resp := decodeStagingBody(t, w)
	if code != http.StatusLengthRequired {
		t.Fatalf("missing header must 411, got %d", code)
	}
	if resp.Reason != "invalid_declared_length" {
		t.Fatalf("reason: %q", resp.Reason)
	}
}

func TestStagedUpload_InvalidDeclaredHeader411(t *testing.T) {
	_, _, h, _ := stagingHandlerFixture(t, 1000, 100000)
	for _, bad := range []string{"abc", "-5", "0"} {
		w := httptest.NewRecorder()
		h(w, stagingRequest(t, "hello", bad))
		if w.Code != http.StatusLengthRequired {
			t.Fatalf("invalid header %q must 411, got %d", bad, w.Code)
		}
	}
}

func TestStagedUpload_OverRead400(t *testing.T) {
	stager, _, h, _ := stagingHandlerFixture(t, 1000, 100000)
	w := httptest.NewRecorder()
	h(w, stagingRequest(t, "0123456789", "5"))
	code, resp := decodeStagingBody(t, w)
	if code != http.StatusBadRequest {
		t.Fatalf("over-read must 400, got %d", code)
	}
	if resp.Reason != "declared_length_exceeded" {
		t.Fatalf("reason: %q (the design-pinned 400 literal)", resp.Reason)
	}
	// Nothing staged, nothing reserved (§4.1: unreserved bytes can never land).
	if _, files := stager.StagedBytesAndFiles(); files != 0 {
		t.Fatalf("over-read left %d staged files", files)
	}
	if stager.ReservedBytes() != 0 {
		t.Fatalf("over-read left %d reserved", stager.ReservedBytes())
	}
}

func TestStagedUpload_BudgetExhausted507(t *testing.T) {
	_, rec, h, _ := stagingHandlerFixture(t, 4, 100000)
	w := httptest.NewRecorder()
	h(w, stagingRequest(t, "hello", "5"))
	code, resp := decodeStagingBody(t, w)
	if code != http.StatusInsufficientStorage {
		t.Fatalf("budget 4 < declared 5 must 507, got %d", code)
	}
	if resp.Reason != "staging_full" {
		t.Fatalf("reason: %q", resp.Reason)
	}
	if len(rec.calls) != 0 {
		t.Fatalf("no apply may run on admission rejection")
	}
}

func TestStagedUpload_CountCapBusy429(t *testing.T) {
	// The admission-busy arm at the handler level (§4.6: cap → 429
	// staging_busy), pinned because the nightly SR-6 row was the only
	// thing catching a wrong rejection CLASS at the boundary — the
	// apply-busy test shares only the status mapping, not this branch
	// (upload_staging.go's Admit-busy → 429). Budget and f_bavail huge:
	// the cap is the ONLY binding clause.
	s, rec, h, _ := stagingHandlerFixture(t, 1<<30, 1<<30)
	for i, id := range []string{"held-1", "held-2", "held-3", "held-4"} {
		if c := s.Admit(id, 1); c != "" {
			t.Fatalf("held admission %d: %q", i+1, c)
		}
	}
	w := httptest.NewRecorder()
	h(w, stagingRequest(t, "hello", "5"))
	code, resp := decodeStagingBody(t, w)
	if code != http.StatusTooManyRequests || resp.Reason != "staging_busy" {
		t.Fatalf("5th concurrent at the cap must 429 staging_busy (§6.6 boundary), got %d %q", code, resp.Reason)
	}
	if len(rec.calls) != 0 {
		t.Fatalf("no apply may run on admission rejection")
	}
	s.Release("held-1")
	w2 := httptest.NewRecorder()
	h(w2, stagingRequest(t, "hello", "5"))
	if code, _ := decodeStagingBody(t, w2); code != http.StatusCreated {
		t.Fatalf("after a release the slot reopens (retryable 429), got %d", code)
	}
}

func TestStagedUpload_ClauseB507(t *testing.T) {
	// Budget large, f_bavail (4) below the declared (5): clause B rejects.
	_, _, h, _ := stagingHandlerFixture(t, 1000, 4)
	w := httptest.NewRecorder()
	h(w, stagingRequest(t, "hello", "5"))
	code, resp := decodeStagingBody(t, w)
	if code != http.StatusInsufficientStorage || resp.Reason != "staging_full" {
		t.Fatalf("clause B: got %d %q", code, resp.Reason)
	}
}

func TestStagedUpload_ApplyTimeout504HoldsStaged(t *testing.T) {
	stager, rec, h, _ := stagingHandlerFixture(t, 1000, 100000)
	rec.mu.Lock()
	rec.Delay = 500 * time.Millisecond // applyTimeout is 50ms in the fixture
	rec.mu.Unlock()
	w := httptest.NewRecorder()
	h(w, stagingRequest(t, "hello", "5"))
	code, resp := decodeStagingBody(t, w)
	if code != http.StatusGatewayTimeout {
		t.Fatalf("apply timeout must 504, got %d", code)
	}
	if resp.Reason != "apply_timeout" {
		t.Fatalf("reason: %q", resp.Reason)
	}
	// §3.3: staged object REMAINS, reservation HOLDS.
	if _, files := stager.StagedBytesAndFiles(); files != 1 {
		t.Fatalf("timeout must leave the staged object, got %d", files)
	}
	if stager.ReservedBytes() != 5 {
		t.Fatalf("timeout must hold the reservation, got %d", stager.ReservedBytes())
	}
}

func TestStagedUpload_ApplyRejected507WithCode(t *testing.T) {
	stager, rec, h, _ := stagingHandlerFixture(t, 1000, 100000)
	rec.mu.Lock()
	rec.Err = &uploadApplyError{Code: "checksum_mismatch", Message: "digest mismatch"}
	rec.mu.Unlock()
	w := httptest.NewRecorder()
	h(w, stagingRequest(t, "hello", "5"))
	code, resp := decodeStagingBody(t, w)
	if code != http.StatusInsufficientStorage {
		t.Fatalf("apply rejection must 507, got %d", code)
	}
	if resp.Reason != "apply_rejected" || resp.Code != "checksum_mismatch" {
		t.Fatalf("shape: reason=%q code=%q", resp.Reason, resp.Code)
	}
	// Abort lifecycle: unlinked + released.
	if _, files := stager.StagedBytesAndFiles(); files != 0 {
		t.Fatalf("apply rejection must unlink, got %d", files)
	}
	if stager.ReservedBytes() != 0 {
		t.Fatalf("apply rejection must release, got %d", stager.ReservedBytes())
	}
}

func TestStagedUpload_ApplyBusy429(t *testing.T) {
	_, rec, h, _ := stagingHandlerFixture(t, 1000, 100000)
	rec.mu.Lock()
	rec.Err = &uploadApplyError{Code: "busy", Message: "supervisor queue full"}
	rec.mu.Unlock()
	w := httptest.NewRecorder()
	h(w, stagingRequest(t, "hello", "5"))
	code, resp := decodeStagingBody(t, w)
	if code != http.StatusTooManyRequests || resp.Reason != "staging_busy" {
		t.Fatalf("busy shape: got %d %q (design §4.6: busy → 429 staging_busy)", code, resp.Reason)
	}
}

func TestStagedUpload_MarginObserved(t *testing.T) {
	_, rec, h, _ := stagingHandlerFixture(t, 1000, 100000)
	rec.mu.Lock()
	rec.Result = &uploadApplyResult{Applied: true, Path: "/workspace/uploads/x-notes.txt", Size: 5, MarginConsumed: true}
	rec.mu.Unlock()
	w := httptest.NewRecorder()
	h(w, stagingRequest(t, "hello", "5"))
	if w.Code != http.StatusCreated {
		t.Fatalf("margin-consumed is an observation, not a rejection: got %d", w.Code)
	}
}

func TestStagedUpload_ApplyDestDiskFull507(t *testing.T) {
	_, rec, h, _ := stagingHandlerFixture(t, 1000, 100000)
	rec.mu.Lock()
	rec.Err = &uploadApplyError{Code: "dest_disk_full", Message: "no space"}
	rec.mu.Unlock()
	w := httptest.NewRecorder()
	h(w, stagingRequest(t, "hello", "5"))
	code, resp := decodeStagingBody(t, w)
	if code != http.StatusInsufficientStorage || resp.Reason != "dest_disk_full" {
		t.Fatalf("dest-disk shape: got %d %q", code, resp.Reason)
	}
}

func TestStagedUpload_DeclaredOverCap413(t *testing.T) {
	_, _, h, _ := stagingHandlerFixture(t, 1<<30, 1<<40)
	// The declared value is envelope-inclusive: cap + allowance passes
	// (a cap-exact file's multipart total), cap + allowance + 1 rejects.
	over := int64(25<<20) + (64 << 10) + 1
	w := httptest.NewRecorder()
	h(w, stagingRequest(t, "hello", strconv.FormatInt(over, 10)))
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("declared > cap+allowance must 413 pre-admission, got %d", w.Code)
	}
}

func TestStagedUpload_CapExactWithEnvelopeAdmitted(t *testing.T) {
	// A file at exactly the cap declares cap + envelope on the API hop —
	// must NOT 413 at agentd (the envelope allowance mirrors the API's
	// pre-read gate).
	_, _, h, _ := stagingHandlerFixture(t, 1<<30, 1<<40)
	exact := int64(25<<20) + (64 << 10) // cap + allowance exactly — the multipart total of a cap-exact file
	w := httptest.NewRecorder()
	h(w, stagingRequest(t, "hello", strconv.FormatInt(exact, 10)))
	if w.Code == http.StatusRequestEntityTooLarge {
		t.Fatalf("cap-exact file with envelope must pass the agentd gate (the divergence worker 2's cross-check caught), got %d", w.Code)
	}
}

// --- The production apply seam (the r1-review reproduction made permanent) ---

// TestUploadApplyClient_LongCopyWithinBudgetSucceeds is the r2 review's
// empirical reproduction, green: a 2.5s copy under a 5s apply budget —
// strictly BEYOND the 2s control-plane default, inside the apply
// budget (the production shape: 2s default, 60s apply timeout) — must
// SUCCEED. The r1 code died at the 2s conn deadline and misrouted to
// abort+507; the r2 code's inverted min kept dying; the r3 parameter-
// ization (300ms, which never crossed the bug's bound and passed
// against the buggy code) is rejected — the delay must stay > the
// default. The ctx deadline IS the bound now.
func TestUploadApplyClient_LongCopyWithinBudgetSucceeds(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		conn, aerr := ln.Accept()
		if aerr != nil {
			return
		}
		_, _ = io.ReadFull(conn, make([]byte, 1))
		// The delay MUST exceed the 2s control-plane default (the bug's
		// bound) while staying inside the 5s apply budget — the r3-rejected
		// 300ms parameterization passed against the buggy code too.
		time.Sleep(2500 * time.Millisecond)
		_ = json.NewEncoder(conn).Encode(map[string]any{
			"v": 1, "id": 1,
			"result": map[string]any{"applied": true, "path": "/workspace/uploads/x-n", "size": 1},
		})
		_ = conn.Close()
	}()

	c := newControlClient(ln.Addr().String()) // 2s default — must NOT bound this call
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, aerr := c.UploadApply(ctx, uploadApplyRequest{UploadID: "id", StagedName: "id", Size: 1, SHA256: "x", TargetName: "n"})
	if aerr != nil {
		t.Fatalf("a copy within the apply budget must succeed, got %+v", aerr)
	}
	if res == nil || !res.Applied {
		t.Fatalf("result: %+v", res)
	}
}

// TestUploadApplyClient_BeyondBudgetIsTheTimeoutClass: the copy
// outlives the apply budget → the failure classifies deterministically
// as §3.3 timeout (the conn arm IS the ctx arm — no clock race).
func TestUploadApplyClient_BeyondBudgetIsTheTimeoutClass(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		conn, aerr := ln.Accept()
		if aerr != nil {
			return
		}
		time.Sleep(2 * time.Second) // outlive the 150ms budget decisively
		_ = conn.Close()
	}()

	c := newControlClient(ln.Addr().String())
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	_, aerr := c.UploadApply(ctx, uploadApplyRequest{UploadID: "id", StagedName: "id", Size: 1, SHA256: "x", TargetName: "n"})
	if aerr == nil {
		t.Fatal("expected the timeout class")
	}
	if !isApplyTimeout(aerr) {
		t.Fatalf("beyond-budget apply must classify as timeout (§3.3), got code=%q err=%v", aerr.Code, aerr)
	}
	if !errors.Is(aerr, context.DeadlineExceeded) {
		t.Fatalf("the cause must be the deadline, got %v", aerr.Unwrap())
	}
}

// TestUploadApplyClient_ClosedEnumErrorMapping pins the supervisor's
// error-code mapping through the real client wire path.
func TestUploadApplyClient_ClosedEnumErrorMapping(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		conn, aerr := ln.Accept()
		if aerr != nil {
			return
		}
		_, _ = io.ReadFull(conn, make([]byte, 1)) // let the request land
		_ = json.NewEncoder(conn).Encode(map[string]any{
			"v": 1, "id": 1,
			"error": map[string]any{"code": "checksum_mismatch", "message": "digest mismatch"},
		})
		_ = conn.Close()
	}()

	c := newControlClient(ln.Addr().String())
	_, aerr := c.UploadApply(context.Background(), uploadApplyRequest{UploadID: "id", StagedName: "id", Size: 1, SHA256: "x", TargetName: "n"})
	if aerr == nil || aerr.Code != "checksum_mismatch" {
		t.Fatalf("closed-enum mapping: got %+v", aerr)
	}
}

// TestStagingAdmission_ConcurrentNeverExceedsBudget is the §7
// semaphore race pin: N goroutines racing Admit under a tight budget —
// the reserved total can never exceed clause (A)'s bound.
func TestStagingAdmission_ConcurrentNeverExceedsBudget(t *testing.T) {
	cfg := testStagingConfig(t, 100, 0, 64)
	cfg.statfs = fakeStatfs{avail: 1 << 30}.statfs
	s := newUploadStager(cfg, nil)

	const racers = 32
	var wg sync.WaitGroup
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = s.Admit(fmt.Sprintf("racer-%d", i), 7) // 32×7=224 ≫ 100: most must lose
		}(i)
	}
	wg.Wait()
	if got := s.ReservedBytes(); got > 100 {
		t.Fatalf("racing admissions exceeded the budget: reserved=%d > 100", got)
	}
	if got := len(s.reservations); got > 64 {
		t.Fatalf("concurrency cap exceeded: %d", got)
	}
}

// TestBuildSidecarDeps_NoErrorLogsWithoutTmpfs pins the CI failure's
// fix THROUGH the real wiring: buildSidecarDeps with a file where the
// staging parent should be must NOT error-log (the harness captured
// "boot dir establish failed" on every CI runner before the guard).
// Mutation-verified class: the guard removed from buildSidecarDeps
// makes the captured log non-empty (the stager-level helper tests in
// the sibling pin cover both helper arms).
func TestBuildSidecarDeps_NoErrorLogsWithoutTmpfs(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LLMSAFESPACES_UPLOADS_STAGING_PATH", blocker+"/staged-upload-files")
	t.Setenv("AGENTD_CONTROL_PLANE_PASSWORD", "pw")

	var buf bytes.Buffer
	prev := log
	log = zap.New(zapcore.NewCore(zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig()), zapcore.AddSync(&buf), zap.ErrorLevel))
	defer func() { log = prev }()

	_ = buildSidecarDeps(sidecarConfig{password: "pw", controlAddr: "127.0.0.1:1"})
	if msg := buf.String(); msg != "" {
		t.Fatalf("buildSidecarDeps must not error-log without a tmpfs (the CI failure class), got: %s", msg)
	}
}

// TestStagingBootGuard_BothArms pins the helper itself: a
// non-directory parent skips the boot block; a real one runs it.
func TestStagingBootGuard_BothArms(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LLMSAFESPACES_UPLOADS_STAGING_PATH", blocker+"/staged-upload-files")
	stager := newUploadStager(stagingConfigFromEnv(), nil)
	if err := stager.ensureStagingDir(); err == nil {
		t.Fatal("ensureStagingDir past a file parent must fail — the guard is load-bearing")
	}
	if stagingBootShouldRun(stager.cfg.stagingDir) {
		t.Fatal("a non-directory parent must not trigger the boot block")
	}
	t.Setenv("LLMSAFESPACES_UPLOADS_STAGING_PATH", t.TempDir()+"/staged-upload-files")
	stager2 := newUploadStager(stagingConfigFromEnv(), nil)
	if !stagingBootShouldRun(stager2.cfg.stagingDir) {
		t.Fatal("a real parent must trigger the boot block")
	}
	if err := stager2.ensureStagingDir(); err != nil {
		t.Fatalf("ensureStagingDir with a real parent: %v", err)
	}
}

// --- PR 2.5: the agentd side of the env-name contract ---

// TestStagingConfigFromEnv_ParsesKnobs pins the agentd-side env names
// literally (the PR-2.5 contract's missing half — a one-sided rename of
// any name on EITHER side of the contract must fail a test; before this
// pin an agentd-side rename shipped green and silently reverted the
// knobs to defaults).
func TestStagingConfigFromEnv_ParsesKnobs(t *testing.T) {
	t.Setenv("LLMSAFESPACES_UPLOADS_STAGING_PATH", "/custom/staging")
	t.Setenv("UPLOAD_STAGING_BUDGET", "1048576")
	t.Setenv("UPLOAD_STAGING_CREDENTIAL_FLOOR", "262144")
	t.Setenv("UPLOAD_STAGING_MAX_CONCURRENT", "9")
	t.Setenv("UPLOAD_STAGING_TTL_MS", "45000")
	t.Setenv("UPLOAD_APPLY_TIMEOUT_MS", "30000")

	cfg := stagingConfigFromEnv()
	if cfg.stagingDir != "/custom/staging" {
		t.Fatalf("stagingDir: %q", cfg.stagingDir)
	}
	if cfg.budget != 1048576 {
		t.Fatalf("budget: %d", cfg.budget)
	}
	if cfg.credentialFloor != 262144 {
		t.Fatalf("credentialFloor: %d", cfg.credentialFloor)
	}
	if cfg.maxConcurrent != 9 {
		t.Fatalf("maxConcurrent: %d", cfg.maxConcurrent)
	}
	if cfg.ttl != 45*time.Second {
		t.Fatalf("ttl: %v", cfg.ttl)
	}
	if cfg.applyTimeout != 30*time.Second {
		t.Fatalf("applyTimeout: %v", cfg.applyTimeout)
	}
}

// TestStagingConfigFromEnv_InvalidFallsToDefaults pins the invalid →
// default arm (the PR-2.5 contract's parse semantics).
func TestStagingConfigFromEnv_InvalidFallsToDefaults(t *testing.T) {
	t.Setenv("UPLOAD_STAGING_BUDGET", "-5")
	t.Setenv("UPLOAD_STAGING_MAX_CONCURRENT", "notanumber")
	t.Setenv("UPLOAD_STAGING_TTL_MS", "0")

	cfg := stagingConfigFromEnv()
	if cfg.budget != defaultStagingBudget {
		t.Fatalf("invalid budget must default, got %d", cfg.budget)
	}
	if cfg.maxConcurrent != defaultMaxConcurrent {
		t.Fatalf("invalid concurrent must default, got %d", cfg.maxConcurrent)
	}
	if cfg.ttl != defaultStagingTTL {
		t.Fatalf("zero ttl must default, got %v", cfg.ttl)
	}
}

// TestUploadApplyEngineFromEnv_ParsesKnobs pins the supervisor side of
// the SAME names (the two in-pod consumers must not diverge — the
// divergent-clocks bug class).
func TestUploadApplyEngineFromEnv_ParsesKnobs(t *testing.T) {
	t.Setenv("LLMSAFESPACES_UPLOADS_STAGING_PATH", "/custom/staging2")
	t.Setenv("UPLOAD_DEST_MARGIN", "10485760")
	t.Setenv("UPLOAD_STAGING_TTL_MS", "45000")
	t.Setenv("UPLOAD_APPLY_TIMEOUT_MS", "30000")

	e := uploadApplyEngineFromEnv()
	if e.stagingRoot != "/custom/staging2" {
		t.Fatalf("stagingRoot: %q", e.stagingRoot)
	}
	if e.destMargin != 10485760 {
		t.Fatalf("destMargin: %d", e.destMargin)
	}
	if e.ttl != 45*time.Second {
		t.Fatalf("ttl: %v", e.ttl)
	}
	if e.applyDeadline != 30*time.Second {
		t.Fatalf("applyDeadline: %v", e.applyDeadline)
	}
}

// TestBuildSidecarDeps_BootBlockRunsWhenTmpfsParentExists pins the
// guard's TRUE arm through the real wiring: with an existing staging
// parent, buildSidecarDeps establishes the dir (0750), boot-scrubs a
// stale temp, and writes the boot gauges — the §4.1.1 boot-reclaim
// contract. Deleting the boot block (if false) ships this red.
func TestBuildSidecarDeps_BootBlockRunsWhenTmpfsParentExists(t *testing.T) {
	parent := t.TempDir()
	stagingDir := filepath.Join(parent, "staged-upload-files")
	if err := os.MkdirAll(stagingDir, 0o750); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(stagingDir, "staging-old-id-name.txt.tmp")
	if err := os.WriteFile(stale, []byte("x"), 0o640); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LLMSAFESPACES_UPLOADS_STAGING_PATH", stagingDir)
	t.Setenv("AGENTD_CONTROL_PLANE_PASSWORD", "pw")

	_ = buildSidecarDeps(sidecarConfig{password: "pw", controlAddr: "127.0.0.1:1"})

	if _, err := os.Stat(stagingDir); err != nil {
		t.Fatalf("the staging dir must exist post-boot: %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("the boot scrub must reclaim the stale temp (the §4.1.1 boot-reclaim contract)")
	}
}

// TestBuildSidecarDeps_NoGaugesWithoutTmpfs pins the gauges half: the
// shared metrics singleton sees no staging series where the boot block
// is guarded off.
func TestBuildSidecarDeps_NoGaugesWithoutTmpfs(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LLMSAFESPACES_UPLOADS_STAGING_PATH", blocker+"/staged-upload-files")
	t.Setenv("AGENTD_CONTROL_PLANE_PASSWORD", "pw")

	before := testutil.CollectAndCount(pkgOpsMetrics.uploadStagingBytes, "workspace_agentd_upload_staging_bytes")
	_ = buildSidecarDeps(sidecarConfig{password: "pw", controlAddr: "127.0.0.1:1"})
	after := testutil.CollectAndCount(pkgOpsMetrics.uploadStagingBytes, "workspace_agentd_upload_staging_bytes")
	if after != before {
		t.Fatalf("no staging gauge series may appear without a tmpfs: before=%d after=%d", before, after)
	}
}

// TestStagingEnvSentinels_ZeroIsValid pins the n ≥ 0 parse arms: a
// literal "0" is honored (not conflated with unset) for both
// floor-capable knobs — the agentd half of the controller's floor
// sentinel.
func TestStagingEnvSentinels_ZeroIsValid(t *testing.T) {
	t.Setenv("UPLOAD_STAGING_CREDENTIAL_FLOOR", "0")
	if cfg := stagingConfigFromEnv(); cfg.credentialFloor != 0 {
		t.Fatalf("floor=0 must parse as 0 (not the 24Mi default), got %d", cfg.credentialFloor)
	}
	t.Setenv("UPLOAD_DEST_MARGIN", "0")
	if e := uploadApplyEngineFromEnv(); e.destMargin != 0 {
		t.Fatalf("margin=0 must parse as 0 (not the 64Mi default), got %d", e.destMargin)
	}
}

// TestBuildSidecarDeps_SweeperPlacementGuarded pins the sweeper's
// GUARD PLACEMENT (the r8/r9 gap — mutation B, the exact 55af8d5c
// revert, passed the full suite): where the staging parent is absent,
// the sweeper goroutine is NEVER STARTED (the channel stays open);
// where the parent exists, it starts (the channel closes). Reverting
// the placement (sweeper outside the guard) closes the channel in the
// no-tmpfs case and fails this pin.
func TestBuildSidecarDeps_SweeperPlacementGuarded(t *testing.T) {
	// No-tmpfs arm: the sweeper must NOT start.
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LLMSAFESPACES_UPLOADS_STAGING_PATH", blocker+"/staged-upload-files")
	t.Setenv("AGENTD_CONTROL_PLANE_PASSWORD", "pw")
	deps := buildSidecarDeps(sidecarConfig{password: "pw", controlAddr: "127.0.0.1:1"})
	select {
	case <-deps.uploadStager.sweepStarted:
		t.Fatal("the sweeper must NOT start where the staging parent is absent (the no-metrics-writes-without-a-tmpfs contract)")
	default:
	}

	// Tmpfs arm: the sweeper MUST start.
	t.Setenv("LLMSAFESPACES_UPLOADS_STAGING_PATH", t.TempDir()+"/staged-upload-files")
	deps2 := buildSidecarDeps(sidecarConfig{password: "pw", controlAddr: "127.0.0.1:1"})
	select {
	case <-deps2.uploadStager.sweepStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("the sweeper must start where the staging parent exists (the boot-reclaim contract)")
	}
}

// TestStagingSweeper_DeterministicTicks (#1532): the sweep loop's tick
// semantics asserted by DRIVING ticks — which files age out is decided
// by the FAKE now the tick carries, not by real-time luck; idempotence
// and the gauge push are counted, not slept past.
func TestStagingSweeper_DeterministicTicks(t *testing.T) {
	mc := newManualClock(t)
	mc.install(t)

	cfg := testStagingConfig(t, 1000, 0, 4)
	m := &recordingStagingMetrics{}
	s := newUploadStager(cfg, m)

	aged := filepath.Join(cfg.stagingDir, "aged-id")
	fresh := filepath.Join(cfg.stagingDir, "fresh-id")
	if err := os.MkdirAll(cfg.stagingDir, 0o750); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{aged, fresh, cfg.stagingDir + "/part.tmp"} {
		if err := os.WriteFile(p, []byte("x"), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	// The aged object's mtime is REAL-past; the sweep compares against
	// the tick's fake now — fix the fake so the decision is arithmetic:
	// aged is 2×ttl old at the tick, fresh is not.
	past := time.Now().Add(-2 * cfg.ttl)
	if err := os.Chtimes(aged, past, past); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.startStagingSweeper(ctx, time.Hour) // interval irrelevant under the fake

	// Sync on the observable: close(sweepStarted) happens before the
	// goroutine exists — no wait needed; but the loop must be SELECTING
	// before the tick lands or the buffered tick is simply consumed
	// later (outcome asserts below are pace-independent).
	mc.tick()

	require.Eventually(t, func() bool {
		_, err := os.Stat(aged)
		return err != nil // aged gone
	}, 5*time.Second, 10*time.Millisecond, "the tick must age out the ttl-old object")

	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("fresh object must survive the sweep: %v", err)
	}
	if _, err := os.Stat(cfg.stagingDir + "/part.tmp"); err != nil {
		t.Fatalf("the fresh .tmp part must survive (age-gated, not name-gated): %v", err)
	}
	require.Equal(t, 1, m.gaugeCount(), "exactly one gauge push per tick")

	// Idempotence: a second tick at the SAME fake now sweeps nothing
	// new and pushes exactly one more gauge.
	mc.tick()
	require.Eventually(t, func() bool { return m.gaugeCount() == 2 }, 5*time.Second, 10*time.Millisecond)
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("fresh must still survive the second sweep: %v", err)
	}
}
