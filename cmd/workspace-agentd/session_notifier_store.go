// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// session_notifier_store.go — session idle-subscriptions v1, §4.1: the
// durable subscription registry. Three persisted document classes in
// ONE JSON file at /sandbox-runtime/session-subscriptions.json (the
// session-shortids replica's storage class):
//
//   - rows:    SubscriptionRow edges, canonical-id keyed (phrase input
//     is resolved at the MCP boundary; §7.8 tolerates the cold-replica
//     window — this file never stores phrases);
//   - latches: per-broadcaster {LastDerivedBusy, SawBusy} — the §4.2
//     saw-busy latch, persisted so an agentd restart can still settle
//     a busy episode it observed before dying (§7.3);
//   - outbox:  pendingNotification intents — written BEFORE the POST
//     and cleared on success, the at-least-once half of ruling 4.
//
// Persistence discipline: atomic write + fsync (temp file, fsync,
// rename — the shortids mechanics WITH the fsync the outbox contract
// wants: a hard death between the enqueue and the POST must leave the
// intent on disk, never a torn half-file). The store is advisory in
// the same posture as the shortids replica: an absent or unparseable
// file boots empty (subscriptions re-register; the failure mode is a
// lost wake-up, never a wrong one), and every mutator best-effort
// persists — a write failure keeps the in-memory truth and logs.
//
// Guards that need graph knowledge live at this layer as pure data
// predicates (wouldCreateCycle, subscriberCount) so the notifier
// (session_notifier.go) can enforce them with typed, loud refusals.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// defaultSessionSubscriptionsPath is the memory-backed registry file
// (same emptyDir class as the shortids replica; survives agentd
// restarts, dies with the pod — re-subscription is the recovery).
const defaultSessionSubscriptionsPath = "/sandbox-runtime/session-subscriptions.json"

// maxSubscribersPerBroadcaster is the §4.1 registration cap (default
// 32): the storm backstop. The exclusions and the cycle refusal are
// the primary defenses; this bound makes the worst fan-out finite even
// under an abusive caller.
const maxSubscribersPerBroadcaster = 32

// Subscription event kinds (ruling 1: ERROR settles notify too, with
// distinct wording — the kinds select WHICH settles a row wakes on).
const (
	subscriptionEventIdle  = "idle"
	subscriptionEventError = "error"
)

// validSubscriptionEvents is the closed v1 vocabulary (§5: accepted
// values "idle", "error"; anything else is a loud refusal).
var validSubscriptionEvents = map[string]bool{
	subscriptionEventIdle:  true,
	subscriptionEventError: true,
}

// SubscriptionRow is one subscription edge: Subscriber watches
// Broadcaster and is woken on the settles named in Events. Canonical
// harness ids both ends (the addressing surface resolves phrases; this
// row never carries one).
type SubscriptionRow struct {
	Broadcaster string    `json:"broadcaster"`
	Subscriber  string    `json:"subscriber"`
	Events      []string  `json:"events"`
	CreatedAt   time.Time `json:"created_at"`
}

// sessionLatch is the §4.2 per-broadcaster latch. SawBusy is the
// attribution requirement: unknown→idle never notifies (a fresh
// projection after reseed/restart must not read as a settle), while a
// busy we observed — in this generation OR persisted across a restart
// — settles. LastDerivedBusy is the busy edge the latch last saw.
type sessionLatch struct {
	LastDerivedBusy bool `json:"last_derived_busy"`
	SawBusy         bool `json:"saw_busy"`
}

// pendingNotification is one outbox intent (ruling 4, option A): the
// settle fired, the POST has not yet been acknowledged. Cleared on
// delivery success; retried once at boot by the delivery task, then
// dropped loudly. Duplicates are tolerated by contract.
type pendingNotification struct {
	Broadcaster string    `json:"broadcaster"`
	Subscriber  string    `json:"subscriber"`
	Event       string    `json:"event"`
	EnqueuedAt  time.Time `json:"enqueued_at"`
}

// subscriptionFile is the on-disk document (all three classes at once
// so one atomic rename moves them together).
type subscriptionFile struct {
	Rows    []*SubscriptionRow       `json:"rows"`
	Latches map[string]*sessionLatch `json:"latches"`
	Outbox  []*pendingNotification   `json:"outbox"`
	Version int                      `json:"version"`
}

// sessionSubscriptionStore owns the registry. All methods are nil-safe
// (a nil store is an empty registry — subscription calls degrade to
// no-ops, tracking degrades to latch-less, never panics).
type sessionSubscriptionStore struct {
	mu      sync.Mutex
	path    string
	edges   []*SubscriptionRow
	latches map[string]*sessionLatch
	outbox  []*pendingNotification
}

