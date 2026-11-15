// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// pi_state.go — the event pump: turn pi RPC session events into the
// agentd statusz parity state (pi second harness S2; scoping §1.2-1.3).
//
// THE SETTLE CONTRACT (upstream json.md, pinned v1.5.2 — and validated
// against the real binary, which emitted three agent_end records under
// auto-retry before ANY settle): `agent_end` closes ONE low-level run
// and explicitly does NOT mean idle — automatic retry, overflow
// recovery, compaction retry, steering, or follow-up work can follow.
// `agent_settled` is the ONLY idle transition: "Pi will not continue
// automatically through retries, compaction recovery, or queued
// messages." The pump therefore marks busy on agent_start and clears
// busy ONLY on agent_settled; agent_end counts runs (diagnostic), it
// never clears busy.
//
// #1685 message-level progress (owner ruling, mirrored from the
// opencode tracker's lastMessageAges): lastMessageAt stamps on a
// COMPLETED message from either side — message_end with role user
// (user-sent) or assistant (LLM-completed). Stream chunks
// (message_update), tool execution, and the system preamble do NOT
// stamp. LastProgressSeconds = age of lastMessageAt on the
// longest-quiet BUSY session; 0 when unknown (fails toward silence).
//
// D6 busy ages (#998): busySince stamps at agent_start; BusyAges maps
// session→seconds; OldestBusySeconds is the max. Sessions cleared by
// agent_settled stop contributing age in the same snapshot that
// renders them idle (one view, the #1574 status-and-age-from-one-truth
// rule — here by construction: both derive from the same state cell).

import (
	"encoding/json"
	"sort"
	"sync"
	"time"

	"github.com/lenaxia/llmsafespaces/pkg/agentd"
)

// piUsage is the cumulative provider usage object carried on
// message_update and message_end (json.md).
type piUsage struct {
	Input      int64 `json:"input"`
	Output     int64 `json:"output"`
	CacheRead  int64 `json:"cacheRead"`
	CacheWrite int64 `json:"cacheWrite"`
}

// piSessionState is the per-session derivation cell. One writer per
// source (the child's reader goroutine via applyEvent; the health loop
// via applyStatePoll) under the aggregator's mutex.
type piSessionState struct {
	ID string

	busy      bool
	busySince time.Time
	settledAt time.Time
	runsEnded int // agent_end count without settle (diagnostic)

	lastMessageAt time.Time
	lastEventAt   time.Time

	tokens       agentd.SessionTokens
	model        string
	title        string
	contextUsed  int64
	contextLimit int64

	// streaming/compacting mirror get_state's authoritative flags (the
	// corroboration input; the pump's event view stays primary).
	streaming  bool
	compacting bool

	// live is true while a child process is attached to this session.
	live bool
}

// applyEvent folds one session event into the state. now is injected
// for deterministic tests. Unknown event types are a no-op (defensive
// decode; upstream's own consumer posture). raw is the record's full
// JSON — the few events that carry payload at the envelope top level
// (session_info_changed.name) decode from it.
func (s *piSessionState) applyEvent(rec *piRecord, raw []byte, now time.Time) {
	s.lastEventAt = now
	switch rec.Type {
	case "agent_start":
		if !s.busy {
			s.busy = true
			s.busySince = now
		}
	case "agent_end":
		// NOT idle — see the settle contract above.
		s.runsEnded++
	case "agent_settled":
		s.busy = false
		s.settledAt = now
	case "message_end":
		s.applyMessageEnd(rec, now)
	case "session_info_changed":
		var top struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(raw, &top) == nil && top.Name != "" {
			s.title = top.Name
		}
	}
}

// applyMessageEnd stamps the #1685 message clock and folds usage. The
// message object sits at the envelope top level (json.md), so it is
// decoded from the raw record, not from response-data.
func (s *piSessionState) applyMessageEnd(rec *piRecord, now time.Time) {
	var top struct {
		Message json.RawMessage `json:"message"`
	}
	if json.Unmarshal(rec.Raw, &top) != nil || len(top.Message) == 0 {
		return
	}
	var msg struct {
		Role  string   `json:"role"`
		Usage *piUsage `json:"usage"`
	}
	if json.Unmarshal(top.Message, &msg) != nil {
		return
	}
	switch msg.Role {
	case "user", "assistant":
		s.lastMessageAt = now
	}
	if msg.Role == "assistant" && msg.Usage != nil {
		s.tokens = agentd.SessionTokens{
			Input:      msg.Usage.Input,
			Output:     msg.Usage.Output,
			CacheRead:  msg.Usage.CacheRead,
			CacheWrite: msg.Usage.CacheWrite,
		}
	}
}

