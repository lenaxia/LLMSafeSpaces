// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// session_shortids.go — design 0066 §4.3: the agentd-side REPLICA of
// the short-id registry. A plain JSON map at
// /sandbox-runtime/session-shortids.json (the dev-preview-headers
// pattern: memory-backed emptyDir class, 0600, atomic rename, no fsync
// — the storage is tmpfs and the state rebuilds from the control
// plane).
//
// One owner: the API's registration seam is the only place a short id
// is born (§4.2 rule 1). This store only ever RECEIVES entries — boot
// pull, on-miss pull, and create_session's register-one round trip.
// It is advisory by contract: any divergence degrades resolution to
// did-you-mean, never corrupts the mapping; loss of the file degrades
// to on-miss pulls; loss of the platform degrades to harness ids.
// Nothing is ever unaddressable.

import (
	"encoding/json"
	"os"
	"sync"
	"sync/atomic"

	"github.com/lenaxia/llmsafespaces/pkg/shortid"
)

// defaultSessionShortIDsPath is the memory-backed replica file.
const defaultSessionShortIDsPath = "/sandbox-runtime/session-shortids.json"

// sessionShortIDsAtomic holds the process-wide store; tests swap it
// (the devPreviewHeadersAtomic precedent).
var sessionShortIDsAtomic atomic.Value // *sessionShortIDStore

func init() {
	sessionShortIDsAtomic.Store(newSessionShortIDStore(defaultSessionShortIDsPath))
}

func currentSessionShortIDs() *sessionShortIDStore {
	return sessionShortIDsAtomic.Load().(*sessionShortIDStore)
}

// sessionShortIDStore is the bidirectional replica map. All methods
// are nil-safe (a nil store is an empty replica — resolution degrades
// to legacy ids and did-you-mean).
type sessionShortIDStore struct {
	mu        sync.RWMutex
	path      string
	byShort   map[string]string // lower(short) -> session id
	bySession map[string]string // session id -> short
}

func newSessionShortIDStore(path string) *sessionShortIDStore {
	s := &sessionShortIDStore{
		path:      path,
		byShort:   map[string]string{},
		bySession: map[string]string{},
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return s
	}
	var m map[string]string
	if err := json.Unmarshal(raw, &m); err != nil {
		return s // unparseable → empty; the boot pull rebuilds
	}
	for k, v := range m {
		s.install(k, v)
	}
	return s
}

// install validates + indexes one entry (lowercased short key).
func (s *sessionShortIDStore) install(shortID, sessionID string) bool {
	shortID = shortid.Normalize(shortID)
	if shortID == "" || sessionID == "" || !shortid.IsShortIDForm(shortID) {
		return false
	}
	s.byShort[shortID] = sessionID
	s.bySession[sessionID] = shortID
	return true
}

// resolveShortID maps a (any-case) short id to the harness id.
func (s *sessionShortIDStore) resolveShortID(shortID string) (string, bool) {
	if s == nil {
		return "", false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	id, ok := s.byShort[shortid.Normalize(shortID)]
	return id, ok
}

// shortIDFor maps a harness id to its short id ("" when unknown).
func (s *sessionShortIDStore) shortIDFor(sessionID string) string {
	if s == nil {
		return ""
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.bySession[sessionID]
}

// len reports the replica's size (test/diagnostic).
func (s *sessionShortIDStore) size() int {
	if s == nil {
		return 0
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.byShort)
}

// put learns one mapping (create_session register round trip) and
// persists. Best-effort: a persist failure keeps the in-memory entry
// (the replica is advisory; the next putAll rewrites the file valid).
func (s *sessionShortIDStore) put(shortID, sessionID string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.install(shortID, sessionID) {
		return
	}
	_ = s.persistLocked()
}

// putAll applies a registry pull as a MERGE (upsert) — a pull never
// removes entries (#1850). The pull's snapshot can predate a
// create_session's register round-trip (the warm worker's in-flight
// pull at spawn): a replace-style apply wiped the fresh phrase→harness
// mapping the moment the racing pull landed, degrading the spawned
// session's emission to the harness id + id_deprecated and breaking
// phrase-addressed sends until a later pull re-learned what the store
// already knew. The registry's append-only contract (a phrase is
// immutable once minted; rows are only born — the single-mint-owner
// seam) makes merge the faithful apply: an entry ABSENT from a pull is
// a stale snapshot or a deleted session — the latter keeps resolving
// to a dead harness id and fails the SessionExists probe cleanly with
// the did-you-mean clause, never a hard error (the replica-is-advisory
// posture this file's header pins).
func (s *sessionShortIDStore) putAll(entries map[string]string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for shortID, sessionID := range entries {
		s.install(shortID, sessionID)
	}
	_ = s.persistLocked()
}

// persistLocked writes the canonical short→session map atomically
// (temp file + rename, 0600 — the dev-preview-headers mechanics).
// Caller holds mu.
func (s *sessionShortIDStore) persistLocked() error {
	raw, err := json.Marshal(s.byShort)
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
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// forget removes a session's mapping entirely — the #1882
// delete-invalidates-the-replica seam. The replica is append-only by
// design (#1850: a pull never removes entries), so pre-#1882 a dead
// session's phrase kept resolving to its dead harness id until the
// next full pod restart — live evidence in the issue: session_metadata
// on a hard-deleted carrier still resolved the phrase. Deletes are
// the ONE event that may retract: the phrase's session is gone, the
// mapping is a lie, and did-you-mean degradation is strictly better
// than silent dead-id resolution. Only agentd's own CONFIRMED delete
// legs call this (deleteOpencodeSession's true return); platform-side
// row removals (reconcile reaps) leave the replica warm — advisory
// posture unchanged, boot pull rebuilds.
func (s *sessionShortIDStore) forget(sessionID string) {
	if s == nil || sessionID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	shortID, ok := s.bySession[sessionID]
	if !ok {
		return
	}
	delete(s.bySession, sessionID)
	// Phrases are write-once per session, so byShort[shortID] is this
	// session's; the equality guard is one line of insurance against
	// ever re-pointing a live entry.
	if s.byShort[shortID] == sessionID {
		delete(s.byShort, shortID)
	}
	_ = s.persistLocked()
}
