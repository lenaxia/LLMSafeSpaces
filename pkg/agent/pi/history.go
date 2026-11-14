// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package pi

// history.go — GetHistory/GetHistoryPage over get_entries and its
// durable since-cursor. "An entry id works as a durable cursor: pass
// the last entry id you have seen as since to get only entries strictly
// after it, even across client restarts" (rpc-commands.md) — the native
// pagination primitive pi has, and the one the acceptance criteria pin
// for GetHistoryPage (#971: full-transcript decode just to slice a page
// was ~95% of session-load latency).
//
// The replay-ring (design 0069's events-since shape): per session the
// adapter keeps the cursor (the last entry id whose translation it
// holds) plus a bounded window of the newest translated messages.
// Steady-state page reads fetch ONLY the delta (get_entries since the
// cursor) — one small round trip — and slice the window. Cold reads
// (first touch, window eviction, cursor invalidation, or a window
// shorter than the requested page) fall back to one full get_entries.
//
// Correctness rule: the delta is appended only when the active branch
// did not MOVE underneath us. get_entries returns leafId every call;
// if it differs from the delta's last entry id, the session was forked
// or rewound and the window is rebuilt from a full fetch (a since
// cursor that matches no entry id is a success:false response — the
// same rebuild path).

import (
	"context"
	"errors"
	"fmt"

	"github.com/lenaxia/llmsafespaces/pkg/session"
)

// ringCapacity is the per-session translated-message window. Bounded
// so a pathological session cannot balloon the API pod; pages larger
// than the window take the full-fetch path.
const ringCapacity = 512

// ringMaxSessions bounds the ring map (adapter-lifetime storage); the
// evict-all-on-overflow choice is deliberately dumb — rings are pure
// caches, correctness never depends on them.
const ringMaxSessions = 256

// historyRing is one session's cursor + newest-message window.
type historyRing struct {
	cursor string // last entry id translated into msgs
	leafID string // active-branch leaf the window reflects
	msgs   []session.Message
}

// fetchEntries issues one get_entries call and returns its data.
func fetchEntries(ctx context.Context, c SessionClient, since string) (*piEntriesData, error) {
	data, err := call(ctx, c, cmdGetEntries(since))
	if err != nil {
		return nil, err
	}
	if data == nil {
		return &piEntriesData{}, nil
	}
	res := piEntriesData{}
	if err := decodeInto(data, &res); err != nil {
		return nil, fmt.Errorf("pi get_entries: decode: %w", err)
	}
	return &res, nil
}

// errCursorStale marks a since the session no longer knows (fork/rewind
// or tree surgery): the caller rebuilds from a full fetch.
var errCursorStale = errors.New("pi get_entries: cursor stale")

// fetchEntriesSince wraps fetchEntries with the cursor-stale
// classification (success:false on an unknown since — the command's
// documented contract).
func fetchEntriesSince(ctx context.Context, c SessionClient, since string) (*piEntriesData, error) {
	if since == "" {
		return fetchEntries(ctx, c, "")
	}
	res, err := fetchEntries(ctx, c, since)
	if err != nil {
		var ce *piCallError
		if errors.As(err, &ce) {
			return nil, errCursorStale
		}
		return nil, err
	}
	return res, nil
}

// leafOf resolves the entries data's leaf as a string (null for an
// empty session).
func leafOf(d *piEntriesData) string {
	if d == nil || d.LeafID == nil {
		return ""
	}
	return *d.LeafID
}

// fullHistory loads every entry and translates the active branch,
// returning the messages and the ring state that matches them.
func fullHistory(ctx context.Context, c SessionClient) ([]session.Message, string, string, error) {
	d, err := fetchEntries(ctx, c, "")
	if err != nil {
		return nil, "", "", err
	}
	leaf := leafOf(d)
	msgs := translateEntries(d.Entries, leaf)
	cursor := leaf
	if len(d.Entries) > 0 {
		cursor = d.Entries[len(d.Entries)-1].ID
	}
	return msgs, cursor, leaf, nil
}

