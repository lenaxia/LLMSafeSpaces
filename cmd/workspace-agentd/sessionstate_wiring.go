// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// Epic 69 US-69.2: construction + wiring of the sessionstate authority.
// The authority itself (cmd/workspace-agentd/sessionstate) is dialect-free
// machinery; the opencode implementations of its two seams live here in the
// wiring layer, consistent with agentd's existing dialect handling (the
// tracker). US-69.3 relocates full contract translation behind the adapter
// seam and grows these implementations.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/lenaxia/llmsafespaces/cmd/workspace-agentd/sessionstate"
	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
	"github.com/lenaxia/llmsafespaces/pkg/agent"
	opencode "github.com/lenaxia/llmsafespaces/pkg/agent/opencode"
	"github.com/lenaxia/llmsafespaces/pkg/version"
	"go.uber.org/zap"
)

// opencodeStoreReader is the US-69.3 StoreReader: opencode's session list
// is the store truth for statuses; the /question + /permission lists are
// the truth for pending inputs. Respects ctx; a wedged opencode surfaces as
// ctx deadline (M3.1).
type opencodeStoreReader struct {
	client *OpenCodeClient
}

func (r opencodeStoreReader) SessionStates(ctx context.Context) (map[string]sessionstate.SessionSeed, error) {
	sessions, err := r.client.ListSessions(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]sessionstate.SessionSeed, len(sessions))
	for _, s := range sessions {
		var status abiv1.SessionStatus
		switch s.Status {
		case "busy":
			status = abiv1.SessionStatus_SESSION_STATUS_BUSY
		case "error":
			status = abiv1.SessionStatus_SESSION_STATUS_ERROR
		default:
			status = abiv1.SessionStatus_SESSION_STATUS_IDLE
		}
		out[s.ID] = sessionstate.SessionSeed{Status: status}
	}
	d := &opencode.Dialect{}
	for _, in := range fetchList(ctx, r.client, "/question", d.ParseQuestionListItem) {
		if seed, ok := out[in.SessionID]; ok {
			seed.PendingInputs = append(seed.PendingInputs, questionToABI(in))
			out[in.SessionID] = seed
		}
	}
	for _, in := range fetchList(ctx, r.client, "/permission", d.ParsePermissionListItem) {
		if seed, ok := out[in.SessionID]; ok {
			seed.PendingInputs = append(seed.PendingInputs, permissionToABI(in))
			out[in.SessionID] = seed
		}
	}
	return out, nil
}

// fetchList GETs a JSON-array endpoint and maps each entry through parse.
// Unreachable/unimplemented endpoints (404, conn refused) are an
// authoritative-empty for PENDING INPUTS specifically — opencode versions
// without the endpoints never had questions; session-list errors above are
// the real store-read failure path.
func fetchList[T any](ctx context.Context, client *OpenCodeClient, path string, parse func(json.RawMessage) (T, error)) []T {
	resp, err := client.doRequest(ctx, path)
	if err != nil || resp.StatusCode >= 400 {
		if resp != nil {
			_ = resp.Body.Close()
		}
		return nil
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) != nil {
		return nil
	}
	var out []T
	for _, item := range items {
		if v, err := parse(item); err == nil {
			out = append(out, v)
		}
	}
	return out
}

func questionToABI(q *agent.QuestionRequest) *abiv1.InputRequest {
	in := &abiv1.InputRequest{Id: q.ID, SessionId: q.SessionID, Kind: abiv1.InputKind_INPUT_KIND_QUESTION}
	if len(q.Questions) > 0 {
		inq := q.Questions[0]
		in.Question = inq.Question
		in.Header = inq.Header
		in.Multiple = inq.Multiple
		in.Custom = inq.Custom
		for _, o := range inq.Options {
			in.Options = append(in.Options, &abiv1.InputOption{Label: o.Label, Description: o.Description})
		}
	}
	if q.Tool != nil {
		in.Tool = &abiv1.ToolRef{MessageId: q.Tool.MessageID, CallId: q.Tool.CallID}
	}
	return in
}

func permissionToABI(p *agent.PermissionRequest) *abiv1.InputRequest {
	in := &abiv1.InputRequest{
		Id: p.ID, SessionId: p.SessionID, Kind: abiv1.InputKind_INPUT_KIND_PERMISSION,
		Permission: p.Permission, Patterns: p.Patterns, Always: p.Always,
	}
	if p.Tool != nil {
		in.Tool = &abiv1.ToolRef{MessageId: p.Tool.MessageID, CallId: p.Tool.CallID}
	}
	return in
}

// platformDirFromEnv resolves the platform/ PVC subPath mount (US-69.2).
func platformDirFromEnv() string {
	if v := os.Getenv("LLMSAFESPACES_PLATFORM_DIR"); v != "" {
		return v
	}
	return sessionstate.DefaultPlatformDir
}

