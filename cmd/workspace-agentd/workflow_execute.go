// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// Epic 64: Workflow node execution endpoint for agentd.
//
// POST /v1/workflow/node/execute — dispatches a single workflow node
// (script/agent/http/condition) and returns its output. Called by the
// API server's workflow engine (US-64.8).
//
// POST /v1/workflow/node/cancel — kills an in-flight node execution.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/expr-lang/expr"
	"github.com/lenaxia/llmsafespaces/pkg/agentd"
	wf "github.com/lenaxia/llmsafespaces/pkg/workflows"
	"github.com/lenaxia/llmsafespaces/pkg/workflows/scriptwrap"
)

type workflowExecuteRequest struct {
	NodeID   string          `json:"nodeId"`
	NodeType string          `json:"nodeType"`
	Spec     json.RawMessage `json:"spec"`
	Input    json.RawMessage `json:"input"`
	Timeout  string          `json:"timeout,omitempty"`
	// WorkflowID/RunID identify the logical execution this dispatch
	// belongs to (#1327): the run's workflow/run IDs on the workflow
	// path, the routine's trigger/fire IDs on the routine path. The
	// agent node derives its harness dedupe key from them. Absent (an
	// older API server), the harness POST stays keyless exactly as
	// before — deriving from partial identity would hand every
	// identity-less dispatch the same key and collapse distinct
	// executions into one transcript message.
	WorkflowID string `json:"workflowId,omitempty"`
	RunID      string `json:"runId,omitempty"`
}

// maxWorkflowExecBodyBytes bounds a /v1/workflow/node/execute request
// body (the #1561/#1564 bounded-read convention, #1565): script
// handlers and webhook-derived node input ride this wire — 16 MiB is
// the historical bound, now enforced loudly (413) instead of by
// silent io.LimitReader truncation.
const maxWorkflowExecBodyBytes = 16 << 20

type workflowExecuteResponse struct {
	Output json.RawMessage `json:"output,omitempty"`
	Branch string          `json:"branch,omitempty"`
	// SessionID is the first-class session identity (#1470): set iff a
	// session still exists when the node finished — preserved modes on
	// any outcome, ephemeral modes only when the teardown failed (the
	// leak is real and must stay discoverable). Older API servers
	// ignore it; older agentd simply never sets it.
	SessionID string `json:"sessionId,omitempty"`
	// DeletedSessionID names the ephemeral session agentd itself
	// deleted (#1882) — disjoint from SessionID by the existence
	// contract: this id does NOT exist when the node finishes. The
	// engine propagates the delete platform-side (tombstone + index
	// row + deleted broadcast); without it the carrier's index row
	// outlives the carrier as a sidebar ghost.
	DeletedSessionID string `json:"deletedSessionId,omitempty"`
}

type workflowExecuteError struct {
	ErrorCode string `json:"errorCode"`
	Detail    string `json:"detail"`
	// SessionID carries the surviving session of a FAILED agent turn
	// (#1470) — same existence contract as the success envelope.
	SessionID string `json:"sessionId,omitempty"`
	// DeletedSessionID: the ephemeral the failure path tore down
	// (#1882) — same contract as the success envelope's field.
	DeletedSessionID string `json:"deletedSessionId,omitempty"`
}

type inFlightExec struct {
	cancel context.CancelFunc
}

type nodeExecRegistry struct {
	execs map[string]*inFlightExec
}

func newNodeExecRegistry() *nodeExecRegistry {
	return &nodeExecRegistry{execs: make(map[string]*inFlightExec)}
}

func (r *nodeExecRegistry) start(nodeID string, cancel context.CancelFunc) {
	if nodeID == "" {
		return
	}
	r.execs[nodeID] = &inFlightExec{cancel: cancel}
}

func (r *nodeExecRegistry) stop(nodeID string) {
	delete(r.execs, nodeID)
}

func (r *nodeExecRegistry) cancelNode(nodeID string) {
	if e, ok := r.execs[nodeID]; ok {
		e.cancel()
	}
}

var registry = newNodeExecRegistry()

// Design 0051 US-3 (§D1): control-plane route. workspacePassword is BOTH
// an accepted credential and the CLIENT credential for agent-node
// opencode session calls (the sidecar retains it as a client secret by
// design). extraAuth carries additional ACCEPTED credentials (the
// agentdPassword in sidecar mode; D6.1 mixed-generation window).
func workflowExecuteHandler(workspacePassword string, extraAuth ...string) http.HandlerFunc {
	return workflowExecuteHandlerForward(nil, workspacePassword, extraAuth...)
}

