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
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/lenaxia/llmsafespaces/cmd/workspace-agentd/sessionstate"
	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
	"go.uber.org/zap"
)

// opencodeSessionEventParser is the US-69.2-stage EventParser: it maps the
// opencode session.status SSE shape (flat and legacy-nested envelopes) onto
// the contract session-status event. Every other event type reports
// ok=false (dropped + counted by the authority) until US-69.3's full
// translation.
type opencodeSessionEventParser struct{}

func (opencodeSessionEventParser) Parse(raw []byte) (*abiv1.Event, bool, error) {
	var flat struct {
		Type       string          `json:"type"`
		Properties json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(raw, &flat); err != nil {
		return nil, false, nil
	}
	props := flat.Properties
	if flat.Type == "" {
		var nested struct {
			Payload struct {
				Type       string          `json:"type"`
				Properties json.RawMessage `json:"properties"`
			} `json:"payload"`
		}
		if err := json.Unmarshal(raw, &nested); err != nil {
			return nil, false, nil
		}
		flat.Type = nested.Payload.Type
		props = nested.Payload.Properties
	}
	if flat.Type != "session.status" || len(props) == 0 {
		return nil, false, nil
	}
	var p struct {
		SessionID string `json:"sessionID"`
		Status    struct {
			Type string `json:"type"`
		} `json:"status"`
	}
	if err := json.Unmarshal(props, &p); err != nil {
		return nil, true, fmt.Errorf("session.status properties: %w", err)
	}
	if p.SessionID == "" {
		return nil, false, nil
	}
	var status abiv1.SessionStatus
	switch p.Status.Type {
	case "idle":
		status = abiv1.SessionStatus_SESSION_STATUS_IDLE
	case "busy", "retry":
		status = abiv1.SessionStatus_SESSION_STATUS_BUSY
	case "error":
		status = abiv1.SessionStatus_SESSION_STATUS_ERROR
	case "compacting":
		status = abiv1.SessionStatus_SESSION_STATUS_COMPACTING
	default:
		return nil, false, nil
	}
	return &abiv1.Event{
		Type:      abiv1.EventType_EVENT_TYPE_SESSION_STATUS,
		SessionId: p.SessionID,
		Status:    status,
	}, true, nil
}

// opencodeStoreReader is the US-69.2-stage StoreReader: opencode's session
// list is the store truth for session statuses. Respects ctx; a wedged
// opencode surfaces as ctx deadline (M3.1).
type opencodeStoreReader struct {
	client *OpenCodeClient
}

func (r opencodeStoreReader) SessionStatuses(ctx context.Context) (map[string]abiv1.SessionStatus, error) {
	sessions, err := r.client.ListSessions(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]abiv1.SessionStatus, len(sessions))
	for _, s := range sessions {
		switch s.Status {
		case "busy":
			out[s.ID] = abiv1.SessionStatus_SESSION_STATUS_BUSY
		case "error":
			out[s.ID] = abiv1.SessionStatus_SESSION_STATUS_ERROR
		default:
			out[s.ID] = abiv1.SessionStatus_SESSION_STATUS_IDLE
		}
	}
	return out, nil
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
		PlatformDir: platformDirFromEnv(),
		Parser:      opencodeSessionEventParser{},
		Store:       opencodeStoreReader{client: client},
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
