// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// disk_recover.go — #1601: the agentd execution surface for mechanical
// disk recovery. POST /v1/disk-recover on the user mux; the engine runs
// where the filesystem view is:
//
//   - single-container mode: in-process (agentd is PID 1 of the
//     workspace container — full RW view of /workspace, /home/sandbox,
//     /tmp as uid 1000).
//   - sidecar mode: forwarded over the control socket to the uid-1000
//     supervisor (the sidecar's /workspace mount is READ-ONLY and it
//     does not mount /home/sandbox at all — controller/internal/
//     workspace/agentd_sidecar.go; same finding as the legacy-key
//     scrub: the supervisor is the only agentd process that can act on
//     the PVC).
//
// Owner ruling on #1601 honored end-to-end: the rescue path works at
// 100% full because it writes nothing — no temp files, no report
// persistence, the report is the HTTP response body only.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/lenaxia/llmsafespaces/pkg/diskrecovery"
)

// diskRecoverTimeout bounds one full sweep (measure + delete loop).
// The handler's engine ctx, the control-socket method ctx, and the
// supervisor engine all share this single bound.
const diskRecoverTimeout = 90 * time.Second

// diskRecoverEngine is the execution seam behind the HTTP handler:
// *diskrecovery.Engine (in-process), socketDiskRecover (sidecar), or a
// fake in tests.
type diskRecoverEngine interface {
	Recover(ctx context.Context, req diskrecovery.Request) (diskrecovery.Report, error)
}

// engineDiskRecover adapts the in-process engine.
type engineDiskRecover struct{ eng *diskrecovery.Engine }

func (e engineDiskRecover) Recover(ctx context.Context, req diskrecovery.Request) (diskrecovery.Report, error) {
	return e.eng.Recover(ctx, req)
}

// runtimeBaseFromEnv selects the compiled-in manifest. The env can only
// CHOOSE among compiled-in bases — never widen one; an unknown value
// fails safe (empty manifest, nothing deletable).
func runtimeBaseFromEnv() string {
	if v := os.Getenv("LLMSAFESPACE_RUNTIME_BASE"); v != "" {
		return v
	}
	return diskrecovery.DefaultRuntimeBase
}

// newDiskRecoverEngine builds the in-process engine over the real
// filesystem (single-container mode and the supervisor side of the
// control socket). Fails loud at boot on a manifest that does not pass
// its own boundary validation — a manifest bug must be a boot error,
// never a per-request refusal in production.
func newDiskRecoverEngine() (diskRecoverEngine, error) {
	manifest := diskrecovery.ManifestFor(runtimeBaseFromEnv())
	if err := diskrecovery.ValidateManifest(manifest); err != nil {
		return nil, fmt.Errorf("disk recovery manifest invalid for base %q: %w", manifest.Base, err)
	}
	usage := func() (int64, int64, error) {
		d := getDiskUsage()
		if d == nil || d.TotalBytes <= 0 {
			return 0, 0, errors.New("disk usage unavailable")
		}
		return d.UsedBytes, d.TotalBytes, nil
	}
	return engineDiskRecover{eng: diskrecovery.NewEngine(manifest, usage)}, nil
}

// maxDiskRecoverBodyBytes bounds the request body (#1561/#1564/#1565
// bounded-read convention): the body is two scalars.
const maxDiskRecoverBodyBytes = 1024

// diskRecoverHandler serves POST /v1/disk-recover — §D1 carve-out pair
// gate (control-plane OR workspace password), identical to every other
// user-mux route the API drives. Response-only by design.
func diskRecoverHandler(workspacePassword, agentdPassword string, eng diskRecoverEngine) http.HandlerFunc {
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
			http.Error(w, "disk recovery not wired", http.StatusServiceUnavailable)
			return
		}
		var body diskrecovery.Request
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxDiskRecoverBodyBytes))
		if err := dec.Decode(&body); err != nil {
			http.Error(w, "bad request: body must be one JSON document {dryRun, targetRatio}", http.StatusBadRequest)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), diskRecoverTimeout)
		defer cancel()
		report, err := eng.Recover(ctx, body)
		if err != nil {
			switch {
			case errors.Is(err, diskrecovery.ErrBusy):
				http.Error(w, "disk recovery already in progress", http.StatusConflict)
			case errors.Is(err, diskrecovery.ErrNoUsage):
				http.Error(w, "disk usage unavailable", http.StatusServiceUnavailable)
			case errors.Is(err, context.DeadlineExceeded):
				http.Error(w, "disk recovery timed out", http.StatusGatewayTimeout)
			default:
				http.Error(w, "disk recovery failed", http.StatusInternalServerError)
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(report)
	}
}

// socketDiskRecover forwards the recover to the supervisor over the
// control socket (sidecar mode): the sidecar cannot touch the PVC, the
// supervisor can.
type socketDiskRecover struct {
	client interface {
		DiskRecover(ctx context.Context, req diskrecovery.Request) (diskrecovery.Report, error)
	}
}

func (s socketDiskRecover) Recover(ctx context.Context, req diskrecovery.Request) (diskrecovery.Report, error) {
	if s.client == nil {
		return diskrecovery.Report{}, errors.New("disk recovery: no control client wired")
	}
	ctx, cancel := context.WithTimeout(ctx, diskRecoverTimeout+5*time.Second)
	defer cancel()
	return s.client.DiskRecover(ctx, req)
}

// diskRecoverControlMethod adapts the engine to the control socket's
// dispatch (sidecar → supervisor). Params: dry_run (bool),
// target_ratio (float). Result: the full diskrecovery.Report as a JSON
// map. The 10s blanket conn deadline cannot cover a bounded sweep of a
// huge modcache — same re-arm discipline as upload_apply: the sweep is
// bounded by a REAL ctx deadline, and the terminal response gets a
// fresh conn arm.
func (s *controlSocketServer) diskRecoverControlMethod(ctx context.Context, conn net.Conn, req controlRequest) controlResponse {
	if s.diskRecover == nil {
		return s.errResp(req.ID, "internal", "disk_recover engine unwired")
	}
	var params struct {
		DryRun      bool    `json:"dry_run"`
		TargetRatio float64 `json:"target_ratio"`
	}
	// Explicit-over-implicit (review r1 F6): a params shape we cannot
	// decode is a bad_request, never a silently-defaulted sweep.
	if req.Params != nil {
		if b, err := json.Marshal(req.Params); err != nil {
			return s.errResp(req.ID, "bad_request", "params not marshalable")
		} else if err := json.Unmarshal(b, &params); err != nil {
			return s.errResp(req.ID, "bad_request", "params must be {dry_run bool, target_ratio number}: "+err.Error())
		}
	}
	methodCtx, cancel := context.WithTimeout(ctx, diskRecoverTimeout)
	defer cancel()
	report, err := s.diskRecover.Recover(methodCtx, diskrecovery.Request{
		DryRun:      params.DryRun,
		TargetRatio: params.TargetRatio,
	})
	if conn != nil {
		_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	}
	if err != nil {
		code := "internal"
		switch {
		case errors.Is(err, diskrecovery.ErrBusy):
			code = "busy"
		case errors.Is(err, diskrecovery.ErrNoUsage):
			code = "no_usage"
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