// workflowExecuteHandlerForward is workflowExecuteHandler with the #1878
// script forwarder: in sidecar mode (fwd != nil) script nodes execute in
// the WORKSPACE container via the control socket; nil runs them
// in-process (single-container mode — the mux is already in the
// workspace container there).
func workflowExecuteHandlerForward(fwd scriptExecForwarder, workspacePassword string, extraAuth ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !checkBasicAuthAny(r, append([]string{workspacePassword}, extraAuth...)...) {
			rejectUnauthorized(w)
			return
		}
		var req workflowExecuteRequest
		// #1565 (the #1561/#1564 convention): exactly one JSON
		// document, loud diagnostics, bounded reads. The wire's one
		// legit client is this repo's HTTPAgentExecutor, which POSTs a
		// single json.Marshal document — trailing data is malformed
		// transport, and oversize rides 413 with the cap named (the
		// old io.LimitReader silently TRUNCATED over-cap bodies into a
		// confusing parse-error 400).
		r.Body = http.MaxBytesReader(w, r.Body, maxWorkflowExecBodyBytes)
		if err := decodeOneDocument(r.Body, &req); err != nil {
			var mbe *http.MaxBytesError
			if errors.As(err, &mbe) {
				writeWorkflowError(w, http.StatusRequestEntityTooLarge, "invalid_request",
					fmt.Sprintf("request body exceeds the %d-byte cap", maxWorkflowExecBodyBytes))
				return
			}
			writeWorkflowError(w, http.StatusBadRequest, "invalid_request", fmt.Sprintf("cannot decode request: %v", err))
			return
		}

		timeout := 10 * time.Minute
		if req.Timeout != "" {
			if d, err := time.ParseDuration(req.Timeout); err == nil && d > 0 {
				timeout = d
			}
		}

		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()

		registry.start(req.NodeID, cancel)
		defer registry.stop(req.NodeID)

		switch req.NodeType {
		case "script":
			execScriptNode(ctx, w, &req, fwd)
		case "http":
			execHTTPNode(ctx, w, &req)
		case "condition":
			execConditionNode(ctx, w, &req)
		case "agent":
			execAgentNode(ctx, workspacePassword, w, &req)
		default:
			writeWorkflowError(w, http.StatusBadRequest, "invalid_node_type", fmt.Sprintf("unsupported node type: %s", req.NodeType))
		}
	}
}

// Design 0051 US-3 (§D1): control-plane route — see workflowExecuteHandler.
func workflowCancelHandler(passwords ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !checkBasicAuthAny(r, passwords...) {
			rejectUnauthorized(w)
			return
		}
		nodeID := r.URL.Query().Get("nodeId")
		if nodeID == "" {
			writeWorkflowError(w, http.StatusBadRequest, "missing_node_id", "nodeId query parameter required")
			return
		}
		registry.cancelNode(nodeID)
		w.WriteHeader(http.StatusNoContent)
	}
}

// Design 0051 US-3 (§D1): control-plane route — see workflowExecuteHandler.
func workflowDeleteSessionHandler(workspacePassword string, extraAuth ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !checkBasicAuthAny(r, append([]string{workspacePassword}, extraAuth...)...) {
			rejectUnauthorized(w)
			return
		}
		sessionID := r.URL.Query().Get("sessionId")
		if sessionID == "" {
			writeWorkflowError(w, http.StatusBadRequest, "missing_session_id", "sessionId query parameter required")
			return
		}
		// #1470 existence contract, route form: 204 = the session is gone
		// (deleted or already missing); 502 = it SURVIVED the delete —
		// the engine's PreserveOnFailure leg records survivors, so a
		// blanket 204 would hide exactly the leak it needs to surface.
		if deleteOpencodeSession(r.Context(), workspacePassword, sessionID) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeWorkflowError(w, http.StatusBadGateway, "session_delete_failed",
			fmt.Sprintf("session %s still exists after delete attempt", sessionID))
	}
}

