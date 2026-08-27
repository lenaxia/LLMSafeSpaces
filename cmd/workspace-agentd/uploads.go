// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// uploads.go — Epic 67 US-67.1: the agentd file-ingest endpoint.
//
// PUT /v1/files?filename=<name> — streamed request body lands on the
// workspace PVC as /workspace/uploads/<uuid>-<sanitized-name> via the
// atomic tmp+fsync+rename pattern (ConfigWriter precedent). Symmetric
// with reload-secrets on the user mux: the API server is the only
// caller, authenticating with the control-plane Basic credential set
// (design 0051 §D1/D6.1 — checkBasicAuthAny, same as reload-secrets).
//
// Design epic-67 D14: the in-pod adversary is the agent itself (uid
// 1000 owns /workspace), so the .tmp is created O_CREATE|O_EXCL (never
// follows a pre-planted symlink) and an EEXIST retries with a fresh
// uuid — the agent cannot race 128 bits of randomness.
//
// Port note: the design doc's architecture sketch says "(port 4098)"
// but its own D1 anchors on symmetry with reload-secrets, which serves
// on the USER mux (agentd.AgentdPort 4097; see server.go wireHTTPServers
// and the API's agentpush dispatch at :4097/v1/reload-secrets). This
// endpoint follows the validated reload-secrets pattern: user mux, 4097,
// Basic auth. Sidecar-mode caveat: the sidecar's /workspace mount is
// read-only (controller agentd_sidecar.go), so uploads fail cleanly
// with 5xx there until a control-socket write op exists — see worklog.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/lenaxia/llmsafespaces/pkg/agentd"
)

const (
	uploadCreateAttempts     = 3
	defaultUploadMaxBytes    = int64(25 << 20)
	defaultUploadBodyTimeout = 5 * time.Minute
	uploadTimeoutEnvFloorMs  = 1000
)

// uploadOutcome labels the /v1/files Prometheus counter (design epic-67
// Observability: agentd counts write failures, cap hits, and bad names).
type uploadOutcome string

const (
	uploadOutcomeAccepted     uploadOutcome = "accepted"
	uploadOutcomeRejectedName uploadOutcome = "rejected_name"
	uploadOutcomeRejectedCap  uploadOutcome = "rejected_cap"
	uploadOutcomeWriteError   uploadOutcome = "write_error"
	uploadOutcomeUnauthorized uploadOutcome = "unauthorized"
)

// uploadSink is the writable-file seam behind PUT /v1/files. Production
// is *os.File (O_EXCL create); tests inject failing writers, failing
// Sync, and op-recording wrappers for the atomicity fault-injection
// scenarios (U1.1.6/U1.1.21/U1.1.13).
type uploadSink interface {
	io.Writer
	Sync() error
	Close() error
}

// fileUploadConfig carries every operational knob of the endpoint.
// uuid/create/rename are injection seams; production defaults come from
// uploadConfigFromEnv.
type fileUploadConfig struct {
	uploadsDir  string
	maxBytes    int64
	bodyTimeout time.Duration
	uuid        func() string
	create      func(path string) (uploadSink, error)
	rename      func(oldpath, newpath string) error
}

func openUploadTmpFile(path string) (uploadSink, error) {
	//nolint:gosec // G302: 0644 is the design contract (epic-67 U1.1.11) — uploads are ordinary agent-readable workspace files
	return os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
}

func uploadsPathFromEnv() string {
	return envOrDefault("LLMSAFESPACES_UPLOADS_PATH", agentd.UploadsPath)
}

func uploadConfigFromEnv() fileUploadConfig {
	cfg := fileUploadConfig{
		uploadsDir:  uploadsPathFromEnv(),
		maxBytes:    defaultUploadMaxBytes,
		bodyTimeout: defaultUploadBodyTimeout,
		uuid:        uuid.NewString,
		create:      openUploadTmpFile,
		rename:      os.Rename,
	}
	if v := os.Getenv("UPLOAD_MAX_BYTES"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 && n+1 > 0 {
			cfg.maxBytes = n
		}
	}
	if v := os.Getenv("UPLOAD_TIMEOUT_MS"); v != "" {
		if ms, err := strconv.Atoi(v); err == nil && ms > uploadTimeoutEnvFloorMs {
			cfg.bodyTimeout = time.Duration(ms) * time.Millisecond
		}
	}
	return cfg
}

