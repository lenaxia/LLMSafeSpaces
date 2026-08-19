// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package outbox implements the D3 durable-prompt outbox (design 0050,
// #907): accept-then-deliver decoupling so a client disconnect can never
// lose an accepted message.
//
// The incident (2026-08-15/16) lost six messages to iOS killing in-flight
// POSTs — the synchronous adapter.Send bound delivery to the client's
// request context, so a disconnect canceled it mid-flight. The outbox
// accepts into Valkey and a detached worker delivers.
//
// Storage layout (per session):
//
//	outboxq:{ws}:{ses}  LIST — FIFO of JSON entries (the durable queue)
//	outboxd:{ws}:{ses}  LIST — staging: entries currently being delivered
//	                          (crash recovery requeues these on start)
//	outboxdedupe:{ws}:{ses}:{cmid} STRING — dedupe marker; VALUE is the
//	                          accepted entry ID (set only AFTER a successful
//	                          push — a marker with no entry is a false
//	                          "duplicate" that silently drops retries)
//	outboxlock:{ws}:{ses} STRING — per-session delivery lock; value is a
//	                          random owner token, released only by the owner
//	                          (compare-and-del), TTL > DeliveryTimeout so a
//	                          long turn cannot expire its own lock
//
// Crash-safety: every multi-step transition is ordered so the worst case
// is DUPLICATE delivery (at-least-once, collapsed at render via
// clientMessageID), never LOSS:
//   - stage-out: RPUSH staging BEFORE LREM main (crash between → entry in
//     both → delivered possibly twice, never zero; Recover dedupes by ID)
//   - restore: reinsert main BEFORE LREM staging
//   - recover: LRANGE (no pop), dedupe against main by ID, then DEL
//
// Delivery semantics: at-least-once; duplicates collapse at render via
// clientMessageID — the stated D3 decision, do not revisit.
package outbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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
	UserID          string          `json:"userID"`          // accepting user (metering at delivery)
	Text            string          `json:"text"`            // prompt text (already validated ≤100KB)
	Model           json.RawMessage `json:"model,omitempty"` // per-prompt model selector (raw JSON)
	AcceptedAt      time.Time       `json:"acceptedAt"`
	Attempts        int             `json:"attempts"`
	NextAttemptAt   time.Time       `json:"nextAttemptAt,omitempty"` // backoff gate (zero = now)
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
	// MaxAttempts bounds retries before an entry parks as error. With the
	// exponential backoff below, 5 attempts span ~3.5 minutes — covering
	// an opencode restart cycle (the #903 reconciler pattern: retries
	// must span agent generations, not exhaust in seconds).
	MaxAttempts = 5
	// Cap is the per-session outbox length limit (accept returns capped).
	Cap = 25
	// DedupeTTL bounds how long a clientMessageID is remembered.
	DedupeTTL = 24 * time.Hour
	// DeliveryTimeout bounds one delivery attempt (a long turn).
	DeliveryTimeout = 10 * time.Minute
	// RetryBackoff is the BASE delay before attempt N+1; it doubles per
	// attempt and caps at MaxBackoff. Var for tests.
	RetryBackoff = 10 * time.Second
	// MaxBackoff caps the exponential retry delay.
	MaxBackoff = 2 * time.Minute
	// LockTTL must exceed DeliveryTimeout: a delivery longer than the
	// lock's TTL would expire mid-turn and let a second worker deliver
	// concurrently (one-in-flight violation).
	LockTTL = DeliveryTimeout + 2*time.Minute
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

func qKey(ws, ses string) string { return "outboxq:" + ws + ":" + ses }
func dKey(ws, ses string) string { return "outboxd:" + ws + ":" + ses }
func dedupeKey(ws, ses, cmid string) string {
	return "outboxdedupe:" + ws + ":" + ses + ":" + cmid
}
func lockKey(ws, ses string) string { return "outboxlock:" + ws + ":" + ses }