func execScriptNode(ctx context.Context, w http.ResponseWriter, req *workflowExecuteRequest, fwd scriptExecForwarder) {
	var data wf.ScriptNodeData
	if err := json.Unmarshal(req.Spec, &data); err != nil {
		writeWorkflowError(w, http.StatusBadRequest, "invalid_node_data", fmt.Sprintf("cannot parse script spec: %v", err))
		return
	}

	var input map[string]any
	if len(req.Input) > 0 {
		if err := json.Unmarshal(req.Input, &input); err != nil {
			writeWorkflowError(w, http.StatusBadRequest, "invalid_input", fmt.Sprintf("cannot parse input: %v", err))
			return
		}
	}

	lang := scriptwrap.Language(data.Language)

	// #1878: in sidecar mode the script executes in the WORKSPACE
	// container (the session environment) — this mux serves from the
	// FROM-scratch uid-2000 sidecar (no /tmp, no interpreters, no gh,
	// read-only /workspace), so the handler forwards over the
	// Appendix-A control socket to the supervisor, which runs it with
	// the opencode spawn env. Failures ride the closed enum loudly.
	if fwd != nil {
		timeoutMS := int64(scriptExecDefaultTimeout / time.Millisecond)
		if dl, ok := ctx.Deadline(); ok {
			timeoutMS = time.Until(dl).Milliseconds()
		}
		params := map[string]any{
			"language":   data.Language,
			"handler":    data.Handler,
			"timeout_ms": timeoutMS,
		}
		if len(req.Input) > 0 {
			params["input"] = json.RawMessage(req.Input)
		}
		outcome, werr := fwd(ctx, params)
		if werr != nil {
			switch werr.Code {
			case scriptExecErrTimeout:
				writeWorkflowError(w, http.StatusGatewayTimeout, "script_timeout", werr.Message)
			case scriptExecErrEnvUnavailable:
				writeWorkflowError(w, http.StatusOK, "script_env_unavailable", werr.Message)
			case scriptExecErrTransport:
				writeWorkflowError(w, http.StatusOK, "script_env_unavailable",
					fmt.Sprintf("cannot reach the workspace-container supervisor over the control socket (sidecar mode, #1878): %s — script nodes execute in the workspace container; the supervisor may be restarting", werr.Message))
			case scriptExecErrBadRequest:
				// Behavior parity with the LOCAL path: deterministic
				// validation failures (unsupported language et al.)
				// surface as script_failed there (scriptwrap's
				// "unsupported language" error text) — the forwarded
				// bad_request maps to the SAME envelope code, message
				// verbatim.
				writeWorkflowError(w, http.StatusOK, "script_failed", werr.Message)
			default:
				writeWorkflowError(w, http.StatusOK, "script_failed", werr.Message)
			}
			return
		}
		var result map[string]any
		if err := json.Unmarshal([]byte(outcome.Output), &result); err != nil {
			writeWorkflowError(w, http.StatusOK, "script_output_invalid", fmt.Sprintf("handler returned non-object JSON: %s", outcome.Output))
			return
		}
		writeWorkflowSuccess(w, result)
		return
	}

	// #1455: probe Execute's prerequisites (writable temp dir,
	// interpreter on PATH) before running. Post-#1878 this local leg is
	// the SINGLE-CONTAINER path (sidecar mode forwards above): a failure
	// here means the workspace container's own TMPDIR/PATH is broken.
	// The named code keeps the failure loud and deterministic for the
	// engine (outside the retry class by construction: an environment
	// does not heal in 2s).
	if envErr := scriptwrap.EnvCheck(lang); envErr != nil {
		writeWorkflowError(w, http.StatusOK, "script_env_unavailable",
			fmt.Sprintf("script node execution environment unavailable in this container: %v. In sidecar mode (agentdSidecar.enabled) script nodes are forwarded to the workspace container's supervisor over the control socket (#1878), so reaching this local check means single-container mode with a broken TMPDIR/PATH in the workspace container (epic-64 script contract; #1455, #1878).", envErr))
		return
	}
	// #1878: even locally (single-container mode), the script child runs
	// in the SESSION environment — the opencode spawn construction
	// (secrets-env sourced, redact PATH prepend) — not agentd's own
	// process env: "the script should be executed in the same
	// environment that the sessions run."
	output, stderr, exitCode, err := scriptwrap.ExecuteWithEnv(ctx, lang, data.Handler, input, opencodeBaseEnv())
	if err != nil {
		if ctx.Err() != nil {
			writeWorkflowError(w, http.StatusGatewayTimeout, "script_timeout", "script execution timed out or was canceled")
			return
		}
		// exitCode is the real process exit only when a process ran;
		// scriptwrap's sentinel -1 marks pre-execution failures whose
		// detail lives in err (e.g. "unsupported language: bash").
		// Reporting "exit -1: <empty stderr>" dropped exactly that
		// detail (#1414).
		if exitCode > 0 {
			writeWorkflowError(w, http.StatusOK, "script_failed", fmt.Sprintf("exit %d: %s", exitCode, stderr))
			return
		}
		writeWorkflowError(w, http.StatusOK, "script_failed", err.Error())
		return
	}

	var result map[string]any
	if err := json.Unmarshal(output, &result); err != nil {
		writeWorkflowError(w, http.StatusOK, "script_output_invalid", fmt.Sprintf("handler returned non-object JSON: %s", string(output)))
		return
	}

	writeWorkflowSuccess(w, result)
}

func execHTTPNode(ctx context.Context, w http.ResponseWriter, req *workflowExecuteRequest) {
	var data wf.HTTPNodeData
	if err := json.Unmarshal(req.Spec, &data); err != nil {
		writeWorkflowError(w, http.StatusBadRequest, "invalid_node_data", fmt.Sprintf("cannot parse http spec: %v", err))
		return
	}

	if data.Timeout != "" {
		if d, err := time.ParseDuration(data.Timeout); err == nil {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, d)
			defer cancel()
		}
	}

	method := data.Method
	if method == "" {
		method = "GET"
	}

	var bodyReader io.Reader
	if data.Body != "" {
		bodyReader = strings.NewReader(data.Body)
	}

	httpReq, err := http.NewRequestWithContext(ctx, method, data.URL, bodyReader)
	if err != nil {
		writeWorkflowError(w, http.StatusOK, "script_failed", fmt.Sprintf("cannot create request: %v", err))
		return
	}

	secrets, _ := loadSecretsEnv()
	for k, v := range data.Headers {
		httpReq.Header.Set(k, resolveSecretRef(v, secrets))
	}

	start := time.Now()
	resp, err := (&http.Client{}).Do(httpReq)
	if err != nil {
		if ctx.Err() != nil {
			writeWorkflowError(w, http.StatusGatewayTimeout, "http_timeout", "HTTP request timed out")
			return
		}
		writeWorkflowError(w, http.StatusOK, "script_failed", err.Error())
		return
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	headersMap := make(map[string]string)
	for k := range resp.Header {
		headersMap[k] = resp.Header.Get(k)
	}

	writeWorkflowSuccess(w, map[string]any{
		"status":      resp.StatusCode,
		"headers":     headersMap,
		"body":        string(respBody),
		"duration_ms": time.Since(start).Milliseconds(),
	})
}

func execConditionNode(_ context.Context, w http.ResponseWriter, req *workflowExecuteRequest) {
	var data wf.ConditionNodeData
	if err := json.Unmarshal(req.Spec, &data); err != nil {
		writeWorkflowError(w, http.StatusBadRequest, "invalid_node_data", fmt.Sprintf("cannot parse condition spec: %v", err))
		return
	}

	var input map[string]any
	if len(req.Input) > 0 {
		_ = json.Unmarshal(req.Input, &input)
	}
	if input == nil {
		input = map[string]any{}
	}

	for _, c := range data.Conditions {
		env := map[string]any{"input": input}
		program, err := expr.Compile(c.Expression, expr.Env(env), expr.AsBool())
		if err != nil {
			continue
		}
		result, err := expr.Run(program, env)
		if err != nil {
			continue
		}
		if b, ok := result.(bool); ok && b {
			writeWorkflowSuccess(w, map[string]any{}, c.ID)
			return
		}
	}
	writeWorkflowSuccess(w, map[string]any{}, "otherwise")
}

