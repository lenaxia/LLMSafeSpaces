// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package opencode

import (
	"encoding/json"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/lenaxia/llmsafespaces/pkg/agent"
	"github.com/lenaxia/llmsafespaces/pkg/agent/opencode/wire"
	"github.com/lenaxia/llmsafespaces/pkg/session"
)

// ClientEventsFromEvent implements agent.Adapter (US-65.8): translates
// one raw agent SSE event into zero or more CONTRACT events for client
// consumption. Clients render contract shapes only — the agent's wire
// names, envelopes, and part shapes stay behind this seam.
//
// Translation table (opencode → pkg/session):
//   - message.part.delta            → EventPartDelta (streaming text)
//   - message.part.updated          → EventPartEnd with the translated
//     Part (snapshot: tool state, reasoning/text updates); a
//     step-finish part instead yields EventSessionUpdated carrying
//     ContextUsage (per-step occupancy semantics preserved)
//   - session.updated               → EventSessionUpdated {Title,
//     ParentID} — NEVER ContextUsage: its tokens are cumulative
//   - session.error, session.next.step.failed → EventError
//
// V2-queue turns (design 0052, opencode ≥1.18.15) emit ONLY the
// session.next.* taxonomy — V1 part events never fire for them:
//   - session.next.text.delta       → EventPartDelta (streaming text)
//   - session.next.text.ended       → EventPartEnd (final text part)
//   - session.next.prompted         → EventPartEnd (user echo: the
//     admitted prompt text — the same signal the V1 echo carried, which
//     the frontend's queued-message strip matches on)
//   - session.next.step.ended       → EventSessionUpdated with
//     ContextUsage from the step's tokens (per-step occupancy)
//
// Events with no client-facing signal (the majority — heartbeats,
// status, plugin/file events) return nil after a string-compare type
// check; the streaming hot path never pays a parse for them.
func (a *Adapter) ClientEventsFromEvent(eventType string, rawData string) []session.Event {
	switch {
	case wire.IsPartUpdated(eventType):
		return a.clientEventsFromPartUpdate(eventType, rawData)
	case wire.IsPartDelta(eventType):
		return clientEventsFromPartDelta(eventType, rawData)
	case wire.IsSessionUpdated(eventType):
		return clientEventsFromSessionUpdated(rawData)
	case eventType == "session.error" || eventType == "session.next.step.failed":
		return clientEventsFromError(eventType, rawData)
	case eventType == "session.next.text.delta":
		return clientEventsFromNextTextDelta(rawData)
	case eventType == "session.next.text.started":
		return clientEventsFromNextTextStarted(rawData)
	case eventType == "session.next.text.ended":
		return clientEventsFromNextTextEnded(rawData)
	case eventType == "session.next.prompted":
		return clientEventsFromNextPrompted(rawData)
	case eventType == "session.next.step.ended":
		return clientEventsFromNextStepEnded(rawData)
	default:
		return nil
	}
}

// RetryFromEvent implements agent.Adapter: decodes an agent backoff/
// retry notification into the platform retry shape. Retry detail is
// UI-shaped by nature and rides the platform session.status SSE event
// rather than the contract (no contract change needed).
func (a *Adapter) RetryFromEvent(eventType string, rawData string) (string, *agent.ClientRetryStatus, bool) {
	if eventType != "session.status" {
		return "", nil, false
	}
	var env wire.Envelope
	if json.Unmarshal([]byte(rawData), &env) != nil || len(env.Properties) == 0 {
		return "", nil, false
	}
	var p struct {
		SessionID string `json:"sessionID"`
		Status    struct {
			Type    string `json:"type"`
			Attempt int    `json:"attempt"`
			Message string `json:"message"`
			Next    int64  `json:"next"`
			Action  string `json:"action"`
		} `json:"status"`
	}
	if json.Unmarshal(env.Properties, &p) != nil || p.SessionID == "" || p.Status.Type != "retry" {
		return "", nil, false
	}
	return p.SessionID, &agent.ClientRetryStatus{
		Attempt: p.Status.Attempt,
		Message: p.Status.Message,
		NextAt:  p.Status.Next,
		Action:  p.Status.Action,
	}, true
}