// historyPage is the shared GetHistory/GetHistoryPage core. limit<=0
// means the full transcript (GetHistory).
func (a *Adapter) historyPage(ctx context.Context, c SessionClient, workspaceID, sessionID string, limit int) ([]session.Message, error) {
	if limit <= 0 {
		msgs, cursor, leaf, err := fullHistory(ctx, c)
		if err != nil {
			return nil, err
		}
		a.storeRingLoaded(ringKey{workspace: workspaceID, session: sessionID}, trimRing(&historyRing{cursor: cursor, leafID: leaf, msgs: msgs}))
		return msgs, nil
	}

	key := ringKey{workspace: workspaceID, session: sessionID}
	if r, ok := a.loadRing(key); ok {
		if msgs, fresh, err := a.ringDelta(ctx, c, r); err == nil {
			fresh = trimRing(fresh)
			a.storeRingLoaded(key, fresh)
			return newestLimit(msgs, limit), nil
		} else if !errors.Is(err, errCursorStale) {
			return nil, err
		}
		// Stale cursor: rebuild below.
	}

	msgs, cursor, leaf, err := fullHistory(ctx, c)
	if err != nil {
		return nil, err
	}
	a.storeRingLoaded(key, trimRing(&historyRing{cursor: cursor, leafID: leaf, msgs: msgs}))
	return newestLimit(msgs, limit), nil
}

// ringDelta fetches only the entries after the ring's cursor and
// advances the window. The returned ring is the advanced copy; the
// stored one is replaced by the caller on success only.
func (a *Adapter) ringDelta(ctx context.Context, c SessionClient, r *historyRing) ([]session.Message, *historyRing, error) {
	d, err := fetchEntriesSince(ctx, c, r.cursor)
	if err != nil {
		return nil, nil, err
	}
	leaf := leafOf(d)
	if len(d.Entries) == 0 {
		// No movement — unless the active leaf moved without new
		// entries visible after our cursor (tree surgery): rebuild.
		if leaf != "" && leaf != r.leafID {
			return nil, nil, errCursorStale
		}
		return r.msgs, r, nil
	}
	last := d.Entries[len(d.Entries)-1].ID
	if leaf != "" && leaf != last {
		// The delta's tail is not the active leaf (fork/rewind moved
		// the branch): the window cannot be extended linearly.
		return nil, nil, errCursorStale
	}
	// The delta entries are, by construction, the newest appends on
	// the active branch (strictly after the cursor, append order).
	// ToolResults inside the delta fuse with calls inside the delta;
	// a call straddling the cursor edge stays pending in the window
	// until the next cold rebuild — the same mid-turn-read posture as
	// the opencode seam.
	fresh := &historyRing{cursor: last, leafID: leaf, msgs: append(append([]session.Message(nil), r.msgs...), translateLinear(d.Entries)...)}
	if len(fresh.msgs) > ringCapacity {
		fresh.msgs = fresh.msgs[len(fresh.msgs)-ringCapacity:]
	}
	return fresh.msgs, fresh, nil
}

// translateLinear translates entries in append order WITHOUT tree
// resolution (the delta path: entries strictly after the cursor are, by
// construction, the newest appends on the active branch).
func translateLinear(entries []piEntry) []session.Message {
	results := make(map[string]*piAgentMessage)
	for i := range entries {
		if entries[i].Message != nil && entries[i].Message.Role == "toolResult" && entries[i].Message.ToolCallID != "" {
			results[entries[i].Message.ToolCallID] = entries[i].Message
		}
	}
	var msgs []session.Message
	for i := range entries {
		if m, ok := translateEntry(&entries[i], entries[i].Message, results); ok {
			msgs = append(msgs, m)
		}
	}
	return msgs
}

type ringKey struct{ workspace, session string }

func (a *Adapter) loadRing(key ringKey) (*historyRing, bool) {
	a.ringsMu.Lock()
	defer a.ringsMu.Unlock()
	r, ok := a.rings[key]
	return r, ok
}

// storeRingLoaded installs a ring under key, with the bounded-map
// eviction (dumb full evict — rings are pure caches).
func (a *Adapter) storeRingLoaded(key ringKey, r *historyRing) {
	a.ringsMu.Lock()
	defer a.ringsMu.Unlock()
	if len(a.rings) >= ringMaxSessions {
		a.rings = make(map[ringKey]*historyRing, ringMaxSessions)
	}
	a.rings[key] = r
}

// trimRing bounds a ring's window to ringCapacity (the full-history
// path keeps the newest window only).
func trimRing(r *historyRing) *historyRing {
	if len(r.msgs) > ringCapacity {
		r.msgs = r.msgs[len(r.msgs)-ringCapacity:]
	}
	return r
}

// newestLimit returns the newest limit messages, oldest-first within
// the slice (the GetHistoryPage contract).
func newestLimit(msgs []session.Message, limit int) []session.Message {
	if limit <= 0 || len(msgs) <= limit {
		return msgs
	}
	out := make([]session.Message, limit)
	copy(out, msgs[len(msgs)-limit:])
	return out
}
