// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// pi_rpc.go — the id-correlated JSONL command client for one pi RPC-mode
// child (pi second harness, story S2; scoping report §1.2-§1.4, §2.3
// item 4). Speaks the four record families of pi's RPC protocol
// (upstream docs/rpc.md, pinned v1.5.2):
//
//	stdin  Command            — one JSON object per line, optional "id"
//	stdout response           — id-correlated command result (or a
//	                            no-id "parse" response for malformed
//	                            stdin JSON)
//	stdout session event      — agent/message/tool/queue/compaction/
//	                            retry activity (bash_execution_update is
//	                            the one id-bearing event family)
//	both   Extension UI record — extension_ui_request (pi→client) /
//	                            extension_ui_response (client→pi)
//
// Framing contract (rpc.md "Framing", validated against the pinned
// binary): strict JSONL, records split ONLY on LF with an optional
// preceding CR stripped — never on Unicode line/paragraph separators,
// and NEVER through bufio.Scanner's 64 KiB cap (a real agent_end
// record carries the full message array and blows straight past it;
// the read loop uses bufio.Reader.ReadString, which has no length
// limit). stdout is read continuously for the life of the child (pi
// honors stdout backpressure, so a stalled reader stalls pi); stderr
// is drained to the logger as diagnostics only and never parsed as
// protocol. stdin writes are serialized and blocking — the OS pipe is
// the backpressure mechanism, and a write that outlives its deadline
// marks the child wedged so callers fail fast instead of piling on.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"go.uber.org/zap"
)

// piCallTimeout is the default per-command deadline used by Call when
// the caller's context carries none. Generous on purpose: pi answers
// abort/get_state only after in-flight work settles (rpc-commands.md
// "abort ... wait for the session to become idle before responding"),
// so a busy session legitimately takes seconds to answer state probes.
const piCallTimeout = 30 * time.Second

// piStdinWriteTimeout bounds one serialized stdin write. A full pipe
// that does not drain within this window means the child stopped
// reading commands — the wedge classification, not backpressure.
// A var (not const) so the wedge tests can tighten it instead of
// waiting the production window.
var piStdinWriteTimeout = 15 * time.Second

// piRecord is the minimal decode of ONE stdout record: the envelope
// every family shares (type, id) plus the response family's fields.
// Session events are carried as raw JSON; the event pump (pi_state.go)
// decodes the per-type payloads it cares about and tolerates unknown
// types (the defensive-decode posture upstream itself documents for
// session entries).
type piRecord struct {
	Type string `json:"type"`
	ID   string `json:"id,omitempty"`

	// Response family (absent on session events).
	Command string          `json:"command,omitempty"`
	Success *bool           `json:"success,omitempty"`
	Error   string          `json:"error,omitempty"`
	Data    json.RawMessage `json:"data,omitempty"`

	// Raw is the complete record JSON. Session events carry payload at
	// the envelope TOP level (message_end.message, agent_settled
	// .aborted, session_info_changed.name, …) — only responses nest
	// under data — so the pump decodes specifics from here.
	Raw json.RawMessage `json:"-"`
}

// isResponse reports whether the record is a command response (the
// id-correlated family), as opposed to a session event or extension
// UI record. Keyed on Type so an unknown event named "response" can
// never be misrouted.
func (r *piRecord) isResponse() bool { return r.Type == "response" }

// piResponse is a completed command result delivered to a Call waiter.
type piResponse struct {
	record piRecord
	// decodeErr is set when the line was not valid JSON — delivered as
	// a failed response so the waiter observes the protocol error
	// instead of hanging on a lost reply.
	decodeErr error
}

// piEventObserver receives every non-response stdout record (session
// events, extension UI records, and uncorrelated bash_execution_update
// chunks). Called on the reader goroutine — implementations must not
// block; buffer or hand off if slow.
type piEventObserver func(rec *piRecord, raw []byte)

// piSpawnConfig is the resolved spawn contract for one pi child
// (scoping §2.3 item 4). Binary comes from the S1 overlay verify
// (LLMSAFESPACES_PI_BINARY, sha256-checked before any spawn); the
// session dir lives on the PVC; the agent dir holds PI_CODING_AGENT_DIR
// state (S3's config writer populates it).
type piSpawnConfig struct {
	Binary     string
	AgentDir   string
	SessionDir string
	SessionID  string // optional: pin the child to one platform session
	// Ephemeral adds --no-session (an in-memory session that is not
	// persisted — cli.md). Platform sessions are PERSISTED (resume
	// across restart is the §5 contract), so children the supervisor
	// manages leave this false; it exists for probe/utility children.
	Ephemeral bool
	// Offline adds --offline (PI_OFFLINE=1 semantics: disable startup
	// network operations — the egress-restricted-pod posture, scoping
	// §8 R4). Wired explicitly per spawn, not inherited.
	Offline bool
	// Env carries extra env pairs appended after the process env
	// (PI_CODING_AGENT_DIR is always set from AgentDir).
	Env []string
	// Dir is the child's working directory ("" = inherit).
	Dir string
}

