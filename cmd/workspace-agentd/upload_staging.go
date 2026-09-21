// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// upload_staging.go — design 0060 §4.1–§4.3: the sidecar-mode upload
// staging leg. The sidecar's /workspace is read-only (US-4b), so uploads
// stage on the shared /sandbox-runtime tmpfs under a two-clause budget
// and are applied to the PVC by the supervisor via the control socket
// (`upload_apply`, §3.2 — landed in the supervisor lane). Single-container
// mode never constructs an uploadStager and keeps the direct-write path.
//
// Admission (0060 §4.1, normative):
//
//	(A) reservedUploads() + newBytes ≤ UPLOAD_STAGING_BUDGET
//	(B) credentialUsage() + UPLOAD_STAGING_CREDENTIAL_FLOOR
//	    + reservedUploads() + newBytes ≤ statfs(tmpfs).f_bavail
//
// Clause (A) is the byte-weighted semaphore's enforcement point; clause
// (B) guarantees credentials are never evicted or blocked and makes
// adversary junk convert to clean pre-acceptance 507s (junk arriving
// after admission is the §3.5 clean-abort case).
//
// Lifecycle (0060 §4.1, load-bearing for the §6.1 residency bound):
// reservations are HELD UNTIL THE BYTES LEAVE THE TMPFS — released on
// ack (after the post-ack unlink), on abort (the error path unlinks the
// partial synchronously, then releases), or on scrub. Unlink-before-
// release is what makes the no-crash walked bound hold.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"

	"github.com/lenaxia/llmsafespaces/pkg/agentd"
)

const (
	defaultStagingBudget   = int64(48 << 20)
	defaultCredentialFloor = int64(24 << 20)
	defaultStagingTTL      = 15 * time.Minute
	defaultMaxConcurrent   = 4
	defaultApplyTimeout    = 60 * time.Second
	// stagingEnvelopeAllowance mirrors the API's uploadEnvelopeAllowance
	// (api/internal/handlers/uploads.go) — the multipart envelope the
	// client's Content-Length carries over the raw file bytes.
	stagingEnvelopeAllowance             = int64(64 << 10)
	stagingChunkWindow                   = 256 << 10
	stagingFilePerm          os.FileMode = 0o640
)

// errDeclaredExceeded is the 400-class over-read: the body exceeded the
// declared bound (0060 §4.1 — a lying-small caller can never stage
// unreserved bytes).
var errDeclaredExceeded = errors.New("body exceeds declared length")

// stagingRejectClass names the two admission rejections (0060 §4.6:
// 507 staging_full vs 429 staging_busy — distinct client recoveries).
type stagingRejectClass string

const (
	rejectStagingFull stagingRejectClass = "staging_full"
	rejectStagingBusy stagingRejectClass = "staging_busy"
)

// stagingConfig carries the leg's knobs (0060 §8 item 1 defaults).
type stagingConfig struct {
	stagingDir      string
	budget          int64
	credentialFloor int64
	ttl             time.Duration
	maxConcurrent   int
	applyTimeout    time.Duration
	statfs          func(path string) (*syscall.Statfs_t, error)
	uuid            func() string
}

func stagingConfigFromEnv() stagingConfig {
	cfg := stagingConfig{
		stagingDir:      "/sandbox-runtime/staged-upload-files",
		budget:          defaultStagingBudget,
		credentialFloor: defaultCredentialFloor,
		ttl:             defaultStagingTTL,
		maxConcurrent:   defaultMaxConcurrent,
		applyTimeout:    defaultApplyTimeout,
		statfs:          statfsOf,
		uuid:            newUploadID,
	}
	if v := os.Getenv("LLMSAFESPACES_UPLOADS_STAGING_PATH"); v != "" {
		cfg.stagingDir = v
	}
	if v := os.Getenv("UPLOAD_STAGING_BUDGET"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			cfg.budget = n
		}
	}
	if v := os.Getenv("UPLOAD_STAGING_CREDENTIAL_FLOOR"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
			cfg.credentialFloor = n
		}
	}
	if v := os.Getenv("UPLOAD_STAGING_TTL_MS"); v != "" {
		if ms, err := strconv.ParseInt(v, 10, 64); err == nil && ms > 0 {
			cfg.ttl = time.Duration(ms) * time.Millisecond
		}
	}
	if v := os.Getenv("UPLOAD_STAGING_MAX_CONCURRENT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.maxConcurrent = n
		}
	}
	if v := os.Getenv("UPLOAD_APPLY_TIMEOUT_MS"); v != "" {
		if ms, err := strconv.ParseInt(v, 10, 64); err == nil && ms > 0 {
			cfg.applyTimeout = time.Duration(ms) * time.Millisecond
		}
	}
	return cfg
}

