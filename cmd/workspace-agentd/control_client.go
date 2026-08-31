// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// control_client.go — US-2 (design 0051 Appendix A): the sidecar's
// control-socket client. One TCP connection per request, one JSON object
// each way (A.1) — no pooling, no sessions: the protocol is deliberately
// minimal and the client mirrors it.
//
// Failure posture: transport errors (supervisor restarting, socket gone)
// are ordinary Go errors; protocol errors come back as *controlError with
// the wire code. Callers must treat every failure as "no evidence /
// no action" — never a panic, never a retry loop (A.3 volumes are 5s+).

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sync/atomic"
	"time"
)

// ControlSocketAddr is the fixed v1 supervisor address (A.0).
func ControlSocketAddr() string { return fmt.Sprintf("127.0.0.1:%d", ControlSocketPort) }

// controlClientError wraps the wire controlError (A.3 closed code set)
// with a client-side Error() prefix; the Code field stays inspectable
// for callers routing on version_unsupported vs bad_request.
type controlClientError struct {
	ctl controlError
}

func (e *controlClientError) Error() string {
	return fmt.Sprintf("control socket: %s: %s", e.ctl.Code, e.ctl.Message)
}

func (e *controlClientError) Code() string { return e.ctl.Code }

// typed results — mirrors of the A.2 result objects.

type controlHello struct {
	Supervisor string `json:"supervisor"`
	PID        int    `json:"pid"`
	ChildState string `json:"child_state"`
}

type controlStatus struct {
	ChildPID      int       `json:"child_pid"`
	ChildState    string    `json:"child_state"`
	Restarts      int       `json:"restarts"`
	LastRestartAt time.Time `json:"-"`
	// US-70.1 terminal-verified spawn-env state (design 0057 I4/I10):
	// what the last-spawned child actually spawned with, plus the
	// delivery degrade reason when incomplete.
	SpawnedRev       string `json:"-"`
	SpawnEnvDegraded bool   `json:"-"`
	SpawnEnvReason   string `json:"-"`
	// R2b (#1165): the file-class delivery state — terminal rev over the
	// files the uid-1000 supervisor wrote, plus its degrade reason.
	FilesRev         string `json:"-"`
	SpawnFilesReason string `json:"-"`
}

type controlRestartResult struct {
	Restarted  bool `json:"restarted"`
	InProgress bool `json:"in_progress"`
}

// controlClient speaks protocol v1 to the supervisor.
type controlClient struct {
	addr    string
	timeout time.Duration
	version int

	nextIDAtomic atomic.Int64
}

func newControlClient(addr string) *controlClient {
	return &controlClient{addr: addr, timeout: 2 * time.Second, version: controlProtocolVersion}
}

func (c *controlClient) nextID() int64 { return c.nextIDAtomic.Add(1) }

// call performs one request/response round trip.
func (c *controlClient) call(ctx context.Context, method string, params map[string]any) (map[string]any, error) {
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "tcp", c.addr)
	if err != nil {
		return nil, fmt.Errorf("control socket: dial %s: %w", c.addr, err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(c.timeout))

	id := c.nextID()
	req := struct {
		V      int            `json:"v"`
		ID     int64          `json:"id"`
		Method string         `json:"method"`
		Params map[string]any `json:"params,omitempty"`
	}{V: c.version, ID: id, Method: method, Params: params}
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return nil, fmt.Errorf("control socket: write %s: %w", method, err)
	}

	var resp struct {
		V      int            `json:"v"`
		ID     int64          `json:"id"`
		Result map[string]any `json:"result"`
		Error  *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(bufio.NewReader(conn)).Decode(&resp); err != nil {
		return nil, fmt.Errorf("control socket: read %s: %w", method, err)
	}
	if resp.Error != nil {
		return nil, &controlClientError{ctl: controlError{Code: resp.Error.Code, Message: resp.Error.Message}}
	}
	return resp.Result, nil
}

func (c *controlClient) Hello(ctx context.Context) (*controlHello, error) {
	res, err := c.call(ctx, "hello", map[string]any{})
	if err != nil {
		return nil, err
	}
	h := &controlHello{}
	_ = json.Unmarshal(mustMarshal(res), h)
	return h, nil
}