// newSessionSubscriptionStore loads (best-effort) and returns the
// store. An empty path selects the production default (task 4's main
// wiring); an absent, unparseable, or crash-torn file boots empty: the
// atomic-or-absent contract means the only readable states are
// complete ones.
func newSessionSubscriptionStore(path string) *sessionSubscriptionStore {
	if path == "" {
		path = defaultSessionSubscriptionsPath
	}
	s := &sessionSubscriptionStore{
		path:    path,
		latches: map[string]*sessionLatch{},
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return s
	}
	var f subscriptionFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return s // unparseable → empty; the advisory posture
	}
	for _, row := range f.Rows {
		if row == nil || row.Broadcaster == "" || row.Subscriber == "" {
			continue
		}
		s.edges = append(s.edges, row)
	}
	for id, l := range f.Latches {
		if id != "" && l != nil {
			s.latches[id] = l
		}
	}
	s.outbox = f.Outbox
	return s
}

// validateSubscriptionEvents normalizes the requested event set: an
// empty request defaults to BOTH kinds (ruling 1's belt-and-suspenders
// — an erroring worker must wake its manager by default, or the
// manager polls forever on a dead turn); explicit lists are validated
// against the closed vocabulary and deduplicated in stable order.
func validateSubscriptionEvents(events []string) ([]string, error) {
	if len(events) == 0 {
		return []string{subscriptionEventIdle, subscriptionEventError}, nil
	}
	seen := map[string]bool{}
	var out []string
	for _, ev := range events {
		if !validSubscriptionEvents[ev] {
			return nil, fmt.Errorf("unsupported subscription event %q (accepted: %q, %q)",
				ev, subscriptionEventIdle, subscriptionEventError)
		}
		if !seen[ev] {
			seen[ev] = true
			out = append(out, ev)
		}
	}
	return out, nil
}

// subscribe upserts one edge (idempotent re-subscribe updates the
// event set; §5) and persists. Caller-side guards (self, cycle, cap,
// subtask) are the notifier's job — this is the pure data op.
func (s *sessionSubscriptionStore) subscribe(broadcaster, subscriber string, events []string, now time.Time) *SubscriptionRow {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, row := range s.edges {
		if row.Broadcaster == broadcaster && row.Subscriber == subscriber {
			row.Events = events
			_ = s.persistLocked()
			return row
		}
	}
	row := &SubscriptionRow{Broadcaster: broadcaster, Subscriber: subscriber, Events: events, CreatedAt: now}
	s.edges = append(s.edges, row)
	_ = s.persistLocked()
	return row
}

// unsubscribe removes one edge; reports whether a row was removed
// (idempotent absent edge → false).
func (s *sessionSubscriptionStore) unsubscribe(broadcaster, subscriber string) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, row := range s.edges {
		if row.Broadcaster == broadcaster && row.Subscriber == subscriber {
			s.edges = append(s.edges[:i], s.edges[i+1:]...)
			_ = s.persistLocked()
			return true
		}
	}
	return false
}

// allRows returns a private copy of every edge (callers mutate freely).
func (s *sessionSubscriptionStore) allRows() []SubscriptionRow {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]SubscriptionRow, 0, len(s.edges))
	for _, row := range s.edges {
		out = append(out, *row)
	}
	return out
}

// rowsForBroadcaster returns the wake fan-out set (a copy).
func (s *sessionSubscriptionStore) rowsForBroadcaster(broadcaster string) []SubscriptionRow {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []SubscriptionRow
	for _, row := range s.edges {
		if row.Broadcaster == broadcaster {
			out = append(out, *row)
		}
	}
	return out
}

// rowsForSubscriber returns the caller's outbound edges (a copy) —
// list_subscriptions' backing read (§5).
func (s *sessionSubscriptionStore) rowsForSubscriber(subscriber string) []SubscriptionRow {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []SubscriptionRow
	for _, row := range s.edges {
		if row.Subscriber == subscriber {
			out = append(out, *row)
		}
	}
	return out
}

// hasRow reports the exact edge's presence (the cap's idempotent
// re-subscribe carve-out).
func (s *sessionSubscriptionStore) hasRow(broadcaster, subscriber string) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, row := range s.edges {
		if row.Broadcaster == broadcaster && row.Subscriber == subscriber {
			return true
		}
	}
	return false
}

// subscriberCount counts the broadcaster's current fan-out.
func (s *sessionSubscriptionStore) subscriberCount(broadcaster string) int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int
	for _, row := range s.edges {
		if row.Broadcaster == broadcaster {
			n++
		}
	}
	return n
}