func statfsOf(path string) (*syscall.Statfs_t, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// credentialSurfaces are the tmpfs paths whose live usage clause (B)
// protects (0060 §4.1) — walked at every admission, relative to the
// tmpfs root (the staging dir's parent).
var credentialSurfaces = []string{
	"staged-secret-files",
	"spawn-files-ledger.json",
	"secrets-env",
	"admin-prompt.md",
	"rt",
}

// uploadApplyRequest is the §3.2 closed-param set. uploadID is also the
// staged basename (validated `^[0-9a-f-]{36}$` by the supervisor; the
// sidecar only ever passes uuid.NewString output).
type uploadApplyRequest struct {
	UploadID   string `json:"upload_id"`
	StagedName string `json:"staged_name"`
	Size       int64  `json:"size"`
	SHA256     string `json:"sha256"`
	TargetName string `json:"target_name"`
}

// uploadApplyResult is the §3.2 success shape (dest_avail_after and
// margin_consumed are the A.1-legal additive fields the supervisor
// computes; agentd counts the margin observation from the flag).
type uploadApplyResult struct {
	Applied        bool   `json:"applied"`
	Path           string `json:"path"`
	Size           int64  `json:"size"`
	DestAvailAfter int64  `json:"dest_avail_after"`
	MarginConsumed bool   `json:"margin_consumed"`
}

// uploadApplyError carries the supervisor's closed error enum (§3.2);
// transport failures use Code "transport" and wrap the cause so the
// §3.3 timeout class (context.DeadlineExceeded) is detectable.
type uploadApplyError struct {
	Code    string
	Message string
	cause   error
}

func (e *uploadApplyError) Error() string {
	return e.Code + ": " + e.Message
}

func (e *uploadApplyError) Unwrap() error { return e.cause }

// uploadApplier is the seam the handler calls after staging; production
// is the control-socket client, tests inject fakes.
type uploadApplier func(ctx context.Context, req uploadApplyRequest) (*uploadApplyResult, *uploadApplyError)

// uploadStager is the admission controller + staging-surface owner.
type uploadStager struct {
	cfg stagingConfig

	mu           sync.Mutex
	reservations map[string]int64 // uploadID → reserved bytes (held until bytes leave)

	metrics stagingMetrics
}

// stagingMetrics is the observability seam (0060 §4.6) — production
// writes the ops gauges/counters; tests record calls.
type stagingMetrics interface {
	RecordStagingGauges(stagedBytes, reservedBytes, credentialBytes int64, files int)
	RecordUploadBytes(direction string, n int64)
}

type noopStagingMetrics struct{}

func (noopStagingMetrics) RecordStagingGauges(int64, int64, int64, int) {}
func (noopStagingMetrics) RecordUploadBytes(string, int64)              {}

// ensureStagingDir establishes the §4.1.1 contract at boot: 0750 dir,
// gid 1000 by process inheritance (the sidecar runs uid 2000 / gid
// 1000 per the pod spec — the supervisor shares gid 1000, so staged
// 0640 files are group-readable across the boundary; this is the
// validated dependency, not fsGroup wiring).
func (s *uploadStager) ensureStagingDir() error {
	return os.MkdirAll(s.cfg.stagingDir, 0o750)
}

func newUploadStager(cfg stagingConfig, m stagingMetrics) *uploadStager {
	if m == nil {
		m = noopStagingMetrics{}
	}
	if cfg.statfs == nil {
		cfg.statfs = statfsOf
	}
	if cfg.uuid == nil {
		cfg.uuid = newUploadID
	}
	return &uploadStager{cfg: cfg, reservations: make(map[string]int64), metrics: m}
}

func newUploadID() string {
	return uuid.NewString()
}

// CredentialUsage walks the credential surfaces under the tmpfs root
// (the staging dir's parent). Recomputed at every admission — the
// surfaces are few and small (0060 §4.1).
func (s *uploadStager) CredentialUsage() int64 {
	root := filepath.Dir(s.cfg.stagingDir)
	var total int64
	for _, name := range credentialSurfaces {
		total += dirUsage(filepath.Join(root, name))
	}
	return total
}

func dirUsage(path string) int64 {
	var total int64
	_ = filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
		if err != nil || info == nil {
			return nil
		}
		if !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	return total
}

// Admit implements the two-clause admission (0060 §4.1). On success the
// reservation is held under the returned id; the caller MUST eventually
// Release it (ack+unlink, abort+unlink, or scrub finalize).
func (s *uploadStager) Admit(id string, newBytes int64) stagingRejectClass {
	s.mu.Lock()
	defer s.mu.Unlock()

	var reserved int64
	for _, n := range s.reservations {
		reserved += n
	}
	if reserved+newBytes > s.cfg.budget {
		return rejectStagingFull
	}
	if len(s.reservations) >= s.cfg.maxConcurrent {
		return rejectStagingBusy
	}

	avail := s.availableBytes()
	if s.CredentialUsageLocked()+s.cfg.credentialFloor+reserved+newBytes > avail {
		return rejectStagingFull
	}

	s.reservations[id] = newBytes
	return ""
}

// availableBytes is f_bavail of the tmpfs, or 0 when unstatable (the
// reject-safe direction — 0060 §4.1 clause B).
func (s *uploadStager) availableBytes() int64 {
	root := filepath.Dir(s.cfg.stagingDir)
	st, err := s.cfg.statfs(root)
	if err != nil {
		return 0
	}
	//nolint:gosec // G115: on 64-bit platforms these conversions cannot overflow the sums admission compares against (bytes vs a ≤2^63 budget); the clamp keeps the contract explicit on any width.
	avail := int64(st.Bavail) * int64(st.Bsize)
	if avail < 0 {
		return 0
	}
	return avail
}

// CredentialUsageLocked is the clause-(B) walk; the mutex name documents
// the admission-path ownership (walks are read-only).
func (s *uploadStager) CredentialUsageLocked() int64 { return s.CredentialUsage() }

// ReconcileDown narrows a reservation to the actual staged size at
// completion (0060 §4.1: the declared bound is the conservative upper
// bound; the reservation reconciles DOWN at rename completion).
func (s *uploadStager) ReconcileDown(id string, actual int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cur, ok := s.reservations[id]; ok && actual < cur {
		s.reservations[id] = actual
	}
}

// Release drops a reservation (ack+unlink or abort+unlink — always
// AFTER the bytes have left the tmpfs, 0060 §4.1).
func (s *uploadStager) Release(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.reservations, id)
}