// applyStatePoll folds a successful get_state response (corroboration
// + the model/title/context facts the event stream does not carry).
// isStreaming is AUTHORITATIVE for busy when the two disagree (a
// restart gap can swallow agent_settled; a missed agent_start leaves
// the pump idle while pi streams) — the poll reconciles the cell.
func (s *piSessionState) applyStatePoll(data json.RawMessage, now time.Time) {
	var st struct {
		SessionID    string `json:"sessionId"`
		SessionName  string `json:"sessionName"`
		IsStreaming  bool   `json:"isStreaming"`
		IsCompacting bool   `json:"isCompacting"`
		Model        *struct {
			ID            string `json:"id"`
			Provider      string `json:"provider"`
			ContextWindow int64  `json:"contextWindow"`
		} `json:"model"`
	}
	if json.Unmarshal(data, &st) != nil {
		return
	}
	s.streaming = st.IsStreaming
	s.compacting = st.IsCompacting
	if st.Model != nil {
		s.model = st.Model.Provider + "/" + st.Model.ID
		if st.Model.ContextWindow > 0 {
			s.contextLimit = st.Model.ContextWindow
		}
	}
	if st.SessionName != "" {
		s.title = st.SessionName
	}
	if st.IsStreaming != s.busy {
		if st.IsStreaming && !s.busy {
			s.busy = true
			s.busySince = now
		} else if !st.IsStreaming && s.busy {
			s.busy = false
			s.settledAt = now
		}
	}
}

// applyStatsPoll folds a successful get_session_stats response: the
// authoritative cumulative tokens + contextUsage (tokens/contextWindow
// may be null right after compaction — json null decodes to zero here,
// which we then ignore via the >0 guard on contextUsed only).
func (s *piSessionState) applyStatsPoll(data json.RawMessage) {
	var st struct {
		Tokens *struct {
			Input      int64 `json:"input"`
			Output     int64 `json:"output"`
			CacheRead  int64 `json:"cacheRead"`
			CacheWrite int64 `json:"cacheWrite"`
		} `json:"tokens"`
		ContextUsage *struct {
			Tokens        *int64 `json:"tokens"`
			ContextWindow *int64 `json:"contextWindow"`
		} `json:"contextUsage"`
	}
	if json.Unmarshal(data, &st) != nil {
		return
	}
	if st.Tokens != nil {
		s.tokens = agentd.SessionTokens{
			Input:      st.Tokens.Input,
			Output:     st.Tokens.Output,
			CacheRead:  st.Tokens.CacheRead,
			CacheWrite: st.Tokens.CacheWrite,
		}
	}
	if st.ContextUsage != nil {
		if st.ContextUsage.Tokens != nil {
			s.contextUsed = *st.ContextUsage.Tokens
		}
		if st.ContextUsage.ContextWindow != nil && *st.ContextUsage.ContextWindow > 0 {
			s.contextLimit = *st.ContextUsage.ContextWindow
		}
	}
}

// piStateAggregator is the per-pod parity surface: every live child's
// events fold into per-session cells, and Snapshot renders the exact
// shapes agentd's statusz consumes (`[]agentd.SessionInfo`,
// OldestBusySeconds/BusyAges/LastProgressSeconds — pkg/agentd/types.go
// #391-398 / #466-477).
type piStateAggregator struct {
	mu       sync.Mutex
	sessions map[string]*piSessionState
	// tap (S5 bridge): optional raw-event fan-out installed once at
	// boot. Read under mu (installed before the loops start), invoked
	// OUTSIDE the lock on a copied slice — subscribers never race the
	// fold and never alias the pump's buffer.
	tap func(sessionID string, raw []byte)
}

// setEventTap installs the raw-event tap (the bridge's SSE fan-out).
// Pre-loops only (construction time); a nil tap unsubscribes nothing.
func (a *piStateAggregator) setEventTap(tap func(sessionID string, raw []byte)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.tap = tap
}

func newPiStateAggregator() *piStateAggregator {
	return &piStateAggregator{sessions: make(map[string]*piSessionState)}
}

// cell returns (creating if needed) the session's state cell.
func (a *piStateAggregator) cell(id string) *piSessionState {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cellLocked(id)
}

// cellLocked is cell with the caller holding mu.
func (a *piStateAggregator) cellLocked(id string) *piSessionState {
	s, ok := a.sessions[id]
	if !ok {
		s = &piSessionState{ID: id}
		a.sessions[id] = s
	}
	return s
}

// ApplyEvent folds one event for one session. The whole mutation runs
// under the aggregator mutex — a bare cell() hand-out would race
// Snapshot's locked reads (pinned by -race).
func (a *piStateAggregator) ApplyEvent(sessionID string, rec *piRecord, raw []byte, now time.Time) {
	a.mu.Lock()
	tap := a.tap
	a.cellLocked(sessionID).applyEvent(rec, raw, now)
	a.mu.Unlock()
	if tap != nil {
		cp := make([]byte, len(raw))
		copy(cp, raw)
		tap(sessionID, cp)
	}
}