// renderTemplateRefs replaces {{.path}} references in an agent prompt
// with values from the node input, in ONE left-to-right pass —
// replacements are emitted straight to the output builder and are
// never re-scanned, so a VALUE shaped like a ref (or like a sentinel)
// cannot trigger further expansion: an externally-supplied webhook
// payload field cannot pull other fields into the prompt.
//
// At each match: an exact top-level key hit wins first (any key
// charset — KEY matching is the pre-#1417 behavior; value rendering
// improved: composites as JSON, nil as null; a flat key literally
// named "body.topic" beats path walking), then dotted paths walk
// nested maps — matching the condition nodes' expression depth
// (webhook runs put the payload under body; hyphens allowed).
// Scalars render bare; composites as compact JSON; unresolvable refs
// stay literal.
func renderTemplateRefs(prompt string, input map[string]any) string {
	var b strings.Builder
	last := 0
	for _, loc := range templateRefPattern.FindAllStringIndex(prompt, -1) {
		b.WriteString(prompt[last:loc[0]])
		ref := prompt[loc[0]:loc[1]]
		path := strings.TrimSuffix(strings.TrimPrefix(ref, "{{."), "}}")
		if v, ok := input[path]; ok {
			b.WriteString(renderTemplateValue(v))
		} else if v, ok := walkTemplatePath(input, path); ok {
			b.WriteString(renderTemplateValue(v))
		} else {
			b.WriteString(ref)
		}
		last = loc[1]
	}
	b.WriteString(prompt[last:])
	return b.String()
}

