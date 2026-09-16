// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package opencode

// loopback.go — the in-pod tool seam: session/message surface methods on
// the contained Client. This is the ONLY place the agentd MCP tools'
// opencode wire knowledge lives (Rule 12: one seam, no scatter). The
// wire contract below is empirically validated against opencode 1.18.15
// on a live pod (2026-09-13) and pinned by loopback_test.go +
// loopback_integration_test.go; see docs/testing/agentd-mcp-tools-test-plan.md §2.
//
// Validated shapes:
//   - POST /session                     {title?}              → session JSON (id, ...)
//   - POST /session/{id}                {"title": t}          → 2xx
//   - DELETE /session/{id}                                    → 2xx
//   - POST /session/{id}/message        {messageID?, parts, model?} → {info, parts}
//     · parts: TextPartInput {type:"text", text} and FilePartInput
//       {type:"file", mime, filename, url:"data:<mime>;base64,..."} —
//       image bytes DO reach the model as attachments (live-proven).
//     · model: the OBJECT {modelID, providerID} — the string form 400s
//       (#909). Overrides the session default for THIS prompt only.
//     · Synchronous: returns the completed assistant message. A BUSY
//       session BLOCKS the call until its turn ends (live-proven, no
//       409/queue) — callers must never target a session whose turn is
//       waiting on them (deadlock by construction).
//   - POST /session/{id}/summarize      {providerID, modelID} → 200 "true"
//     · The working compact on 1.18.x (the V2 /api/.../compact is 503
//       "not available yet"). On a BUSY session it queues server-side
//       and completes at the turn boundary (live-proven: 200 after the
//       generation finished).
//   - GET  /session/status              → {sesID: {type:"busy"|"idle"}}
//   - GET  /session/{id}/message?limit= → page + X-Next-Cursor header
//   - GET  /api/session/{id}/context    → {data: [{id,time,text,type}]}
//   - GET  /config/providers            → per-model limit.context +
//     capabilities.input.image (catalog)

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/lenaxia/llmsafespaces/pkg/agentd"
	"github.com/lenaxia/llmsafespaces/pkg/session"
)

// SessionSummary is the typed V1 session-list entry (validated live on
// 1.18.15). Only the fields the MCP tools consume are modeled; unknown
// fields pass through ignored.
type SessionSummary struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Agent   string `json:"agent"`
	Version string `json:"version"`
	Model   *struct {
		ID         string `json:"id"`
		ProviderID string `json:"providerID"`
	} `json:"model"`
	Time struct {
		Created int64 `json:"created"`
		Updated int64 `json:"updated"`
	} `json:"time"`
	Tokens *struct {
		Input     int64 `json:"input"`
		Output    int64 `json:"output"`
		Reasoning int64 `json:"reasoning"`
		Cache     struct {
			Read  int64 `json:"read"`
			Write int64 `json:"write"`
		} `json:"cache"`
	} `json:"tokens"`
	Summary *struct {
		Additions int64 `json:"additions"`
		Deletions int64 `json:"deletions"`
		Files     int64 `json:"files"`
	} `json:"summary"`
}

// ImageAttachment is one image bound for a message's file part. Data is
// the raw bytes; the seam renders the data URL the wire requires.
type ImageAttachment struct {
	Filename string
	MIME     string
	Data     []byte
}

// SendResult is the parsed synchronous V1 message response.
type SendResult struct {
	MessageID string
	ModelID   string
	Text      string
}

// ModelInfo is the catalog view of one model (GET /config/providers).
// ImageInput is only meaningful when ImageInputKnown is true — the
// catalog entry carried no capabilities block when it is false, and
// callers must treat that as UNKNOWN, never as text-only (#1307
// fail-safe direction: a false "text-only" strips user images from
// vision-capable models).
type ModelInfo struct {
	ContextLimit    int64
	ImageInput      bool
	ImageInputKnown bool
}