// ReleaseIfHeld drops the reservation only when present (the scrub
// finalize path — a crash orphan holds none).
func (s *uploadStager) ReleaseIfHeld(id string) { s.Release(id) }

// ReservedBytes snapshots the reserved gauge truth.
func (s *uploadStager) ReservedBytes() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	var total int64
	for _, n := range s.reservations {
		total += n
	}
	return total
}

// StagedBytesAndFiles walks the staging dir (the walked gauge truth,
// reconciled by the sweeper — 0060 §4.6).
func (s *uploadStager) StagedBytesAndFiles() (int64, int) {
	var total int64
	var files int
	entries, err := os.ReadDir(s.cfg.stagingDir)
	if err != nil {
		return 0, 0
	}
	for _, e := range entries {
		if info, err := e.Info(); err == nil && !e.IsDir() {
			total += info.Size()
			files++
		}
	}
	return total, files
}

// RecordGauges pushes the §4.6 gauge snapshot.
func (s *uploadStager) RecordGauges() {
	staged, files := s.StagedBytesAndFiles()
	s.metrics.RecordStagingGauges(staged, s.ReservedBytes(), s.CredentialUsage(), files)
}

// stagedPath is the completed-object name; partPath the in-flight one.
func (s *uploadStager) stagedPath(id string) string { return filepath.Join(s.cfg.stagingDir, id) }
func (s *uploadStager) partPath(id string) string   { return s.cfg.stagingDir + "/" + id + ".tmp" }

