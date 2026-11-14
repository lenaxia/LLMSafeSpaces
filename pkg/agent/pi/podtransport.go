// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package pi

// podtransport.go — the PRODUCTION Transport (S5 harness-selection
// plumbing): the API-server-side client of the in-pod agentd bridge
// (cmd/workspace-agentd/pi_bridge.go, served on the harness port by
// supervise-pi). A dumb pipe by contract: it resolves the workspace's
// pod + password (the same resolvers the opencode adapter uses),
// speaks the bridge's HTTP surface, and injects the session id into
// raw event records (the S4 transport contract) — every byte of pi
// wire knowledge stays in this package either way.
//
// Session/attach semantics map onto the bridge's spawn-or-resume rpc
// endpoint: Session() returns a client whose first Call attaches; a
// session whose file is absent surfaces the bridge's 404 as
// agent.ErrSessionNotFound (errPiSessionGone) exactly where the
// adapter classifies it.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/lenaxia/llmsafespaces/pkg/agent"
	"github.com/lenaxia/llmsafespaces/pkg/agentd"
)

// PodTransport drives one cluster's pi pods over the bridge.
type PodTransport struct {
	pw     func(ctx context.Context, workspaceID string) (string, error)
	ip     agent.WorkspacePodResolver
	client *http.Client
	logger *zap.Logger
}

// NewPodTransport constructs the transport from the platform wiring
// inputs (the AdapterFactoryDeps resolvers).
func NewPodTransport(
	pw func(ctx context.Context, workspaceID string) (string, error),
	ip agent.WorkspacePodResolver,
	logger *zap.Logger,
) *PodTransport {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &PodTransport{
		pw:     pw,
		ip:     ip,
		client: &http.Client{Timeout: 120 * time.Second},
		logger: logger,
	}
}

var _ Transport = (*PodTransport)(nil)

// target resolves the workspace's bridge base URL + password. An
// empty pod IP is the canonical no-running-pod verdict (the opencode
// adapter's contract, mirrored).
type target struct {
	baseURL  string
	password string
}

func (t *PodTransport) resolve(ctx context.Context, workspaceID string) (target, error) {
	pw, err := t.pw(ctx, workspaceID)
	if err != nil {
		return target{}, fmt.Errorf("pi transport: workspace password: %w", err)
	}
	ip, err := t.ip.GetWorkspacePodIP(ctx, "", workspaceID)
	if err != nil {
		return target{}, fmt.Errorf("pi transport: workspace pod: %w", err)
	}
	if ip == "" {
		return target{}, fmt.Errorf("workspace %s: %w", workspaceID, agent.ErrNoRunningPod)
	}
	return target{
		baseURL:  fmt.Sprintf("http://%s:%d", ip, agentd.AgentPort),
		password: pw,
	}, nil
}

// do issues one bridge request with basic auth; non-2xx returns the
// body's error field as an error.
func (tgt target) do(ctx context.Context, client *http.Client, method, path string, body io.Reader, contentType string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, tgt.baseURL+path, body)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(agentd.AuthUsername, tgt.password)
	if contentType != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

func (tgt target) call(ctx context.Context, client *http.Client, method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = strings.NewReader(string(payload))
	}
	resp, err := tgt.do(ctx, client, method, path, rd, "application/json")
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		if resp.StatusCode == http.StatusNotFound {
			return fmt.Errorf("%s: %w", e.Error, errPiSessionGone)
		}
		if e.Error == "" {
			e.Error = resp.Status
		}
		return errors.New(e.Error)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// --- Transport ------------------------------------------------------------

