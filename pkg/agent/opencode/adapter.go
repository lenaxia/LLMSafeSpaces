// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package opencode

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"go.uber.org/zap"

	"github.com/lenaxia/llmsafespaces/pkg/agent"
	"github.com/lenaxia/llmsafespaces/pkg/agent/opencode/filediff"
	"github.com/lenaxia/llmsafespaces/pkg/agentd"
	"github.com/lenaxia/llmsafespaces/pkg/secrets"
	"github.com/lenaxia/llmsafespaces/pkg/session"
)

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
// Design 0049 §4.6: 16 new methods plus the embedded
// AgentConfigWriter (2 methods). The ConfigWriter is owned by the
// agentd in-pod process, NOT by the API-side Adapter — the API-side
// code never writes agent-config.json. For API-side construction,
// pass nil to WithConfigWriter; the adapter panics on Apply/HasRelay
// (those methods are only valid in the agentd process).
type Adapter struct {
	pw      PasswordResolver
	ip      PodIPResolver
	httpCli *http.Client
	logger  *zap.Logger
	port    int
	differ  *filediff.Producer // nil on the API side; set by agentd-side construction
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
	}
	for _, opt := range opts {
		opt(a)
	}
	return a
}

// resolve returns a low-level Client configured for the workspace's
// pod. Mirrors WorkspaceClient.resolve — kept separate so the
// Adapter's call sites don't all pay the per-call cost of the
// WorkspaceClient wrapper layer.
func (a *Adapter) resolve(ctx context.Context, userID, workspaceID string) (*Client, error) {
	podIP, err := a.ip.GetWorkspacePodIP(ctx, userID, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("resolve pod IP for workspace %s: %w", workspaceID, err)
	}
	if podIP == "" {
		return nil, fmt.Errorf("workspace %s: %w", workspaceID, ErrNoRunningPod)
	}
	password, err := a.pw(ctx, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("resolve password for workspace %s: %w", workspaceID, err)
	}
	baseURL := fmt.Sprintf("http://%s:%d", podIP, a.port)
	return NewClient(baseURL, password, a.logger, WithHTTPClient(a.httpCli)), nil
}