func (c *controlClient) Status(ctx context.Context) (*controlStatus, error) {
	res, err := c.call(ctx, "status", map[string]any{})
	if err != nil {
		return nil, err
	}
	raw := struct {
		ChildPID         int    `json:"child_pid"`
		ChildState       string `json:"child_state"`
		Restarts         int    `json:"restarts"`
		LastRestartAt    string `json:"last_restart_at"`
		SpawnedRev       string `json:"spawned_rev"`
		SpawnEnvDegraded bool   `json:"spawn_env_degraded"`
		SpawnEnvReason   string `json:"spawn_env_reason"`
		FilesRev         string `json:"files_rev"`
		SpawnFilesReason string `json:"spawn_files_reason"`
	}{}
	if err := json.Unmarshal(mustMarshal(res), &raw); err != nil {
		return nil, fmt.Errorf("control socket: status decode: %w", err)
	}
	st := &controlStatus{
		ChildPID:         raw.ChildPID,
		ChildState:       raw.ChildState,
		Restarts:         raw.Restarts,
		SpawnedRev:       raw.SpawnedRev,
		SpawnEnvDegraded: raw.SpawnEnvDegraded,
		SpawnEnvReason:   raw.SpawnEnvReason,
		FilesRev:         raw.FilesRev,
		SpawnFilesReason: raw.SpawnFilesReason,
	}
	if raw.LastRestartAt != "" {
		t, err := time.Parse(time.RFC3339, raw.LastRestartAt)
		if err != nil {
			return nil, fmt.Errorf("control socket: status timestamp: %w", err)
		}
		st.LastRestartAt = t
	}
	return st, nil
}

// Restart requests a child restart with a closed-enum reason (A.4
// invariant 2) and the SIGTERM→SIGKILL grace in seconds.
func (c *controlClient) Restart(ctx context.Context, reason string, graceSeconds int) (*controlRestartResult, error) {
	res, err := c.call(ctx, "restart", map[string]any{
		"reason":        reason,
		"grace_seconds": graceSeconds,
	})
	if err != nil {
		return nil, err
	}
	r := &controlRestartResult{}
	_ = json.Unmarshal(mustMarshal(res), r)
	return r, nil
}

// SupervisorSpawnStatus is the status method's terminal spawn fields
// (US-70.1): the rev the child actually spawned with (I4) and the active
// degrade reason (I10, "" healthy).
type SupervisorSpawnStatus struct {
	SpawnedRev string `json:"spawned_rev"`
	Degraded   string `json:"degraded"`
}

// SpawnStatus fetches the supervisor's terminal spawn state via the
// control socket's status method.
func (c *controlClient) SpawnStatus(ctx context.Context) (*SupervisorSpawnStatus, error) {
	res, err := c.call(ctx, "status", map[string]any{})
	if err != nil {
		return nil, err
	}
	out := &SupervisorSpawnStatus{}
	_ = json.Unmarshal(mustMarshal(res), out)
	return out, nil
}

// SpawnEnv stores the composed child env for the NEXT spawn (US-0.2(a):
// memory-only, write-only — there is deliberately no read-back).
func (c *controlClient) SpawnEnv(ctx context.Context, env map[string]string) error {
	_, err := c.call(ctx, "spawn_env", map[string]any{"env": env})
	return err
}

// Metrics returns the workspace container's cgroup numbers (A.2).
func (c *controlClient) Metrics(ctx context.Context) (*cgroupMetrics, error) {
	res, err := c.call(ctx, "metrics", map[string]any{})
	if err != nil {
		return nil, err
	}
	wrapper := struct {
		Cgroup *cgroupMetrics `json:"cgroup"`
	}{}
	if err := json.Unmarshal(mustMarshal(res), &wrapper); err != nil {
		return nil, fmt.Errorf("control socket: metrics decode: %w", err)
	}
	if wrapper.Cgroup == nil {
		wrapper.Cgroup = &cgroupMetrics{}
	}
	return wrapper.Cgroup, nil
}

func mustMarshal(v any) []byte {
	data, err := json.Marshal(v)
	if err != nil {
		return []byte("{}")
	}
	return data
}