// walkTemplatePath resolves a dotted path through nested maps.
func walkTemplatePath(input map[string]any, path string) (any, bool) {
	var cur any = input
	for _, seg := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = m[seg]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

func renderTemplateValue(v any) string {
	if str, ok := v.(string); ok {
		return str
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}

// templateRefPattern matches a ref whose body contains NO braces and
// no newlines: an unclosed {{.x must never swallow a following valid
// ref — neither across a newline (no (?s)) nor on the same line (a
// brace-free body stops the lazy match at the first }} where the valid
// ref's own {{ begins). Multi-line/brace-containing keys are not
// addressable anyway. KEY matching is by map lookup — the matched
// body is not constrained to any whitelist vocabulary.
var templateRefPattern = regexp.MustCompile(`\{\{\.([^{}\n]+?)\}\}`)

func execAgentNode(ctx context.Context, password string, w http.ResponseWriter, req *workflowExecuteRequest) {
	var data wf.AgentNodeData
	if err := json.Unmarshal(req.Spec, &data); err != nil {
		writeWorkflowError(w, http.StatusBadRequest, "invalid_node_data", fmt.Sprintf("cannot parse agent spec: %v", err))
		return
	}

	prompt := data.Prompt
	if len(req.Input) > 0 {
		var input map[string]any
		_ = json.Unmarshal(req.Input, &input)
		prompt = renderTemplateRefs(prompt, input)
	}

	// A8 pre-validation (NODE-EXECUTE-CONTRACT §A8, validated probe on
	// pinned opencode): opencode does NOT error on an unknown agentID —
	// it silently falls back to the default (`build`) agent, so a
	// stage-persona dispatch referencing a role the pod never
	// materialized would run the build agent with NONE of the role's
	// system prompt while appearing successful. Validate the agent
	// name against opencode's live registry (GET /agent) BEFORE the
	// session create and reject unknown names with error_code
	// agent_not_found in the 200 envelope — the caller (the board
	// spawner) reads ErrorCode and degrades the dispatch persona-less
	// instead of spawning a silently role-less worker. Empty agent
	// (the default dispatch) skips the gate — there is nothing to
	// resolve. A failed LIST probe fails the node loudly too
	// (agent_list_failed): an unvalidatable dispatch is not a
	// validatable-in-practice one.
	if data.Agent != "" {
		names, failCode, failDetail := opencodeAgentNames(ctx, password)
		if failCode != "" {
			writeWorkflowError(w, http.StatusOK, failCode, failDetail)
			return
		}
		if _, ok := names[data.Agent]; !ok {
			writeWorkflowError(w, http.StatusOK, agentd.ErrorCodeAgentNotFound,
				fmt.Sprintf("agent %q not found (A8 pre-validation: opencode would silently fall back to the build agent)", data.Agent))
			return
		}
	}

	sessionMode := data.Session
	if sessionMode == "" {
		sessionMode = "ephemeral"
	}

	createdEphemeral := false
	sessionID := data.SessionID
	if sessionID == "" {
		var failCode, failDetail string
		sessionID, failCode, failDetail = createOpencodeSession(ctx, password)
		if sessionID == "" {
			// #1457: a FAILED CREATE is never a missing session — a
			// transient opencode 5xx here must stay distinguishable
			// from the message-leg 404 below so the engine's retry
			// classifier can see it.
			writeWorkflowError(w, http.StatusOK, failCode, failDetail)
			return
		}
		createdEphemeral = true
		// Board worker provenance: a session minted by the BOARD worker
		// dispatch (the spawner's NodeID) is recorded at spawn — no
		// manager gate anymore (the #1747 §5 grant is repealed, ruling
		// 2026-10-27 / tsk-copper-refectory-generates); the record now
		// feeds exactly one refusal, the task-worker self-certification
		// pin — every met-flag-writing manager shape refuses for a
		// spawned worker (board_update_story's inline setCriteriaMet
		// check-off and patchFile criteria replace, plus pre-met
		// criteria on board_create_story/board_import_plan; #1946's
		// grant stays reviewer-persona-only).
		if req.NodeID == "board-worker" {
			boardWorkerSessions.record(sessionID)
		}
	}

	// Board-worker spawns return EARLY (the 2026-10-29 live-fire): the
	// board spawner needs the session IDENTITY, not the turn's result —
	// and gating the response on turn completion made the identity
	// stamps (assignee_session, the session_origins worker mark) hostage
	// to the worker's ENTIRE first turn. A first turn longer than the
	// API executor's 15-minute client timeout produced exactly the
	// live-fire: the client errored, spawnSync skipped both stamps, and
	// a fully-working worker ran on as an anonymous orphan (no card
	// link, no busy indicator, no Workers-folder classification, the
	// lease ladder blind to its row). So for the board-worker NodeID
	// ONLY: dispatch the charter message DETACHED (WithoutCancel — the
	// request context dies with the response; the goroutine ends when
	// the turn does, one per spawn) and answer with the session id the
	// moment it exists. IDENTITY PRECEDES DELIVERY OUTCOME BY DESIGN
	// (review round 1's wording): a delivery failure now differs from
	// the old blocking shape — the stamps land and the lease sits
	// re-armed up to its window before the lapse heal, rather than the
	// old session-less error envelope. The detached leg is unreachable
	// by /v1/workflow/node/cancel (the registry unregisters the node
	// ~1s in); no production caller today — worker abort rides
	// /session/<id>/abort. NOTE: the NodeID literal "board-worker" is a
	// RESERVED dispatch id shared with the provenance gate below — a
	// user DAG agent node must never carry it (it would take this early
	// arm and, in ephemeral mode, leak its session past the skipped
	// teardown); the production spawner is the only issuer
	// (session: "new").
	if req.NodeID == "board-worker" {
		//nolint:gosec,contextcheck // G118: deliberate — the charter delivery outlives the spawn request BY DESIGN (the request context dies with the early envelope; the bound lives in the helper); a delivery failure is the lease-lapse heal's case
		go dispatchAgentMessageDetached(password, sessionID, data.Agent, prompt, req.WorkflowID, req.NodeID, req.RunID)
		writeWorkflowSuccessSession(w, map[string]any{
			"session_id": sessionID,
			"dispatched": true,
		}, sessionID, "")
		return
	}

	// #1470: every failure past this point knows the session, and the
	// envelope must report sessionId iff the session still EXISTS when
	// the node finishes — preserved modes keep it, ephemeral modes tear
	// down and omit it, a failed teardown reports the leaked session
	// (reality over intent). WithoutCancel + a bound so the timeout
	// leg's dead context can neither skip nor wedge the cleanup DELETE.
	// #1882: a successful teardown reports the DELETED id
	// (deletedSessionId) so the engine can propagate the delete.
	deletedSessionID := ""
	failAgentNode := func(status int, code, detail string) {
		surviving := sessionID
		deleted := ""
		if createdEphemeral && sessionMode == "ephemeral" {
			dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer cancel() //nolint:gocritic // defer-in-closure: fires when the handler returns
			if deleteOpencodeSession(dctx, password, sessionID) {
				surviving = ""
				deleted = sessionID
			}
		}
		writeWorkflowErrorEnvelope(w, status, workflowExecuteError{
			ErrorCode: code, Detail: detail,
			SessionID: surviving, DeletedSessionID: deleted,
		})
	}

	// #1327: the agent POST carries the execution-derived dedupe key.
	// The pinned harness (opencode 1.18.15, G1 probe on PR #1323)
	// validates the msg_-prefix, uses a repeated messageID verbatim as
	// the user message's store ID, and UPSERTS on collision — so a key
	// stable across the retry/re-drive of the SAME logical node
	// execution bounds the session transcript to one message per
	// execution. Empty identity (older API servers) keeps today's
	// keyless wire body byte-for-byte.
	dedupeKey := workflowAgentMessageKey(req.WorkflowID, req.NodeID, req.RunID)
	body := fmt.Sprintf(`{"agentID":%q,"parts":[{"type":"text","text":%q}]}`, data.Agent, prompt)
	if dedupeKey != "" {
		body = fmt.Sprintf(`{"messageID":%q,"agentID":%q,"parts":[{"type":"text","text":%q}]}`, dedupeKey, data.Agent, prompt)
	}
	httpReq, err := http.NewRequestWithContext(ctx, "POST",
		fmt.Sprintf("%s/session/%s/message", getAgentAddr(), sessionID),
		strings.NewReader(body))
	if err != nil {
		failAgentNode(http.StatusOK, "script_failed", err.Error())
		return
	}
	httpReq.SetBasicAuth(agentd.AuthUsername, password)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := (&http.Client{}).Do(httpReq)
	if err != nil {
		if ctx.Err() != nil {
			// #1470 transport-blindness fix (orchestrator ruling, Option
			// A): the AGENT-node timeout rides the 200+errorCode envelope
			// like every other agent failure, so the executor's normal
			// parse path surfaces this response — and its sessionId — to
			// the engine's ErrorCode branch (which records surviving
			// sessions). Still non-retryable: the classifier's switch
			// matches only script_failed/session_create_failed. The
			// script/http-node timeout legs stay 504 — their consumers
			// have no ErrorCode handling yet.
			failAgentNode(http.StatusOK, "script_timeout", "agent call timed out")
			return
		}
		failAgentNode(http.StatusOK, "script_failed", err.Error())
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		// The session is GONE — reporting an id would assert a session
		// that does not exist, so this leg stays session-less.
		writeWorkflowError(w, http.StatusOK, "session_not_found", fmt.Sprintf("session %s not found", sessionID))
		return
	}
	if resp.StatusCode != http.StatusOK {
		failAgentNode(http.StatusOK, "script_failed", fmt.Sprintf("opencode returned %d", resp.StatusCode))
		return
	}

	msgResp, err := parseAgentNodeResponse(resp.Body)
	if err != nil {
		failAgentNode(http.StatusOK, "script_output_invalid", fmt.Sprintf("cannot parse opencode response: %v", err))
		return
	}

	var texts []string
	for _, p := range msgResp.Parts {
		if p.Type == "text" {
			texts = append(texts, p.Text)
		}
	}

	result := map[string]any{
		"response": strings.Join(texts, "\n"),
		"tokens":   msgResp.Info.Tokens,
		"prompt":   prompt,
		"parts":    msgResp.Parts,
	}

	if data.EnforceStructuredOutput && len(data.OutputSchema) > 0 {
		var parsed any
		if err := json.Unmarshal([]byte(strings.Join(texts, "")), &parsed); err != nil {
			failAgentNode(http.StatusOK, "schema_mismatch", fmt.Sprintf("agent output is not valid JSON: %v", err))
			return
		}
		result["response"] = parsed
	}

	// #1882: the success leg's ephemeral teardown reports the deleted
	// id via the shared outer variable (failAgentNode paths return
	// before reaching here, so the reset is unambiguous).
	deletedSessionID = ""
	if createdEphemeral && sessionMode == "ephemeral" {
		if deleteOpencodeSession(ctx, password, sessionID) {
			deletedSessionID = sessionID
			sessionID = ""
			result["session_deleted"] = true
		}
		// A failed teardown leaves the session alive: both the payload
		// and the envelope report the leak rather than claim deletion
		// (#1470 reality contract — the session must stay discoverable).
	}
	result["session_id"] = sessionID

	writeWorkflowSuccessSession(w, result, sessionID, deletedSessionID)
}

// Key-shape constants for workflowAgentMessageKey (#1327). The prefix
// cap keeps the whole key within 64 chars (a common store-ID budget)
// while the per-component cap keeps one long component from evicting
// the others' readable share; the hash suffix carries uniqueness, so
// truncation only costs readability.
const (
	agentKeyComponentCap = 16
	agentKeyPrefixCap    = 55 // "msg_wf_" + components + separators; +1+"-"+8 hash → ≤64
)

// workflowAgentMessageKey derives the harness message dedupe key for a
// workflow agent-node execution: msg_wf_<workflow>_<node>_<run>-<hash8>
// (#1327). Retries/re-drives of the SAME logical node execution re-POST
// the same prompt; the harness upserts on a repeated msg_-prefixed
// messageID, so a key that is stable across the retry and distinct
// across executions bounds the session transcript to one message per
// logical execution. Any missing identity component returns "" — the
// caller then POSTs keyless (fail open): a partial-identity key would
// be shared by every identity-less dispatch and let distinct
// executions upsert each other's transcript messages.
//
// The hash suffix (sha256 over the RAW identity, unit-separator
// framed, first 4 bytes hex) keeps keys distinct where the readable
// prefix collides: node IDs are user-authored, and sanitization folds
// distinct raw IDs onto the same safe alphabet (a/b and a_b both
// sanitize to a_b — without the hash one node's POST could upsert
// another node's transcript message).
func workflowAgentMessageKey(workflowID, nodeID, runID string) string {
	if workflowID == "" || nodeID == "" || runID == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(workflowID + "\x1f" + nodeID + "\x1f" + runID))
	prefix := fmt.Sprintf("msg_wf_%s_%s_%s",
		sanitizeKeyComponent(workflowID),
		sanitizeKeyComponent(nodeID),
		sanitizeKeyComponent(runID))
	if len(prefix) > agentKeyPrefixCap {
		prefix = prefix[:agentKeyPrefixCap]
	}
	return prefix + "-" + hex.EncodeToString(sum[:4])
}

// sanitizeKeyComponent folds a key component onto the harness-safe
// alphabet [A-Za-z0-9._-] and caps its length.
func sanitizeKeyComponent(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()
	if len(out) > agentKeyComponentCap {
		out = out[:agentKeyComponentCap]
	}
	return out
}

func writeWorkflowSuccess(w http.ResponseWriter, output any, branch ...string) {
	resp := workflowExecuteResponse{}
	if len(branch) > 0 {
		resp.Branch = branch[0]
	}
	writeWorkflowSuccessResponse(w, output, resp)
}

// writeWorkflowSuccessResponse writes the raw success envelope — the
// #1882 success legs build it directly (session survival + the
// reported teardown id are disjoint envelope fields).
// writeWorkflowSuccessSession writes the success envelope with the
// node's surviving session identity (#1470); an empty sessionID renders
// identically to the session-less success writer (omitempty). The
// #1882 deletedSessionID rides alongside when an ephemeral teardown
// deleted the carrier.
func writeWorkflowSuccessSession(w http.ResponseWriter, output any, sessionID, deletedSessionID string) {
	writeWorkflowSuccessResponse(w, output, workflowExecuteResponse{SessionID: sessionID, DeletedSessionID: deletedSessionID})
}

func writeWorkflowSuccessResponse(w http.ResponseWriter, output any, resp workflowExecuteResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	out, _ := json.Marshal(output)
	resp.Output = out
	_ = json.NewEncoder(w).Encode(resp)
}

func writeWorkflowError(w http.ResponseWriter, status int, code, detail string) {
	writeWorkflowErrorSession(w, status, code, detail, "", "")
}

// writeWorkflowErrorSession writes the error envelope with the session
// that survived a failed agent turn (#1470); an empty sessionID renders
// writeWorkflowErrorSession writes the error envelope with the session
// that survived a failed agent turn (#1470); an empty sessionID renders
// identically to the session-less writers (omitempty). The #1882
// deletedSessionID rides alongside when the failure leg's ephemeral
// teardown deleted the carrier.
func writeWorkflowErrorSession(w http.ResponseWriter, status int, code, detail, sessionID, deletedSessionID string) {
	writeWorkflowErrorEnvelope(w, status, workflowExecuteError{ErrorCode: code, Detail: detail, SessionID: sessionID, DeletedSessionID: deletedSessionID})
}

// writeWorkflowErrorEnvelope writes the raw error envelope (#1882: the
// failure leg reports the ephemeral it tore down alongside the
// survivor — disjoint fields, the existence contract holds).
func writeWorkflowErrorEnvelope(w http.ResponseWriter, status int, e workflowExecuteError) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(e)
}