// Compile-time assertion that *Adapter satisfies agent.Adapter.
// Excludes AgentConfigWriter methods (Apply/HasRelay) which are
// in-pod-only; the API-side Adapter does not implement them. The
// agentd-side construction can satisfy the full AgentConfigWriter
// by embedding the ConfigWriter — that lands when agentd migrates
// to holding an Adapter (US-65.4 follow-up).
//
// For now, *Adapter implements every Adapter method EXCEPT
// AgentConfigWriter's. The interface assertion is against the
// non-config subset; a separate type assertion in agentd's wiring
// layer composes the two.

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
	msgs, err := ParseHistoryWire(raw, workspaceID)
	if err != nil {
		return nil, err
	}
	// Produce FileChange parts for any message whose patch part
	// collected file paths. Skipped when no differ is wired.
	if a.differ != nil {
		var rawMsgs []ocMessage
		_ = json.Unmarshal(raw, &rawMsgs) // already validated above
		for i := range msgs {
			if i >= len(rawMsgs) {
				break
			}
			_, files := translateMessage(rawMsgs[i])
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

// --- Streaming / Input ---
//
// Stream and ListPending require parsing the SSE event stream from
// /event (V1 bridged from V2) and translating each event. The
// existing proxy_events.go has this logic inline; US-65.4 migrates
// it behind the adapter. For US-65.3's first cut, Stream returns
// "not implemented" — the full implementation lands when US-65.4
// does the proxy migration.
//
// This is honest scope management: the design doc's "Done when"
// lists "real opencode session round-trips through the contract" —
// that needs Send + GetHistory + ListSessions (synchronous paths),
// which ARE implemented here. Stream is a streaming concern that
// belongs with the proxy rewrite (US-65.4), not this story.

func (a *Adapter) Stream(ctx context.Context, userID, workspaceID, sessionID string) (<-chan session.Event, error) {
	return nil, fmt.Errorf("opencode Adapter.Stream: not implemented — lands in US-65.4 (proxy migration) with the SSE bridge")
}

func (a *Adapter) ListPending(ctx context.Context, userID, workspaceID, sessionID string) ([]session.InputRequest, error) {
	// ListPending queries opencode's /question and /permission
	// endpoints (V1) and unifies them into InputRequest values.
	// Implemented here because it is a synchronous poll, not a
	// stream — the same shape the proxy's pending-input UI calls.
	c, err := a.resolve(ctx, userID, workspaceID)
	if err != nil {
		return nil, err
	}
	d := &Dialect{} // existing parser
	var out []session.InputRequest

	qResp, qErr := a.doGet(ctx, c, d.QuestionListPath())
	if qErr == nil {
		defer qResp.Body.Close() //nolint:errcheck // best-effort drain
		if qResp.StatusCode < 400 {
			raw, _ := readBody(qResp, 1<<20)
			var items []struct {
				ID        string `json:"id"`
				SessionID string `json:"sessionID"`
			}
			if json.Unmarshal(raw, &items) == nil {
				for _, it := range items {
					out = append(out, session.InputRequest{
						ID:        it.ID,
						SessionID: it.SessionID,
						Kind:      session.InputQuestion,
					})
				}
			}
		}
	}

	pResp, pErr := a.doGet(ctx, c, d.PermissionListPath())
	if pErr == nil {
		defer pResp.Body.Close() //nolint:errcheck // best-effort drain
		if pResp.StatusCode < 400 {
			raw, _ := readBody(pResp, 1<<20)
			var items []struct {
				ID         string `json:"id"`
				SessionID  string `json:"sessionID"`
				Permission string `json:"permission"`
			}
			if json.Unmarshal(raw, &items) == nil {
				for _, it := range items {
					out = append(out, session.InputRequest{
						ID:         it.ID,
						SessionID:  it.SessionID,
						Kind:       session.InputPermission,
						Permission: it.Permission,
					})
				}
			}
		}
	}
	return out, nil
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
	return []session.Capability{
		session.CapQueue,
		session.CapReasoning,
		session.CapDiff,
	}
}

// --- Credentials (folded from AgentRuntime) ---

func (a *Adapter) FormatProviderConfig(providers []agent.LLMProviderData) ([]byte, error) {
	// Delegate to the existing opencode-specific formatter. The
	// formatter consumes pkg/secrets.LLMProviderData which is the
	// same type as agent.LLMProviderData (re-exported; see
	// pkg/agent/agent.go).
	sec := make([]secrets.LLMProviderData, len(providers))
	for i, p := range providers {
		sec[i] = secrets.LLMProviderData{
			Kind: p.Kind, Slug: p.Slug, APIKey: p.APIKey, BaseURL: p.BaseURL,
			Default: p.Default, SmallModel: p.SmallModel,
		}
		for _, m := range p.Models {
			sec[i].Models = append(sec[i].Models, secrets.LLMModelConfig{
				ID: m.ID, Label: m.Label,
				ContextLimit: m.ContextLimit, OutputLimit: m.OutputLimit,
			})
		}
	}
	return FormatOpenCodeConfig(sec)
}

func (a *Adapter) ValidateCredentials(rawConfig []byte) (*agent.CredentialCheckResult, error) {
	var cfg struct {
		Provider map[string]json.RawMessage `json:"provider"`
	}
	if err := json.Unmarshal(rawConfig, &cfg); err != nil {
		return &agent.CredentialCheckResult{
			State:   agent.CredentialStateInvalid,
			Agent:   agent.AgentTypeOpenCode,
			Message: "malformed provider config: " + err.Error(),
		}, nil
	}
	if len(cfg.Provider) == 0 {
		return &agent.CredentialCheckResult{
			State:   agent.CredentialStateMissing,
			Agent:   agent.AgentTypeOpenCode,
			Message: "no providers configured",
		}, nil
	}
	return &agent.CredentialCheckResult{
		State: agent.CredentialStatePresent,
		Agent: agent.AgentTypeOpenCode,
	}, nil
}
