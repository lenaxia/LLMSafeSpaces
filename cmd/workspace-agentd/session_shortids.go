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

// putAll replaces the whole replica (boot/on-miss pull) and persists.
func (s *sessionShortIDStore) putAll(entries map[string]string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byShort = map[string]string{}
	s.bySession = map[string]string{}
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