// sessionIDPattern pins what may be interpolated into a seam URL path:
// the agent's opaque IDs. Anything else (control chars, slashes, "..")
// is rejected before any request is built — a sessionID containing a
// control character otherwise makes the URL unparseable or path-injects
// (review finding, PR #1364 — the pre-existing pattern this seam
// widened).
var sessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// validateSessionID guards every seam method that interpolates an ID
// into a path.
func validateSessionID(sessionID string) error {
	if !sessionIDPattern.MatchString(sessionID) {
		return fmt.Errorf("invalid session id %q", sessionID)
	}
	return nil
}

// SessionCreate creates a session; title may be empty (omitted — the
// agent auto-titles).
func (c *Client) SessionCreate(ctx context.Context, title string) (string, error) {
	body := map[string]any{}
	if title != "" {
		body["title"] = title
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/session", bytes.NewReader(raw))
	if err != nil {
		return "", err
	}
	resp, err := c.do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close() //nolint:errcheck // best-effort drain
	if resp.StatusCode >= 400 {
		return "", c.statusError("POST /session", resp)
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := decodeStrict(io.LimitReader(resp.Body, 1<<20), &out); err != nil {
		return "", fmt.Errorf("POST /session: decode: %w", err)
	}
	if out.ID == "" {
		return "", fmt.Errorf("POST /session: response carried no id")
	}
	return out.ID, nil
}

// SessionRename sets a session's title. The verb is PATCH — POST is
// silently accepted and IGNORED by pinned opencode (live-proven
// 2026-09-13: POST /session/{id} returned 200 with the title unchanged;
// PATCH updates it immediately; the SDK's sessionUpdate is PATCH).
func (c *Client) SessionRename(ctx context.Context, sessionID, title string) error {
	if err := validateSessionID(sessionID); err != nil {
		return err
	}

	raw, err := json.Marshal(map[string]string{"title": title})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch,
		fmt.Sprintf("%s/session/%s", c.baseURL, sessionID), bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("PATCH /session/%s: build: %w", sessionID, err)
	}
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close() //nolint:errcheck // best-effort drain
	if resp.StatusCode >= 400 {
		return c.statusError("PATCH /session/"+sessionID, resp)
	}
	return nil
}

// SessionDelete removes a session and its message history.
func (c *Client) SessionDelete(ctx context.Context, sessionID string) error {
	if err := validateSessionID(sessionID); err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodDelete,
		fmt.Sprintf("%s/session/%s", c.baseURL, sessionID), nil)
	if err != nil {
		return fmt.Errorf("DELETE /session/%s: build: %w", sessionID, err)
	}
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close() //nolint:errcheck // best-effort drain
	if resp.StatusCode >= 400 {
		return c.statusError("DELETE /session/"+sessionID, resp)
	}
	return nil
}

// SplitModelRef splits a "provider/model" reference into the V1 override
// pair. First-segment routing (opencode's own rule — the adapter's
// modelOverride): "a/b/c" routes via "a" with model "b/c". A bare flat
// ID is unexpressible (opencode parses it as provider-with-empty-model)
// and is rejected before any wire call.
func SplitModelRef(model string) (providerID, modelID string, err error) {
	model = strings.TrimSpace(model)
	prov, id, ok := strings.Cut(model, "/")
	degenerate := !ok || prov == "" || id == "" ||
		strings.HasPrefix(id, "/") || strings.HasSuffix(id, "/") || strings.Contains(id, "//")
	if degenerate {
		return "", "", fmt.Errorf("model must be qualified as provider/model (e.g. \"anthropic/claude-sonnet-4-5\"), got %q", model)
	}
	return prov, id, nil
}

