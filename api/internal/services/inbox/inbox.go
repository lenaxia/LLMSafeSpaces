// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package inbox implements the unanswered-question inbox (#1313, epic-71
// stream 3a): the outbox's sibling for inbound asks. Every surfaced ask is
// recorded here at ask-projection time; records survive workspace
// suspension (Redis is API-side) and re-present on the snapshot flight as
// "while you were away" prompts. Exactly two exits per record (S11):
// answered-in-history or dismissed — both are terminal statuses updated
// IN PLACE; a terminal record is immutable and never re-presents, so a
// duplicate resolution event cannot resurrect a cleared prompt.
//
// Storage layout (per workspace:session):
//
//	inboxq:{ws}:{ses}  HASH — ask ID → JSON Record; whole-key TTL bounds
//	                     abandoned sessions; per-session pending cap with
//	                     FIFO eviction of the oldest pending record.
//
// Atomicity: upsert and resolve run as Lua scripts — a racing Record
// refresh can never overwrite a terminal status (checked inside the
// script), and cap eviction + insert is one atomic step.
package inbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/go-redis/redis/v8"
)

// Ask kinds (mirror the session contract's input kinds; kept local so the
// store carries no dependency, same as the outbox).
const (
	KindQuestion   = "question"
	KindPermission = "permission"
)

// Record statuses. Pending is the only mutable state; answered/dismissed
// are terminal and immutable.
const (
	StatusPending   = "pending"
	StatusAnswered  = "answered"
	StatusDismissed = "dismissed"
)

// Tunables (vars for tests).
var (
	// Cap is the per-session pending-record limit; the oldest pending
	// record is evicted (FIFO) when a new record would exceed it. A
	// long-ignored session cannot accumulate an unbounded prompt stack.
	Cap = 10
	// TTL bounds the whole per-session inbox key. 24h matches the outbox
	// dedupe marker: asks a user ignored for a day are dead ends the
	// transcript already closed.
	TTL = 24 * time.Hour
)

// ToolRef identifies the tool call that raised the ask.
type ToolRef struct {
	MessageID string `json:"messageID,omitempty"`
	CallID    string `json:"callID,omitempty"`
}

// Option is one selectable choice of a question ask.
type Option struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// Record is one inbox entry: the full ask, re-presentable without the
// harness that raised it. The Lua scripts pattern-match on
// `"status":"pending"` and "recordedNs":<int> literals — the struct's
// json tags are part of that contract (tested together).
type Record struct {
	ID         string    `json:"id"`
	SessionID  string    `json:"sessionID"`
	Kind       string    `json:"kind"`
	Status     string    `json:"status"`
	RecordedNs int64     `json:"recordedNs"`
	RecordedAt time.Time `json:"recordedAt"`

	Question string   `json:"question,omitempty"`
	Header   string   `json:"header,omitempty"`
	Options  []Option `json:"options,omitempty"`
	Multiple bool     `json:"multiple,omitempty"`
	Custom   bool     `json:"custom,omitempty"`

	Permission string   `json:"permission,omitempty"`
	Patterns   []string `json:"patterns,omitempty"`
	Always     []string `json:"always,omitempty"`

	Tool *ToolRef `json:"tool,omitempty"`
}

var (
	// ErrInvalid is a malformed call: empty IDs, kind/status outside the
	// enums, or a session-ID mismatch between key and record.
	ErrInvalid = errors.New("inbox: invalid record")
)

func key(workspaceID, sessionID string) string {
	return fmt.Sprintf("inboxq:%s:%s", workspaceID, sessionID)
}

