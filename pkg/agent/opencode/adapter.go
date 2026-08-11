// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package opencode

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/lenaxia/llmsafespaces/pkg/agent"
	"github.com/lenaxia/llmsafespaces/pkg/agent/opencode/filediff"
	"github.com/lenaxia/llmsafespaces/pkg/agentd"
	"github.com/lenaxia/llmsafespaces/pkg/session"
)

// Compile-time assertion: *Adapter satisfies agent.Adapter.
// AgentConfigWriter is NOT part of Adapter (see agent.Adapter doc:
// the two seams run in different processes — agentd owns the config
// writer, the API server owns the Adapter). US-65.4's proxy wiring
// holds an agent.Adapter; agentd's reload path holds an
// agent.AgentConfigWriter. The two never meet in one type.
var _ agent.Adapter = (*Adapter)(nil)

// Adapter implements agent.Adapter for the opencode runtime.
//
// Construction: NewAdapter(pw, ip, logger, opts...) resolves each
// call to the workspace's pod IP + password via the supplied
// resolvers, then delegates to the low-level *Client. The shared
// HTTP client pools connections across all workspaces.
//
// Translation: every method translates opencode's wire shapes to
// the platform contract via the pure functions in translate.go.
// The translator never sees an HTTP response — it gets bytes — so
// it is independently testable.
//
// Design 0049 §4.6: 16 methods. AgentConfigWriter is intentionally
// NOT implemented here — see the agent.Adapter doc comment for why
// (the two seams run in different processes with different
// filesystem capabilities).
//
// Credential methods (FormatProviderConfig, ValidateCredentials)
// delegate to the existing OpenCodeAgent (agent.AgentRuntime) to
// avoid behavior divergence. R3 from PR #714 review: duplication
// between Adapter and OpenCodeAgent was a maintenance hazard;
// delegation keeps one source of truth.
type Adapter struct {
	pw      PasswordResolver
	ip      PodIPResolver
	httpCli *http.Client
	logger  *zap.Logger
	port    int
	differ  *filediff.Producer // nil on the API side; set by agentd-side construction
	runtime *OpenCodeAgent     // delegates Type/ValidateCredentials/FormatProviderConfig
}

// AdapterOption configures an Adapter at construction.
type AdapterOption func(*Adapter)

// WithAdapterHTTPClient injects a shared *http.Client so connections
// are pooled across all workspace calls. When unset, a tuned default
// is used (mirrors WorkspaceClient.newTunedHTTPClient).
func WithAdapterHTTPClient(hc *http.Client) AdapterOption {
	return func(a *Adapter) {
		if hc != nil {
			a.httpCli = hc
		}
	}
}

// WithFileDiffProducer wires a filediff.Producer so FileChange parts
// are produced from `git diff` on the workspace PVC. Required for
// GetHistory and Stream to emit FileChange parts; without it, patch
// parts are silently dropped (their file paths are still collected
// but no Patch text is produced). Used by the agentd-side
// construction; the API-side Adapter has no filesystem access to the
// PVC and skips this.
func WithFileDiffProducer(p *filediff.Producer) AdapterOption {
	return func(a *Adapter) { a.differ = p }
}

// WithAdapterPort overrides the agent port the Adapter connects to.
// Defaults to agentd.AgentPort (4096). Used by tests that bind a
// fake server to a random port; production callers never override.
func WithAdapterPort(port int) AdapterOption {
	return func(a *Adapter) { a.port = port }
}

// NewAdapter constructs an opencode Adapter that resolves workspace →
// podIP + password on each call. The httpCli is shared across all
// calls for connection pooling; port defaults to agentd.AgentPort.
func NewAdapter(pw PasswordResolver, ip PodIPResolver, logger *zap.Logger, opts ...AdapterOption) *Adapter {
	if logger == nil {
		logger = zap.NewNop()
	}
	a := &Adapter{
		pw:      pw,
		ip:      ip,
		httpCli: newTunedHTTPClient(),
		logger:  logger,
		port:    agentd.AgentPort,
		runtime: &OpenCodeAgent{},
	}
	for _, opt := range opts {
		opt(a)
	}
	return a
}

