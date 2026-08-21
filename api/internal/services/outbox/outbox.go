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
// Delivery semantics: at-least-once for PROVEN-not-delivered failures
// only. An attempt whose outcome is UNKNOWN (send timeout mid-turn,
// transport cut mid-flight) is never blind-retried: opencode persists the
// user message before the turn starts, so the message is most likely in
// the transcript. Unknown outcomes move the entry to verifying and a
// Verifier (transcript check) decides: delivered → remove; absent →
// retry; inconclusive → recheck with backoff (#987 — the sent-once/
// delivered-3x incident class).
//
// Crash-safety: every multi-step transition is ordered so the worst case
// is DUPLICATE delivery (at-least-once, collapsed at render via
// clientMessageID), never LOSS:
//   - stage-out: RPUSH staging BEFORE LREM main (crash between → entry in
//     both → delivered possibly twice, never zero; Recover dedupes by ID)
//   - restore: reinsert main BEFORE LREM staging
//   - recover: LRANGE (no pop), dedupe against main by ID, then DEL
//   - recover status: staged entries requeue as VERIFYING — the outcome
//     of the interrupted send is unknown, same as a timeout
package outbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
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
	Attempts        int             `json:"attempts"`                 // definitive failures only
	LastAttemptAt   time.Time       `json:"lastAttemptAt,omitempty"`  // send-window start (verifier anchor)
	VerifyAttempts  int             `json:"verifyAttempts,omitempty"` // inconclusive verification passes
	NextAttemptAt   time.Time       `json:"nextAttemptAt,omitempty"`  // backoff gate (zero = now)
	LastError       string          `json:"lastError,omitempty"`
	Status          string          `json:"status"` // pending | delivering | verifying | error
}

// Status values.
const (
	StatusPending    = "pending"
	StatusDelivering = "delivering"
	StatusVerifying  = "verifying"
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

	// bookkeepingTimeout bounds post-delivery Redis cleanup (staging
	// LRem, state restore, lock release) on a context DETACHED from the
	// driver's cancellation. Generous and fixed: it guards against
	// unbounded hangs only — never coupled to DeliveryTimeout, which the
	// stress harness shrinks to 10ms.
	bookkeepingTimeout = 5 * time.Second
	// VerifyDelay is the initial delay before the first verification
	// pass after an ambiguous send (lets a racing persist land).
	VerifyDelay = 3 * time.Second
	// VerifyBackoff is the BASE delay between inconclusive verification
	// passes; it doubles per pass and caps at MaxVerifyBackoff.
	VerifyBackoff = 3 * time.Second
	// MaxVerifyBackoff caps the verification recheck delay.
	MaxVerifyBackoff = 2 * time.Minute
	// MaxVerifyAttempts bounds inconclusive passes before the entry
	// parks as error (agent unreachable for the full backoff span).
	MaxVerifyAttempts = 40
)

// ErrCapped is returned when the session's outbox is at Cap.
var ErrCapped = errors.New("session outbox is full")

// Duplicate is returned when the clientMessageID was already accepted.
// AcceptedID carries the original entry's ID.
type Duplicate struct{ AcceptedID string }

func (d *Duplicate) Error() string {
	return "duplicate clientMessageID: " + d.AcceptedID
}

// AmbiguousError marks a deliverer outcome as UNKNOWN: the send may or
// may not have persisted (timeout mid-turn, transport cut mid-flight).
// The deliverer wraps such errors via Ambiguous; the outbox responds by
// verifying against the agent transcript instead of blind-retrying
// (#987: blind retries re-POST the same text as a NEW message and
// duplicate the turn).
type AmbiguousError struct{ Err error }

func (a *AmbiguousError) Error() string { return "ambiguous delivery: " + a.Err.Error() }
func (a *AmbiguousError) Unwrap() error { return a.Err }

// Ambiguous wraps err as an unknown-outcome delivery failure.
func Ambiguous(err error) error {
	if err == nil {
		return nil
	}
	return &AmbiguousError{Err: err}
}