// piChild manages ONE pi --mode rpc process: the pipes, the id/response
// correlation table, the continuous stdout reader, and the stderr
// diagnostics drain. Restart policy, health derivation, and session
// attribution live above this type (pi_policy.go / pi_supervisor.go).
type piChild struct {
	cfg     piSpawnConfig
	log     *zap.Logger
	onEvent piEventObserver

	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser

	writeMu         sync.Mutex // serializes stdin writes (backpressure point)
	mu              sync.Mutex // guards pending + wedged + shutdownStarted
	pending         map[string]chan piResponse
	nextID          atomic.Uint64
	wedged          bool
	shutdownStarted bool

	// idPrefix namespaces command ids per child so logs from several
	// children never show colliding correlation ids.
	idPrefix string

	// lastReadAt/lastWriteAt are written by the reader/writer paths and
	// read (racy-but-bounded, like managedProcess.pid's staleness
	// contract) by the health loop as wedge evidence.
	lastReadAt  atomic.Int64 // unix nanos
	lastWriteAt atomic.Int64

	// exited closes when the underlying process is reaped; exitErr
	// carries cmd.Wait()'s result. Owned by the wait goroutine.
	exited  chan struct{}
	exitErr error

	startedAt time.Time
}

// newPiChild builds (but does not start) a pi child.
func newPiChild(cfg piSpawnConfig, log *zap.Logger, onEvent piEventObserver) *piChild {
	return &piChild{
		cfg:      cfg,
		log:      log.With(zap.String("component", "pi_child")),
		onEvent:  onEvent,
		pending:  make(map[string]chan piResponse),
		idPrefix: fmt.Sprintf("p%d-", os.Getpid()),
		exited:   make(chan struct{}),
	}
}

// piChildCmd assembles the argv + env for the RPC-mode spawn. Single
// construction point (the opencodeServeCmd precedent) so the persisted
// and ephemeral shapes cannot drift:
//
//	pi --mode rpc [--no-session] [--offline] --session-dir <PVC>
//	   [--session-id <id>]
//
// with PI_CODING_AGENT_DIR=<AgentDir> in the environment.
func piChildCmd(cfg piSpawnConfig) *exec.Cmd {
	argv := []string{cfg.Binary, "--mode", "rpc"}
	if cfg.Ephemeral {
		argv = append(argv, "--no-session")
	}
	if cfg.Offline {
		argv = append(argv, "--offline")
	}
	argv = append(argv, "--session-dir", cfg.SessionDir)
	if cfg.SessionID != "" {
		argv = append(argv, "--session-id", cfg.SessionID)
	}
	//nolint:gosec,noctx // G204/noctx: argv is built from the
	// sha256-verified overlay binary path and fixed flags only — no
	// shell, no caller strings. The child is a long-lived daemon, not a
	// per-call command (the opencodeServeCmd precedent).
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = append(os.Environ(), "PI_CODING_AGENT_DIR="+cfg.AgentDir)
	cmd.Env = append(cmd.Env, cfg.Env...)
	if cfg.Dir != "" {
		cmd.Dir = cfg.Dir
	}
	return cmd
}

// start spawns the process and the pipe goroutines. The cmd is built
// here from the spawn config (piChildCmd) unless a test pre-populated
// c.cmd. The caller owns shutdown via the Shutdown ladder; start is
// once-per-piChild (a restarted child is a NEW piChild, the
// managedProcess generation model).
func (c *piChild) start() error {
	if c.cmd == nil {
		c.cmd = piChildCmd(c.cfg)
	}
	stdin, err := c.cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("pi stdin pipe: %w", err)
	}
	stdout, err := c.cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("pi stdout pipe: %w", err)
	}
	stderr, err := c.cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("pi stderr pipe: %w", err)
	}
	if err := c.cmd.Start(); err != nil {
		return fmt.Errorf("pi start: %w", err)
	}
	c.stdin, c.stdout = stdin, stdout
	c.startedAt = time.Now()
	c.lastReadAt.Store(time.Now().UnixNano())
	c.lastWriteAt.Store(time.Now().UnixNano())

	// The reaper contract (#904): children of a subreaping supervisor
	// must never be stolen by the orphan reaper's Wait. The pi child is
	// tracked from Start to Wait here, exactly like managedProcess's.
	pkgOrphanReaper.track(c.cmd.Process.Pid)

	go c.readLoop(stdout)
	go c.drainStderr(stderr)
	go func() {
		c.exitErr = c.cmd.Wait()
		pkgOrphanReaper.untrack(c.cmd.Process.Pid)
		close(c.exited)
	}()
	c.log.Info("pi rpc child started",
		zap.Int("pid", c.cmd.Process.Pid),
		zap.String("sessionID", c.cfg.SessionID),
		zap.Bool("ephemeral", c.cfg.Ephemeral))
	return nil
}

