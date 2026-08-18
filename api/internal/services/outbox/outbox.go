// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package outbox implements the D3 durable-prompt outbox (design 0050,
// #907): accept-then-deliver decoupling so a client disconnect can never
// lose an accepted message.
//
// The incident (2026-08-15/16) lost six messages to iOS killing in-flight
// POSTs — the synchronous adapter.Send bound delivery to the client's
// request context, so a disconnect canceled it mid-flight. The outbox
// accepts into Valkey (AOF-persisted) and a detached worker delivers.
//
// Storage layout (per session):
//
//	outboxq:{ws}:{ses}  LIST — FIFO of JSON entries (the durable queue)
//	outboxd:{ws}:{ses}  LIST — staging: entries currently being delivered
//	                          (crash recovery requeues these on start)
//	outboxdedupe:{cmid} STRING — clientMessageID dedupe marker (SET NX, TTL)
//	outboxlock:{ws}:{ses} STRING — per-session delivery lock (reduces
//	                          cross-replica duplicate delivery)
//
// Delivery semantics: at-least-once (a crash after opencode persists but
// before the entry is removed can redeliver); duplicates collapse at
// render via clientMessageID — the stated D3 decision, do not revisit.
package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/go-redis/redis/v8"
)

// Entry is one accepted prompt in the outbox.
type Entry struct {
	ID              string          `json:"id"`              // outbox entry ID (unique per accept)
	ClientMessageID string          `json:"clientMessageID"` // caller-supplied dedupe key
	Text            string          `json:"text"`            // prompt text (already validated ≤100KB)
	Model           json.RawMessage `json:"model,omitempty"` // per-prompt model selector (raw JSON)
	AcceptedAt      time.Time       `json:"acceptedAt"`
	Attempts        int             `json:"attempts"`
	LastError       string          `json:"lastError,omitempty"`
	Status          string          `json:"status"` // pending | delivering | error
}

// Status values.
const (
	StatusPending    = "pending"
	StatusDelivering = "delivering"
	StatusError      = "error"
)

// Tunables (vars for tests).
var (
	// MaxAttempts bounds retries before an entry parks as error.
	MaxAttempts = 5
	// Cap is the per-session outbox length limit (accept returns capped).
	Cap = 25
	// DedupeTTL bounds how long a clientMessageID is remembered.
	DedupeTTL = 24 * time.Hour
	// DeliveryTimeout bounds one delivery attempt (a long turn).
	DeliveryTimeout = 10 * time.Minute
	// RetryBackoff is the delay before a failed entry is retried.
	RetryBackoff = 5 * time.Second
	// LockTTL bounds the per-session delivery lock.
	LockTTL = 2 * time.Minute
)

// ErrCapped is returned when the session's outbox is at Cap.
var ErrCapped = errors.New("session outbox is full")

// Duplicate is returned when the clientMessageID was already accepted.
// AcceptedID carries the original entry's ID.
type Duplicate struct{ AcceptedID string }

func (d *Duplicate) Error() string {
	return "duplicate clientMessageID: " + d.AcceptedID
}

// Deliverer delivers one entry. The worker calls it with a detached,
// timeout-bounded context — it must not reference the accepting request.
type Deliverer func(ctx context.Context, workspaceID, sessionID string, e Entry) error

// Service is the outbox. All methods are safe for concurrent use; the
// Valkey client is borrowed (lifecycle owned by the caller).
type Service struct {
	client *redis.Client
}

// New returns a Service backed by client, or nil if client is nil
// (outbox disabled — callers must nil-check).
func New(client *redis.Client) *Service {
	if client == nil {
		return nil
	}
	return &Service{client: client}
}

func qKey(ws, ses string) string    { return "outboxq:" + ws + ":" + ses }
func dKey(ws, ses string) string    { return "outboxd:" + ws + ":" + ses }
func lockKey(ws, ses string) string { return "outboxlock:" + ws + ":" + ses }