// Accept validates nothing (callers validate); it caps, persists, and
// only THEN records the dedupe marker. Marker ordering matters: a marker
// set before persistence survives a failed push for the dedupe TTL and
// turns every retry into a false "duplicate" — silent loss (the exact
// class D3 exists to kill; round-1 finding 3). Returns the accepted
// entry (status pending). A Duplicate return carries the original ID —
// callers should respond 200 with it, not an error.
func (s *Service) Accept(ctx context.Context, workspaceID, sessionID, userID, clientMessageID, text string, model json.RawMessage) (*Entry, error) {
	if clientMessageID != "" {
		// Fast-path the duplicate check (marker value = original ID);
		// the authoritative marker write happens AFTER the push below.
		if v, err := s.client.Get(ctx, dedupeKey(workspaceID, sessionID, clientMessageID)).Result(); err == nil {
			return nil, &Duplicate{AcceptedID: v}
		}
	}
	n, err := s.client.LLen(ctx, qKey(workspaceID, sessionID)).Result()
	if err != nil {
		return nil, fmt.Errorf("outbox len: %w", err)
	}
	if int(n) >= Cap {
		return nil, ErrCapped // no marker written: a retry after drain succeeds
	}
	e := &Entry{
		ID:              fmt.Sprintf("ob_%d_%s", time.Now().UnixNano(), sanitize(clientMessageID)),
		ClientMessageID: clientMessageID,
		UserID:          userID,
		Text:            text,
		Model:           model,
		AcceptedAt:      time.Now().UTC(),
		Status:          StatusPending,
	}
	b, err := json.Marshal(e)
	if err != nil {
		return nil, fmt.Errorf("outbox marshal: %w", err) // no marker
	}
	if err := s.client.RPush(ctx, qKey(workspaceID, sessionID), b).Err(); err != nil {
		return nil, fmt.Errorf("outbox push: %w", err) // no marker
	}
	// Persisted. NOW the marker (value = entry ID) — and if a concurrent
	// accept raced us to the marker, keep theirs and drop our duplicate
	// entry (their push is durable; ours would double-deliver the same
	// clientMessageID).
	if clientMessageID != "" {
		mk := dedupeKey(workspaceID, sessionID, clientMessageID)
		first, err := s.client.SetNX(ctx, mk, e.ID, DedupeTTL).Result()
		if err == nil && !first {
			s.client.LRem(ctx, qKey(workspaceID, sessionID), 1, b)
			if v, gerr := s.client.Get(ctx, mk).Result(); gerr == nil {
				return nil, &Duplicate{AcceptedID: v}
			}
			return nil, &Duplicate{}
		}
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
	add := func(pattern string) {
		var cursor uint64
		for {
			keys, next, err := s.client.Scan(ctx, cursor, pattern, 100).Result()
			if err != nil {
				return
			}
			for _, k := range keys {
				parts := strings.SplitN(strings.TrimPrefix(k, strings.SplitN(pattern, "*", 2)[0]), ":", 2)
				if len(parts) != 2 {
					continue
				}
				key := parts[0] + "\x00" + parts[1]
				if !seen[key] {
					seen[key] = true
					out = append(out, [2]string{parts[0], parts[1]})
				}
			}
			cursor = next
			if cursor == 0 {
				break
			}
		}
	}
	add("outboxq:*")
	add("outboxd:*")
	return out
}

// backoffFor returns the delay before attempt n+1 (n = failures so far):
// base doubling per attempt, capped.
func backoffFor(attempts int) time.Duration {
	d := RetryBackoff
	for i := 1; i < attempts && d < MaxBackoff; i++ {
		d *= 2
	}
	if d > MaxBackoff {
		d = MaxBackoff
	}
	return d
}

// acquireLock takes the per-session delivery lock with an owner token.
// TTL exceeds DeliveryTimeout so a long turn cannot expire its own lock.
func (s *Service) acquireLock(ctx context.Context, ws, ses string) (string, bool) {
	b := make([]byte, 8)
	_, _ = rand.Read(b) //nolint:gosec // lock token, not a secret
	token := "lk_" + hex.EncodeToString(b)
	ok, err := s.client.SetNX(ctx, lockKey(ws, ses), token, LockTTL).Result()
	if err != nil || !ok {
		return "", false
	}
	return token, true
}

// releaseLock deletes the lock ONLY if we still own it (compare-and-del).
// A bare DEL could remove a lock a slower worker's TTL-expiry let another
// worker legitimately acquire.
var releaseLockScript = redis.NewScript(`
if redis.call("get", KEYS[1]) == ARGV[1] then
	return redis.call("del", KEYS[1])
end
return 0
`)

func (s *Service) releaseLock(ctx context.Context, ws, ses, token string) {
	if token == "" {
		return
	}
	_ = releaseLockScript.Run(ctx, s.client, []string{lockKey(ws, ses)}, token).Err()
}

// DeliverOnce delivers the first due pending entry for one session
// (same semantics as the Run loop's per-session step). Exported for the
// handler-level tests; the Run loop is the production driver.
func (s *Service) DeliverOnce(ctx context.Context, ws, ses string, d Deliverer) bool {
	return s.deliverOne(ctx, ws, ses, d)
}

// deliverOne delivers the first due pending entry for one session under
// the per-session lock. Returns true if work was done. Backoff-gated
// entries (NextAttemptAt in the future) are skipped, not consumed.
func (s *Service) deliverOne(ctx context.Context, ws, ses string, d Deliverer) bool {
	token, ok := s.acquireLock(ctx, ws, ses)
	if !ok {
		return false // another worker (this replica or another) owns it
	}
	defer s.releaseLock(ctx, ws, ses, token)

	qk := qKey(ws, ses)
	vals, err := s.client.LRange(ctx, qk, 0, -1).Result()
	if err != nil || len(vals) == 0 {
		return false
	}
	now := time.Now().UTC()
	idx := -1
	var e Entry
	for i, v := range vals {
		var cand Entry
		if json.Unmarshal([]byte(v), &cand) == nil && cand.Status != StatusError {
			if cand.NextAttemptAt.IsZero() || !cand.NextAttemptAt.After(now) {
				idx, e = i, cand
				break
			}
		}
	}
	if idx < 0 {
		return false
	}

	// Stage-out (loss-proof ordering): RPUSH staging FIRST, then LREM
	// main. A crash between the two leaves the entry in BOTH — duplicate
	// delivery is possible (at-least-once), loss is not; Recover dedupes
	// by ID.
	staged := mustMarshal(e)
	s.client.RPush(ctx, dKey(ws, ses), staged)
	s.client.LRem(ctx, qk, 1, vals[idx])

	if err := deliverDetached(d, ws, ses, e); err == nil {
		s.client.LRem(ctx, dKey(ws, ses), 1, staged)
		return true
	}
	// Failure: restore main FIRST (LInsert before the current occupant,
	// preserving order), then LREM staging — the mirrored crash window
	// duplicates rather than loses.
	e.Attempts++
	e.LastError = err.Error()
	if e.Attempts >= MaxAttempts {
		e.Status = StatusError
	} else {
		e.Status = StatusPending
		e.NextAttemptAt = time.Now().UTC().Add(backoffFor(e.Attempts))
	}
	updated := string(mustMarshal(e))
	cur, _ := s.client.LRange(ctx, qk, 0, -1).Result()
	switch {
	case len(cur) == 0 || idx >= len(cur):
		s.client.RPush(ctx, qk, updated)
	case idx == 0:
		s.client.LPush(ctx, qk, updated)
	default:
		s.client.LInsert(ctx, qk, "BEFORE", cur[idx], updated)
	}
	s.client.LRem(ctx, dKey(ws, ses), 1, staged)
	return true
}

// Recover requeues staged (delivering) entries left by a crash — run at
// worker start. LRANGE (no pop) + ID-dedupe against main, then DEL: the
// crash windows above can leave an entry in both lists; the ID check
// prevents Recover itself from duplicating.
func (s *Service) Recover(ctx context.Context) int {
	n := 0
	for _, pair := range s.sessions(ctx) {
		ws, ses := pair[0], pair[1]
		dk := dKey(ws, ses)
		staged, err := s.client.LRange(ctx, dk, 0, -1).Result()
		if err != nil || len(staged) == 0 {
			continue
		}
		main, _ := s.client.LRange(ctx, qKey(ws, ses), 0, -1).Result()
		inMain := map[string]bool{}
		for _, v := range main {
			var e Entry
			if json.Unmarshal([]byte(v), &e) == nil {
				inMain[e.ID] = true
			}
		}
		for _, v := range staged {
			var e Entry
			if json.Unmarshal([]byte(v), &e) != nil {
				continue
			}
			if inMain[e.ID] {
				continue // crash window left it in both — main wins
			}
			e.Status = StatusPending
			s.client.LPush(ctx, qKey(ws, ses), string(mustMarshal(e)))
			n++
		}
		s.client.Del(ctx, dk)
	}
	return n
}

// Run drives delivery until ctx is done. Sessions are scanned each tick
// and delivered CONCURRENTLY (bounded semaphore): deliverOne blocks for
// up to DeliveryTimeout on a long turn, and a single sequential loop
// would let one slow session starve every other session's outbox — the
// starvation shape design 0050 exists to eliminate. The per-session lock
// serializes same-session delivery; cross-session delivery is parallel.
func (s *Service) Run(ctx context.Context, d Deliverer, tick time.Duration) {
	s.Recover(ctx)
	t := time.NewTicker(tick)
	defer t.Stop()
	sem := make(chan struct{}, 32)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			for _, pair := range s.sessions(ctx) {
				select {
				case <-ctx.Done():
					return
				case sem <- struct{}{}:
				}
				go func(ws, ses string) {
					defer func() { <-sem }()
					s.deliverOne(ctx, ws, ses, d)
				}(pair[0], pair[1])
			}
		}
	}
}

// deliverDetached runs the deliverer under a FRESH, timeout-bounded
// context — detached BY DESIGN (D3): delivery must survive the
// accepting request's cancellation; this helper is exactly the
// disconnect-immunity contract. It takes no context so no caller can
// accidentally re-couple delivery to a request lifetime.
func deliverDetached(d Deliverer, ws, ses string, e Entry) error {
	ctx, cancel := context.WithTimeout(context.Background(), DeliveryTimeout)
	defer cancel()
	return d(ctx, ws, ses, e)
}

// Dismiss removes an entry by ID (the queue UI's dismiss action).
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
			e.NextAttemptAt = time.Time{}
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