// pid returns the child's pid (0 before start).
func (c *piChild) pid() int {
	if c.cmd != nil && c.cmd.Process != nil {
		return c.cmd.Process.Pid
	}
	return 0
}

// readLoop is the continuous stdout reader: split records on LF only,
// strip an optional preceding CR, decode, and dispatch by family. Runs
// until pipe EOF (child exit); a decode failure of ONE line is logged
// and never kills the loop (the pump must tolerate a mid-stream hiccup
// — though per rpc.md the stream is well-formed JSONL).
func (c *piChild) readLoop(r io.Reader) {
	br := bufio.NewReaderSize(r, 64*1024)
	for {
		line, err := br.ReadString('\n')
		if line != "" {
			c.lastReadAt.Store(time.Now().UnixNano())
			c.dispatch([]byte(strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")))
		}
		if err != nil {
			if !errors.Is(err, io.EOF) && c.notShutdown() {
				c.log.Warn("pi stdout read ended", zap.Error(err))
			}
			return
		}
	}
}

// notShutdown reports whether no Shutdown has begun (reader-side
// quiet logging after intentional exit).
func (c *piChild) notShutdown() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.shutdownStarted
}

// dispatch routes one decoded record per the four-family contract.
func (c *piChild) dispatch(raw []byte) {
	rec := &piRecord{}
	if err := json.Unmarshal(raw, rec); err != nil {
		c.log.Warn("pi record decode failed", zap.Error(err))
		return
	}
	rec.Raw = append(rec.Raw, raw...)
	switch {
	case rec.isResponse():
		c.deliverResponse(rec)
	case rec.ID != "" && c.takePendingIfAny(rec.ID) != nil:
		// An id-bearing non-response record (bash_execution_update
		// correlated to a bash command): the S2 supervisor issues no
		// bash commands, so this is unreachable today — the branch
		// exists so the family can never leak into the event pump.
	default:
		if c.onEvent != nil {
			c.onEvent(rec, raw)
		}
	}
}

// deliverResponse hands one response record to its Call waiter. A
// response with an unknown/absent id (late reply after Call timeout,
// or the no-id "parse" family) is logged, never dropped silently.
func (c *piChild) deliverResponse(rec *piRecord) {
	ch := c.takePendingIfAny(rec.ID)
	if ch == nil {
		c.log.Debug("pi response without waiter",
			zap.String("id", rec.ID),
			zap.String("command", rec.Command),
			zap.Bool("success", rec.Success != nil && *rec.Success))
		return
	}
	ch <- piResponse{record: *rec}
}

// takePendingIfAny removes and returns the waiter channel for id, or
// nil when no Call is outstanding under that id.
func (c *piChild) takePendingIfAny(id string) chan piResponse {
	if id == "" {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	ch, ok := c.pending[id]
	if ok {
		delete(c.pending, id)
	}
	return ch
}

// Call writes one command object and blocks for its id-correlated
// response. The command map may carry its own "id" (reserved for
// extension_ui_response); otherwise a unique id is assigned. Deadline:
// the caller's context, defaulting to piCallTimeout.
func (c *piChild) Call(ctx context.Context, command map[string]any) (json.RawMessage, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, piCallTimeout)
		defer cancel()
	}
	c.mu.Lock()
	if c.wedged {
		c.mu.Unlock()
		return nil, errors.New("pi child wedged: stdin not draining")
	}
	c.mu.Unlock()

	id, _ := command["id"].(string)
	if id == "" {
		id = fmt.Sprintf("%s%d", c.idPrefix, c.nextID.Add(1))
		command["id"] = id
	}
	ch := make(chan piResponse, 1)
	c.mu.Lock()
	if c.shutdownStarted {
		c.mu.Unlock()
		return nil, errors.New("pi child shutting down")
	}
	c.pending[id] = ch
	c.mu.Unlock()

	payload, err := json.Marshal(command)
	if err != nil {
		c.takePendingIfAny(id)
		return nil, fmt.Errorf("pi command encode: %w", err)
	}
	if err := c.writeLine(payload); err != nil {
		c.takePendingIfAny(id)
		return nil, err
	}

	select {
	case resp := <-ch:
		if resp.decodeErr != nil {
			return nil, fmt.Errorf("pi response decode: %w", resp.decodeErr)
		}
		if resp.record.Success == nil || !*resp.record.Success {
			msg := resp.record.Error
			if msg == "" {
				msg = "pi command failed"
			}
			return nil, errors.New(msg)
		}
		return resp.record.Data, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("pi call %q: %w", respCommand(command), ctx.Err())
	case <-c.exited:
		return nil, fmt.Errorf("pi call %q: child exited", respCommand(command))
	}
}

