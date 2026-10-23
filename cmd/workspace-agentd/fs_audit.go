// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// fs_audit.go — #1808: the agentd execution surface for the zero-fill
// corruption audit. POST /v1/fs-audit on the user mux; the engine runs
// where the filesystem view is — identical topology to disk_recover.go
// (#1601):
//
//   - single-container mode: in-process (agentd is PID 1 of the
//     workspace container — full RW view of /workspace).
//   - sidecar mode: forwarded over the control socket to the uid-1000
//     supervisor (the only agentd process that can read the PVC).
//
// Owner-ruling posture inherited from #1601: RESPONSE-ONLY — the audit
// writes nothing, ever (it is a detector, not a repairer; zero-filled
// bytes are unrecoverable, so the report's job is to make the damage
// VISIBLE and stop it propagating, which the pre-push guard does at
// the git seam).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/lenaxia/llmsafespaces/pkg/fsaudit"
)

// fsAuditTimeout bounds one full sweep (walk + bounded reads). The
// engine's read budgets bound the work; this arm bounds the walk.
const fsAuditTimeout = 60 * time.Second

// fsAuditEngine is the execution seam behind the HTTP handler:
// *fsaudit.Engine (in-process), socketFsAudit (sidecar), or a fake in
// tests.
type fsAuditEngine interface {
	Audit(ctx context.Context, req fsaudit.Request) (fsaudit.Report, error)
}

// engineFsAudit adapts the in-process engine.
type engineFsAudit struct{ eng *fsaudit.Engine }

func (e engineFsAudit) Audit(ctx context.Context, req fsaudit.Request) (fsaudit.Report, error) {
	return e.eng.Audit(ctx, req)
}

// newFsAuditEngine builds the in-process engine over the workspace
// base (the PVC subPath every view shares; /home/sandbox and /tmp are
// subpath mounts of the same volume — the same base disk-recover's
// statfs reads).
func newFsAuditEngine() fsAuditEngine {
	return engineFsAudit{eng: fsaudit.NewEngine("/workspace")}
}

// maxFsAuditBodyBytes bounds the request body (#1561/#1564/#1565
// bounded-read convention): the body is one root string + two caps.
const maxFsAuditBodyBytes = 1024

// fsAuditHandler serves POST /v1/fs-audit — §D1 carve-out pair gate
// (control-plane OR workspace password), identical to every other
// user-mux route the API drives. Response-only by design. An EMPTY
// body is valid (audit the whole base — the frontend button's
// zero-config call).
func fsAuditHandler(workspacePassword, agentdPassword string, eng fsAuditEngine) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !checkBasicAuthAny(r, agentdPassword, workspacePassword) {
			rejectUnauthorized(w)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if eng == nil {
			http.Error(w, "fs audit not wired", http.StatusServiceUnavailable)
			return
		}
		var body fsaudit.Request
		if r.ContentLength > 0 {
			dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxFsAuditBodyBytes))
			if err := dec.Decode(&body); err != nil {
				http.Error(w, "bad request: body must be one JSON document {root, maxFileBytes, maxTotalBytes} or empty", http.StatusBadRequest)
				return
			}
		}
		ctx, cancel := context.WithTimeout(r.Context(), fsAuditTimeout)
		defer cancel()
		report, err := eng.Audit(ctx, body)
		if err != nil {
			switch {
			case errors.Is(err, fsaudit.ErrBusy):
				http.Error(w, "fs audit already in progress", http.StatusConflict)
			case errors.Is(err, context.DeadlineExceeded):
				http.Error(w, "fs audit timed out", http.StatusGatewayTimeout)
			default:
				http.Error(w, "fs audit failed", http.StatusInternalServerError)
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(report)
	}
}

// socketFsAudit forwards the audit to the supervisor over the control
// socket (sidecar mode): the sidecar's /workspace mount is READ-ONLY —
// readable for a scan, but the #1601 topology finding applies
// unchanged (the supervisor owns the PVC-facing execution surfaces), so
// the audit rides the same leg disk-recover does.
type socketFsAudit struct {
	client interface {
		FsAudit(ctx context.Context, req fsaudit.Request) (fsaudit.Report, error)
	}
}

