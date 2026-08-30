// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package wire is the ONLY code in the platform that decodes opencode wire
// bytes (HTTP bodies and /event SSE envelopes). The API-side adapter, the
// SSE tracker's metering path, and the in-pod agentd's usage parser all
// consume it; nothing else may import it. Shapes are pinned by golden
// fixtures in ../testdata — see the opencode upgrade runbook.
//
// Parsing is dual-shape tolerant by design. Two reasons with live-capture
// evidence (see testdata/REFRESH.md): (1) the same logical event type is
// named differently on different surfaces — the /event SSE stream emits
// unsuffixed types ("message.part.updated") while the persisted event
// store emits version-suffixed types ("message.part.updated.1"); (2) the
// fleet runs mixed opencode versions during image rollouts. Decoders
// accept every supported shape rather than branching on versions at
// runtime.
package wire

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Tokens mirrors opencode's token-usage shape, stable across 1.15.x–1.18.x.
type Tokens struct {
	Total     int64 `json:"total,omitempty"`
	Input     int64 `json:"input"`
	Output    int64 `json:"output"`
	Reasoning int64 `json:"reasoning"`
	Cache     struct {
		Read  int64 `json:"read"`
		Write int64 `json:"write"`
	} `json:"cache"`
}

// PromptTokens returns the raw prompt occupancy in opencode's accounting
// convention: input + cache.read + cache.write. Providers with no cache
// convention report it all in input; the sum stays correct.
func (t Tokens) PromptTokens() int64 {
	return t.Input + t.Cache.Read + t.Cache.Write
}

// Envelope is the /event SSE wire envelope shared by all event types.
type Envelope struct {
	ID         string          `json:"id"`
	Type       string          `json:"type"`
	Properties json.RawMessage `json:"properties"`
}

// IsPartUpdated reports whether eventType is a message-part update. The
// live /event stream emits "message.part.updated"; the persisted event
// store emits "message.part.updated.N" — both must match (see REFRESH.md).
func IsPartUpdated(eventType string) bool {
	return eventType == "message.part.updated" || isSuffixed(eventType, "message.part.updated")
}

// IsStepEnded reports whether eventType is the legacy per-step usage event
// (pre-1.18.10). The legacy name is unversioned.
func IsStepEnded(eventType string) bool {
	return eventType == "session.next.step.ended"
}

// isSuffixed reports whether eventType is base+"."+digits (a versioned
// event name, e.g. "session.updated.1").
func isSuffixed(eventType, base string) bool {
	if !strings.HasPrefix(eventType, base+".") {
		return false
	}
	suffix := eventType[len(base)+1:]
	if suffix == "" {
		return false
	}
	if _, err := strconv.Atoi(suffix); err != nil {
		return false
	}
	return true
}

// StepUsage is the decoded per-step usage signal from any supported wire
// shape: the session it belongs to and that step's token accounting.
type StepUsage struct {
	SessionID string
	Tokens    Tokens
}

// ParseStepUsage decodes a raw /event payload carrying per-step usage:
// the legacy standalone step-ended event (≤1.15.x era) or a part-update
// event whose part is a step-finish (1.18.x). ok=false when the event
// carries no usage (any other part type, or an event type that never
// carries usage). err is non-nil when the event TYPE claims usage but the
// payload fails to decode — that is wire drift and must surface, not
// silently drop.
//
// The eventType check runs before any byte conversion or JSON decode, so
// the non-usage majority of stream traffic (deltas are the hottest and
// largest events) costs only string compares through this function.
// Matched part-update events pay one envelope decode — inherent, since
// only the payload says whether the part is a step-finish.
func ParseStepUsage(eventType string, raw string) (StepUsage, bool, error) {
	var parse func(json.RawMessage) (StepUsage, bool, error)
	switch {
	case IsStepEnded(eventType):
		parse = parseLegacyStepEnded
	case IsPartUpdated(eventType):
		parse = parsePartUpdate
	default:
		return StepUsage{}, false, nil
	}
	var env Envelope
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		return StepUsage{}, false, fmt.Errorf("usage event envelope undecodable: %w", err)
	}
	return parse(env.Properties)
}