func (a *Adapter) clientEventsFromPartUpdate(eventType string, rawData string) []session.Event {
	var env wire.Envelope
	if json.Unmarshal([]byte(rawData), &env) != nil || len(env.Properties) == 0 {
		return nil
	}
	var p struct {
		SessionID string          `json:"sessionID"`
		Part      json.RawMessage `json:"part"`
	}
	if json.Unmarshal(env.Properties, &p) != nil || p.SessionID == "" {
		a.logger.Warn("opencode part event lacks sessionID — wire drift?",
			zap.String("eventType", eventType))
		return nil
	}
	// Usage check FIRST via the seam's raw-properties parse: a
	// step-finish part's tokens are not carried by ocPart, and a
	// legacy standalone step-ended event parses the same way (mixed
	// fleet). Both yield the per-step ContextUsage session update.
	if u, ok, _ := wire.ParseStepUsageProps(eventType, env.Properties); ok {
		return []session.Event{{
			Type:      session.EventSessionUpdated,
			Timestamp: time.Now().UTC(),
			SessionID: p.SessionID,
			Session:   &session.Session{ID: p.SessionID, ContextUsage: &session.ContextUsage{Used: u.Tokens.PromptTokens()}},
		}}
	}
	var part ocPart
	if len(p.Part) > 0 {
		if json.Unmarshal(p.Part, &part) != nil {
			part = ocPart{}
		}
	}
	sp, _ := translatePart(part)
	if sp.Type == "" {
		return nil
	}
	return []session.Event{{
		Type:      session.EventPartEnd,
		Timestamp: time.Now().UTC(),
		SessionID: p.SessionID,
		MessageID: part.MessageID,
		PartID:    part.ID,
		Part:      &sp,
	}}
}

func clientEventsFromPartDelta(eventType string, rawData string) []session.Event {
	var env wire.Envelope
	if json.Unmarshal([]byte(rawData), &env) != nil || len(env.Properties) == 0 {
		return nil
	}
	var p struct {
		SessionID string `json:"sessionID"`
		MessageID string `json:"messageID"`
		PartID    string `json:"partID"`
		Delta     string `json:"delta"`
	}
	if json.Unmarshal(env.Properties, &p) != nil || p.SessionID == "" || p.Delta == "" {
		return nil
	}
	return []session.Event{{
		Type:      session.EventPartDelta,
		Timestamp: time.Now().UTC(),
		SessionID: p.SessionID,
		MessageID: p.MessageID,
		PartID:    p.PartID,
		Delta:     p.Delta,
	}}
}

func clientEventsFromSessionUpdated(rawData string) []session.Event {
	var env wire.Envelope
	if json.Unmarshal([]byte(rawData), &env) != nil || len(env.Properties) == 0 {
		return nil
	}
	var p struct {
		SessionID string `json:"sessionID"`
		Info      *struct {
			ID       string `json:"id"`
			Title    string `json:"title"`
			ParentID string `json:"parentID"`
		} `json:"info"`
	}
	if json.Unmarshal(env.Properties, &p) != nil || p.Info == nil || p.Info.ID == "" {
		return nil
	}
	if p.Info.Title == "" && p.Info.ParentID == "" {
		return nil
	}
	return []session.Event{{
		Type:      session.EventSessionUpdated,
		Timestamp: time.Now().UTC(),
		SessionID: p.Info.ID,
		Session: &session.Session{
			ID:       p.Info.ID,
			Title:    p.Info.Title,
			ParentID: p.Info.ParentID,
		},
	}}
}

func clientEventsFromError(eventType string, rawData string) []session.Event {
	var env wire.Envelope
	if json.Unmarshal([]byte(rawData), &env) != nil || len(env.Properties) == 0 {
		return nil
	}
	var p struct {
		SessionID string `json:"sessionID"`
		Error     *struct {
			Name    string `json:"name"`
			Type    string `json:"type"`
			Message string `json:"message"`
			Data    *struct {
				Message    string `json:"message"`
				ProviderID string `json:"providerID"`
			} `json:"data"`
		} `json:"error"`
	}
	if json.Unmarshal(env.Properties, &p) != nil || p.SessionID == "" || p.Error == nil {
		return nil
	}
	code := p.Error.Name
	if code == "" {
		code = p.Error.Type
	}
	msg := p.Error.Message
	if p.Error.Data != nil {
		if msg == "" {
			msg = p.Error.Data.Message
		}
		// Auth failures name the provider in the message so the client
		// can surface it (the contract Error has no provider field).
		if p.Error.Data.ProviderID != "" {
			msg = fmt.Sprintf("%s (%s)", msg, p.Error.Data.ProviderID)
		}
	}
	if code == "" && msg == "" {
		return nil
	}
	return []session.Event{{
		Type:      session.EventError,
		Timestamp: time.Now().UTC(),
		SessionID: p.SessionID,
		Error:     &session.Error{Code: code, Message: msg},
	}}
}

// clientEventsFromNextTextStarted translates the V2 text-block start
// into a MODE-FLIPPING part.end: the frontend routes part.delta events
// by the mode the last part.end set, and the preceding prompted echo
// leaves it in "user-echo" (discard). An empty assistant text part.end
// primes the streaming buffer ("text" mode + the part slot), so the
// following deltas append live — without this, a V2 turn's text would
// appear only at text.ended. The empty-text branch of the part.end
// handler exists for exactly this priming shape (it appends an empty
// text part and records the slot index).
func clientEventsFromNextTextStarted(rawData string) []session.Event {
	var env wire.Envelope
	if json.Unmarshal([]byte(rawData), &env) != nil || len(env.Properties) == 0 {
		return nil
	}
	var p struct {
		SessionID          string `json:"sessionID"`
		AssistantMessageID string `json:"assistantMessageID"`
		TextID             string `json:"textID"`
	}
	if json.Unmarshal(env.Properties, &p) != nil || p.SessionID == "" {
		return nil
	}
	return []session.Event{{
		Type:      session.EventPartEnd,
		Timestamp: time.Now().UTC(),
		SessionID: p.SessionID,
		MessageID: p.AssistantMessageID,
		PartID:    p.TextID,
		Part:      &session.Part{Type: session.PartText, ID: p.TextID, Text: ""},
	}}
}