func (s socketFsAudit) Audit(ctx context.Context, req fsaudit.Request) (fsaudit.Report, error) {
	if s.client == nil {
		return fsaudit.Report{}, errors.New("fs audit: no control client wired")
	}
	ctx, cancel := context.WithTimeout(ctx, fsAuditTimeout+5*time.Second)
	defer cancel()
	return s.client.FsAudit(ctx, req)
}

// fsAuditControlMethod adapts the engine to the control socket's
// dispatch (sidecar → supervisor). Params: root (string),
// max_file_bytes, max_total_bytes (numbers). Result: the full
// fsaudit.Report as a JSON map. Same re-arm discipline as
// disk_recover: the sweep is bounded by a REAL ctx deadline, the
// terminal response gets a fresh conn arm.
func (s *controlSocketServer) fsAuditControlMethod(ctx context.Context, conn net.Conn, req controlRequest) controlResponse {
	if s.fsAudit == nil {
		return s.errResp(req.ID, "internal", "fs_audit engine unwired")
	}
	var params struct {
		Root          string `json:"root"`
		MaxFileBytes  int64  `json:"max_file_bytes"`
		MaxTotalBytes int64  `json:"max_total_bytes"`
	}
	// Explicit-over-implicit (disk_recover r1 F6 parity): a params
	// shape we cannot decode is a bad_request, never a silently
	// defaulted scan.
	if req.Params != nil {
		if b, err := json.Marshal(req.Params); err != nil {
			return s.errResp(req.ID, "bad_request", "params not marshalable")
		} else if err := json.Unmarshal(b, &params); err != nil {
			return s.errResp(req.ID, "bad_request", "params must be {root string, max_file_bytes number, max_total_bytes number}: "+err.Error())
		}
	}
	methodCtx, cancel := context.WithTimeout(ctx, fsAuditTimeout)
	defer cancel()
	report, err := s.fsAudit.Audit(methodCtx, fsaudit.Request{
		Root:          params.Root,
		MaxFileBytes:  params.MaxFileBytes,
		MaxTotalBytes: params.MaxTotalBytes,
	})
	if conn != nil {
		_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	}
	if err != nil {
		code := "internal"
		switch {
		case errors.Is(err, fsaudit.ErrBusy):
			code = "busy"
		case errors.Is(err, context.DeadlineExceeded):
			code = "timeout"
		}
		return controlResponse{V: controlProtocolVersion, ID: idOr(req.ID),
			Error: &controlError{Code: code, Message: err.Error()}}
	}
	raw, merr := json.Marshal(report)
	if merr != nil {
		return s.errResp(req.ID, "internal", merr.Error())
	}
	var result map[string]any
	if err := json.Unmarshal(raw, &result); err != nil {
		return s.errResp(req.ID, "internal", err.Error())
	}
	return controlResponse{V: controlProtocolVersion, ID: idOr(req.ID), Result: result}
}

// FsAudit invokes the #1808 fs_audit method: the supervisor scans the
// PVC view with the fsaudit engine. The engine's sentinel errors ride
// the closed error-code enum back as typed errors so the sidecar's
// HTTP handler maps them to 409/504 exactly as the in-process path
// does. The caller's ctx deadline (the handler's fsAuditTimeout arm)
// is the bound — the 2s default does not apply to this long-held call.
func (c *controlClient) FsAudit(ctx context.Context, req fsaudit.Request) (fsaudit.Report, error) {
	timeout := c.timeout
	if dl, ok := ctx.Deadline(); ok {
		timeout = time.Until(dl)
	}
	res, err, _ := c.callDeadline(ctx, "fs_audit", map[string]any{
		"root":            req.Root,
		"max_file_bytes":  req.MaxFileBytes,
		"max_total_bytes": req.MaxTotalBytes,
	}, timeout)
	if err != nil {
		var ce *controlClientError
		if errors.As(err, &ce) {
			switch ce.ctl.Code {
			case "busy":
				return fsaudit.Report{}, fsaudit.ErrBusy
			case "timeout":
				return fsaudit.Report{}, fmt.Errorf("fs audit timed out on the supervisor: %w", context.DeadlineExceeded)
			}
		}
		if ctx.Err() != nil {
			return fsaudit.Report{}, ctx.Err()
		}
		return fsaudit.Report{}, err
	}
	out := fsaudit.Report{}
	if uerr := json.Unmarshal(mustMarshal(res), &out); uerr != nil {
		return fsaudit.Report{}, uerr
	}
	return out, nil
}