// stageStream streams body → <id>.tmp with the hard read-cap at declared
// (0060 §4.1: over-read → errDeclaredExceeded), sha256 on the fly, then
// same-fs renames to <id>. Returns the actual size and hex digest.
// The caller owns the reservation lifecycle around this call.
func (s *uploadStager) stageStream(ctx context.Context, body io.Reader, id string, declared int64) (int64, string, error) {
	if err := os.MkdirAll(s.cfg.stagingDir, 0o750); err != nil {
		return 0, "", fmt.Errorf("staging dir: %w", err)
	}
	part := s.partPath(id)
	//nolint:gosec // G304: path is server-generated (uuid under the fixed staging dir)
	f, err := os.OpenFile(part, os.O_CREATE|os.O_EXCL|os.O_WRONLY, stagingFilePerm)
	if err != nil {
		return 0, "", fmt.Errorf("staging create: %w", err)
	}
	defer func() {
		if f != nil {
			_ = f.Close()
			_ = os.Remove(part) // unlink-before-release ordering is the caller's; this is the crash-backstop
		}
	}()

	hash := sha256.New()
	capped := io.LimitReader(body, declared+1)
	buf := make([]byte, stagingChunkWindow)
	var written int64
	for {
		if err := ctx.Err(); err != nil {
			return 0, "", err
		}
		n, rerr := capped.Read(buf)
		if n > 0 {
			written += int64(n)
			if written > declared {
				return 0, "", errDeclaredExceeded
			}
			if _, werr := f.Write(buf[:n]); werr != nil {
				return 0, "", fmt.Errorf("staging write: %w", werr)
			}
			if _, herr := hash.Write(buf[:n]); herr != nil {
				return 0, "", fmt.Errorf("staging hash: %w", herr)
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return 0, "", rerr
		}
	}
	if err := f.Sync(); err != nil {
		return 0, "", fmt.Errorf("staging fsync: %w", err)
	}
	if err := f.Close(); err != nil {
		f = nil
		return 0, "", fmt.Errorf("staging close: %w", err)
	}
	f = nil
	if err := os.Rename(part, s.stagedPath(id)); err != nil {
		_ = os.Remove(part)
		return 0, "", fmt.Errorf("staging rename: %w", err)
	}
	s.metrics.RecordUploadBytes("staged_in", written)
	return written, hex.EncodeToString(hash.Sum(nil)), nil
}

// abortStaged unlinks the staged object (any state) and releases — the
// §3.5 abort ordering (unlink BEFORE release).
func (s *uploadStager) abortStaged(id string) {
	_ = os.Remove(s.stagedPath(id))
	_ = os.Remove(s.partPath(id))
	s.Release(id)
}

// ackStaged unlinks after a successful apply and releases.
func (s *uploadStager) ackStaged(id string) {
	_ = os.Remove(s.stagedPath(id))
	s.Release(id)
}

// scrubStagingDir removes everything in the staging dir older than ttl
// (boot: ttl=0 scrubs all) and finalizes the reservations still held
// for what it removed. Returns the removed count.
func (s *uploadStager) scrubStagingDir(ttl time.Duration, now time.Time) int {
	entries, err := os.ReadDir(s.cfg.stagingDir)
	if err != nil {
		return 0
	}
	removed := 0
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || e.IsDir() {
			continue
		}
		if ttl > 0 && now.Sub(info.ModTime()) < ttl {
			continue
		}
		if err := os.Remove(filepath.Join(s.cfg.stagingDir, e.Name())); err == nil {
			removed++
			s.ReleaseIfHeld(stripTmpSuffix(e.Name()))
		}
	}
	if removed > 0 {
		// §4.6 staging_scrubbed: scrub activity is observable.
		pkgOpsMetrics.RecordUploadOutcome(uploadWorkspaceID(), uploadOutcomeStagingScrubbed)
	}
	return removed
}