// Verdict is a verifier's decision about an ambiguous delivery attempt.
type Verdict int

const (
	// VerdictInconclusive: the transcript could not be read (agent
	// unreachable, page coverage incomplete) — recheck with backoff.
	VerdictInconclusive Verdict = iota
	// VerdictDelivered: the entry's text is present in the agent
	// transcript within the send window — the entry is complete.
	VerdictDelivered
	// VerdictAbsent: the transcript proves the message never persisted
	// — a definitive failure, safe to re-send.
	VerdictAbsent
)

// Verifier resolves ambiguous delivery attempts against agent state.
// It must be read-only and idempotent; the outbox calls it under the
// per-session lock with a timeout-bounded context.
type Verifier func(ctx context.Context, workspaceID, sessionID string, e Entry) Verdict

// DeliveredHook fires exactly once per entry on confirmed delivery —
// both the synchronous 2xx path and the verified path. SSE
// queue.update/sent, metering, and session-index recording ride it.
type DeliveredHook func(workspaceID, sessionID string, e Entry)

// Deliverer delivers one entry. The worker calls it with a detached,
// timeout-bounded context — it must not reference the accepting request.
type Deliverer func(ctx context.Context, workspaceID, sessionID string, e Entry) error

// Service is the outbox. All methods are safe for concurrent use; the
// Valkey client is borrowed (lifecycle owned by the caller).
type Service struct {
	client *redis.Client
	// verifier resolves ambiguous outcomes; nil degrades them to the
	// legacy retry path (dev/test wiring only — production always wires
	// the transcript verifier).
	verifier Verifier
	// onDelivered fires once per confirmed delivery (nil = no-op).
	onDelivered DeliveredHook
}

// New returns a Service backed by client, or nil if client is nil
// (outbox disabled — callers must nil-check).
func New(client *redis.Client) *Service {
	if client == nil {
		return nil
	}
	return &Service{client: client}
}

// SetVerifier wires the ambiguity resolver. Call before Run.
func (s *Service) SetVerifier(v Verifier) { s.verifier = v }

// SetOnDelivered wires the confirmed-delivery hook. Call before Run.
func (s *Service) SetOnDelivered(h DeliveredHook) { s.onDelivered = h }

func qKey(ws, ses string) string { return "outboxq:" + ws + ":" + ses }
func dKey(ws, ses string) string { return "outboxd:" + ws + ":" + ses }
func dedupeKey(ws, ses, cmid string) string {
	return "outboxdedupe:" + ws + ":" + ses + ":" + cmid
}
func lockKey(ws, ses string) string { return "outboxlock:" + ws + ":" + ses }