// upsertScript atomically records one ask. KEYS[1] = inbox key.
// ARGV: 1 = ask ID, 2 = payload, 3 = cap, 4 = TTL seconds.
// Returns 1 when written, 0 when the existing record is terminal
// (immutable no-op).
var upsertScript = redis.NewScript(`
local existing = redis.call('HGET', KEYS[1], ARGV[1])
if existing then
  if string.find(existing, '"status":"pending"', 1, true) then
    redis.call('HSET', KEYS[1], ARGV[1], ARGV[2])
    redis.call('EXPIRE', KEYS[1], tonumber(ARGV[4]))
    return 1
  end
  return 0
end
local pending = 0
local oldest = false
local oldestNs = nil
local all = redis.call('HGETALL', KEYS[1])
for i = 1, #all, 2 do
  local v = all[i + 1]
  if string.find(v, '"status":"pending"', 1, true) then
    pending = pending + 1
    local ns = string.match(v, '"recordedNs":(%d+)')
    if ns and (oldestNs == nil or tonumber(ns) < tonumber(oldestNs)) then
      oldestNs = ns
      oldest = all[i]
    end
  end
end
if pending >= tonumber(ARGV[3]) and oldest then
  redis.call('HDEL', KEYS[1], oldest)
end
redis.call('HSET', KEYS[1], ARGV[1], ARGV[2])
redis.call('EXPIRE', KEYS[1], tonumber(ARGV[4]))
return 1
`)

// resolveScript atomically terminalizes a pending record in place by
// patching the status literal; terminal or absent records are no-ops.
// KEYS[1] = inbox key. ARGV: 1 = ask ID, 2 = terminal status, 3 = TTL
// seconds. Returns 1 when the record transitioned, 0 otherwise.
var resolveScript = redis.NewScript(`
local existing = redis.call('HGET', KEYS[1], ARGV[1])
if not existing then
  return 0
end
if not string.find(existing, '"status":"pending"', 1, true) then
  return 0
end
local patched = string.gsub(existing, '"status":"pending"', '"status":"' .. ARGV[2] .. '"')
redis.call('HSET', KEYS[1], ARGV[1], patched)
redis.call('EXPIRE', KEYS[1], tonumber(ARGV[3]))
return 1
`)

// Service is the inbox store. Borrowed client, lifecycle owned by the
// caller (same construction as the outbox).
type Service struct {
	client *redis.Client
}

// New returns the inbox service, or nil when there is no Redis client
// (inbox disabled — same convention as outbox.New).
func New(client *redis.Client) *Service {
	if client == nil {
		return nil
	}
	return &Service{client: client}
}

func validateRecord(workspaceID string, rec Record) error {
	if workspaceID == "" || rec.ID == "" || rec.SessionID == "" {
		return fmt.Errorf("%w: empty workspace, ask, or session ID", ErrInvalid)
	}
	if rec.Kind != KindQuestion && rec.Kind != KindPermission {
		return fmt.Errorf("%w: kind %q", ErrInvalid, rec.Kind)
	}
	if rec.Status != StatusPending {
		return fmt.Errorf("%w: records enter as pending, got %q", ErrInvalid, rec.Status)
	}
	if rec.RecordedAt.IsZero() {
		return fmt.Errorf("%w: zero RecordedAt", ErrInvalid)
	}
	return nil
}

// Record upserts an ask: absent → written; pending → refreshed in place;
// terminal → immutable no-op. When the session's pending count is at Cap
// the oldest pending record is evicted (FIFO) to make room.
func (s *Service) Record(ctx context.Context, workspaceID string, rec Record) error {
	if err := validateRecord(workspaceID, rec); err != nil {
		return err
	}
	rec.RecordedNs = rec.RecordedAt.UnixNano()
	payload, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("inbox: marshal record: %w", err)
	}
	if _, err := upsertScript.Run(ctx, s.client,
		[]string{key(workspaceID, rec.SessionID)},
		rec.ID, string(payload), Cap, int(TTL.Seconds())).Result(); err != nil {
		return fmt.Errorf("inbox: record: %w", err)
	}
	return nil
}