func loadSecretsEnv() (map[string]string, error) {
	// #1455: share the US-4b coordinate — in sidecar mode secrets-env
	// lives at the relocated path, and the hardcoded single-container
	// default silently starved http-node {{secrets.*}} refs.
	data, err := os.ReadFile(secretsEnvPathFromEnv())
	if err != nil {
		return map[string]string{}, err
	}
	result := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		idx := strings.Index(line, "=")
		if idx < 0 {
			continue
		}
		result[line[:idx]] = line[idx+1:]
	}
	return result, nil
}

func resolveSecretRef(s string, secrets map[string]string) string {
	if !strings.Contains(s, "{{secrets.") {
		return s
	}
	result := s
	for name, val := range secrets {
		result = strings.ReplaceAll(result, "{{secrets."+name+"}}", val)
	}
	return result
}

// dispatchAgentMessageDetached delivers the board-worker charter
// message WITHOUT holding the spawn response (the 2026-10-29
// live-fire fix's delivery leg): the goroutine owns the full blocking
// POST — opencode answers at TURN COMPLETION, and a worker's first
// turn can run for hours — under a context bound generous enough to
// outlive any real first turn (2h; past it the POST aborts and the
// lease-lapse contract heals, exactly as a delivery failure would).
// The dedupe key derivation mirrors the blocking path (#1327): a
// re-drive of the same logical spawn upserts the same transcript
// message. Failures LOG and nothing else — the spawn already
// succeeded, the worker self-directs, and a worker that never started
// is the lease-lapse heal's case, not this goroutine's.
func dispatchAgentMessageDetached(password, sessionID, agent, prompt, workflowID, nodeID, runID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
	defer cancel()
	dedupeKey := workflowAgentMessageKey(workflowID, nodeID, runID)
	body := fmt.Sprintf(`{"agentID":%q,"parts":[{"type":"text","text":%q}]}`, agent, prompt)
	if dedupeKey != "" {
		body = fmt.Sprintf(`{"messageID":%q,"agentID":%q,"parts":[{"type":"text","text":%q}]}`, dedupeKey, agent, prompt)
	}
	httpReq, err := http.NewRequestWithContext(ctx, "POST",
		fmt.Sprintf("%s/session/%s/message", getAgentAddr(), sessionID),
		strings.NewReader(body))
	if err != nil {
		slog.Warn("board: worker charter dispatch: request build failed", "session", sessionID, "err", err.Error())
		return
	}
	httpReq.SetBasicAuth(agentd.AuthUsername, password)
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{}).Do(httpReq)
	if err != nil {
		slog.Warn("board: worker charter dispatch failed (lease-lapse heals)", "session", sessionID, "err", err.Error())
		return
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		slog.Warn("board: worker charter dispatch non-200 (lease-lapse heals)",
			"session", sessionID, "status", resp.StatusCode)
	}
}