// acceptScript makes the cap check + push ATOMIC (stress-found #987):
// a check-then-act LLen/RPush pair lets concurrent accepts — across
// API replicas sharing Valkey — overshoot the session cap. Returns -1
// when capped (no push), else the new length.
var acceptScript = redis.NewScript(`
if redis.call("llen", KEYS[1]) >= tonumber(ARGV[1]) then
	return -1
end
redis.call("rpush", KEYS[1], ARGV[2])
return redis.call("llen", KEYS[1])
`)

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
	n, err := acceptScript.Run(ctx, s.client, []string{qKey(workspaceID, sessionID)}, Cap, string(b)).Int64()
	if err != nil {
		return nil, fmt.Errorf("outbox push: %w", err) // no marker
	}
	if n < 0 {
		return nil, ErrCapped // no marker written: a retry after drain succeeds
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

// deliverOne advances the first due entry for one session under the
// per-session lock: pending entries are sent, verifying entries are
// verified. Returns true if work was done. Backoff-gated entries
// (NextAttemptAt in the future) are skipped, not consumed.
func (s *Service) deliverOne(ctx context.Context, ws, ses string, d Deliverer) bool {
	token, ok := s.acquireLock(ctx, ws, ses)
	if !ok {
		return false // another worker (this replica or another) owns it
	}
	defer s.releaseLock(ctx, ws, ses, token)
	// Lock release must survive driver cancellation (Run abandons
	// in-flight workers on ctx.Done; a held lock would block the next
	// worker for LockTTL). Detached + bounded, same principle as bctx
	// below — which is canceled by the time deferred cleanup runs.
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		s.releaseLock(cleanupCtx, ws, ses, token)
	}()

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

	if e.Status == StatusVerifying {
		return s.verifyOne(ctx, ws, ses, qk, vals, idx, e)
	}

	// Stage-out (loss-proof ordering): RPUSH staging FIRST, then LREM
	// main. A crash between the two leaves the entry in BOTH — duplicate
	// delivery is possible (at-least-once), loss is not; Recover dedupes
	// by ID. Pre-delivery staging rides the driver ctx: cancellation here
	// simply means the entry was never staged out; the next worker picks
	// it up.
	staged := mustMarshal(e)
	s.client.RPush(ctx, dKey(ws, ses), staged)
	s.client.LRem(ctx, qk, 1, vals[idx])

	e.LastAttemptAt = now
	derr := deliverDetached(ctx, d, ws, ses, e)
	// Bookkeeping context: minted AFTER delivery, detached from driver
	// cancellation (Run abandons in-flight workers on ctx.Done; a detached
	// deliverer outlives it by design — D3), bounded so a wedged Redis
	// cannot pin the goroutine forever. Two load-bearing details:
	// (1) minted AFTER the deliverer returns — it may consume the whole
	//     DeliveryTimeout; a pre-minted ctx is expired on arrival;
	// (2) a FIXED generous budget, NOT DeliveryTimeout — the bound exists
	//     to prevent unbounded hangs, not to enforce delivery timing, and
	//     the stress harness legitimately shrinks DeliveryTimeout to 10ms
	//     (bookkeeping under contention needs far more than that).
	bctx, bcancel := context.WithTimeout(context.WithoutCancel(ctx), bookkeepingTimeout)
	defer bcancel()
	if derr == nil {
		s.client.LRem(bctx, dKey(ws, ses), 1, staged)
		s.fireOnDelivered(ws, ses, e)
		return true
	}
	var amb *AmbiguousError
	if errors.As(derr, &amb) && s.verifier != nil {
		// Unknown outcome: the send may have persisted (opencode
		// persists before the turn). Verify against the transcript —
		// never blind-retry (#987).
		e.Status = StatusVerifying
		e.VerifyAttempts = 0
		e.LastError = "ambiguous: " + amb.Err.Error()
		e.NextAttemptAt = now.Add(VerifyDelay)
		s.restoreStaged(bctx, qk, dKey(ws, ses), idx, staged, e)
		return true
	}
	// Failure: restore main FIRST (LInsert before the current occupant,
	// preserving order), then LREM staging — the mirrored crash window
	// duplicates rather than loses.
	e.Attempts++
	e.LastError = derr.Error()
	if e.Attempts >= MaxAttempts {
		e.Status = StatusError
	} else {
		e.Status = StatusPending
		e.NextAttemptAt = now.Add(backoffFor(e.Attempts))
	}
	s.restoreStaged(bctx, qk, dKey(ws, ses), idx, staged, e)
	return true
}

