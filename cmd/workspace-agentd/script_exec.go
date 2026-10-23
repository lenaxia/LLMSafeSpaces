// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// script_exec.go — #1878: script nodes execute in the WORKSPACE container.
//
// Owner ruling (2026-10-23): "the script should be executed in the same
// environment that the sessions run." In sidecar mode agentd serves the
// workflow mux from the FROM-scratch uid-2000 container — no /tmp, no
// interpreters, no gh, and a read-only /workspace — so script nodes
// either failed loudly (#1455's script_env_unavailable) or, on the
// trigger path, silently no-opped. The seam that already crosses the
// container boundary is the design-0051 Appendix-A control socket:
// upload_apply (#1244) and disk_recover (#1601) established the
// long-held, ctx-bounded method with a closed error enum. script_exec
// joins them — the sidecar forwards the handler source to the
// workspace-container supervisor (uid 1000, PID 1), which runs it via
// scriptwrap with the SAME environment construction the opencode child
// spawns with (opencodeBaseEnv + the live spawn delta), so interpreters,
// the mise shims, gh, and the secrets-env (GH_TOKEN et al.) resolve
// exactly as they do for session tool children.
//
// A.4 invariants hold by construction: no method returns env VALUES
// (the child env is built supervisor-side; the socket carries only
// script source in and script output out), and the capability is the
// same-uid equivalence class — every session already executes arbitrary
// code as uid 1000 with the full spawn env.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/lenaxia/llmsafespaces/pkg/workflows/scriptwrap"
)

// maxScriptExecHandlerBytes bounds the handler source the socket will
// execute — the same 16 MiB cap the workflow mux enforces on its
// request body (maxWorkflowExecBodyBytes, #1565): the handler rode that
// wire to reach this socket, so the budgets match.
const maxScriptExecHandlerBytes = 16 << 20

// scriptExecDefaultTimeout and scriptExecMaxTimeout bound the exec
// window. The mux's per-request timeout (workflowExecuteRequest.Timeout,
// default 10m, engine sends 5m for scripts) is authoritative; the
// defaults here apply only when the caller's ctx carries no deadline
// (older agentd) and the ceiling rejects anything beyond the mux's own
// maximum so a crafted timeout cannot hold a supervisor connection
// open past what the HTTP layer would have allowed.
const (
	scriptExecDefaultTimeout = 5 * time.Minute
	scriptExecMaxTimeout     = 10 * time.Minute
)

// Closed error-code enum (A.1/A.3 discipline — the upload_apply /
// disk_recover set, script edition). Every failure maps onto the
// workflow error envelope 1:1; nothing is swallowed.
const (
	scriptExecErrBadRequest     = "bad_request"
	scriptExecErrEnvUnavailable = "script_env_unavailable"
	scriptExecErrFailed         = "script_failed"
	scriptExecErrTimeout        = "script_timeout"
	scriptExecErrTransport      = "transport"
)

// scriptExecOutcome is the success result: the wrapper's raw stdout
// (a string — the caller validates JSON-object shape exactly as the
// local path does; carrying raw bytes as a string keeps a non-JSON
// stdout wire-safe), captured stderr, and the process exit code.
type scriptExecOutcome struct {
	Output   string `json:"output"`
	Stderr   string `json:"stderr,omitempty"`
	ExitCode int    `json:"exit_code"`
}

// scriptExecWireError is the typed failure: one of the closed enum
// codes above plus a self-teaching message.
type scriptExecWireError struct {
	Code    string
	Message string
}

func (e *scriptExecWireError) Error() string { return e.Code + ": " + e.Message }

// scriptExecEngine is the supervisor-side executor behind the
// script_exec control-socket method.
type scriptExecEngine struct {
	// childEnv builds the environment every script child runs with —
	// the opencode spawn construction (#1878: the session environment).
	childEnv func() []string
	// exec is the executor seam; production is scriptwrap.ExecuteWithEnv
	// (tests substitute to avoid interpreter dependence where the row
	// is about mapping, not execution).
	exec func(ctx context.Context, language scriptwrap.Language, handlerSource string, input any, env []string) (output json.RawMessage, stderr string, exitCode int, err error)
}

