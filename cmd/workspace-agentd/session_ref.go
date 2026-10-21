// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// session_ref.go — design 0066 §4.4 acceptance + §4.6 did-you-mean on
// the agentd MCP tools. The precedence, exactly as the design states
// it:
//
//  1. exact short id, case-insensitive — the replica (with an on-miss
//     pull from the platform's registry);
//  2. exact legacy harness id — byte-equal passthrough, NEVER rewritten
//     and never probed first (an unindexed-but-live session must stay
//     addressable — the #1663 lesson; the seam's own 404 remains the
//     verdict and carries the suggestion clause);
//  3. alias — the session's current title OR the harness slug
//     ('sunny-wolf'), workspace-scoped, UNAMBIGUOUS ONLY: a name two
//     sessions share is a did-you-mean, never a guess.
//
// Id-shaped refs that match nothing pass through untouched (the seam
// arbitrates downstream — byte-identical to today). Alias-shaped refs
// that match nothing cannot be ids at all: they fail HERE with the
// did-you-mean clause instead of a bare seam 404.

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lenaxia/llmsafespaces/pkg/agent/opencode"
	"github.com/lenaxia/llmsafespaces/pkg/sessionref"
	"github.com/lenaxia/llmsafespaces/pkg/shortid"
)

// shortIDPullTTL bounds how often an unresolved short id may trigger a
// registry pull (the on-miss path must not hammer the platform when a
// caller loops on a typo).
const shortIDPullTTL = 30 * time.Second

var (
	shortIDPullMu   sync.Mutex
	shortIDLastPull time.Time
)

// pullSessionShortIDs fetches the workspace's registry from the
// platform and replaces the replica. Best-effort: errors return false
// (resolution degrades; nothing fails).
func pullSessionShortIDs(ctx context.Context) bool {
	client, saToken, workspaceID, err := podPlatformClient()
	if err != nil {
		return false
	}
	pullCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	entries, err := client.SessionShortIDRegistry(pullCtx, saToken, workspaceID)
	if err != nil {
		return false
	}
	m := make(map[string]string, len(entries))
	for _, e := range entries {
		m[e.ShortID] = e.SessionID
	}
	currentSessionShortIDs().putAll(m)
	return true
}

// pullSessionShortIDsRateLimited is the on-miss pull behind the TTL (the
// MCP ref-resolution path — the caller loops on a typo, so a fixed window
// bounds platform hammering). The EMISSION warm uses a different gate: the
// queue+worker+backoff in this file — see its doc for why the fixed TTL is
// wrong for the multi-session warm case.
func pullSessionShortIDsRateLimited(ctx context.Context) bool {
	shortIDPullMu.Lock()
	if time.Since(shortIDLastPull) < shortIDPullTTL {
		shortIDPullMu.Unlock()
		return false
	}
	shortIDLastPull = time.Now()
	shortIDPullMu.Unlock()
	return pullSessionShortIDs(ctx)
}

// Emission-path warm worker (#1817 r6 findings 1-4). The per-emission
// goroutine and its in-flight CAS could not bound a hot stream: every
// unresolved frame forked a goroutine, an overlapping id was dropped, and
// without the TTL a registry that never resolved an id pulled once per
// frame. This is ONE long-lived worker (sync.Once) fed by a bounded wake
// channel: misses only enqueue an id and signal; the worker coalesces a
// batch into one pull and applies per-id exponential backoff (1s → 30s)
// to ids the registry still does not resolve, so pulls stay bounded. Ids
// arriving while a pull is in flight stay pending and are pulled on the
// next loop — never dropped.
const (
	shortIDWarmBackoffMin = time.Second
	shortIDWarmBackoffMax = 30 * time.Second
	// Entries idle this long are pruned (a session deleted/renamed away
	// cannot pin a queue slot forever).
	shortIDWarmEntryTTL = 10 * time.Minute
	// Hard ceiling on the backoff map: prune the oldest entries past it.
	shortIDWarmMaxEntries = 4096
)