func (t *PodTransport) Session(ctx context.Context, userID, workspaceID, sessionID string) (SessionClient, error) {
	if err := validateSessionID(sessionID); err != nil {
		return nil, err
	}
	tgt, err := t.resolve(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	return &bridgeSessionClient{t: t, tgt: tgt, sessionID: sessionID}, nil
}

func (t *PodTransport) CreateSession(ctx context.Context, userID, workspaceID, sessionID string) (SessionClient, error) {
	if err := validateSessionID(sessionID); err != nil {
		return nil, err
	}
	tgt, err := t.resolve(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	if err := tgt.call(ctx, t.client, http.MethodPost, "/v1/pi/sessions/"+sessionID, map[string]any{}, nil); err != nil {
		return nil, err
	}
	return &bridgeSessionClient{t: t, tgt: tgt, sessionID: sessionID}, nil
}

func (t *PodTransport) Control(ctx context.Context, userID, workspaceID string) (SessionClient, error) {
	tgt, err := t.resolve(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	return &bridgeSessionClient{t: t, tgt: tgt, control: true}, nil
}

func (t *PodTransport) ListSessionFiles(ctx context.Context, userID, workspaceID string) ([]SessionFileMeta, error) {
	tgt, err := t.resolve(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	var out struct {
		Sessions []struct {
			ID         string `json:"id"`
			Name       string `json:"name"`
			ModifiedAt string `json:"modifiedAt"`
			SizeBytes  int64  `json:"sizeBytes"`
		} `json:"sessions"`
	}
	if err := tgt.call(ctx, t.client, http.MethodGet, "/v1/pi/sessions", nil, &out); err != nil {
		return nil, err
	}
	metas := make([]SessionFileMeta, 0, len(out.Sessions))
	for _, s := range out.Sessions {
		mod := time.Time{}
		if s.ModifiedAt != "" {
			if ts, err := time.Parse(time.RFC3339Nano, s.ModifiedAt); err == nil {
				mod = ts
			}
		}
		metas = append(metas, SessionFileMeta{ID: s.ID, Name: s.Name, ModifiedAt: mod, SizeBytes: s.SizeBytes})
	}
	return metas, nil
}

func (t *PodTransport) OpenSessionFile(ctx context.Context, userID, workspaceID, sessionID string) (io.ReadCloser, error) {
	tgt, err := t.resolve(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	resp, err := tgt.do(ctx, t.client, http.MethodGet, "/v1/pi/sessions/"+sessionID+"/file", nil, "")
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("pi session %s file: %w", sessionID, errPiSessionGone)
	}
	if resp.StatusCode >= 400 {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("pi session file fetch: %s", resp.Status)
	}
	return resp.Body, nil
}

func (t *PodTransport) RemoveSessionFile(ctx context.Context, userID, workspaceID, sessionID string) error {
	tgt, err := t.resolve(ctx, workspaceID)
	if err != nil {
		return err
	}
	return tgt.call(ctx, t.client, http.MethodDelete, "/v1/pi/sessions/"+sessionID, nil, nil)
}

// --- SessionClient ---------------------------------------------------------

type bridgeSessionClient struct {
	t         *PodTransport
	tgt       target
	sessionID string
	control   bool
}

func (c *bridgeSessionClient) Call(ctx context.Context, command map[string]any) (json.RawMessage, error) {
	payload, err := json.Marshal(command)
	if err != nil {
		return nil, fmt.Errorf("pi command encode: %w", err)
	}
	path := "/v1/pi/sessions/" + c.sessionID + "/rpc"
	if c.control {
		path = "/v1/pi/control/rpc"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.tgt.baseURL+path, strings.NewReader(string(payload)))
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(agentd.AuthUsername, c.tgt.password)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.t.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(body, &e)
		if e.Error == "" {
			e.Error = resp.Status
		}
		return nil, errors.New(e.Error)
	}
	return json.RawMessage(body), nil
}

// Events subscribes to the session's live records over SSE. Each
// subscription opens its own bridge stream (the bridge fans out
// server-side; N subscribers = N streams, the HTTP analogue of the
// in-process fan-out). Records carry the injected sessionId (the S4
// transport contract — ContextUsageFromEvent's non-empty-sessionID
// guarantee).
func (c *bridgeSessionClient) Events(ctx context.Context) (<-chan EventRecord, error) {
	if c.control {
		return nil, errors.New("pi control client carries no event stream")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.tgt.baseURL+"/v1/pi/sessions/"+c.sessionID+"/events", nil)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(agentd.AuthUsername, c.tgt.password)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := c.t.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("pi events subscribe: %s", resp.Status)
	}
	ch := make(chan EventRecord, 64)
	go func() {
		defer close(ch)
		defer func() { _ = resp.Body.Close() }()
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
		for scanner.Scan() {
			line := scanner.Bytes()
			if len(line) == 0 || line[0] == ':' {
				continue // SSE comment/keepalive
			}
			if !strings.HasPrefix(string(line), "data: ") {
				continue
			}
			raw := strings.TrimPrefix(string(line), "data: ")
			select {
			case ch <- EventRecord{Raw: injectSessionID([]byte(raw), c.sessionID)}:
			case <-ctx.Done():
				return
			}
		}
		if err := scanner.Err(); err != nil && ctx.Err() == nil {
			select {
			case ch <- EventRecord{Err: fmt.Errorf("pi event stream: %w", err)}:
			default:
			}
		}
	}()
	return ch, nil
}