// verifyOne resolves a verifying entry in place (never staged out —
// verification is read-only; a crash mid-verify re-verifies idempotently).
func (s *Service) verifyOne(ctx context.Context, ws, ses, qk string, vals []string, idx int, e Entry) bool {
	now := time.Now().UTC()
	if s.verifier == nil {
		// No verifier wired (legacy/dev): degrade to the retry path —
		// better a bounded duplicate risk than an entry stranded in
		// verifying forever.
		e.Status = StatusPending
		e.Attempts++
		e.NextAttemptAt = now.Add(backoffFor(e.Attempts))
		s.client.LSet(ctx, qk, int64(idx), string(mustMarshal(e)))
		return true
	}
	switch s.verifier(ctx, ws, ses, e) {
	case VerdictDelivered:
		s.client.LRem(ctx, qk, 1, vals[idx])
		s.fireOnDelivered(ws, ses, e)
		return true
	case VerdictAbsent:
		e.Status = StatusPending
		e.Attempts++
		e.VerifyAttempts = 0
		e.LastError = ""
		if e.Attempts >= MaxAttempts {
			e.Status = StatusError
			e.LastError = "delivery confirmed absent; retry bound reached"
		} else {
			e.NextAttemptAt = now.Add(backoffFor(e.Attempts))
		}
		s.client.LSet(ctx, qk, int64(idx), string(mustMarshal(e)))
		return true
	default: // inconclusive — agent unreachable or page coverage incomplete
		e.VerifyAttempts++
		if e.VerifyAttempts >= MaxVerifyAttempts {
			e.Status = StatusError
			e.LastError = "delivery unverifiable: agent unreachable"
		} else {
			e.NextAttemptAt = now.Add(verifyBackoffFor(e.VerifyAttempts))
		}
		s.client.LSet(ctx, qk, int64(idx), string(mustMarshal(e)))
		return true
	}
}

// restoreStaged reinserts the (updated) entry into the main list at its
// position and removes it from staging — the failure/ambiguous restore
// ordering that duplicates rather than loses on a crash between steps.
func (s *Service) restoreStaged(ctx context.Context, qk, dk string, idx int, staged []byte, e Entry) {
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
	s.client.LRem(ctx, dk, 1, staged)
}

func (s *Service) fireOnDelivered(ws, ses string, e Entry) {
	if s.onDelivered != nil {
		s.onDelivered(ws, ses, e)
	}
}

// verifyBackoffFor returns the delay before verify pass n (n = inconclusive
// passes so far): base doubling per pass, capped.
func verifyBackoffFor(passes int) time.Duration {
	d := VerifyBackoff
	for i := 1; i < passes && d < MaxVerifyBackoff; i++ {
		d *= 2
	}
	if d > MaxVerifyBackoff {
		d = MaxVerifyBackoff
	}
	return d
}

// Recover requeues staged (delivering) entries left by a crash — run at
// worker start. LRANGE (no pop) + ID-dedupe against main, then DEL: the
// crash windows above can leave an entry in both lists; the ID check
// prevents Recover itself from duplicating. Requeued entries enter
// VERIFYING: the interrupted send's outcome is unknown — blind re-send
// is the #987 duplicate class.
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
			e.Status = StatusVerifying
			e.NextAttemptAt = time.Time{}
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
// Run drives delivery until ctx is canceled, THEN joins every worker
// it spawned before returning. The join is load-bearing: abandoning
// in-flight deliverOne goroutines on exit (a) leaks them past the
// caller's lifetime — observed as goroutine-stack races against
// subsequent package-var mutations in tests, and as unbounded teardown
// stalls behind detached deliveries in production shutdown — and (b)
// makes "Run returned" mean nothing. Workers quiesce promptly on a
// canceled ctx: pre-delivery Redis ops fail fast, in-flight deliveries
// complete under their detached contexts (D3) with detached-bounded
// bookkeeping, bounded by DeliveryTimeout + bookkeepingTimeout.
func (s *Service) Run(ctx context.Context, d Deliverer, tick time.Duration) {
	s.Recover(ctx)
	t := time.NewTicker(tick)
	defer t.Stop()
	sem := make(chan struct{}, 32)
	var workers sync.WaitGroup
	defer workers.Wait()
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
				workers.Add(1)
				go func(ws, ses string) {
					defer workers.Done()
					defer func() { <-sem }()
					s.deliverOne(ctx, ws, ses, d)
				}(pair[0], pair[1])
			}
		}
	}
}

// deliverDetached runs the deliverer under a context that is DERIVED
// from the caller's but explicitly detached from its cancellation
// (context.WithoutCancel), with its own delivery timeout. This is the
// D3 disconnect-immunity contract expressed with the stdlib's
// purpose-built primitive: delivery survives the accepting request's
// cancellation, bounded independently.
func deliverDetached(parent context.Context, d Deliverer, ws, ses string, e Entry) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), DeliveryTimeout)
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
			e.VerifyAttempts = 0
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