// resolve returns a low-level Client configured for the workspace's
// pod. Delegates to the shared resolveWorkspaceClient helper so the
// resolution logic (pod IP lookup, password lookup, baseURL
// construction) has one source of truth across WorkspaceClient and
// Adapter (PR #714 review R3: avoid two maintenance paths).
func (a *Adapter) resolve(ctx context.Context, userID, workspaceID string) (*Client, error) {
	return resolveWorkspaceClient(ctx, a.ip, a.pw, a.port, a.httpCli, a.logger, userID, workspaceID)
}

// --- Sessions ---

func (a *Adapter) CreateSession(ctx context.Context, userID, workspaceID, title string) (*session.Session, error) {
	c, err := a.resolve(ctx, userID, workspaceID)
	if err != nil {
		return nil, err
	}
	body := map[string]any{}
	if title != "" {
		body["title"] = title
	}
	resp, err := a.doPost(ctx, c, "/session", body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close() //nolint:errcheck // best-effort drain
	if resp.StatusCode >= 400 {
		return nil, a.httpError("POST /session", resp)
	}
	raw, err := readBody(resp, 64*1024)
	if err != nil {
		return nil, fmt.Errorf("POST /session: read body: %w", err)
	}
	return ParseSessionWire(raw, workspaceID)
}

func (a *Adapter) GetSession(ctx context.Context, userID, workspaceID, sessionID string) (*session.Session, error) {
	c, err := a.resolve(ctx, userID, workspaceID)
	if err != nil {
		return nil, err
	}
	resp, err := a.doGet(ctx, c, "/session/"+sessionID)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close() //nolint:errcheck // best-effort drain
	if resp.StatusCode >= 400 {
		return nil, a.httpError("GET /session/"+sessionID, resp)
	}
	raw, err := readBody(resp, 64*1024)
	if err != nil {
		return nil, fmt.Errorf("GET /session/%s: read body: %w", sessionID, err)
	}
	return ParseSessionWire(raw, workspaceID)
}

func (a *Adapter) ListSessions(ctx context.Context, userID, workspaceID string) ([]session.Session, error) {
	c, err := a.resolve(ctx, userID, workspaceID)
	if err != nil {
		return nil, err
	}
	resp, err := a.doGet(ctx, c, "/session")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close() //nolint:errcheck // best-effort drain
	if resp.StatusCode >= 400 {
		return nil, a.httpError("GET /session", resp)
	}
	raw, err := readBody(resp, 1<<20)
	if err != nil {
		return nil, fmt.Errorf("GET /session: read body: %w", err)
	}
	return ParseSessionListWire(raw, workspaceID)
}

func (a *Adapter) RenameSession(ctx context.Context, userID, workspaceID, sessionID, title string) error {
	c, err := a.resolve(ctx, userID, workspaceID)
	if err != nil {
		return err
	}
	resp, err := a.doPost(ctx, c, "/session/"+sessionID, map[string]any{"title": title})
	if err != nil {
		return err
	}
	defer resp.Body.Close() //nolint:errcheck // best-effort drain
	if resp.StatusCode >= 400 {
		return a.httpError("POST /session/"+sessionID, resp)
	}
	return nil
}

func (a *Adapter) DeleteSession(ctx context.Context, userID, workspaceID, sessionID string) error {
	c, err := a.resolve(ctx, userID, workspaceID)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.baseURL+"/session/"+sessionID, nil)
	if err != nil {
		return fmt.Errorf("DELETE /session/%s: build: %w", sessionID, err)
	}
	req.SetBasicAuth(agentd.AuthUsername, c.password)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("DELETE /session/%s: %w", sessionID, err)
	}
	defer resp.Body.Close() //nolint:errcheck // best-effort drain
	if resp.StatusCode >= 400 {
		return a.httpError("DELETE /session/"+sessionID, resp)
	}
	return nil
}

// --- Messaging ---