var (
	warmWorkerOnce   sync.Once
	warmWorkerCtx    context.Context
	warmWorkerCancel context.CancelFunc
	warmWorkerWg     sync.WaitGroup

	warmWake = make(chan struct{}, 1)
	warmMu   sync.Mutex
	// warmPending holds ids awaiting a pull. warmBackoff holds per-id
	// negative-cache state (absent = due now). Both are guarded by warmMu.
	warmPending = map[string]struct{}{}
	warmBackoff = map[string]shortIDWarmBackoff{}

	// shortIDWarmWorkerStarts is a test hook: incremented exactly once,
	// when the singleton worker goroutine starts.
	shortIDWarmWorkerStarts int32
)

type shortIDWarmBackoff struct {
	delay     time.Duration
	notBefore time.Time
	lastSeen  time.Time
}

// ensureShortIDWarmWorker starts the singleton worker. The first caller's
// context governs its lifetime (production passes bgCtx so shutdown joins
// it; the emission path falls back to a process-lifetime context).
func ensureShortIDWarmWorker(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	warmWorkerOnce.Do(func() {
		warmWorkerCtx, warmWorkerCancel = context.WithCancel(ctx)
		warmWorkerWg.Add(1)
		go func() {
			defer warmWorkerWg.Done()
			shortIDWarmWorker(warmWorkerCtx)
		}()
	})
}

// warmSessionShortIDsOnMiss records one unresolved emission id and wakes
// the worker. It NEVER spawns a goroutine and never blocks on the pull.
func warmSessionShortIDsOnMiss(harnessID string) {
	if harnessID == "" {
		return
	}
	ensureShortIDWarmWorker(context.Background())
	now := time.Now()
	warmMu.Lock()
	warmPending[harnessID] = struct{}{}
	if b, ok := warmBackoff[harnessID]; ok {
		b.lastSeen = now
		warmBackoff[harnessID] = b
	}
	warmMu.Unlock()
	select {
	case warmWake <- struct{}{}:
	default:
	}
}

func shortIDWarmWorker(ctx context.Context) {
	atomic.AddInt32(&shortIDWarmWorkerStarts, 1)
	for {
		select {
		case <-ctx.Done():
			return
		case <-warmWake:
		}
		for {
			if ctx.Err() != nil {
				return
			}
			due, wait := shortIDWarmDue()
			if len(due) == 0 {
				if wait <= 0 {
					break // nothing pending — park on warmWake
				}
				timer := time.NewTimer(wait)
				select {
				case <-ctx.Done():
					timer.Stop()
					return
				case <-warmWake:
					timer.Stop()
				case <-timer.C:
				}
				continue
			}
			_ = pullSessionShortIDs(ctx)
			shortIDWarmSettle(due)
		}
	}
}

// shortIDWarmDue takes the currently due ids out of pending and returns the
// wait until the earliest still-backed-off id (0 when none are waiting).
func shortIDWarmDue() ([]string, time.Duration) {
	now := time.Now()
	warmMu.Lock()
	defer warmMu.Unlock()
	shortIDWarmPruneLocked(now)
	var due []string
	var next time.Time
	for id := range warmPending {
		b, backedOff := warmBackoff[id]
		if !backedOff || !now.Before(b.notBefore) {
			due = append(due, id)
			delete(warmPending, id)
			continue
		}
		if next.IsZero() || b.notBefore.Before(next) {
			next = b.notBefore
		}
	}
	if len(due) == 0 && !next.IsZero() {
		return nil, time.Until(next)
	}
	return due, 0
}

// shortIDWarmSettle records the outcome of one pull for the ids it covered:
// resolved ids leave both maps; unresolved ids stay pending under a doubled
// (capped) backoff. The store is consulted before taking warmMu — the store's
// lock is never nested under the warm lock.
func shortIDWarmSettle(ids []string) {
	resolved := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if currentSessionShortIDs().shortIDFor(id) != "" {
			resolved[id] = struct{}{}
		}
	}
	now := time.Now()
	warmMu.Lock()
	defer warmMu.Unlock()
	for _, id := range ids {
		if _, ok := resolved[id]; ok {
			delete(warmPending, id)
			delete(warmBackoff, id)
			continue
		}
		b := warmBackoff[id]
		if b.delay <= 0 {
			b.delay = shortIDWarmBackoffMin
		} else {
			b.delay *= 2
			if b.delay > shortIDWarmBackoffMax {
				b.delay = shortIDWarmBackoffMax
			}
		}
		b.notBefore = now.Add(b.delay)
		if b.lastSeen.IsZero() {
			b.lastSeen = now
		}
		warmBackoff[id] = b
		warmPending[id] = struct{}{}
	}
	shortIDWarmPruneLocked(now)
}