// wouldCreateCycle answers whether adding the edge broadcaster→
// subscriber closes a wake cycle (§7.2). Wake propagation flows
// broadcaster → subscriber (a settle wakes the subscriber; ITS settle
// then wakes its own subscribers): the new edge is safe iff the
// broadcaster is NOT already reachable from the subscriber along
// existing edges. The graph is tiny (≤32 per broadcaster); a DFS with
// a visited set is exact and bounded. A↔B is the shortest catch; the
// trivial self edge is refused earlier with its own error.
func (s *sessionSubscriptionStore) wouldCreateCycle(broadcaster, subscriber string) bool {
	if s == nil || broadcaster == subscriber {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	subs := map[string][]string{}
	for _, row := range s.edges {
		subs[row.Broadcaster] = append(subs[row.Broadcaster], row.Subscriber)
	}
	visited := map[string]bool{}
	stack := []string{subscriber}
	for len(stack) > 0 {
		id := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if id == broadcaster {
			return true
		}
		if visited[id] {
			continue
		}
		visited[id] = true
		stack = append(stack, subs[id]...)
	}
	return false
}

// latchFor returns the broadcaster's latch (zero value when absent).
func (s *sessionSubscriptionStore) latchFor(broadcaster string) sessionLatch {
	if s == nil {
		return sessionLatch{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if l := s.latches[broadcaster]; l != nil {
		return *l
	}
	return sessionLatch{}
}

// setLatch persists the broadcaster's latch, skipping the disk write
// when nothing changed (a hot frame stream must not fsync per frame —
// only per transition).
func (s *sessionSubscriptionStore) setLatch(broadcaster string, l sessionLatch) {
	if s == nil || broadcaster == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if cur := s.latches[broadcaster]; cur != nil && *cur == l {
		return
	}
	cp := l
	s.latches[broadcaster] = &cp
	_ = s.persistLocked()
}

// enqueue records a notification intent BEFORE delivery (ruling 4:
// at-least-once; the outbox write precedes the POST).
func (s *sessionSubscriptionStore) enqueue(p pendingNotification) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.outbox = append(s.outbox, &p)
	_ = s.persistLocked()
}

// dismiss removes the first intent equal to p (the delivery-success
// clear). Reports whether anything was removed.
func (s *sessionSubscriptionStore) dismiss(p pendingNotification) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, q := range s.outbox {
		if q != nil && *q == p {
			s.outbox = append(s.outbox[:i], s.outbox[i+1:]...)
			_ = s.persistLocked()
			return true
		}
	}
	return false
}

// outboxLen reports the pending intent count (tests + boot retry).
func (s *sessionSubscriptionStore) outboxLen() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.outbox)
}

// pendingNotifications snapshots the outbox (values, in order) — the
// boot retry-once drain's input (RetryOutbox): what the previous
// generation left durable is retried exactly once, then dropped.
func (s *sessionSubscriptionStore) pendingNotifications() []pendingNotification {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]pendingNotification, 0, len(s.outbox))
	for _, p := range s.outbox {
		if p != nil {
			out = append(out, *p)
		}
	}
	return out
}

// pruneSessions drops every row, latch, and outbox intent touching a
// session the predicate calls gone (§4.1 lazy cleanup: broadcaster or
// subscriber archived/deleted → the edge is dead weight; notifications
// to archived subscribers bounce anyway). Returns the dropped row
// count.
func (s *sessionSubscriptionStore) pruneSessions(gone func(string) bool) int {
	if s == nil || gone == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.edges[:0]
	dropped := 0
	for _, row := range s.edges {
		if gone(row.Broadcaster) || gone(row.Subscriber) {
			dropped++
			continue
		}
		kept = append(kept, row)
	}
	s.edges = kept
	for id := range s.latches {
		if gone(id) {
			delete(s.latches, id)
		}
	}
	keptOut := s.outbox[:0]
	for _, p := range s.outbox {
		if p != nil && (gone(p.Broadcaster) || gone(p.Subscriber)) {
			continue
		}
		keptOut = append(keptOut, p)
	}
	s.outbox = keptOut
	if dropped > 0 {
		_ = s.persistLocked()
	}
	return dropped
}

// persistLocked writes the whole document atomically: temp file,
// fsync, rename, directory fsync (the seq-cursor's full durability
// discipline, cursor.go — the sessionstate module's own crash-safety
// pattern; caller holds mu). The file fsync is the outbox's crash
// contract — the enqueue is only durable once the bytes hit the file
// before the rename makes them visible; the directory fsync makes the
// rename itself durable.
func (s *sessionSubscriptionStore) persistLocked() error {
	raw, err := json.MarshalIndent(subscriptionFile{
		Rows:    s.edges,
		Latches: s.latches,
		Outbox:  s.outbox,
		Version: 1,
	}, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(raw); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil { // the crash-safety half
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return err
	}
	// Directory fsync: make the rename itself durable (cursor.go:99-107).
	d, err := os.Open(filepath.Dir(s.path))
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}