func (a *Adapter) Send(ctx context.Context, userID, workspaceID, sessionID, text string, opts session.SendOpts) (*session.Message, error) {
	// Send is synchronous: deliver via /session/:id/message (the V1
	// endpoint opencode uses for synchronous send). The response is
	// the completed assistant message.
	c, err := a.resolve(ctx, userID, workspaceID)
	if err != nil {
		return nil, err
	}
	body := map[string]any{
		"parts": []map[string]any{
			{"type": "text", "text": text},
		},
	}
	if opts.Model != nil {
		body["model"] = opts.Model.ID
	}
	resp, err := a.doPost(ctx, c, "/session/"+sessionID+"/message", body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close() //nolint:errcheck // best-effort drain
	if resp.StatusCode >= 400 {
		return nil, a.httpError("POST /session/"+sessionID+"/message", resp)
	}
	raw, err := readBody(resp, 4<<20)
	if err != nil {
		return nil, fmt.Errorf("POST /session/%s/message: read body: %w", sessionID, err)
	}
	var om ocMessage
	if err := json.Unmarshal(raw, &om); err != nil {
		return nil, fmt.Errorf("POST /session/%s/message: parse: %w", sessionID, err)
	}
	msg, files := translateMessage(om)
	if len(files) > 0 && a.differ != nil {
		msg.Parts = append(msg.Parts, a.fileChangeParts(ctx, files)...)
	}
	return &msg, nil
}

func (a *Adapter) SendAsync(ctx context.Context, userID, workspaceID, sessionID, text string, opts session.SendOpts) (string, error) {
	// SendAsync uses opencode's V2 prompt endpoint with delivery:queue
	// (Epic 63). The response is admit-and-schedule; we return the
	// admitted message ID so the caller can correlate via Stream.
	c, err := a.resolve(ctx, userID, workspaceID)
	if err != nil {
		return "", err
	}
	delivery := V2DeliveryQueue
	if opts.Admission == session.AdmissionSteer {
		delivery = V2DeliverySteer
	}
	resp, err := c.PromptV2(ctx, sessionID, text, delivery)
	if err != nil {
		return "", err
	}
	return resp.ID, nil
}

func (a *Adapter) Abort(ctx context.Context, userID, workspaceID, sessionID string) error {
	// Abort uses the V2 non-destructive interrupt (Epic 63 US-63.4):
	// stops in-flight work but preserves queued input.
	c, err := a.resolve(ctx, userID, workspaceID)
	if err != nil {
		return err
	}
	return c.InterruptV2(ctx, sessionID)
}

func (a *Adapter) GetHistory(ctx context.Context, userID, workspaceID, sessionID string) ([]session.Message, error) {
	c, err := a.resolve(ctx, userID, workspaceID)
	if err != nil {
		return nil, err
	}
	resp, err := a.doGet(ctx, c, "/session/"+sessionID+"/message")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close() //nolint:errcheck // best-effort drain
	if resp.StatusCode >= 400 {
		return nil, a.httpError("GET /session/"+sessionID+"/message", resp)
	}
	raw, err := readBody(resp, 16<<20)
	if err != nil {
		return nil, fmt.Errorf("GET /session/%s/message: read body: %w", sessionID, err)
	}
	msgs, changedFilesPerMsg, downgraded, err := ParseHistoryWire(raw, workspaceID)
	if err != nil {
		return nil, err
	}
	if downgraded > 0 {
		a.logger.Warn("opencode history: some messages could not be decoded and were downgraded to system notices",
			zap.Int("downgraded", downgraded),
			zap.Int("total", len(msgs)),
			zap.String("workspaceID", workspaceID),
			zap.String("sessionID", sessionID),
		)
	}
	// Produce FileChange parts for any message whose patch part
	// collected file paths. Skipped when no differ is wired.
	if a.differ != nil {
		for i := range msgs {
			if i >= len(changedFilesPerMsg) {
				break
			}
			files := changedFilesPerMsg[i]
			if len(files) > 0 {
				msgs[i].Parts = append(msgs[i].Parts, a.fileChangeParts(ctx, files)...)
			}
		}
	}
	return msgs, nil
}

// fileChangeParts produces FileChange Part entries for the supplied
// paths via the wired filediff.Producer. Errors are logged and the
// parts are silently skipped — a git-diff failure must not fail the
// history call (the rest of the transcript is still valuable).
func (a *Adapter) fileChangeParts(ctx context.Context, files []string) []session.Part {
	if a.differ == nil || len(files) == 0 {
		return nil
	}
	diffs, err := a.differ.DiffFiles(ctx, files)
	if err != nil {
		a.logger.Warn("adapter: filediff failed; FileChange parts skipped",
			zap.Error(err), zap.Strings("files", files))
		return nil
	}
	parts := make([]session.Part, 0, len(diffs))
	for _, d := range diffs {
		status := session.ChangeModified
		if strings.HasPrefix(d.Patch, "diff --git a/"+d.Path) && strings.Contains(d.Patch, "--- /dev/null") {
			status = session.ChangeAdded
		} else if strings.Contains(d.Patch, "+++ /dev/null") {
			status = session.ChangeDeleted
		}
		parts = append(parts, session.Part{
			Type: session.PartFileChange,
			FileChange: &session.FileDiff{
				Path:   d.Path,
				Status: status,
				Patch:  d.Patch,
			},
		})
	}
	return parts
}

// Stream subscribes to the workspace's /event SSE endpoint, translates
// each event to session.Event, and sends on the returned channel. The
// channel closes when the context is canceled or the upstream closes
// the connection. Unknown event types are dropped (not sent on the
// channel). Scanner errors (connection breakage) are emitted as
// session.EventError events before the channel closes.
//
// Thread-safety: each Stream call opens its own HTTP connection. Safe
// for concurrent use across workspaces.
func (a *Adapter) Stream(ctx context.Context, userID, workspaceID, sessionID string) (<-chan session.Event, error) {
	c, err := a.resolve(ctx, userID, workspaceID)
	if err != nil {
		return nil, err
	}

	// Open SSE connection to /event.
	url := fmt.Sprintf("%s/event", c.baseURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build /event request: %w", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	req.SetBasicAuth(agentd.AuthUsername, c.password)

	resp, err := c.httpClient.Do(req) //nolint:bodyclose // Body is closed by the goroutine via defer; closing here would break the stream
	if err != nil {
		return nil, fmt.Errorf("connect to /event: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close() //nolint:errcheck,bodyclose // error path, best-effort close
		return nil, fmt.Errorf("/event returned %d", resp.StatusCode)
	}

	ch := make(chan session.Event, 32)
	go func() {
		defer resp.Body.Close() //nolint:errcheck
		defer close(ch)

		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			if ctx.Err() != nil {
				return
			}
			line := scanner.Bytes()
			if len(line) == 0 || line[0] != 'd' {
				continue // skip non-data lines (event:, id:, blank)
			}
			data := bytes.TrimPrefix(line, []byte("data: "))
			if len(data) == len(line) {
				continue // not a data line
			}

			evt, ok := translateSSEEvent(data)
			if !ok {
				continue
			}
			select {
			case ch <- evt:
			case <-ctx.Done():
				return
			}
		}
		if err := scanner.Err(); err != nil && ctx.Err() == nil {
			select {
			case ch <- session.Event{Type: session.EventError, Error: &session.Error{Message: err.Error()}}:
			case <-ctx.Done():
			}
		}
	}()

	return ch, nil
}

// translateSSEEvent converts one opencode SSE event payload (the JSON
// after "data: ") to a session.Event. The wire shape is:
//
//	{"id":"...","type":"<event-type>","properties":{...}}
//
// The type maps to session.EventType; properties carry the payload
// (sessionID, messageID, parts, status, etc).
func translateSSEEvent(data []byte) (session.Event, bool) {
	var raw struct {
		Type       string          `json:"type"`
		Properties json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return session.Event{}, false
	}

	translatedType := translateEventType(raw.Type)
	if translatedType == "" {
		return session.Event{}, false // unknown event type, drop
	}

	evt := session.Event{
		Type:      translatedType,
		Timestamp: time.Now().UTC(),
	}

	// Extract common fields from properties.
	var props struct {
		SessionID string          `json:"sessionID"`
		MessageID string          `json:"messageID"`
		PartID    string          `json:"partID"`
		Text      string          `json:"text"`
		Status    json.RawMessage `json:"status"`
		Error     json.RawMessage `json:"error"`
	}
	_ = json.Unmarshal(raw.Properties, &props)
	evt.SessionID = props.SessionID
	evt.MessageID = props.MessageID
	evt.PartID = props.PartID
	evt.Delta = props.Text

	// session.status: extract status.type.
	if len(props.Status) > 0 {
		var st struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(props.Status, &st) == nil {
			evt.Status = translateStatus(st.Type)
		}
	}

	// session.error: error can be a string or an object.
	if len(props.Error) > 0 {
		// Try string first (observed in stream_events_test.go).
		var errStr string
		if json.Unmarshal(props.Error, &errStr) == nil {
			evt.Error = &session.Error{Message: errStr}
		} else {
			var errObj struct {
				Message string `json:"message"`
			}
			if json.Unmarshal(props.Error, &errObj) == nil {
				evt.Error = &session.Error{Message: errObj.Message}
			}
		}
	}

	// Parse input events via Dialect.
	d := &Dialect{}
	if d.IsQuestionAsked(raw.Type) {
		req, err := d.ParseQuestionRequest(raw.Type, raw.Properties)
		if err == nil && req != nil {
			evt.Input = &session.InputRequest{
				ID:        req.ID,
				SessionID: req.SessionID,
				Kind:      session.InputQuestion,
			}
			if len(req.Questions) > 0 {
				evt.Input.Question = req.Questions[0].Question
				evt.Input.Header = req.Questions[0].Header
				evt.Input.Multiple = req.Questions[0].Multiple
				evt.Input.Custom = req.Questions[0].Custom
				for _, o := range req.Questions[0].Options {
					evt.Input.Options = append(evt.Input.Options, session.InputOption{
						Label: o.Label, Description: o.Description,
					})
				}
			}
		}
	} else if d.IsPermissionAsked(raw.Type) {
		req, err := d.ParsePermissionRequest(raw.Type, raw.Properties)
		if err == nil && req != nil {
			evt.Input = &session.InputRequest{
				ID:         req.ID,
				SessionID:  req.SessionID,
				Kind:       session.InputPermission,
				Permission: req.Permission,
				Patterns:   req.Patterns,
				Always:     req.Always,
			}
		}
	}

	return evt, true
}

// translateEventType maps opencode SSE event types to session.Event
// types. Unknown types map to the empty string (the event is dropped
// by the caller).
func translateEventType(t string) session.EventType {
	switch t {
	case "session.status":
		return session.EventSessionStatus
	case "session.updated":
		return session.EventSessionUpdated
	case "message.part.delta":
		return session.EventPartDelta
	case "session.next.prompt.admitted":
		return session.EventMessageStart // admission = message lifecycle start
	case "session.next.prompted":
		return session.EventSessionStatus // promotion triggers a status check
	case "step.started":
		return session.EventPartStart
	case "step.ended":
		return session.EventPartEnd
	case "text.started":
		return session.EventPartStart
	case "text.ended":
		return session.EventPartEnd
	case "question.asked":
		return session.EventInputRequest
	case "question.replied", "question.rejected":
		return session.EventInputResolved
	case "permission.asked":
		return session.EventInputRequest
	case "permission.replied":
		return session.EventInputResolved
	case "session.error":
		return session.EventError
	default:
		return ""
	}
}

func (a *Adapter) ListPending(ctx context.Context, userID, workspaceID, sessionID string) ([]session.InputRequest, error) {
	// ListPending queries opencode's /question and /permission
	// endpoints (V1) and unifies them into InputRequest values.
	// Implemented here because it is a synchronous poll, not a
	// stream — the same shape the proxy's pending-input UI calls.
	//
	// Error handling: transport errors and 5xx responses are logged
	// at warn (not silently swallowed) so a connectivity failure
	// does not mask as "no pending input". 4xx responses other than
	// 404 are also logged. 404 is treated as "endpoint not implemented
	// in this opencode version" and silently returns empty.
	c, err := a.resolve(ctx, userID, workspaceID)
	if err != nil {
		return nil, err
	}
	d := &Dialect{} // existing parser
	var out []session.InputRequest

	qResp, qErr := a.doGet(ctx, c, d.QuestionListPath())
	if qErr != nil {
		a.logger.Warn("adapter ListPending: GET /question transport error",
			zap.Error(qErr), zap.String("workspaceID", workspaceID), zap.String("sessionID", sessionID))
	} else {
		defer qResp.Body.Close() //nolint:errcheck // best-effort drain
		switch {
		case qResp.StatusCode >= 500:
			a.logger.Warn("adapter ListPending: GET /question returned server error",
				zap.Int("status", qResp.StatusCode))
		case qResp.StatusCode >= 400 && qResp.StatusCode != http.StatusNotFound:
			a.logger.Warn("adapter ListPending: GET /question returned client error",
				zap.Int("status", qResp.StatusCode))
		case qResp.StatusCode < 400:
			out = append(out, a.parsePendingQuestions(qResp)...)
		}
	}

	pResp, pErr := a.doGet(ctx, c, d.PermissionListPath())
	if pErr != nil {
		a.logger.Warn("adapter ListPending: GET /permission transport error",
			zap.Error(pErr), zap.String("workspaceID", workspaceID), zap.String("sessionID", sessionID))
	} else {
		defer pResp.Body.Close() //nolint:errcheck // best-effort drain
		switch {
		case pResp.StatusCode >= 500:
			a.logger.Warn("adapter ListPending: GET /permission returned server error",
				zap.Int("status", pResp.StatusCode))
		case pResp.StatusCode >= 400 && pResp.StatusCode != http.StatusNotFound:
			a.logger.Warn("adapter ListPending: GET /permission returned client error",
				zap.Int("status", pResp.StatusCode))
		case pResp.StatusCode < 400:
			out = append(out, a.parsePendingPermissions(pResp)...)
		}
	}
	return out, nil
}

// parsePendingQuestions reads /question response body and converts
// each entry to an InputRequest with Kind=InputQuestion. Uses the
// Dialect's ParseQuestionRequest to parse the full question shape
// (text, header, options, multiple, custom, tool) — NOT just id +
// sessionID. The previous stub parser (PR #717 review critical bug)
// discarded all content fields, producing blank question events in
// the SSE snapshot path.
func (a *Adapter) parsePendingQuestions(resp *http.Response) []session.InputRequest {
	raw, _ := readBody(resp, 1<<20)
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) != nil {
		return nil
	}
	d := &Dialect{}
	out := make([]session.InputRequest, 0, len(items))
	for _, item := range items {
		req, err := d.ParseQuestionRequest("question.asked", item)
		if err != nil || req == nil {
			continue
		}
		ir := session.InputRequest{
			ID:        req.ID,
			SessionID: req.SessionID,
			Kind:      session.InputQuestion,
		}
		// Flatten the first QuestionInfo into the InputRequest's
		// top-level fields. opencode's question.asked event carries
		// one QuestionsInfo per event in practice; the InputRequest
		// shape is the unified single-question representation.
		if len(req.Questions) > 0 {
			q := req.Questions[0]
			ir.Question = q.Question
			ir.Header = q.Header
			ir.Multiple = q.Multiple
			ir.Custom = q.Custom
			for _, o := range q.Options {
				ir.Options = append(ir.Options, session.InputOption{
					Label:       o.Label,
					Description: o.Description,
				})
			}
		}
		if req.Tool != nil {
			ir.Tool = &session.ToolRef{
				MessageID: req.Tool.MessageID,
				CallID:    req.Tool.CallID,
			}
		}
		out = append(out, ir)
	}
	return out
}

// parsePendingPermissions reads /permission response body and converts
// each entry to an InputRequest with Kind=InputPermission. Uses the
// Dialect's ParsePermissionRequest to parse the full permission shape
// (permission, patterns, metadata, always, tool) — NOT just id +
// sessionID + permission. The previous stub parser discarded patterns,
// always, and tool, producing incomplete permission events.
func (a *Adapter) parsePendingPermissions(resp *http.Response) []session.InputRequest {
	raw, _ := readBody(resp, 1<<20)
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) != nil {
		return nil
	}
	d := &Dialect{}
	out := make([]session.InputRequest, 0, len(items))
	for _, item := range items {
		req, err := d.ParsePermissionRequest("permission.asked", item)
		if err != nil || req == nil {
			continue
		}
		ir := session.InputRequest{
			ID:         req.ID,
			SessionID:  req.SessionID,
			Kind:       session.InputPermission,
			Permission: req.Permission,
			Patterns:   req.Patterns,
			Always:     req.Always,
		}
		if req.Tool != nil {
			ir.Tool = &session.ToolRef{
				MessageID: req.Tool.MessageID,
				CallID:    req.Tool.CallID,
			}
		}
		out = append(out, ir)
	}
	return out
}