// ParseStepUsageProps is ParseStepUsage for callers that already stripped
// the envelope (agentd's legacy nested-payload path). Same semantics.
func ParseStepUsageProps(eventType string, props json.RawMessage) (StepUsage, bool, error) {
	switch {
	case IsStepEnded(eventType):
		return parseLegacyStepEnded(props)
	case IsPartUpdated(eventType):
		return parsePartUpdate(props)
	default:
		return StepUsage{}, false, nil
	}
}

func parseLegacyStepEnded(props json.RawMessage) (StepUsage, bool, error) {
	var p struct {
		SessionID string  `json:"sessionID"`
		Tokens    *Tokens `json:"tokens"`
	}
	if len(props) == 0 {
		return StepUsage{}, false, fmt.Errorf("step.ended event lacks properties")
	}
	if err := json.Unmarshal(props, &p); err != nil {
		return StepUsage{}, false, fmt.Errorf("step.ended properties undecodable: %w", err)
	}
	if p.SessionID == "" || p.Tokens == nil {
		return StepUsage{}, false, fmt.Errorf("step.ended event claims usage but lacks sessionID or tokens")
	}
	return StepUsage{SessionID: p.SessionID, Tokens: *p.Tokens}, true, nil
}

func parsePartUpdate(props json.RawMessage) (StepUsage, bool, error) {
	var p struct {
		SessionID string `json:"sessionID"`
		Part      struct {
			Type   string  `json:"type"`
			Tokens *Tokens `json:"tokens"`
		} `json:"part"`
	}
	if len(props) == 0 {
		return StepUsage{}, false, fmt.Errorf("part-update event lacks properties")
	}
	if err := json.Unmarshal(props, &p); err != nil {
		return StepUsage{}, false, fmt.Errorf("part-update properties undecodable: %w", err)
	}
	if p.Part.Type != "step-finish" {
		return StepUsage{}, false, nil
	}
	if p.SessionID == "" || p.Part.Tokens == nil {
		return StepUsage{}, false, fmt.Errorf("step-finish part claims usage but lacks sessionID or tokens")
	}
	return StepUsage{SessionID: p.SessionID, Tokens: *p.Part.Tokens}, true, nil
}

// SessionUsage is the decoded session-level CUMULATIVE usage and model
// attribution from session.updated events — the metering/billing signal.
// Unlike StepUsage (per-step occupancy), these counters only grow;
// delta computation is the caller's policy, not the decoder's.
type SessionUsage struct {
	SessionID    string
	ModelID      string
	ProviderID   string
	InputTokens  int64
	OutputTokens int64
	CostUSD      float64
	// CostMalformed reports a cost field that is neither a number nor
	// an object with a numeric "cost" — decoded as CostUSD=0 so billing
	// proceeds, but callers should warn: it is a wire-drift signal.
	CostMalformed bool
}

// IsSessionUpdated reports whether eventType is a session.updated event,
// tolerating the store surface's version suffix ("session.updated.1").
func IsSessionUpdated(eventType string) bool {
	return eventType == "session.updated" || isSuffixed(eventType, "session.updated")
}

// ParseSessionUpdated decodes a session.updated event's cumulative usage
// and model attribution. ok=false (no error) when the event carries no
// info block — session.updated legitimately fires without one (early
// lifecycle). err is non-nil only for usage-bearing payloads that fail
// to decode (wire drift). Envelope-based, like ParseStepUsage.
func ParseSessionUpdated(eventType string, raw string) (SessionUsage, bool, error) {
	if !IsSessionUpdated(eventType) {
		return SessionUsage{}, false, nil
	}
	var env Envelope
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		return SessionUsage{}, false, fmt.Errorf("session.updated envelope undecodable: %w", err)
	}
	return ParseSessionUpdatedProps(env.Properties)
}

