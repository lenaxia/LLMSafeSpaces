// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package wire is the ONLY code in the platform that decodes opencode wire
// bytes (HTTP bodies and /event SSE envelopes). The API-side adapter
// consumes it today; the in-pod agentd's own parsers migrate onto it as
// deferred follow-up work. Nothing else may import it. Shapes are pinned
// by golden fixtures in ../testdata — see the opencode upgrade runbook.
//
// Parsing is dual-shape tolerant by design: the fleet runs mixed opencode
// versions during image rollouts, so decoders accept every supported
// version's shape (e.g. both unsuffixed and version-suffixed event type
// names) rather than branching on versions at runtime.
package wire

import (
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

// IsPartUpdated reports whether eventType is a message-part update,
// tolerating version suffixes ("message.part.updated.1").
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