// Accept validates nothing (callers validate); it dedupes, caps, and
// persists. Returns the accepted entry (status pending). A Duplicate
// return means the clientMessageID was already accepted — the caller
// should respond 200 with the original ID, not an error.
func (s *Service) Accept(ctx context.Context, workspaceID, sessionID, clientMessageID, text string, model json.RawMessage) (*Entry, error) {
	if clientMessageID != "" {
		ok, err := s.client.SetNX(ctx, "outboxdedupe:"+clientMessageID, "1", DedupeTTL).Result()
		if err != nil {
			return nil, fmt.Errorf("outbox dedupe: %w", err)
		}
		if !ok {
			return nil, &Duplicate{}
		}
	}
	n, err := s.client.LLen(ctx, qKey(workspaceID, sessionID)).Result()
	if err != nil {
		return nil, fmt.Errorf("outbox len: %w", err)
	}
	if int(n) >= Cap {
		return nil, ErrCapped
	}
	e := &Entry{
		ID:              fmt.Sprintf("ob_%d_%s", time.Now().UnixNano(), sanitize(clientMessageID)),
		ClientMessageID: clientMessageID,
		Text:            text,
		Model:           model,
		AcceptedAt:      time.Now().UTC(),
		Status:          StatusPending,
	}
	b, err := json.Marshal(e)
	if err != nil {
		return nil, fmt.Errorf("outbox marshal: %w", err)
	}
	if err := s.client.RPush(ctx, qKey(workspaceID, sessionID), b).Err(); err != nil {
		return nil, fmt.Errorf("outbox push: %w", err)
	}
	return e, nil
}

// List returns the session's outbox entries (FIFO order): pending and
// error entries from the main list plus any stuck in delivering.
func (s *Service) List(ctx context.Context, workspaceID, sessionID string) ([]Entry, error) {
	vals, err := s.client.LRange(ctx, qKey(workspaceID, sessionID), 0, -1).Result()
	if err != nil {
		return nil, fmt.Errorf("outbox list: %w", err)
	}
	out := make([]Entry, 0, len(vals))
	for _, v := range vals {
		var e Entry
		if json.Unmarshal([]byte(v), &e) == nil {
			out = append(out, e)
		}
	}
	dvals, err := s.client.LRange(ctx, dKey(workspaceID, sessionID), 0, -1).Result()
	if err == nil {
		for _, v := range dvals {
			var e Entry
			if json.Unmarshal([]byte(v), &e) == nil {
				e.Status = StatusDelivering
				out = append(out, e)
			}
		}
	}
	return out, nil
}

