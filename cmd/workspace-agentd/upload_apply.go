// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// upload_apply.go — design 0060 §3.2/§4.4/§9.2: the supervisor-side
// upload_apply engine + the uid-1000 destination scrub. The supervisor
// (uid 1000, the only standing control-plane component that can write
// /workspace in a sidecar pod) streams a staged upload from the shared
// tmpfs onto the PVC with the write-time gates, verifies it, and makes
// it visible atomically — ownership by construction, US-4b preserved.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"sync"
	"time"
)

const defaultDestMarginBytes = int64(64 << 20)

// uploadUUIDPattern constrains upload_id/staged_name: a uuid-v4 shape
// (§3.2's closed-param set — no arbitrary paths cross the socket).
var uploadUUIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

var sha256HexPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// uploadApplyEngine is the §3.2 method implementation + the §4.3
// destination scrub owner.
type uploadApplyEngine struct {
	stagingRoot string
	uploadsDir  string
	destMargin  int64
	ttl         time.Duration

	// applyMu serializes applies (§8 item 3's simplest choice): the
	// copies are independent (uuid targets), but a bounded serial queue
	// keeps fd pressure flat and makes the §6.6 characterization honest.
	applyMu sync.Mutex

	// seams
	statfs func(path string) (*statfsT, error)
	uuid   func() string
	rename func(oldpath, newpath string) error
	now    func() time.Time
}

func uploadApplyEngineFromEnv() *uploadApplyEngine {
	e := &uploadApplyEngine{
		stagingRoot: "/sandbox-runtime/staged-upload-files",
		uploadsDir:  uploadsPathFromEnv(),
		destMargin:  defaultDestMarginBytes,
		ttl:         defaultStagingTTL,
		statfs:      statfsOf,
		uuid:        newUploadID,
		rename:      os.Rename,
		now:         time.Now,
	}
	if v := os.Getenv("LLMSAFESPACES_UPLOADS_STAGING_PATH"); v != "" {
		e.stagingRoot = v
	}
	if v := os.Getenv("UPLOAD_DEST_MARGIN"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
			e.destMargin = n
		}
	}
	if v := os.Getenv("UPLOAD_STAGING_TTL_MS"); v != "" {
		if ms, err := strconv.ParseInt(v, 10, 64); err == nil && ms > 0 {
			e.ttl = time.Duration(ms) * time.Millisecond
		}
	}
	return e
}

// applyError is the supervisor-side closed enum (§3.2). bad_request is
// reserved for malformed params (A.3's wire class); the rest are the
// method's semantic failures.
type applyError struct {
	code string
	msg  string
}

func (e *applyError) Error() string { return e.code + ": " + e.msg }