// ParseSessionUpdatedProps is ParseSessionUpdated for callers holding
// already-stripped properties (the SSE tracker's dispatch path).
func ParseSessionUpdatedProps(props json.RawMessage) (SessionUsage, bool, error) {
	var p struct {
		Info *struct {
			ID    string `json:"id"`
			Model struct {
				ID         string `json:"id"`
				ProviderID string `json:"providerID"`
				Provider   string `json:"provider"`
			} `json:"model"`
			Tokens struct {
				Input  int64 `json:"input"`
				Output int64 `json:"output"`
			} `json:"tokens"`
			Cost json.RawMessage `json:"cost"`
		} `json:"info"`
	}
	if len(props) == 0 {
		return SessionUsage{}, false, nil
	}
	if err := json.Unmarshal(props, &p); err != nil {
		return SessionUsage{}, false, fmt.Errorf("session.updated properties undecodable: %w", err)
	}
	if p.Info == nil {
		return SessionUsage{}, false, nil
	}
	// Session identity is info.id — the field the metering path has
	// always keyed on. properties.sessionID is redundant on the wire
	// (same value); an empty info.id must stay empty so callers can
	// warn-and-skip malformed billing events (pinned behavior).
	u := SessionUsage{
		SessionID:    p.Info.ID,
		ModelID:      p.Info.Model.ID,
		ProviderID:   p.Info.Model.ProviderID,
		InputTokens:  p.Info.Tokens.Input,
		OutputTokens: p.Info.Tokens.Output,
	}
	if u.ProviderID == "" {
		u.ProviderID = p.Info.Model.Provider
	}
	if len(p.Info.Cost) > 0 {
		trimmed := bytes.TrimSpace(p.Info.Cost)
		var costFloat float64
		if json.Unmarshal(trimmed, &costFloat) == nil {
			u.CostUSD = costFloat
		} else {
			// 1.18.x object shape: "cost" is the dollar amount;
			// "total" there is a token count — never use it as cost.
			var costObj struct {
				Cost float64 `json:"cost"`
			}
			if json.Unmarshal(trimmed, &costObj) == nil {
				u.CostUSD = costObj.Cost
			} else {
				u.CostMalformed = true
			}
		}
	}
	return u, true, nil
}

// knownEventTypes is the pinned event taxonomy: every type observed in
// the golden fixtures (live /event capture + persisted store; see
// ../testdata/REFRESH.md) plus legacy event-system names retained for
// mixed-fleet rollout tolerance.
// The fixture-coverage test forces every fixture type to be listed here,
// so a fixture refresh that introduces a type extends this set in the
// same change.
// Event-type literals (the dialect seam — Rule 12): consumers outside
// pkg/agent/opencode reference these constants, never raw strings
// (repolint: agent-event-literals gate).
const (
	EventTypeSessionStatus      = "session.status"
	EventTypeSessionCreated     = "session.created"
	EventTypeSessionUpdated     = "session.updated"
	EventTypeSessionIdle        = "session.idle"
	EventTypeMessagePartUpdated = "message.part.updated"
	EventTypeMessagePartDelta   = "message.part.delta"
)

var knownEventTypes = map[string]bool{
	"session.status":               true,
	"session.updated":              true,
	"session.created":              true,
	"session.idle":                 true,
	"session.diff":                 true,
	"session.error":                true,
	"message.created":              true,
	"message.updated":              true,
	"message.part.updated":         true,
	"message.part.delta":           true,
	"server.connected":             true,
	"server.heartbeat":             true,
	"plugin.added":                 true,
	"catalog.updated":              true,
	"reference.updated":            true,
	"integration.updated":          true,
	"file.edited":                  true,
	"file.watcher.updated":         true,
	"session.next.step.ended":      true,
	"session.next.step.started":    true,
	"session.next.step.failed":     true,
	"session.next.prompt.admitted": true,
	"session.next.prompted":        true,
	"session.next.text.started":    true,
	"session.next.text.delta":      true,
	"session.next.text.ended":      true,
}

// IsKnownEventType reports whether eventType belongs to the pinned
// opencode taxonomy (version suffixes tolerated: the persisted store
// emits suffixed variants of the same logical types). Unknown types are
// not errors — they are the drift signal consumers count and warn on
// (#739 Gap 2: a taxonomy rename must be observable, not silent).
func IsKnownEventType(eventType string) bool {
	if eventType == "" {
		return false
	}
	if knownEventTypes[eventType] {
		return true
	}
	// Version-suffixed variants (numeric last segment only — same rule
	// as isSuffixed) are the same logical type as their base.
	if i := strings.LastIndex(eventType, "."); i >= 0 {
		if suffix := eventType[i+1:]; suffix != "" {
			if _, err := strconv.Atoi(suffix); err == nil {
				return knownEventTypes[eventType[:i]]
			}
		}
	}
	return false
}

// IsPartDelta reports whether eventType is a message-part delta event,
// tolerating the store surface's version suffix ("message.part.delta.1").
func IsPartDelta(eventType string) bool {
	return eventType == "message.part.delta" || isSuffixed(eventType, "message.part.delta")
}