func (a *Adapter) Resolve(ctx context.Context, userID, workspaceID, requestID, reply string) error {
	c, err := a.resolve(ctx, userID, workspaceID)
	if err != nil {
		return err
	}
	d := &Dialect{}
	// Try question reply first; if 404, try permission reply. The
	// adapter does not know which kind the requestID refers to without
	// a ListPending round-trip; the caller learns the kind via
	// ListPending and could call a kind-specific method. For the
	// simple case we accept the cost of one extra call.
	qResp, qErr := a.doPost(ctx, c, d.QuestionReplyPath(requestID), map[string]any{"reply": reply})
	if qErr == nil {
		defer qResp.Body.Close() //nolint:errcheck // best-effort drain
		if qResp.StatusCode < 400 {
			return nil
		}
		if qResp.StatusCode != http.StatusNotFound {
			return a.httpError("POST "+d.QuestionReplyPath(requestID), qResp)
		}
	}
	pResp, pErr := a.doPost(ctx, c, d.PermissionReplyPath(requestID), map[string]any{"reply": reply})
	if pErr != nil {
		return pErr
	}
	defer pResp.Body.Close() //nolint:errcheck // best-effort drain
	if pResp.StatusCode >= 400 {
		return a.httpError("POST "+d.PermissionReplyPath(requestID), pResp)
	}
	return nil
}