// ApplyStatePoll folds one get_state data payload for one session.
func (a *piStateAggregator) ApplyStatePoll(sessionID string, data json.RawMessage, now time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cellLocked(sessionID).applyStatePoll(data, now)
}

// ApplyStatsPoll folds one get_session_stats data payload.
func (a *piStateAggregator) ApplyStatsPoll(sessionID string, data json.RawMessage) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cellLocked(sessionID).applyStatsPoll(data)
}

// SetLive marks whether a child process is attached to the session.
func (a *piStateAggregator) SetLive(sessionID string, live bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cellLocked(sessionID).live = live
}

// GenerationStart applies the #1632 generation contract at a child
// respawn: the fresh process cannot be mid-turn, so any busy episode
// the dead generation left behind is void — busy/busySince/runsEnded
// reset (OldestBusySeconds must never keep growing across a restart;
// the wedge remedy would otherwise be invisible to the escalation
// inputs). Persisted facts (tokens/model/title/context — resumed from
// disk by the new child) and the #1685 lastMessageAt history stamp
// survive: they are records of past messages, not an unhealthy
// episode.
func (a *piStateAggregator) GenerationStart(sessionID string, _ uint64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.cellLocked(sessionID)
	s.busy = false
	s.busySince = time.Time{}
	s.runsEnded = 0
	s.streaming = false
	s.compacting = false
}

// Forget drops a session's cell entirely (session deleted).
func (a *piStateAggregator) Forget(sessionID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.sessions, sessionID)
}

// harnessStatusSnapshot is the parity surface rendered by the pump — the
// fields statusz needs, in agentd's own types.
// The type is harness-neutral (the statusz parity shape, S5's
// harness-driver seam) and crosses the control socket as the
// harness_status result — hence the JSON tags (agentd.SessionInfo and
// agentd.ContextUsage already carry their own).
type harnessStatusSnapshot struct {
	Sessions            []agentd.SessionInfo `json:"sessions,omitempty"`
	SessionsActive      int                  `json:"sessionsActive,omitempty"`
	OldestBusySeconds   int                  `json:"oldestBusySeconds,omitempty"`
	BusyAges            map[string]int       `json:"busyAges,omitempty"`
	LastProgressSeconds int                  `json:"lastProgressSeconds,omitempty"`
	// AgentVersion surfaces the pinned binary's --version (probed at
	// supervisor boot; empty = unreported). statusz renders it as the
	// AgentVersion field the RC-A fleet-currency machinery reads.
	AgentVersion string `json:"agentVersion,omitempty"`
	// Context is the aggregate window shape statusz renders
	// (TotalTokens = the common model limit; UsedTokens stays 0 for
	// the same reason the opencode path sets it 0 — a sum over
	// heterogeneous sessions is not meaningful).
	Context *agentd.ContextUsage `json:"context,omitempty"`
}

// Snapshot renders the parity fields at one instant. Deterministic
// order (sessions sorted by ID) so tests and consumers see stable
// output. Derivations mirror buildStatuszHandler's opencode logic:
// busy ages from busySince over BUSY sessions only; last progress =
// max lastMessageAt AGE over busy sessions; zero values mean "no
// evidence", never "alert".
func (a *piStateAggregator) Snapshot(now time.Time) harnessStatusSnapshot {
	a.mu.Lock()
	defer a.mu.Unlock()

	snap := harnessStatusSnapshot{BusyAges: make(map[string]int)}
	ids := make([]string, 0, len(a.sessions))
	for id := range a.sessions {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	var lastProgress int
	var contextLimit int64
	for _, id := range ids {
		s := a.sessions[id]
		status := "idle"
		if s.busy {
			status = "busy"
			snap.SessionsActive++
			secs := int(now.Sub(s.busySince).Seconds())
			if secs < 0 {
				secs = 0
			}
			snap.BusyAges[id] = secs
			if secs > snap.OldestBusySeconds {
				snap.OldestBusySeconds = secs
			}
			if !s.lastMessageAt.IsZero() {
				age := int(now.Sub(s.lastMessageAt).Seconds())
				if age < 0 {
					age = 0
				}
				if age > lastProgress {
					lastProgress = age
				}
			}
		}
		if s.contextLimit > contextLimit {
			contextLimit = s.contextLimit
		}
		tokens := s.tokens
		info := agentd.SessionInfo{
			ID:          s.ID,
			Title:       s.title,
			Status:      status,
			Tokens:      &tokens,
			Model:       s.model,
			ContextUsed: s.contextUsed,
		}
		snap.Sessions = append(snap.Sessions, info)
	}
	snap.LastProgressSeconds = lastProgress
	if contextLimit > 0 {
		snap.Context = &agentd.ContextUsage{UsedTokens: 0, TotalTokens: contextLimit}
	}
	return snap
}