func stripTmpSuffix(name string) string {
	return strings.TrimSuffix(name, ".tmp")
}

// startStagingSweeper runs the TTL sweep on a bounded ticker and pushes
// the §4.6 gauge snapshot each tick.
func (s *uploadStager) startStagingSweeper(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 10 * time.Minute
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.scrubStagingDir(s.cfg.ttl, time.Now())
				s.RecordGauges()
			}
		}
	}()
}

// handleStagedUpload is the sidecar-mode PUT /v1/files flow (design
// 0060 §3.1): admission → staged stream (hard read-cap at declared) →
// same-fs rename → upload_apply (bounded wait) → ack/abort lifecycle.
// Auth and filename sanitization happened in the caller.
func handleStagedUpload(w http.ResponseWriter, r *http.Request, cfg fileUploadConfig, stager *uploadStager, apply uploadApplier, name string) {
	wsID := uploadWorkspaceID()

	// The admission input (§4.1): the declared body size, mandatory.
	// The API forwards it from the client's Content-Length; a direct
	// caller without it is the D14 shape — 411 either way.
	declared, derr := parseDeclaredBodyBytes(r.Header.Get("X-LLS-Declared-Body-Bytes"))
	if derr != nil {
		pkgOpsMetrics.RecordUploadOutcome(wsID, uploadOutcomeRejectedDeclaredInvalid)
		writeUploadErrorClass(w, http.StatusLengthRequired, "declared body length required", "invalid_declared_length", "")
		return
	}
	// The declared value the API forwards is the CLIENT's multipart
	// Content-Length — envelope-inclusive (boundary + part headers), so a
	// file at exactly the cap legitimately declares cap + envelope. The
	// API pre-gates at cap + the same allowance (api uploads.go's
	// uploadEnvelopeAllowance, mirrored here); direct callers declare
	// raw-body sizes where the allowance is merely generous — the budget
	// clauses still bind.
	if declared > cfg.maxBytes+stagingEnvelopeAllowance {
		pkgOpsMetrics.RecordUploadOutcome(wsID, uploadOutcomeRejectedCap)
		writeUploadError(w, http.StatusRequestEntityTooLarge, "file exceeds size cap")
		return
	}

	id := stager.cfg.uuid()
	if class := stager.Admit(id, declared); class != "" {
		if class == rejectStagingBusy {
			pkgOpsMetrics.RecordUploadOutcome(wsID, uploadOutcomeRejectedStagingBusy)
			writeUploadErrorClass(w, http.StatusTooManyRequests, "staging busy", "staging_busy", "")
		} else {
			pkgOpsMetrics.RecordUploadOutcome(wsID, uploadOutcomeRejectedStagingFull)
			writeUploadErrorClass(w, http.StatusInsufficientStorage,
				"staging budget exhausted — retry after in-flight uploads settle or free tmpfs", "staging_full", "")
		}
		return
	}

	if err := http.NewResponseController(w).SetReadDeadline(time.Now().Add(cfg.bodyTimeout)); err != nil {
		//nolint:staticcheck // logging seam parity with the direct path; the deadline is best-effort on unsupported controllers
		_ = err
	}

	size, digest, err := stager.stageStream(r.Context(), r.Body, id, declared)
	if err != nil {
		// Unlink BEFORE release (§3.5's normative ordering — the §6.1
		// walked bound rests on it); the mid-stream ENOSPC abort is the
		// 507 staging_write_error class.
		stager.abortStaged(id)
		if errors.Is(err, errDeclaredExceeded) {
			pkgOpsMetrics.RecordUploadOutcome(wsID, uploadOutcomeRejectedDeclaredExceed)
			writeUploadErrorClass(w, http.StatusBadRequest, "body exceeds declared length", "declared_length_exceeded", "")
			return
		}
		pkgOpsMetrics.RecordUploadOutcome(wsID, uploadOutcomeStagingWriteError)
		writeUploadErrorClass(w, http.StatusInsufficientStorage, "staging write failed", "staging_write_error", "")
		return
	}
	// Reconcile the reservation DOWN to the actual staged size (§4.1).
	stager.ReconcileDown(id, size)

	if apply == nil {
		stager.abortStaged(id)
		pkgOpsMetrics.RecordUploadOutcome(wsID, uploadOutcomeApplyRejected)
		writeUploadErrorClass(w, http.StatusInsufficientStorage, "upload apply failed: apply seam unwired", "apply_rejected", "staged_missing")
		return
	}

	actx, cancel := context.WithTimeout(r.Context(), stager.cfg.applyTimeout)
	defer cancel()
	result, aerr := apply(actx, uploadApplyRequest{
		UploadID: id, StagedName: id, Size: size, SHA256: digest, TargetName: name,
	})
	if aerr != nil {
		// §3.3: timeout leaves the staged object in place (reservation
		// held until TTL scrub); every other failure aborts (unlink then
		// release).
		if isApplyTimeout(aerr) {
			pkgOpsMetrics.RecordUploadOutcome(wsID, uploadOutcomeApplyTimeout)
			writeUploadErrorClass(w, http.StatusGatewayTimeout, "upstream apply timeout", "apply_timeout", "")
			return
		}
		stager.abortStaged(id)
		outcome, status, human, reason := mapApplyError(aerr)
		pkgOpsMetrics.RecordUploadOutcome(wsID, outcome)
		writeUploadErrorClass(w, status, human, reason, aerr.Code)
		return
	}

	// Ack: unlink after the successful apply, then release (§4.1).
	stager.ackStaged(id)
	pkgOpsMetrics.RecordUploadOutcome(wsID, uploadOutcomeAccepted)
	// §4.4/§4.6: the supervisor-computed margin flag is the export
	// channel — count the observation here, never infer it.
	if result != nil && result.MarginConsumed {
		pkgOpsMetrics.RecordDestOutcome("dest_margin_consumed")
	}
	// §4.6: copied_out counts the ack's verified size (equal to the
	// staged size by the verification gate; the ack value is the truth).
	copied := size
	if result != nil && result.Size > 0 {
		copied = result.Size
	}
	stager.metrics.RecordUploadBytes("copied_out", copied)
	destPath := "/workspace/uploads/" + id + "-" + name
	if result != nil && result.Path != "" {
		destPath = result.Path
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(agentd.FileUploadResponse{Path: destPath, Name: name, Size: size})
}

// parseDeclaredBodyBytes validates the admission header (§4.1):
// decimal bytes, > 0, ≤ int64. Missing/invalid → 411.
func parseDeclaredBodyBytes(raw string) (int64, error) {
	if raw == "" {
		return 0, errDeclaredRequired
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n <= 0 {
		return 0, errDeclaredRequired
	}
	return n, nil
}

var errDeclaredRequired = errors.New("declared body length required")

// isApplyTimeout reports the §3.3 class: the apply seam's bounded wait
// expired (the transport cause carries DeadlineExceeded; the supervisor
// never emits a timeout code — that class is agentd-local).
func isApplyTimeout(e *uploadApplyError) bool {
	if e == nil {
		return false
	}
	return errors.Is(e, context.DeadlineExceeded)
}

// mapApplyError maps the §3.2 closed enum to the §4.6 client shapes:
// outcome, status, human text, machine reason.
func mapApplyError(aerr *uploadApplyError) (uploadOutcome, int, string, string) {
	switch aerr.Code {
	case "busy":
		return uploadOutcomeRejectedStagingBusy, http.StatusTooManyRequests, "staging busy", "staging_busy"
	case "dest_disk_full":
		return uploadOutcomeApplyRejected, http.StatusInsufficientStorage,
			"workspace disk is full (write-time check)", "dest_disk_full"
	case "checksum_mismatch":
		return uploadOutcomeChecksumMismatch, http.StatusInsufficientStorage,
			"upload apply failed: " + aerr.Message, "apply_rejected"
	default:
		return uploadOutcomeApplyRejected, http.StatusInsufficientStorage,
			"upload apply failed: " + aerr.Message, "apply_rejected"
	}
}