// shortIDWarmPruneLocked drops idle entries and enforces the hard ceiling.
// Caller holds warmMu.
func shortIDWarmPruneLocked(now time.Time) {
	cutoff := now.Add(-shortIDWarmEntryTTL)
	for id, b := range warmBackoff {
		if b.lastSeen.Before(cutoff) {
			delete(warmBackoff, id)
			delete(warmPending, id)
		}
	}
	for len(warmBackoff) > shortIDWarmMaxEntries {
		var oldestID string
		var oldest time.Time
		for id, b := range warmBackoff {
			if oldestID == "" || b.lastSeen.Before(oldest) {
				oldestID, oldest = id, b.lastSeen
			}
		}
		delete(warmBackoff, oldestID)
		delete(warmPending, oldestID)
	}
}

// bootPullSessionShortIDs warms the replica at agentd start (§4.3
// population path (a)). Detached context, bounded, best-effort.
func bootPullSessionShortIDs(ctx context.Context) {
	pullCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	pullSessionShortIDs(pullCtx)
}

// emitID renders a canonical harness id for TOOL OUTPUT (#1756 §1):
// the phrase when the replica knows it, the harness id in the
// degraded window (replica cold). Output-only; resolution/inputs are
// unchanged.
func emitID(canonical string) string {
	if short := currentSessionShortIDs().shortIDFor(canonical); short != "" {
		return short
	}
	return canonical
}

// emitIDFields is emitID plus the degraded marker parity session_list
// rows already carry ("id_deprecated"): when the replica does not
// know the phrase (cold), the harness id ships FLAGGED on single-id
// results instead of silently raw (#1756 §1 consistency).
func emitIDFields(canonical string) (id string, degraded bool) {
	if short := currentSessionShortIDs().shortIDFor(canonical); short != "" {
		return short, false
	}
	return canonical, true
}

// deprecationNote returns the #1756 one-release input-deprecation
// clause for a ref addressed in a deprecated form ("" for canonical
// phrase/alias inputs).
func deprecationNote(ref string) string {
	if !shortid.IsDeprecatedInputForm(ref) {
		return ""
	}
	return " — DEPRECATED: you addressed this session by its full/hashed id; use the 3-word phrase (see session_list)"
}

// errSessionRef is the structured resolution failure returned to tool
// callers: the message carries the did-you-mean clause (the #1667
// style — suggest-only, never an auto-correct).
type errSessionRef struct {
	msg string
}

func (e *errSessionRef) Error() string { return e.msg }