// clientEventsFromNextTextDelta translates a V2 streaming text delta
// (session.next.text.delta). The wire carries assistantMessageID and
// textID where the V1 taxonomy carried messageID/partID — both map
// onto the contract fields unchanged.
func clientEventsFromNextTextDelta(rawData string) []session.Event {
	var env wire.Envelope
	if json.Unmarshal([]byte(rawData), &env) != nil || len(env.Properties) == 0 {
		return nil
	}
	var p struct {
		SessionID          string `json:"sessionID"`
		AssistantMessageID string `json:"assistantMessageID"`
		TextID             string `json:"textID"`
		Delta              string `json:"delta"`
	}
	if json.Unmarshal(env.Properties, &p) != nil || p.SessionID == "" || p.Delta == "" {
		return nil
	}
	return []session.Event{{
		Type:      session.EventPartDelta,
		Timestamp: time.Now().UTC(),
		SessionID: p.SessionID,
		MessageID: p.AssistantMessageID,
		PartID:    p.TextID,
		Delta:     p.Delta,
	}}
}

// clientEventsFromNextTextEnded translates the V2 final-text event
// (session.next.text.ended) into the part.end the streaming renderer
// consumes (ChatPage replaces the accumulated delta buffer with the
// final snapshot).
func clientEventsFromNextTextEnded(rawData string) []session.Event {
	var env wire.Envelope
	if json.Unmarshal([]byte(rawData), &env) != nil || len(env.Properties) == 0 {
		return nil
	}
	var p struct {
		SessionID          string `json:"sessionID"`
		AssistantMessageID string `json:"assistantMessageID"`
		TextID             string `json:"textID"`
		Text               string `json:"text"`
	}
	if json.Unmarshal(env.Properties, &p) != nil || p.SessionID == "" {
		return nil
	}
	return []session.Event{{
		Type:      session.EventPartEnd,
		Timestamp: time.Now().UTC(),
		SessionID: p.SessionID,
		MessageID: p.AssistantMessageID,
		PartID:    p.TextID,
		Part:      &session.Part{Type: session.PartText, ID: p.TextID, Text: p.Text},
	}}
}

// clientEventsFromNextPrompted translates the V2 promotion event into
// the user-echo part.end — the SAME contract signal the V1 echo
// carried, which the frontend's queued-message strip matches on to
// clear pills and suppress agent-side rendering of the user's text.
func clientEventsFromNextPrompted(rawData string) []session.Event {
	var env wire.Envelope
	if json.Unmarshal([]byte(rawData), &env) != nil || len(env.Properties) == 0 {
		return nil
	}
	var p struct {
		SessionID string `json:"sessionID"`
		MessageID string `json:"messageID"`
		Prompt    struct {
			Text string `json:"text"`
		} `json:"prompt"`
	}
	if json.Unmarshal(env.Properties, &p) != nil || p.SessionID == "" || p.Prompt.Text == "" {
		return nil
	}
	return []session.Event{{
		Type:      session.EventPartEnd,
		Timestamp: time.Now().UTC(),
		SessionID: p.SessionID,
		MessageID: p.MessageID,
		Part:      &session.Part{Type: session.PartText, Text: p.Prompt.Text},
	}}
}

// clientEventsFromNextStepEnded translates the V2 step-end event into
// the per-step ContextUsage session update — mirroring the V1
// step-finish part's EventSessionUpdated (tokens.input under the same
// prompt-occupancy semantics).
func clientEventsFromNextStepEnded(rawData string) []session.Event {
	var env wire.Envelope
	if json.Unmarshal([]byte(rawData), &env) != nil || len(env.Properties) == 0 {
		return nil
	}
	var p struct {
		SessionID string `json:"sessionID"`
		Tokens    *struct {
			Input int64 `json:"input"`
			Cache struct {
				Read  int64 `json:"read"`
				Write int64 `json:"write"`
			} `json:"cache"`
		} `json:"tokens"`
	}
	if json.Unmarshal(env.Properties, &p) != nil || p.SessionID == "" || p.Tokens == nil {
		return nil
	}
	return []session.Event{{
		Type:      session.EventSessionUpdated,
		Timestamp: time.Now().UTC(),
		SessionID: p.SessionID,
		Session: &session.Session{
			ID:           p.SessionID,
			ContextUsage: &session.ContextUsage{Used: p.Tokens.Input + p.Tokens.Cache.Read + p.Tokens.Cache.Write},
		},
	}}
}