// newStateAuthority builds the authority and returns it plus the raw-event
// hook for the SSE tracker. S1 shadow semantics: a missing/unwritable
// platform dir (staged rollout, chart not yet carrying the subPath) degrades
// to an in-memory cursor with a loud WARN — the surface must be additive
// and harmless (design 0055 M4). At S2 the authority flag makes the durable
// cursor a boot requirement instead.
func newStateAuthority(client *OpenCodeClient, password, controlPlanePassword string) *sessionstate.Authority {
	cfg := sessionstate.Config{
		PlatformDir:  platformDirFromEnv(),
		Parser:       opencode.ABITranslator{},
		Store:        opencodeStoreReader{client: client},
		Admitter:     opencodeAdmitter{password: password},
		Capabilities: bootCapabilityReport(client),
		// D6.1 pair: accept either credential across mixed-generation
		// windows; empty entries are skipped by the auth gate.
		Passwords: []string{controlPlanePassword, password},
		Logger:    log,
	}
	a, err := sessionstate.New(cfg)
	if err != nil {
		// Degraded mode: durable cursor unavailable (staged rollout, chart
		// not yet carrying the platform/ subPath). Seq resets on restart
		// until the volume lands — visible, loud, never fatal at S1.
		log.Warn("sessionstate: durable cursor unavailable — running with non-durable seq (staged rollout?)",
			zap.String("dir", cfg.PlatformDir), zap.Error(err))
		cfg.PlatformDir = os.TempDir() + "/llmsafespaces-sessionstate-cursor"
		a, err = sessionstate.New(cfg)
		if err != nil {
			log.Error("sessionstate: authority construction failed — module disabled for this boot", zap.Error(err))
			return nil
		}
	}
	return a
}

// startStateAuthorityReseed drives the boot reseed: opencode may not be
// reachable yet, so retry with backoff until the first success. Generation
// changes reseed through the same serialized path.
func startStateAuthorityReseed(ctx context.Context, a *sessionstate.Authority, reason sessionstate.ReseedReason) {
	if a == nil {
		return
	}
	go func() {
		backoff := 500 * time.Millisecond
		const maxBackoff = 15 * time.Second
		for {
			if ctx.Err() != nil {
				return
			}
			if err := a.Reseed(ctx, reason); err == nil {
				return
			} else if ctx.Err() == nil {
				log.Debug("sessionstate: boot reseed retry", zap.Error(err))
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			if backoff*2 > maxBackoff {
				backoff = maxBackoff
			} else {
				backoff *= 2
			}
		}
	}()
}

// bootCapabilityReport builds the static capability report served on every
// snapshot frame (US-69.4). Provenance: the 0053 overlay anchor
// (AGENTD_IMAGE_VOLUME — the digest-pinned delivery the self-verify gate
// enforces); BYO/legacy bases report UNPINNED (the M4 wiring rejects
// authority-flag-on for them). Harness version: ONE bounded boot-time
// discovery call (the report is static afterwards — M3.1: no harness calls
// on hot paths). Supported actions: none declared until US-69.9 wires the
// action surface; file delivery parts are NotSupported on opencode per D3.
func bootCapabilityReport(client *OpenCodeClient) *abiv1.CapabilityReport {
	provenance := abiv1.Provenance_PROVENANCE_PLATFORM_PINNED
	if os.Getenv("AGENTD_IMAGE_VOLUME") != "1" {
		provenance = abiv1.Provenance_PROVENANCE_UNPINNED
	}
	harnessVersion := "unknown"
	if client != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		if _, v, err := client.IsHealthy(ctx); err == nil && v != "" {
			harnessVersion = v
		}
		cancel()
	}
	return &abiv1.CapabilityReport{
		Provenance:             provenance,
		Harness:                "opencode",
		HarnessVersion:         harnessVersion,
		AgentdVersion:          version.Version,
		SupportedActions:       nil,
		SupportedDeliveryParts: []abiv1.DeliveryPartKind{abiv1.DeliveryPartKind_DELIVERY_PART_KIND_TEXT},
		AbiVersion:             "1",
	}
}

// opencodeAdmitter is the US-69.7 admission seam implementation: POST the
// V2 prompt endpoint on the pod's opencode (localhost :4096, §D1 Basic
// credential). Delivery mode "queue" (0052 semantics: durable admission,
// drains on idle/wake). The returned message ID is the promotion
// correlation input (M2).
type opencodeAdmitter struct {
	password string
}

func (o opencodeAdmitter) Admit(ctx context.Context, sessionID, text, model string) (string, error) {
	if text == "" {
		return "", fmt.Errorf("admit: empty text")
	}
	body := map[string]any{
		"prompt":   map[string]any{"text": text},
		"delivery": "queue",
	}
	if model != "" {
		body["model"] = map[string]any{"id": model}
	}
	b, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	url := fmt.Sprintf("%s/api/session/%s/prompt", getAgentAddr(), sessionID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return "", err
	}
	req.SetBasicAuth("opencode", o.password)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("admit: status %d: %s", resp.StatusCode, string(errBody))
	}
	var out struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return "", err
	}
	return out.Data.ID, nil
}