// respCommand names a command for error strings (diagnostics only).
func respCommand(command map[string]any) string {
	if t, ok := command["type"].(string); ok {
		return t
	}
	return "?"
}

// writeLine serializes one LF-terminated JSON write to stdin. The
// blocking write IS the backpressure mechanism; a write that exceeds
// piStdinWriteTimeout without draining marks the child wedged (all
// later Calls fail fast; the health loop restarts it).
func (c *piChild) writeLine(payload []byte) error {
	type writeResult struct {
		n   int
		err error
	}
	done := make(chan writeResult, 1)
	go func() {
		c.writeMu.Lock()
		defer c.writeMu.Unlock()
		n, err := c.stdin.Write(append(payload, '\n'))
		if err == nil && n > 0 {
			c.lastWriteAt.Store(time.Now().UnixNano())
		}
		done <- writeResult{n, err}
	}()
	select {
	case r := <-done:
		if r.err != nil {
			return fmt.Errorf("pi stdin write: %w", r.err)
		}
		return nil
	case <-time.After(piStdinWriteTimeout):
		c.mu.Lock()
		c.wedged = true
		c.mu.Unlock()
		return errors.New("pi stdin write timed out: child not draining (marked wedged)")
	}
}

// drainStderr continuously copies the child's stderr to the logger at
// debug level: diagnostics only, never protocol (rpc.md).
func (c *piChild) drainStderr(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 16*1024), 256*1024)
	for sc.Scan() {
		c.log.Debug("pi stderr", zap.String("line", sc.Text()))
	}
}

// Shutdown runs the rpc.md ladder for this child:
//
//  1. close stdin (EOF) — pi disposes the runtime and WAITS FOR
//     PENDING WORK (an in-flight turn) before exiting, so the caller
//     grants a bounded grace;
//  2. on grace expiry, SIGTERM — once a shutdown has started, "a
//     stdin EOF or a single signal does not interrupt it; a second
//     signal exits at once" — the supervisor's repeat kill is that
//     second signal;
//  3. after termGrace, SIGKILL; the process is reaped either way.
//
// The ladder SUCCEEDS when the process is gone — the wait error
// ("signal: terminated"/"killed") is the EXPECTED ladder outcome on
// the signaled rungs, not a failure, so it is never returned
// (managedProcess.stop ignores waitErr the same way). Idempotent;
// safe for concurrent callers (first wins the ladder).
func (c *piChild) Shutdown(ctx context.Context, grace, termGrace time.Duration) error {
	c.mu.Lock()
	if c.shutdownStarted {
		c.mu.Unlock()
		<-c.exited
		return nil
	}
	c.shutdownStarted = true
	stdin := c.stdin
	pid := c.pid()
	c.mu.Unlock()

	if stdin != nil {
		_ = stdin.Close()
	}
	select {
	case <-c.exited:
		return nil
	case <-ctx.Done():
	case <-time.After(grace):
	}
	if pid > 0 {
		// syscall.Kill instead of cmd.Process.Signal/Kill to avoid a
		// data race with the wait goroutine's cmd.Wait() (the
		// managedProcess precedent, worklog 0125 Bug 2).
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}
	select {
	case <-c.exited:
		return nil
	case <-ctx.Done():
	case <-time.After(termGrace):
	}
	if pid > 0 {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
	<-c.exited
	return nil
}

// Wedged reports the stdin-not-draining classification.
func (c *piChild) Wedged() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.wedged
}

// Alive reports whether the process is still running.
func (c *piChild) Alive() bool {
	select {
	case <-c.exited:
		return false
	default:
		return true
	}
}

// LastReadAge is seconds since the last stdout record (wedge evidence;
// a booted idle pi emits nothing, so this is corroborating input, never
// verdict — same posture as watchdog vitals).
func (c *piChild) LastReadAge() time.Duration {
	return time.Since(time.Unix(0, c.lastReadAt.Load()))
}