// resolveSessionRef applies the §4.4 precedence for one ref. Legacy
// ids and unresolved id-shaped refs cost ZERO extra seam round trips
// (the passthrough is byte-identical and immediate); short ids cost a
// replica lookup (plus one rate-limited pull on miss); aliases cost
// the one bounded GET /session the tools mostly already pay.
func resolveSessionRef(ctx context.Context, password, ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", fmt.Errorf("session_id is required")
	}

	// 1. short id, case-insensitive — replica, then on-miss pull.
	if shortid.IsShortIDForm(ref) {
		if id, ok := currentSessionShortIDs().resolveShortID(ref); ok {
			return id, nil
		}
		if pullSessionShortIDsRateLimited(ctx) {
			if id, ok := currentSessionShortIDs().resolveShortID(ref); ok {
				return id, nil
			}
		}
		// Unresolved short id: id-shaped → pass through. The seam's
		// 404 (with the generalized clause) stays the verdict.
		return ref, nil
	}

	// 2. legacy harness id — byte-equal passthrough, never rewritten.
	if shortid.IsLegacyIDForm(ref) {
		return ref, nil
	}

	// 3. alias — title or slug, workspace-scoped, unambiguous only.
	sessions, err := seamClientWithPassword(password).SessionList(ctx)
	if err != nil {
		// The list fetch failed (transport/5xx): NEVER a suggestion
		// surface — degrade to passthrough and let the seam answer.
		return ref, nil
	}
	folded := strings.ToLower(strings.TrimSpace(ref))
	var matches []opencode.SessionSummary
	for _, s := range sessions {
		if strings.ToLower(strings.TrimSpace(s.Title)) == folded ||
			(s.Slug != "" && strings.ToLower(strings.TrimSpace(s.Slug)) == folded) {
			matches = append(matches, s)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0].ID, nil
	case 0:
		// No title/slug match. Charset-valid refs pass through
		// byte-identical: the seam's 404-class verdict downstream
		// appends the generalized clause (titles + slugs + short ids +
		// harness ids), and ids outside the ses_ shape (the
		// "ses-two" class) keep working. Refs the seam could never
		// accept anyway (spaces, dots, …) fail HERE with the clause —
		// a name was meant, and the caller gets the did-you-mean
		// instead of a client-side charset error.
		if shortid.IsSeamIDForm(ref) {
			return ref, nil
		}
		return "", &errSessionRef{msg: fmt.Sprintf(
			"session %q not found: no session in this workspace is titled (or slugged) %q%s",
			ref, ref, suggestRefClauseFrom(ref, sessions))}
	default:
		names := make([]string, 0, len(matches))
		for _, m := range matches {
			names = append(names, describeSuggestion(sessionref.Suggestion{
				SessionID: m.ID,
				ShortID:   currentSessionShortIDs().shortIDFor(m.ID),
				Title:     m.Title,
			}))
		}
		return "", &errSessionRef{msg: fmt.Sprintf(
			"session alias %q is ambiguous — %d sessions share it (%s); none was used (use the exact session id or short id)",
			ref, len(matches), strings.Join(names, ", "))}
	}
}

// candidateOf builds the §4.6 candidate for one live session: short
// id from the replica, title + slug live, last-active from the
// harness row.
func candidateOf(s opencode.SessionSummary) sessionref.Candidate {
	last := time.Time{}
	if s.Time.Updated > 0 {
		last = time.UnixMilli(s.Time.Updated).UTC()
	}
	return sessionref.Candidate{
		SessionID:  s.ID,
		ShortID:    currentSessionShortIDs().shortIDFor(s.ID),
		Title:      s.Title,
		Slug:       s.Slug,
		LastActive: last,
	}
}

// suggestRefClauseFrom ranks the live sessions against a failed ref
// and formats the did-you-mean clause (the #1667 text style,
// generalized: candidates carry short ids, titles, and slugs).
func suggestRefClauseFrom(requested string, sessions []opencode.SessionSummary) string {
	cands := make([]sessionref.Candidate, 0, len(sessions))
	for _, s := range sessions {
		cands = append(cands, candidateOf(s))
	}
	ranked := sessionref.Rank(requested, cands)
	switch len(ranked) {
	case 0:
		return ""
	case 1:
		return fmt.Sprintf(" — did you mean %s? re-copy the exact id from session_list / session_metadata",
			describeSuggestion(ranked[0]))
	default:
		names := make([]string, 0, len(ranked))
		for _, r := range ranked {
			names = append(names, describeSuggestion(r))
		}
		return fmt.Sprintf(" — %d sessions are near %q (%s); none was used — re-copy the exact id from session_list / session_metadata",
			len(ranked), requested, strings.Join(names, ", "))
	}
}

// describeSuggestion renders one candidate for the clause: the short
// id when known (the canonical address), the harness id always (the
// re-copy target), and the title/slug when present.
func describeSuggestion(s sessionref.Suggestion) string {
	label := s.SessionID
	if s.ShortID != "" {
		label = s.ShortID + " (" + s.SessionID + ")"
	}
	if s.Title != "" {
		label += " — " + s.Title
	} else if s.SessionID == "" && s.ShortID != "" {
		label = s.ShortID
	}
	return label
}