// --- Models ---

func (a *Adapter) ListAvailableModels(ctx context.Context, userID, workspaceID string) ([]session.ModelInfo, error) {
	c, err := a.resolve(ctx, userID, workspaceID)
	if err != nil {
		return nil, err
	}
	raw, err := c.ListModels(ctx)
	if err != nil {
		return nil, err
	}
	return parseProviderCatalogForContract(raw)
}

func (a *Adapter) SetModel(ctx context.Context, userID, workspaceID, sessionID string, model session.ModelRef) error {
	c, err := a.resolve(ctx, userID, workspaceID)
	if err != nil {
		return err
	}
	fullID := model.ID
	if model.Provider != "" {
		fullID = model.Provider + "/" + model.ID
	}
	return c.PatchConfig(ctx, map[string]any{"model": fullID})
}

// --- Capabilities ---

func (a *Adapter) Capabilities() []session.Capability {
	// opencode as bundled in 1.18.10 supports: queue (V2), reasoning
	// emission, and diff (via filediff when wired). Steer is supported
	// by the V2 API but not yet exposed by the platform; rewind/fork/
	// stash are not implemented upstream.
	caps := []session.Capability{
		session.CapQueue,
		session.CapReasoning,
	}
	if a.differ != nil {
		caps = append(caps, session.CapDiff)
	}
	return caps
}

// --- Credentials (folded from AgentRuntime) ---
//
// These delegate to the existing OpenCodeAgent rather than
// re-implementing. R3 from PR #714 review: the previous duplication
// created two maintenance paths and a behavior divergence
// (ValidateCredentials checked the `provider` key specifically vs.
// OpenCodeAgent's any-key check). Delegation keeps one source of
// truth; the agent.AgentRuntime implementation stays authoritative
// for credential validation and formatting.

func (a *Adapter) FormatProviderConfig(providers []agent.LLMProviderData) ([]byte, error) {
	return a.runtime.FormatProviderConfig(providers)
}

func (a *Adapter) ValidateCredentials(rawConfig []byte) (*agent.CredentialCheckResult, error) {
	return a.runtime.ValidateCredentials(rawConfig)
}