// newScriptExecEngine wires the production engine. childEnv must be the
// session-env construction (opencodeBaseEnv + live spawn delta).
func newScriptExecEngine(childEnv func() []string) *scriptExecEngine {
	return &scriptExecEngine{childEnv: childEnv, exec: scriptwrap.ExecuteWithEnv}
}

// run validates the params, probes the environment (loud
// script_env_unavailable — #1455's discipline, workspace-container
// edition), executes, and maps every failure onto the closed enum.
func (e *scriptExecEngine) run(ctx context.Context, params map[string]any) (*scriptExecOutcome, *scriptExecWireError) {
	var p struct {
		Language  string          `json:"language"`
		Handler   string          `json:"handler"`
		Input     json.RawMessage `json:"input"`
		TimeoutMS int64           `json:"timeout_ms"`
	}
	if err := json.Unmarshal(mustMarshal(params), &p); err != nil {
		return nil, &scriptExecWireError{scriptExecErrBadRequest, "cannot parse script_exec params: " + err.Error()}
	}
	lang := scriptwrap.Language(p.Language)
	if lang != scriptwrap.LanguagePython && lang != scriptwrap.LanguageNode {
		return nil, &scriptExecWireError{scriptExecErrBadRequest, fmt.Sprintf("language %q is not supported (valid: python, node) — the script is INLINE source, not a command", p.Language)}
	}
	if p.Handler == "" {
		return nil, &scriptExecWireError{scriptExecErrBadRequest, "handler (the inline source defining handler(input) -> dict) is required"}
	}
	if len(p.Handler) > maxScriptExecHandlerBytes {
		return nil, &scriptExecWireError{scriptExecErrBadRequest, fmt.Sprintf("handler exceeds the %d-byte cap", maxScriptExecHandlerBytes)}
	}
	timeout := scriptExecDefaultTimeout
	if p.TimeoutMS > 0 {
		timeout = time.Duration(p.TimeoutMS) * time.Millisecond
	}
	if timeout > scriptExecMaxTimeout {
		return nil, &scriptExecWireError{scriptExecErrBadRequest, fmt.Sprintf("timeout_ms %d exceeds the %s ceiling", p.TimeoutMS, scriptExecMaxTimeout)}
	}

	// #1455's loud prerequisite probe, workspace-container edition: a
	// broken TMPDIR/PATH HERE (not the sidecar's) is the named cause.
	if envErr := scriptwrap.EnvCheck(lang); envErr != nil {
		return nil, &scriptExecWireError{scriptExecErrEnvUnavailable,
			fmt.Sprintf("script node execution environment unavailable in the workspace container: %v — scripts execute in the session environment (#1878); check the workspace container's TMPDIR/PATH", envErr)}
	}

	var input any
	if len(p.Input) > 0 {
		if err := json.Unmarshal(p.Input, &input); err != nil {
			return nil, &scriptExecWireError{scriptExecErrBadRequest, "cannot parse input: " + err.Error()}
		}
	}

	execCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	output, stderr, exitCode, err := e.exec(execCtx, lang, p.Handler, input, e.childEnv())
	if err != nil {
		if execCtx.Err() != nil {
			return nil, &scriptExecWireError{scriptExecErrTimeout, "script execution timed out or was canceled"}
		}
		if exitCode > 0 {
			return nil, &scriptExecWireError{scriptExecErrFailed, fmt.Sprintf("exit %d: %s", exitCode, stderr)}
		}
		return nil, &scriptExecWireError{scriptExecErrFailed, err.Error()}
	}
	return &scriptExecOutcome{Output: string(output), Stderr: stderr, ExitCode: exitCode}, nil
}