// Resolve terminalizes a pending record in place (answered or dismissed).
// Absent and already-terminal records are no-op successes — resolution
// events are droppable and duplicated, never authoritative re-opens.
func (s *Service) Resolve(ctx context.Context, workspaceID, sessionID, askID, status string) error {
	if status != StatusAnswered && status != StatusDismissed {
		return fmt.Errorf("%w: resolve status %q", ErrInvalid, status)
	}
	if workspaceID == "" || sessionID == "" || askID == "" {
		return fmt.Errorf("%w: empty workspace, session, or ask ID", ErrInvalid)
	}
	if _, err := resolveScript.Run(ctx, s.client,
		[]string{key(workspaceID, sessionID)},
		askID, status, int(TTL.Seconds())).Result(); err != nil {
		return fmt.Errorf("inbox: resolve: %w", err)
	}
	return nil
}

// List returns the session's pending records, oldest first. Terminal
// records never re-present.
func (s *Service) List(ctx context.Context, workspaceID, sessionID string) ([]Record, error) {
	if workspaceID == "" || sessionID == "" {
		return nil, fmt.Errorf("%w: empty workspace or session ID", ErrInvalid)
	}
	all, err := s.client.HGetAll(ctx, key(workspaceID, sessionID)).Result()
	if err != nil {
		return nil, fmt.Errorf("inbox: list: %w", err)
	}
	pending := make([]Record, 0, len(all))
	for _, payload := range all {
		var rec Record
		if err := json.Unmarshal([]byte(payload), &rec); err != nil {
			continue
		}
		if rec.Status == StatusPending {
			pending = append(pending, rec)
		}
	}
	sort.Slice(pending, func(i, j int) bool {
		if pending[i].RecordedAt.Equal(pending[j].RecordedAt) {
			return pending[i].ID < pending[j].ID
		}
		return pending[i].RecordedAt.Before(pending[j].RecordedAt)
	})
	return pending, nil
}

// PendingIDs returns the IDs of the session's pending records — the
// snapshot union's dedupe set (live asks ∪ inbox, live wins).
func (s *Service) PendingIDs(ctx context.Context, workspaceID, sessionID string) (map[string]bool, error) {
	pending, err := s.List(ctx, workspaceID, sessionID)
	if err != nil {
		return nil, err
	}
	ids := make(map[string]bool, len(pending))
	for _, rec := range pending {
		ids[rec.ID] = true
	}
	return ids, nil
}

// ListWorkspace returns the pending records of EVERY session under the
// workspace, globally oldest-first. The snapshot flight is per-workspace
// and the reply routes carry no session ID — both need the cross-session
// view. SCAN-based over inboxq:{ws}:*.
func (s *Service) ListWorkspace(ctx context.Context, workspaceID string) ([]Record, error) {
	if workspaceID == "" {
		return nil, fmt.Errorf("%w: empty workspace ID", ErrInvalid)
	}
	var out []Record
	pattern := fmt.Sprintf("inboxq:%s:*", workspaceID)
	var cursor uint64
	for {
		keys, next, err := s.client.Scan(ctx, cursor, pattern, 100).Result()
		if err != nil {
			return nil, fmt.Errorf("inbox: list workspace scan: %w", err)
		}
		for _, k := range keys {
			all, err := s.client.HGetAll(ctx, k).Result()
			if err != nil {
				continue
			}
			for _, payload := range all {
				var rec Record
				if err := json.Unmarshal([]byte(payload), &rec); err != nil {
					continue
				}
				if rec.Status == StatusPending {
					out = append(out, rec)
				}
			}
		}
		if next == 0 {
			break
		}
		cursor = next
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].RecordedAt.Equal(out[j].RecordedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].RecordedAt.Before(out[j].RecordedAt)
	})
	return out, nil
}

// LookupPending finds one pending record by ask ID across the
// workspace's sessions. Terminal and absent records return ok=false.
func (s *Service) LookupPending(ctx context.Context, workspaceID, askID string) (Record, bool, error) {
	if askID == "" {
		return Record{}, false, fmt.Errorf("%w: empty ask ID", ErrInvalid)
	}
	records, err := s.ListWorkspace(ctx, workspaceID)
	if err != nil {
		return Record{}, false, err
	}
	for _, rec := range records {
		if rec.ID == askID {
			return rec, true, nil
		}
	}
	return Record{}, false, nil
}