// sanitizeUploadFilename delegates to agentd.SanitizeFilename — the single
// shared implementation (design epic-67 D9) consumed by both upload layers:
// this authoritative agentd endpoint and the API's defense-in-depth proxy.
// Behavior identical to the original local implementation (US-67.1), pinned
// by the hostile table in uploads_test.go.
func sanitizeUploadFilename(raw string) (string, bool) {
	return agentd.SanitizeFilename(raw)
}

type uploadErrorResponse struct {
	Error string `json:"error"`
}

func writeUploadError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(uploadErrorResponse{Error: msg})
}

// uploadFilesHandler returns the PUT /v1/files handler. Control-plane
// route: workspacePassword plus extraAuth (the §D1 agentdPassword in
// sidecar mode) — the same credential set reload-secrets accepts.
func uploadFilesHandler(logger *zap.Logger, cfg fileUploadConfig, workspacePassword string, extraAuth ...string) http.HandlerFunc {
	passwords := append([]string{workspacePassword}, extraAuth...)
	return func(w http.ResponseWriter, r *http.Request) {
		if !checkBasicAuthAny(r, passwords...) {
			pkgOpsMetrics.RecordUploadOutcome(uploadWorkspaceID(), uploadOutcomeUnauthorized)
			rejectUnauthorized(w)
			return
		}
		if r.Method != http.MethodPut {
			w.Header().Set("Allow", http.MethodPut)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		name, ok := sanitizeUploadFilename(r.URL.Query().Get("filename"))
		if !ok {
			pkgOpsMetrics.RecordUploadOutcome(uploadWorkspaceID(), uploadOutcomeRejectedName)
			writeUploadError(w, http.StatusBadRequest, "invalid filename")
			return
		}

		//nolint:gosec // G301: 0755 is the design contract (epic-67 U1.1.11) — the uploads dir is traversable like /workspace itself
		if err := os.MkdirAll(cfg.uploadsDir, 0o755); err != nil {
			logger.Warn("upload: cannot create uploads dir", zap.String("dir", cfg.uploadsDir), zap.Error(err))
			pkgOpsMetrics.RecordUploadOutcome(uploadWorkspaceID(), uploadOutcomeWriteError)
			writeUploadError(w, http.StatusInternalServerError, "storage unavailable")
			return
		}

		// Slowloris bound (U1.1.18): a stalled body writer aborts at the
		// connection read deadline instead of pinning a .tmp forever.
		// Unsupported controllers (tests) log and continue — the NetPol
		// + auth remain the primary gates, as on every sibling route.
		if err := http.NewResponseController(w).SetReadDeadline(time.Now().Add(cfg.bodyTimeout)); err != nil {
			logger.Warn("upload: read deadline unsupported", zap.Error(err))
		}

		limited := io.LimitReader(r.Body, cfg.maxBytes+1)
		for attempt := 0; attempt < uploadCreateAttempts; attempt++ {
			id := cfg.uuid()
			tmpPath := filepath.Join(cfg.uploadsDir, id+"-"+name+".tmp")
			finalPath := filepath.Join(cfg.uploadsDir, id+"-"+name)

			sink, err := cfg.create(tmpPath)
			if errors.Is(err, fs.ErrExist) {
				logger.Warn("upload: tmp path squatted, retrying with fresh uuid", zap.String("path", tmpPath))
				continue
			}
			if err != nil {
				logger.Warn("upload: create failed", zap.Error(err))
				pkgOpsMetrics.RecordUploadOutcome(uploadWorkspaceID(), uploadOutcomeWriteError)
				writeUploadError(w, http.StatusInternalServerError, "storage unavailable")
				return
			}

			written, copyErr := io.Copy(sink, limited)
			if copyErr != nil {
				abortUploadTmp(sink, tmpPath)
				logger.Warn("upload: body write failed", zap.Int64("written", written), zap.Error(copyErr))
				pkgOpsMetrics.RecordUploadOutcome(uploadWorkspaceID(), uploadOutcomeWriteError)
				writeUploadError(w, uploadCopyErrorStatus(copyErr), "upload write failed")
				return
			}
			if written > cfg.maxBytes {
				abortUploadTmp(sink, tmpPath)
				pkgOpsMetrics.RecordUploadOutcome(uploadWorkspaceID(), uploadOutcomeRejectedCap)
				writeUploadError(w, http.StatusRequestEntityTooLarge, "file exceeds size cap")
				return
			}
			if err := sink.Sync(); err != nil {
				abortUploadTmp(sink, tmpPath)
				logger.Warn("upload: fsync failed", zap.Error(err))
				pkgOpsMetrics.RecordUploadOutcome(uploadWorkspaceID(), uploadOutcomeWriteError)
				writeUploadError(w, http.StatusInternalServerError, "upload write failed")
				return
			}
			if err := sink.Close(); err != nil {
				//nolint:gosec // G703: tmpPath is server-generated (uuid + operator-configured root); no client bytes reach it
				_ = os.Remove(tmpPath)
				logger.Warn("upload: close failed", zap.Error(err))
				pkgOpsMetrics.RecordUploadOutcome(uploadWorkspaceID(), uploadOutcomeWriteError)
				writeUploadError(w, http.StatusInternalServerError, "upload write failed")
				return
			}
			if err := cfg.rename(tmpPath, finalPath); err != nil {
				//nolint:gosec // G703: tmpPath is server-generated (uuid + operator-configured root); no client bytes reach it
				_ = os.Remove(tmpPath)
				logger.Warn("upload: rename failed", zap.Error(err))
				pkgOpsMetrics.RecordUploadOutcome(uploadWorkspaceID(), uploadOutcomeWriteError)
				writeUploadError(w, http.StatusInternalServerError, "upload finalize failed")
				return
			}

			pkgOpsMetrics.RecordUploadOutcome(uploadWorkspaceID(), uploadOutcomeAccepted)
			logger.Info("upload: stored",
				zap.String("path", finalPath),
				zap.Int64("size", written))
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(agentd.FileUploadResponse{Path: finalPath, Name: name, Size: written})
			return
		}

		logger.Warn("upload: tmp path squatted on every attempt")
		pkgOpsMetrics.RecordUploadOutcome(uploadWorkspaceID(), uploadOutcomeWriteError)
		writeUploadError(w, http.StatusInternalServerError, "storage unavailable")
	}
}

// abortUploadTmp closes the sink and removes the .tmp — the atomic-or-
// absent contract (design epic-67 D3): any mid-write error leaves no
// partial file behind.
func abortUploadTmp(sink uploadSink, tmpPath string) {
	_ = sink.Close()
	//nolint:gosec // G703: tmpPath is server-generated (uuid + operator-configured root); no client bytes reach it
	_ = os.Remove(tmpPath)
}

// uploadCopyErrorStatus maps body-write failures to response codes:
// ENOSPC → 507 (disk full — design epic-67 resource-exhaustion guard),
// read-deadline/timeout → 504 (slowloris), anything else → 500.
func uploadCopyErrorStatus(err error) int {
	if errors.Is(err, syscall.ENOSPC) {
		return http.StatusInsufficientStorage
	}
	if errors.Is(err, os.ErrDeadlineExceeded) || os.IsTimeout(err) {
		return http.StatusGatewayTimeout
	}
	return http.StatusInternalServerError
}

// scrubUploadTmpFiles removes stale uploads/*.tmp left by a crash
// mid-upload (design epic-67 D3 boot scrub). Best-effort per file; the
// count is the number actually removed.
func scrubUploadTmpFiles(uploadsDir string) (int, error) {
	matches, err := filepath.Glob(filepath.Join(uploadsDir, "*.tmp"))
	if err != nil {
		return 0, fmt.Errorf("upload scrub glob: %w", err)
	}
	removed := 0
	for _, m := range matches {
		if err := os.Remove(m); err != nil {
			continue
		}
		removed++
	}
	return removed, nil
}

// scrubUploadsAtBoot runs the boot scrub and records the count. Failure
// is logged and swallowed: scrub is hygiene, never a boot blocker.
func scrubUploadsAtBoot(logger *zap.Logger, uploadsDir string) {
	removed, err := scrubUploadTmpFiles(uploadsDir)
	if err != nil {
		logger.Warn("upload boot scrub failed", zap.String("dir", uploadsDir), zap.Error(err))
		return
	}
	if removed > 0 {
		logger.Info("upload boot scrub removed stale tmp files", zap.Int("count", removed))
	}
	pkgOpsMetrics.RecordUploadScrub(uploadWorkspaceID(), removed)
}

func uploadWorkspaceID() string {
	if id := os.Getenv("WORKSPACE_ID"); id != "" {
		return id
	}
	return "unknown"
}