// scriptExecControlMethod adapts the engine to the control socket's
// dispatch (upload_apply's discipline): the caller's conn ctx bounds
// cancellation (client gone → exec canceled); the conn deadline is
// RE-ARMED before the exec (the blanket 10s handleConn arm must not
// kill the ack of a minutes-long script) and the ack itself gets a
// fresh arm — a deadline that expired mid-exec is un-expired by a new
// SetDeadline as long as no I/O was attempted in between.
func (s *controlSocketServer) scriptExecControlMethod(ctx context.Context, conn net.Conn, req controlRequest, params map[string]any) controlResponse {
	if s.scriptExec == nil {
		return s.errResp(req.ID, "internal", "script_exec engine unwired")
	}
	var timeout time.Duration
	if ms, ok := params["timeout_ms"].(float64); ok && ms > 0 {
		timeout = time.Duration(ms) * time.Millisecond
	}
	if timeout <= 0 || timeout > scriptExecMaxTimeout {
		timeout = scriptExecDefaultTimeout
	}
	if conn != nil {
		_ = conn.SetDeadline(time.Now().Add(timeout + 5*time.Second))
	}
	outcome, werr := s.scriptExec.run(ctx, params)
	if conn != nil {
		_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	}
	if werr != nil {
		return controlResponse{V: controlProtocolVersion, ID: idOr(req.ID),
			Error: &controlError{Code: werr.Code, Message: werr.Message}}
	}
	result := map[string]any{
		"output":    outcome.Output,
		"stderr":    outcome.Stderr,
		"exit_code": outcome.ExitCode,
	}
	return controlResponse{V: controlProtocolVersion, ID: idOr(req.ID), Result: result}
}

// scriptExecForwarder is the sidecar-side seam: forward a script node's
// execution to the workspace-container supervisor. Production is the
// control-socket client; single-container mode leaves it nil (in-process
// execution — the mux already runs in the workspace container there).
type scriptExecForwarder func(ctx context.Context, params map[string]any) (*scriptExecOutcome, *scriptExecWireError)

// ScriptExec invokes the #1878 script_exec method. The caller's ctx
// deadline IS the bound (upload_apply discipline — the exec may hold
// minutes; the 2s control-plane default does not apply to this
// long-held call). Transport failures come back as the closed-enum
// "transport" code so the mux layer can name the control socket in its
// loud script_env_unavailable.
func (c *controlClient) ScriptExec(ctx context.Context, params map[string]any) (*scriptExecOutcome, *scriptExecWireError) {
	timeout := c.timeout
	if dl, ok := ctx.Deadline(); ok {
		timeout = time.Until(dl)
	}
	res, err, _ := c.callDeadline(ctx, "script_exec", params, timeout)
	if err != nil {
		if ctx.Err() != nil {
			// The CALLER's deadline fired — the script (or the round
			// trip it dominates) outlived its budget. That is the
			// timeout class, not transport: the supervisor may be
			// mid-exec entirely healthy (upload_apply derives the same
			// deadline class from a ctx-derived arm).
			return nil, &scriptExecWireError{scriptExecErrTimeout, "script execution timed out or was canceled: " + ctx.Err().Error()}
		}
		var ce *controlClientError
		if errors.As(err, &ce) {
			// The supervisor's closed enum rides the wire error 1:1
			// (method_unknown included — a supervisor without the
			// method is a mixed-generation bug state, named loudly).
			return nil, &scriptExecWireError{ce.ctl.Code, ce.ctl.Message}
		}
		return nil, &scriptExecWireError{scriptExecErrTransport, err.Error()}
	}
	out := &scriptExecOutcome{}
	if uerr := json.Unmarshal(mustMarshal(res), out); uerr != nil {
		return nil, &scriptExecWireError{scriptExecErrTransport, "script_exec decode: " + uerr.Error()}
	}
	return out, nil
}
