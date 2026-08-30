// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// Control socket v1 — design 0051 Appendix A.
//
// supervised sidecar agentd ⇄ supervise-opencode (PID 1 of the workspace
// container), 127.0.0.1:4099. One JSON request per TCP connection, one
// JSON response back; no framing, sessions, or streaming. Deliberately
// unauthenticated per the capability-equivalence rule (A.4): every
// capability this socket grants is strictly weaker than what same-uid
// code already holds (SIGKILL/SIGSTOP/ptrace//proc). The two permanent
// invariants are enforced here, not just documented:
//
//	(1) no method returns env values — spawn_env is write-only;
//	(2) restart takes a closed reason enum, never argv — unknown params
//	    are rejected with bad_request rather than ignored (argv is not an
//	    "additive field", it is a capability).

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"
)

// ControlSocketPort is the fixed v1 port (design 0051 A.0).
const ControlSocketPort = 4099

// controlProtocolVersion is the only supported wire version (A.1).
const controlProtocolVersion = 1

// restartReasons is the CLOSED enum (A.2 / A.4 invariant 2).
var restartReasons = map[string]bool{
	"health_watchdog":   true,
	"relay_injector":    true,
	"session_aware":     true,
	"credential_reload": true,
	"manual":            true,
}

type controlRequest struct {
	V      *int           `json:"v"`
	ID     *int64         `json:"id"`
	Method string         `json:"method"`
	Params map[string]any `json:"params"`
}

type controlResponse struct {
	V      int            `json:"v"`
	ID     int64          `json:"id"`
	Result map[string]any `json:"result,omitempty"`
	Error  *controlError  `json:"error,omitempty"`
}

type controlError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// supervisedProc abstracts the supervised child for the socket handlers
// (production: *managedProcess; tests: the fake in the _test file).
type supervisedProcIface interface {
	Restart(reason string, graceSeconds int) (restarted, inProgress bool)
	State() (pid int, state string, restarts int, lastRestartAt time.Time)
	SetSpawnEnv(env map[string]string)
	SpawnStatus() (rev, degraded string)
}

type controlSocketServer struct {
	ln   net.Listener
	proc supervisedProcIface

	// metricsSource, when non-nil, supplies the `metrics` method's
	// cgroup numbers (US-2: the supervisor's own cgroup = the workspace
	// container's). Nil keeps the US-1 reserved envelope.
	metricsSource func() *cgroupMetrics

	mu     sync.Mutex
	closed bool

	// restartMu serializes restart execution (A.3): one at a time,
	// TryLock for the in_progress report.
	restartMu sync.Mutex
}

func newControlSocketServer(addr string, proc supervisedProcIface) (*controlSocketServer, error) {
	var listenCfg net.ListenConfig
	ln, err := listenCfg.Listen(context.Background(), "tcp", addr) //nolint:noctx // loopback control socket; listener has no request ctx to inherit
	if err != nil {
		return nil, fmt.Errorf("control socket listen %s: %w", addr, err)
	}
	return &controlSocketServer{ln: ln, proc: proc}, nil
}

// addr returns the concrete listener address (":0" resolves).
func (s *controlSocketServer) addr() string { return s.ln.Addr().String() }

func (s *controlSocketServer) close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return s.ln.Close()
}

// serve accepts until close(); each connection is handled on its own
// goroutine. A.3 originally specified single-threaded handling, but that
// lets one slow restart (seconds of child teardown) head-of-line-block
// every status/hello poll — contradicting the idempotency requirement
// the same section makes. Concurrency boundary: reads (hello/status/
// metrics) are lock-free; restart is serialized by restartMu;
// spawn_env stores atomically. The A.3 amendment lands with this code.
func (s *controlSocketServer) serve() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			s.mu.Lock()
			stopped := s.closed
			s.mu.Unlock()
			if stopped {
				return
			}
			slog.Warn("control socket accept error", "err", err.Error())
			continue
		}
		go s.handleConn(conn)
	}
}

func (s *controlSocketServer) handleConn(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

	var req controlRequest
	dec := json.NewDecoder(conn)
	if err := dec.Decode(&req); err != nil {
		writeJSON(conn, controlResponse{V: controlProtocolVersion, ID: 0,
			Error: &controlError{Code: "bad_request", Message: "malformed JSON"}})
		return
	}

	if req.V == nil {
		writeJSON(conn, s.errResp(req.ID, "bad_request", "missing v"))
		return
	}
	if *req.V != controlProtocolVersion {
		writeJSON(conn, controlResponse{V: controlProtocolVersion, ID: idOr(req.ID),
			Error: &controlError{Code: "version_unsupported",
				Message: fmt.Sprintf("protocol version %d not supported (want %d)", *req.V, controlProtocolVersion)}})
		return
	}
	if req.ID == nil {
		writeJSON(conn, s.errResp(req.ID, "bad_request", "missing id"))
		return
	}
	if req.Method == "" {
		writeJSON(conn, s.errResp(req.ID, "bad_request", "missing method"))
		return
	}

	// NOTE: unknown top-level FIELDS are already tolerated (json.Decoder
	// ignores them — A.1 forward compat). Method dispatch is closed:
	switch req.Method {
	case "hello":
		writeJSON(conn, s.hello(req.ID))
	case "status":
		writeJSON(conn, s.status(req.ID))
	case "restart":
		writeJSON(conn, s.restart(req))
	case "spawn_env":
		writeJSON(conn, s.spawnEnv(req))
	case "metrics":
		writeJSON(conn, s.metrics(req.ID))
	default:
		writeJSON(conn, s.errResp(req.ID, "method_unknown",
			fmt.Sprintf("method %q is not part of control protocol v1", req.Method)))
	}
}