// agentRegistryCache holds the last successful GET /agent name set.
// Per the A8 contract the list is static within a pod's life (agents
// are loaded from config at opencode boot; the config is written
// before the muxes serve), so one cached fetch per password serves
// every dispatch — password rotation (pod restart generation) is the
// only invalidation key that can change the answer.
var agentRegistryCache struct {
	sync.Mutex
	password string
	names    map[string]struct{}
}

// opencodeAgentNames resolves opencode's live agent registry
// (GET /agent — the A8 validation source). Returns the name set and
// an empty code on success; on failure returns nil names plus a
// non-empty (code, detail) pair the caller writes as the node's error
// envelope (never a silent pass-through: an unvalidatable agent
// reference must not reach the silent-fallback leg).
func opencodeAgentNames(ctx context.Context, password string) (map[string]struct{}, string, string) {
	agentRegistryCache.Lock()
	cached := agentRegistryCache.names
	cacheOK := agentRegistryCache.password == password
	agentRegistryCache.Unlock()
	if cacheOK && cached != nil {
		return cached, "", ""
	}

	req, _ := http.NewRequestWithContext(ctx, "GET", getAgentAddr()+"/agent", nil)
	req.SetBasicAuth(agentd.AuthUsername, password)
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return nil, "agent_list_failed", fmt.Sprintf("opencode agent list: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, "agent_list_failed", fmt.Sprintf("opencode agent list returned %d", resp.StatusCode)
	}
	var listing []struct {
		Name string `json:"name"`
	}
	if err := decodeStrict(resp.Body, &listing); err != nil {
		return nil, "agent_list_failed", fmt.Sprintf("cannot parse opencode agent list: %v", err)
	}
	names := make(map[string]struct{}, len(listing))
	for _, a := range listing {
		if a.Name != "" {
			names[a.Name] = struct{}{}
		}
	}
	if len(names) == 0 {
		// A 200 with an empty/blank registry is not a usable answer —
		// every name would "fail" validation against a probe that
		// clearly did not describe the live opencode.
		return nil, "agent_list_failed", "opencode agent list: empty registry"
	}
	agentRegistryCache.Lock()
	agentRegistryCache.password = password
	agentRegistryCache.names = names
	agentRegistryCache.Unlock()
	return names, "", ""
}