// SessionSend delivers one synchronous prompt (V1 POST /session/{id}/message)
// with an optional per-prompt model override ("provider/model", "" = session
// default) and optional image attachments rendered as file-part data URLs.
// Returns the completed assistant exchange.
//
// The caller MUST NOT target a session whose turn is waiting on this call
// (busy sessions block — see the file contract).
func (c *Client) SessionSend(ctx context.Context, sessionID, text, model string, images []ImageAttachment) (*SendResult, error) {
	if err := validateSessionID(sessionID); err != nil {
		return nil, err
	}

	parts := make([]map[string]any, 0, len(images)+1)
	parts = append(parts, map[string]any{"type": "text", "text": text})
	for _, img := range images {
		parts = append(parts, map[string]any{
			"type":     "file",
			"mime":     img.MIME,
			"filename": img.Filename,
			"url":      "data:" + img.MIME + ";base64," + base64.StdEncoding.EncodeToString(img.Data),
		})
	}
	body := map[string]any{"parts": parts}
	if model != "" {
		providerID, modelID, err := SplitModelRef(model)
		if err != nil {
			return nil, err
		}
		// Object form only — the string form 400s on pinned opencode (#909).
		body["model"] = map[string]string{"modelID": modelID, "providerID": providerID}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("%s/session/%s/message", c.baseURL, sessionID), bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("POST /session/%s/message: build: %w", sessionID, err)
	}
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close() //nolint:errcheck // best-effort drain
	if resp.StatusCode >= 400 {
		return nil, c.statusError("POST /session/"+sessionID+"/message", resp)
	}
	var out struct {
		Info struct {
			ID      string `json:"id"`
			ModelID string `json:"modelID"`
		} `json:"info"`
		Parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"parts"`
	}
	if err := decodeStrict(io.LimitReader(resp.Body, 64<<20), &out); err != nil {
		return nil, fmt.Errorf("POST /session/%s/message: decode: %w", sessionID, err)
	}
	var texts []string
	for _, p := range out.Parts {
		if p.Type == "text" && p.Text != "" {
			texts = append(texts, p.Text)
		}
	}
	res := &SendResult{
		MessageID: out.Info.ID,
		ModelID:   out.Info.ModelID,
		Text:      strings.Join(texts, "\n"),
	}
	return res, nil
}

// SessionSummarize compacts a session's history via the working V1 route
// (POST /session/{id}/summarize). providerID/modelID select the
// summarizing model. On a busy session the request queues server-side
// and completes at the turn boundary (live-proven) — callers that must
// not block run this from a detached context.
func (c *Client) SessionSummarize(ctx context.Context, sessionID, providerID, modelID string) error {
	if err := validateSessionID(sessionID); err != nil {
		return err
	}

	raw, err := json.Marshal(map[string]string{"providerID": providerID, "modelID": modelID})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("%s/session/%s/summarize", c.baseURL, sessionID), bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("POST /session/%s/summarize: build: %w", sessionID, err)
	}
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close() //nolint:errcheck // best-effort drain
	if resp.StatusCode >= 400 {
		return c.statusError("POST /session/"+sessionID+"/summarize", resp)
	}
	return nil
}

// SessionList returns the typed session inventory (GET /session).
func (c *Client) SessionList(ctx context.Context) ([]SessionSummary, error) {
	raw, err := c.SessionListRaw(ctx)
	if err != nil {
		return nil, err
	}
	var out []SessionSummary
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("GET /session: decode: %w", err)
	}
	return out, nil
}

// SessionListRaw returns the unparsed GET /session body (the
// session_list tool passes it through verbatim — the raw shape is
// richer than the typed subset and is part of that tool's output
// contract).
func (c *Client) SessionListRaw(ctx context.Context) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/session", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close() //nolint:errcheck // best-effort drain
	if resp.StatusCode >= 400 {
		return nil, c.statusError("GET /session", resp)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 4<<20))
}

// SessionMessagesRaw returns one page of the V1 message list plus the
// X-Next-Cursor continuation token ("" when exhausted).
func (c *Client) SessionMessagesRaw(ctx context.Context, sessionID string, limit int, cursor string) ([]byte, string, error) {
	if err := validateSessionID(sessionID); err != nil {
		return nil, "", err
	}

	url := fmt.Sprintf("%s/session/%s/message?limit=%d", c.baseURL, sessionID, limit)
	if cursor != "" {
		url += "&before=" + cursor
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := c.do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close() //nolint:errcheck // best-effort drain
	if resp.StatusCode >= 400 {
		return nil, "", c.statusError("GET /session/"+sessionID+"/message", resp)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, "", fmt.Errorf("GET /session/%s/message: read: %w", sessionID, err)
	}
	return body, resp.Header.Get("X-Next-Cursor"), nil
}

// SessionMessageCount walks the V1 message pagination (bounded) and
// returns the total message count. The bound keeps a pathological
// session from making the metadata call unbounded.
func (c *Client) SessionMessageCount(ctx context.Context, sessionID string) (int, error) {
	if err := validateSessionID(sessionID); err != nil {
		return 0, err
	}

	const pageSize = 500
	const maxPages = 40 // 20k messages ceiling
	total := 0
	cursor := ""
	for page := 0; page < maxPages; page++ {
		body, next, err := c.SessionMessagesRaw(ctx, sessionID, pageSize, cursor)
		if err != nil {
			return total, err
		}
		var msgs []json.RawMessage
		if err := json.Unmarshal(body, &msgs); err != nil {
			return total, fmt.Errorf("message page decode: %w", err)
		}
		total += len(msgs)
		if next == "" || len(msgs) < pageSize {
			return total, nil
		}
		cursor = next
	}
	return total, nil
}

// SessionContextCount returns how many messages are currently inside the
// session's context window (GET /api/session/{id}/context).
func (c *Client) SessionContextCount(ctx context.Context, sessionID string) (int, error) {
	if err := validateSessionID(sessionID); err != nil {
		return 0, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/api/session/%s/context", c.baseURL, sessionID), nil)
	if err != nil {
		return 0, err
	}
	resp, err := c.do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close() //nolint:errcheck // best-effort drain
	if resp.StatusCode >= 400 {
		return 0, c.statusError("GET /api/session/"+sessionID+"/context", resp)
	}
	var out struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := decodeStrict(io.LimitReader(resp.Body, 16<<20), &out); err != nil {
		return 0, fmt.Errorf("GET /api/session/%s/context: decode: %w", sessionID, err)
	}
	return len(out.Data), nil
}

// SessionPromptTokens returns the approximate live context usage in
// tokens: the most recent COMPLETED assistant step's input + cache.read
// + cache.write (the Epic 36 formula — what the next prompt will
// actually cost the window). 0 when no assistant turn exists yet.
//
// Mid-turn, the in-flight assistant message carries a ZEROED token
// stamp (real usage lands at step completion — live-proven 2026-09-14:
// busy sessions reported no context usage because the scan stopped at
// the placeholder). Zero-total stamps are skipped; a real completion
// always carries input > 0, so busy sessions report the last completed
// step — the honest "as of last step" number.
func (c *Client) SessionPromptTokens(ctx context.Context, sessionID string) int64 {
	if validateSessionID(sessionID) != nil {
		return 0
	}

	body, _, err := c.SessionMessagesRaw(ctx, sessionID, 20, "")
	if err != nil {
		return 0
	}
	var msgs []struct {
		Info struct {
			Role   string `json:"role"`
			Tokens *struct {
				Input int64 `json:"input"`
				Cache struct {
					Read  int64 `json:"read"`
					Write int64 `json:"write"`
				} `json:"cache"`
			} `json:"tokens"`
		} `json:"info"`
	}
	if err := json.Unmarshal(body, &msgs); err != nil {
		return 0
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Info.Role != "assistant" || msgs[i].Info.Tokens == nil {
			continue
		}
		if total := msgs[i].Info.Tokens.Input + msgs[i].Info.Tokens.Cache.Read + msgs[i].Info.Tokens.Cache.Write; total > 0 {
			return total
		}
	}
	return 0
}

// SessionModelRef resolves a session's current model (GET /session/{id}
// → model{id, providerID}; shape pinned by testdata/session_get_1_18_10.json).
// Returns nil when the session carries no model.
func (c *Client) SessionModelRef(ctx context.Context, sessionID string) (*session.ModelRef, error) {
	if err := validateSessionID(sessionID); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/session/"+sessionID, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close() //nolint:errcheck // best-effort drain
	if resp.StatusCode >= 400 {
		return nil, c.statusError("GET /session/"+sessionID, resp)
	}
	var s struct {
		Model *struct {
			ID         string `json:"id"`
			ProviderID string `json:"providerID"`
		} `json:"model"`
	}
	if err := decodeStrict(io.LimitReader(resp.Body, 1<<20), &s); err != nil {
		return nil, fmt.Errorf("GET /session/%s: decode: %w", sessionID, err)
	}
	if s.Model == nil || s.Model.ID == "" {
		return nil, nil
	}
	return &session.ModelRef{ID: s.Model.ID, Provider: s.Model.ProviderID}, nil
}

// ModelInfo resolves one model's catalog entry (GET /config/providers):
// its context-window limit and whether it accepts image input.
func (c *Client) ModelInfo(ctx context.Context, providerID, modelID string) (*ModelInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/config/providers", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close() //nolint:errcheck // best-effort drain
	if resp.StatusCode >= 400 {
		return nil, c.statusError("GET /config/providers", resp)
	}
	var result struct {
		Providers []struct {
			ID     string `json:"id"`
			Models map[string]struct {
				ID    string `json:"id"`
				Limit struct {
					Context int64 `json:"context"`
				} `json:"limit"`
				Capabilities *struct {
					Input *struct {
						Image *bool `json:"image"`
					} `json:"input"`
				} `json:"capabilities"`
			} `json:"models"`
		} `json:"providers"`
	}
	if err := decodeStrict(io.LimitReader(resp.Body, 32<<20), &result); err != nil {
		return nil, fmt.Errorf("GET /config/providers: decode: %w", err)
	}
	for _, p := range result.Providers {
		if providerID != "" && p.ID != providerID {
			continue
		}
		if m, ok := p.Models[modelID]; ok {
			info := &ModelInfo{ContextLimit: m.Limit.Context}
			// Partial capabilities blocks stay UNKNOWN: pointerized all
			// the way down so "capabilities":{}, "input":{}, and
			// "input":{"text":true} never flatten into known-false (#1307
			// review r1 finding 1 — the value struct made the repair
			// strip images from possibly-vision models).
			if m.Capabilities != nil && m.Capabilities.Input != nil && m.Capabilities.Input.Image != nil {
				info.ImageInput = *m.Capabilities.Input.Image
				info.ImageInputKnown = true
			}
			return info, nil
		}
	}
	return nil, fmt.Errorf("model %q not found in the workspace catalog (provider %q)", modelID, providerID)
}

// do stamps the shared Basic credential and content type on every seam
// request — the §D1 gate every opencode endpoint enforces.
func (c *Client) do(req *http.Request) (*http.Response, error) {
	req.SetBasicAuth(agentd.AuthUsername, c.password)
	req.Header.Set("Content-Type", "application/json")
	return c.httpClient.Do(req)
}

// decodeStrict decodes exactly ONE JSON value from r and requires the
// stream to end there. A plain Decode silently accepts trailing bytes
// after the first value — the leg-10 wire-drift class (#1308: drifted
// or proxy-corrupted bytes riding a valid HTTP 200 parsing as a
// phantom success). Trailing whitespace (a final newline) stays
// acceptable.
func decodeStrict(r io.Reader, v any) error {
	dec := json.NewDecoder(r)
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		if err != nil {
			return fmt.Errorf("trailing bytes after JSON value: %w", err)
		}
		return errors.New("trailing bytes after JSON value")
	}
	return nil
}

// statusError renders a non-2xx into an error carrying the route, status,
// and a bounded body excerpt (validation messages name the offending
// field — load-bearing for callers diagnosing wire drift).
func (c *Client) statusError(route string, resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return fmt.Errorf("%s: status %d: %s", route, resp.StatusCode, strings.TrimSpace(string(body)))
}

// seamDefaultTimeout bounds the seam's own requests when the caller supplies no
// deadline (the synchronous send inherits the caller's budget — a
// long generation is legitimate).
const seamDefaultTimeout = 10 * time.Minute

// NewLoopbackClient constructs a seam client for THIS pod's opencode.
// Keep-alive is deliberately disabled: the pinned opencode's server can
// silently drop idle pooled connections (live-debugged 2026-09-13 — a
// reused POST hangs forever on a ghost socket with no established
// connection and no FIN; fresh connections always complete). Loopback
// does not need reuse; correctness does.
func NewLoopbackClient(baseURL, password string) *Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DisableKeepAlives = true
	return NewClient(baseURL, password, nil, WithHTTPClient(&http.Client{
		Timeout:   seamDefaultTimeout,
		Transport: transport,
	}))
}