// sessions discovers session keys with a non-empty main or staging list.
// SCAN-based; bounded by Valkey's keyspace.
func (s *Service) sessions(ctx context.Context) [][2]string {
	var out [][2]string
	seen := map[string]bool{}
	var cursor uint64
	for {
		keys, next, err := s.client.Scan(ctx, cursor, "outboxq:*", 100).Result()
		if err != nil {
			return out
		}
		for _, k := range keys {
			parts := strings.SplitN(strings.TrimPrefix(k, "outboxq:"), ":", 2)
			if len(parts) != 2 {
				continue
			}
			ws, ses := parts[0], parts[1]
			key := ws + "\x00" + ses
			if !seen[key] {
				seen[key] = true
				out = append(out, [2]string{ws, ses})
			}
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}
	dcursor := uint64(0)
	for {
		keys, next, err := s.client.Scan(ctx, dcursor, "outboxd:*", 100).Result()
		if err != nil {
			return out
		}
		for _, k := range keys {
			parts := strings.SplitN(strings.TrimPrefix(k, "outboxd:"), ":", 2)
			if len(parts) != 2 {
				continue
			}
			ws, ses := parts[0], parts[1]
			key := ws + "\x00" + ses
			if !seen[key] {
				seen[key] = true
				out = append(out, [2]string{ws, ses})
			}
		}
		dcursor = next
		if dcursor == 0 {
			break
		}
	}
	return out
}

// deliverOne delivers the head-of-queue pending entry for one session
// under the per-session lock. Returns true if work was done.
// DeliverOnce delivers the head-of-queue pending entry for one session
// (same semantics as the Run loop's per-session step). Exported for the
// handler-level tests; the Run loop is the production driver.
func (s *Service) DeliverOnce(ctx context.Context, ws, ses string, d Deliverer) bool {
	return s.deliverOne(ctx, ws, ses, d)
}

func (s *Service) deliverOne(ctx context.Context, ws, ses string, d Deliverer) bool {
	lock := lockKey(ws, ses)
	ok, err := s.client.SetNX(ctx, lock, "1", LockTTL).Result()
	if err != nil || !ok {
		return false // another worker (this or the other replica) owns it
	}
	defer s.client.Del(ctx, lock)

	qk := qKey(ws, ses)
	// Find the first non-error entry (errors park in place; later entries
	// must still flow).
	vals, err := s.client.LRange(ctx, qk, 0, -1).Result()
	if err != nil || len(vals) == 0 {
		return false
	}
	idx := -1
	var e Entry
	for i, v := range vals {
		if json.Unmarshal([]byte(v), &e) == nil && e.Status != StatusError {
			idx = i
			break
		}
	}
	if idx < 0 {
		return false
	}

	// Stage: remove from main, push to delivering (crash recovery).
	staged := mustMarshal(e)
	s.client.LRem(ctx, qk, 1, vals[idx])
	s.client.RPush(ctx, dKey(ws, ses), staged)

	dctx, cancel := context.WithTimeout(context.Background(), DeliveryTimeout)
	defer cancel()
	err = d(dctx, ws, ses, e)
	if err == nil {
		s.client.LRem(ctx, dKey(ws, ses), 1, staged)
		return true
	}
	// Failure: restore to the main list AT ITS POSITION with attempts+1
	// (error status when attempts exhausted). LRem shrank the list, so a
	// bare LSet at the old index can run off the end (empty-list LSet
	// silently fails — found by the retry tests): reinsert relative to
	// the current occupant of the position.
	e.Attempts++
	e.LastError = err.Error()
	if e.Attempts >= MaxAttempts {
		e.Status = StatusError
	} else {
		e.Status = StatusPending
	}
	updated := string(mustMarshal(e))
	s.client.LRem(ctx, dKey(ws, ses), 1, staged)
	cur, _ := s.client.LRange(ctx, qk, 0, -1).Result()
	switch {
	case len(cur) == 0 || idx >= len(cur):
		s.client.RPush(ctx, qk, updated)
	case idx == 0:
		s.client.LPush(ctx, qk, updated)
	default:
		s.client.LInsert(ctx, qk, "BEFORE", cur[idx], updated)
	}
	return true
}

// Recover requeues staged (delivering) entries left by a crash — run at
// worker start. At-least-once: a crash after the deliverer succeeded but
// before removal redelivers; render dedupe collapses it.
func (s *Service) Recover(ctx context.Context) int {
	n := 0
	for _, pair := range s.sessions(ctx) {
		ws, ses := pair[0], pair[1]
		dk := dKey(ws, ses)
		for {
			v, err := s.client.LPop(ctx, dk).Result()
			if err != nil {
				break
			}
			var e Entry
			if json.Unmarshal([]byte(v), &e) == nil {
				e.Status = StatusPending
				s.client.LPush(ctx, qKey(ws, ses), string(mustMarshal(e)))
				n++
			}
		}
	}
	return n
}

// Run drives delivery until ctx is done. One entry in flight per session;
// sessions discovered by scan each tick. Meant to run once per API
// replica — duplicate delivery across replicas is possible but rare
// (per-session lock) and safe (at-least-once + render dedupe).
func (s *Service) Run(ctx context.Context, d Deliverer, tick time.Duration) {
	s.Recover(ctx)
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			for _, pair := range s.sessions(ctx) {
				select {
				case <-ctx.Done():
					return
				default:
				}
				s.deliverOne(ctx, pair[0], pair[1], d)
			}
		}
	}
}

// Dismiss removes an error entry by ID (the queue UI's dismiss action).
func (s *Service) Dismiss(ctx context.Context, workspaceID, sessionID, id string) bool {
	qk := qKey(workspaceID, sessionID)
	vals, err := s.client.LRange(ctx, qk, 0, -1).Result()
	if err != nil {
		return false
	}
	for _, v := range vals {
		var e Entry
		if json.Unmarshal([]byte(v), &e) == nil && e.ID == id {
			s.client.LRem(ctx, qk, 1, v)
			return true
		}
	}
	return false
}

// Retry clears an error entry back to pending (the queue UI's retry).
func (s *Service) Retry(ctx context.Context, workspaceID, sessionID, id string) bool {
	qk := qKey(workspaceID, sessionID)
	vals, err := s.client.LRange(ctx, qk, 0, -1).Result()
	if err != nil {
		return false
	}
	for i, v := range vals {
		var e Entry
		if json.Unmarshal([]byte(v), &e) == nil && e.ID == id && e.Status == StatusError {
			e.Status = StatusPending
			e.Attempts = 0
			e.LastError = ""
			s.client.LSet(ctx, qk, int64(i), string(mustMarshal(e)))
			return true
		}
	}
	return false
}

func sanitize(s string) string {
	if len(s) > 32 {
		s = s[:32]
	}
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		default:
			return '_'
		}
	}, s)
}

func mustMarshal(e Entry) []byte {
	b, _ := json.Marshal(e)
	return b
}