func (s *controlSocketServer) hello(id *int64) controlResponse {
	pid, state, _, _ := s.proc.State()
	return controlResponse{V: controlProtocolVersion, ID: idOr(id),
		Result: map[string]any{
			"supervisor":  "supervise-opencode",
			"pid":         pid,
			"child_state": state,
		}}
}

func (s *controlSocketServer) status(id *int64) controlResponse {
	pid, state, restarts, last := s.proc.State()
	var lastStr string
	if !last.IsZero() {
		lastStr = last.UTC().Format(time.RFC3339)
	}
	// US-70.1 (I4/I10): terminal spawn verification + loud degradation —
	// the sidecar's statusz relays these into the CRD/alert path.
	spawnedRev, degraded := s.proc.SpawnStatus()
	result := map[string]any{
		"child_pid":       pid,
		"child_state":     state,
		"restarts":        restarts,
		"last_restart_at": lastStr,
		"spawned_rev":     spawnedRev,
	}
	if degraded != "" {
		result["degraded"] = degraded
	}
	return controlResponse{V: controlProtocolVersion, ID: idOr(id), Result: result}
}

func (s *controlSocketServer) restart(req controlRequest) controlResponse {
	// A.4 invariant 2: closed reason enum; argv or any capability-shaped
	// param is a REJECTION, not an ignored unknown field.
	if _, hasArgv := req.Params["argv"]; hasArgv {
		return s.errResp(req.ID, "bad_request", "restart does not accept argv")
	}
	reason, _ := req.Params["reason"].(string)
	if !restartReasons[reason] {
		return s.errResp(req.ID, "bad_request",
			fmt.Sprintf("reason %q is not a known restart reason", reason))
	}
	grace := 30
	if g, ok := req.Params["grace_seconds"].(float64); ok && g > 0 && g <= 300 {
		grace = int(g)
	}
	// A.3 idempotency at the server: exactly one Restart runs at a time
	// (restartMu); a request arriving while one is in flight reports
	// in_progress instead of queueing (first restart's params win).
	if !s.restartMu.TryLock() {
		return controlResponse{V: controlProtocolVersion, ID: idOr(req.ID),
			Result: map[string]any{"restarted": false, "in_progress": true}}
	}
	defer s.restartMu.Unlock()

	restarted, _ := s.proc.Restart(reason, grace)
	return controlResponse{V: controlProtocolVersion, ID: idOr(req.ID),
		Result: map[string]any{"restarted": restarted, "in_progress": false}}
}

func (s *controlSocketServer) spawnEnv(req controlRequest) controlResponse {
	envRaw, ok := req.Params["env"].(map[string]any)
	if !ok {
		return s.errResp(req.ID, "bad_request", "spawn_env requires env object")
	}
	env := make(map[string]string, len(envRaw))
	for k, v := range envRaw {
		val, ok := v.(string)
		if !ok {
			return s.errResp(req.ID, "bad_request", "env values must be strings")
		}
		env[k] = val
	}
	s.proc.SetSpawnEnv(env)
	return controlResponse{V: controlProtocolVersion, ID: idOr(req.ID),
		Result: map[string]any{"stored": true}}
}

func (s *controlSocketServer) metrics(id *int64) controlResponse {
	// Field set frozen by A.2 (US-2): memory current/max, cpu usage and
	// throttled — sourced from the WORKSPACE container's cgroup, which
	// only the supervisor can read (0050 finding: the sidecar's own
	// cgroup is the wrong one). No source wired → reserved envelope.
	if s.metricsSource == nil {
		return controlResponse{V: controlProtocolVersion, ID: idOr(id),
			Result: map[string]any{"cgroup": map[string]any{}}}
	}
	m := s.metricsSource()
	if m == nil {
		m = &cgroupMetrics{}
	}
	return controlResponse{V: controlProtocolVersion, ID: idOr(id),
		Result: map[string]any{"cgroup": map[string]any{
			"memory_current_bytes": m.MemoryCurrentBytes,
			"memory_max_bytes":     m.MemoryMaxBytes,
			"cpu_usage_usec":       m.CPUUsageUsec,
			"cpu_throttled_usec":   m.CPUThrottledUsec,
		}}}
}

func (s *controlSocketServer) errResp(id *int64, code, msg string) controlResponse {
	return controlResponse{V: controlProtocolVersion, ID: idOr(id),
		Error: &controlError{Code: code, Message: msg}}
}

func idOr(id *int64) int64 {
	if id == nil {
		return 0
	}
	return *id
}

func writeJSON(conn net.Conn, resp controlResponse) {
	_ = json.NewEncoder(conn).Encode(resp)
}
