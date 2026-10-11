// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package opencode

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- #1663: session-ID fidelity through the create path --------------------
//
// The issue alleged a one-character truncation between the agent store
// and the creation response. Persisted-bytes evidence (PR body) showed
// every surface byte-identical and the real failure was a caller-side
// mis-transcription met by a bare not-found. This pin is the STANDING
// GUARD for the propagation claim: sessions created through the real
// SessionCreate path at the boundary lengths (29/30/31 — the fleet's
// ids are 30; ±1 is the suspected clip window) must come back
// byte-for-byte identical to the store row, and the list/by-ID surfaces
// must serve the same bytes. Unlike the agentd suggestion pins (red
// first), this pin PASSES against current code by design — its job is
// to fail if any future change actually introduces the alleged clip.

// idFidelityFake is a store-faithful fake: POST /session mints the next
// id from a fixed boundary-length schedule, PERSISTS it, and every read
// surface (list, by-ID) serves the persisted bytes verbatim — the only
// authority for "what the store holds" is the fake's map.
type idFidelityFake struct {
	mu      sync.Mutex
	minted  []string
	store   map[string]bool
	nextIdx int
}

// schedule cycles minted id lengths through the boundary: 30 (the
// fleet's real length), 29 (a dropped char), 31 (a spurious char) —
// all syntactically valid under the seam's id pattern.
func (f *idFidelityFake) schedule() string {
	lengths := []int{30, 29, 31}
	n := lengths[f.nextIdx%len(lengths)]
	f.nextIdx++
	body := ""
	for i := 0; i < n-4; i++ {
		body += string(rune('a' + (i+f.nextIdx)%26))
	}
	return "ses_" + body
}

func (f *idFidelityFake) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/session":
			id := f.schedule()
			f.minted = append(f.minted, id)
			f.store[id] = true
			_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "title": "t"})
		case r.Method == http.MethodGet && r.URL.Path == "/session":
			list := make([]map[string]any, 0, len(f.minted))
			for _, id := range f.minted {
				list = append(list, map[string]any{"id": id, "title": "t"})
			}
			_ = json.NewEncoder(w).Encode(list)
		case r.Method == http.MethodGet && r.URL.Path != "/session":
			id := r.URL.Path[len("/session/"):]
			if !f.store[id] {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"name":"NotFoundError"}`))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "title": "t"})
		default:
			t.Errorf("idFidelityFake: unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

// TestSessionCreate_IDFidelity_BoundaryLengths loop-creates through the
// real seam path and diffs response-vs-store per session: the exact
// experiment the issue prescribed, against the exact boundary lengths.
func TestSessionCreate_IDFidelity_BoundaryLengths(t *testing.T) {
	f := &idFidelityFake{store: map[string]bool{}}
	c := newSeamServer(t, f.handler(t))

	const rounds = 45 // 15 full 30/29/31 cycles
	for i := 0; i < rounds; i++ {
		got, err := c.SessionCreate(context.Background(), "")
		require.NoError(t, err, "round %d", i)
		require.Len(t, f.minted, i+1, "round %d: exactly one store row minted", i)
		want := f.minted[i]
		assert.Equal(t, want, got,
			"round %d: response id must equal the store id byte-for-byte (store len %d, response len %d)",
			i, len(want), len(got))
	}

	// Boundary coverage is real, not incidental: the schedule must have
	// minted every boundary length and each must have round-tripped.
	seen := map[int]bool{}
	for _, id := range f.minted {
		seen[len(id)] = true
	}
	for _, want := range []int{29, 30, 31} {
		assert.True(t, seen[want], "schedule must exercise id length %d", want)
	}
}

// TestSessionCreate_IDFidelity_ListAndByID_SameBytes closes the loop:
// the list and by-ID surfaces serve the SAME bytes the creation
// response carried (the incident's cross-surface comparison, pinned).
func TestSessionCreate_IDFidelity_ListAndByID_SameBytes(t *testing.T) {
	f := &idFidelityFake{store: map[string]bool{}}
	c := newSeamServer(t, f.handler(t))

	var created []string
	for i := 0; i < 9; i++ {
		id, err := c.SessionCreate(context.Background(), "")
		require.NoError(t, err)
		created = append(created, id)
	}

	list, err := c.SessionList(context.Background())
	require.NoError(t, err)
	require.Len(t, list, len(created))
	for i, s := range list {
		assert.Equal(t, created[i], s.ID, "list row %d must carry the creation-response bytes", i)
	}

	for _, id := range created {
		exists, err := c.SessionExists(context.Background(), id)
		require.NoError(t, err)
		assert.True(t, exists, "by-ID probe must resolve the exact creation bytes for %s (len %d)", id, len(id))
	}

	// And the mistyped drop form must NOT resolve (the incident's other
	// half: a 29-char shadow of a 30-char id is a different id).
	dropped := created[0][:len(created[0])-1]
	exists, err := c.SessionExists(context.Background(), dropped)
	require.NoError(t, err)
	assert.False(t, exists, "a single-char-dropped id must not resolve to its parent")
}