// createOpencodeSession creates a session via the opencode HTTP API.
// Every failure mode returns a non-empty (code, detail) pair — NEVER
// a bare collapse to "": transport errors, non-200s, unparseable
// bodies, and a 200 with no usable ID all surface errorCode
// session_create_failed carrying the distinguishing detail (#1457),
// so the engine can classify a transient opencode 5xx apart from
// deterministic failures (an empty pair would read as ErrorCode==""
// there — a phantom delivered fire). Only the message leg's 404
// reports session_not_found — a session that existed at create and
// vanished before/during the turn.
func createOpencodeSession(ctx context.Context, password string) (sessionID, failCode, failDetail string) {
	req, _ := http.NewRequestWithContext(ctx, "POST",
		getAgentAddr()+"/session", strings.NewReader("{}"))
	req.SetBasicAuth(agentd.AuthUsername, password)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return "", "session_create_failed", fmt.Sprintf("opencode session create: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", "session_create_failed", fmt.Sprintf("opencode returned %d", resp.StatusCode)
	}
	id, err := parseCreatedSessionID(resp.Body)
	if err != nil {
		return "", "session_create_failed", fmt.Sprintf("cannot parse created session: %v", err)
	}
	// A 200 with no usable ID ({}, {"id":""}) is a create failure,
	// not success: an empty (code, detail) pair would read as
	// ErrorCode=="" at the engine — a phantom delivered fire.
	if id == "" {
		return "", "session_create_failed", "cannot parse created session: empty id"
	}
	return id, "", ""
}

// agentNodeMessage is the opencode V1 message wire shape the workflow
// agent node consumes (info + text parts — the same surface the seam's
// SessionSend parses).
type agentNodeMessage struct {
	Info struct {
		ID     string `json:"id"`
		Agent  string `json:"agent"`
		Tokens struct {
			Input  int `json:"input"`
			Output int `json:"output"`
			Total  int `json:"total"`
		} `json:"tokens"`
	} `json:"info"`
	Parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"parts"`
}

// parseAgentNodeResponse strictly decodes the message 200 (leg-10: a
// drifted body must fail the node, never emit phantom output).
func parseAgentNodeResponse(r io.Reader) (agentNodeMessage, error) {
	var m agentNodeMessage
	err := decodeStrict(r, &m)
	return m, err
}

// parseCreatedSessionID strictly decodes the POST /session 200 (leg-10:
// drift yields no session ID, never a phantom one to delete later).
func parseCreatedSessionID(r io.Reader) (string, error) {
	var s struct {
		ID string `json:"id"`
	}
	if err := decodeStrict(r, &s); err != nil {
		return "", err
	}
	return s.ID, nil
}

// deleteOpencodeSession deletes a finished routine's opencode session and
// reports whether the session is GONE: a 2xx confirmed the delete, a 404
// means it never existed at delete time (already gone — reporting a
// survivor would mint a phantom, #1470's existence contract). Any other
// outcome (transport error, opencode 5xx) means the session must be
// treated as still alive. Callers that must report the REALITY of a
// surviving session use the return; fire-and-forget callers may ignore it.
// #1882: a confirmed delete also invalidates the pod-side short-id
// replica — a dead session's phrase must stop resolving (the
// delete-invalidates-the-replica seam; see forget).
func deleteOpencodeSession(ctx context.Context, password, sessionID string) bool {
	req, err := http.NewRequestWithContext(ctx, "DELETE", //nolint:gosec // G704: local-only, sessionID from opencode
		fmt.Sprintf("%s/session/%s", getAgentAddr(), sessionID), nil)
	if err != nil {
		// Malformed sessionID (control chars) makes the URL unparseable;
		// req would be nil and SetBasicAuth would panic.
		return false
	}
	req.SetBasicAuth(agentd.AuthUsername, password)
	resp, err := (&http.Client{}).Do(req) //nolint:gosec // G704: local-only
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	gone := (resp.StatusCode >= 200 && resp.StatusCode < 300) || resp.StatusCode == http.StatusNotFound
	if gone {
		currentSessionShortIDs().forget(sessionID)
	}
	return gone
}