// Apply validates the closed-param set, gates, streams, verifies, and
// renames. On success the result carries the §3.2 additive ack fields
// (both computed supervisor-side — it owns the margin parameter).
func (e *uploadApplyEngine) Apply(params map[string]any) (map[string]any, *applyError) {
	id, _ := params["upload_id"].(string)
	staged, _ := params["staged_name"].(string)
	sizeF, _ := params["size"].(float64)
	sha, _ := params["sha256"].(string)
	target, _ := params["target_name"].(string)

	switch {
	case !uploadUUIDPattern.MatchString(id):
		return nil, &applyError{code: "bad_request", msg: "upload_id must be a uuid"}
	case !uploadUUIDPattern.MatchString(staged):
		return nil, &applyError{code: "bad_request", msg: "staged_name must be a uuid"}
	case id != staged:
		return nil, &applyError{code: "bad_request", msg: "upload_id and staged_name must match"}
	case sizeF <= 0 || sizeF != float64(int64(sizeF)):
		return nil, &applyError{code: "bad_request", msg: "size must be a positive integer"}
	case !sha256HexPattern.MatchString(sha):
		return nil, &applyError{code: "bad_request", msg: "sha256 must be 64 hex chars"}
	}
	size := int64(sizeF)
	sanitized, ok := sanitizeUploadFilename(target)
	if !ok || sanitized != target {
		return nil, &applyError{code: "target_rejected", msg: "target_name failed sanitization"}
	}

	e.applyMu.Lock()
	defer e.applyMu.Unlock()

	stagedPath := filepath.Join(e.stagingRoot, staged)
	f, err := os.Open(stagedPath) //nolint:gosec // G304: staged is uuid-validated against a fixed root
	if err != nil {
		return nil, &applyError{code: "staged_missing", msg: "staged object not found"}
	}
	defer func() { _ = f.Close() }()

	// §4.4 pre-copy gate: the authoritative write-time check (statfs is
	// ground truth; the CRD ratio was only the fast pre-filter).
	if !e.destAvailAtLeast(size + e.destMargin) {
		return nil, &applyError{code: "dest_disk_full", msg: "destination avail below size + margin"}
	}

	finalName := id + "-" + sanitized
	finalPath := filepath.Join(e.uploadsDir, finalName)
	tmpPath := filepath.Join(e.uploadsDir, finalName+".tmp")
	//nolint:gosec // G301: 0755 is the design contract (epic-68 U1.1.11) — the uploads dir is traversable like /workspace itself
	if err := os.MkdirAll(e.uploadsDir, 0o755); err != nil {
		return nil, &applyError{code: "dest_write_failed", msg: "uploads dir unavailable"}
	}

	hash := sha256.New()
	//nolint:gosec // G304/G703: tmpPath is server-generated (uuid + sanitize-validated name under the fixed uploads dir); the uuid regex + sanitize gate close the traversal class
	out, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, &applyError{code: "dest_write_failed", msg: "tmp create failed"}
	}
	abort := func(reason *applyError) (map[string]any, *applyError) {
		_ = out.Close()
		_ = os.Remove(tmpPath) //nolint:gosec // G703: server-generated verified path (see the OpenFile note)
		return nil, reason
	}

	buf := make([]byte, stagingChunkWindow)
	var written int64
	for {
		n, rerr := f.Read(buf)
		if n > 0 {
			written += int64(n)
			if _, werr := out.Write(buf[:n]); werr != nil {
				return abort(&applyError{code: "dest_write_failed", msg: "copy write failed"})
			}
			if _, herr := hash.Write(buf[:n]); herr != nil {
				return abort(&applyError{code: "dest_write_failed", msg: "hash failed"})
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return abort(&applyError{code: "dest_write_failed", msg: "staged read failed"})
		}
	}
	// §3.4: verify BEFORE the rename — the file becomes visible only
	// after integrity holds (a partial is never visible).
	if written != size {
		return abort(&applyError{code: "size_mismatch", msg: fmt.Sprintf("staged object is %d bytes, declared %d", written, size)})
	}
	if hex.EncodeToString(hash.Sum(nil)) != sha {
		return abort(&applyError{code: "checksum_mismatch", msg: "digest mismatch"})
	}
	//nolint:gosec // G703: verified-path Sync (see the OpenFile note)
	if err := out.Sync(); err != nil {
		return abort(&applyError{code: "dest_write_failed", msg: "fsync failed"})
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmpPath) //nolint:gosec // G703: server-generated verified path (see the OpenFile note)
		return nil, &applyError{code: "dest_write_failed", msg: "close failed"}
	}
	//nolint:gosec // G703: same-fs rename of the verified tmp object
	if err := e.rename(tmpPath, finalPath); err != nil {
		_ = os.Remove(tmpPath)
		return nil, &applyError{code: "dest_write_failed", msg: "rename failed"}
	}

	// §4.4 post-rename observation (not a gate — correctness first):
	// dest_avail_after + the supervisor-computed margin flag are the
	// §3.2 additive ack fields agentd's counters read.
	availAfter := e.destAvail()
	marginConsumed := availAfter >= 0 && availAfter < e.destMargin
	return map[string]any{
		"applied":          true,
		"path":             finalPath,
		"size":             size,
		"dest_avail_after": availAfter,
		"margin_consumed":  marginConsumed,
	}, nil
}

func (e *uploadApplyEngine) destAvail() int64 {
	st, err := e.statfs(e.uploadsDir)
	if err != nil {
		return -1
	}
	//nolint:gosec // G115: 64-bit statfs fields; the clamp pins the contract on any width
	avail := int64(st.Bavail) * int64(st.Bsize)
	if avail < 0 {
		return 0
	}
	return avail
}

func (e *uploadApplyEngine) destAvailAtLeast(need int64) bool {
	return e.destAvail() >= need
}

// scrubDestination removes /workspace/uploads/*.tmp older than ttl
// (ttl=0: everything — the boot arm). The pre-existing sidecar-mode gap
// this closes (design §4.3): the boot scrub's only live call site was
// the single-container path; the sidecar's own call is an RO no-op.
func (e *uploadApplyEngine) scrubDestination(ttl time.Duration) int {
	entries, err := os.ReadDir(e.uploadsDir)
	if err != nil {
		return 0
	}
	removed := 0
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".tmp" {
			continue
		}
		if ttl > 0 {
			if info, ierr := entry.Info(); ierr == nil && e.now().Sub(info.ModTime()) < ttl {
				continue
			}
		}
		if os.Remove(filepath.Join(e.uploadsDir, entry.Name())) == nil {
			removed++
		}
	}
	return removed
}

// startDestinationSweeper runs the TTL arm (the same UPLOAD_STAGING_TTL
// clock as the staging sweeper — one knob, both surfaces).
func (e *uploadApplyEngine) startDestinationSweeper(ctx context.Context, interval time.Duration) {
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
				e.scrubDestination(e.ttl)
			}
		}
	}()
}

// uploadApplyControlMethod adapts the engine to the control socket's
// dispatch: params in, result or the closed error out (A.1: unknown
// param KEYS were already ignored by the decoder).
func (s *controlSocketServer) uploadApplyControlMethod(req controlRequest) controlResponse {
	if s.uploadApply == nil {
		return s.errResp(req.ID, "internal", "upload_apply engine unwired")
	}
	result, aerr := s.uploadApply.Apply(req.Params)
	if aerr != nil {
		if aerr.code == "bad_request" {
			return s.errResp(req.ID, "bad_request", aerr.msg)
		}
		return controlResponse{V: controlProtocolVersion, ID: idOr(req.ID),
			Error: &controlError{Code: aerr.code, Message: aerr.msg}}
	}
	return controlResponse{V: controlProtocolVersion, ID: idOr(req.ID), Result: result}
}
