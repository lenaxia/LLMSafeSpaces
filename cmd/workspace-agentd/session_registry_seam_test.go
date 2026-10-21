// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// The registry→replica seam, crossed end to end against the EXACT
// payload shapes the API handlers emit (#1756 post-union review,
// missing-test 1): the registry pull's sessionID is the harness id
// (InternalID) with the phrase as shortID — after the pull, the
// replica must resolve phrase→harness in both directions — and the
// archived set's ids are harness ids, so the list filter drops rows
// keyed by the harness id, never the phrase.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRegistryPullToReplicaSeam(t *testing.T) {
	platform := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/internal/v1/session-registry":
			// Byte-shape the PodSessionRegistryHandler emits: sessionID
			// = InternalID (harness), shortID = phrase; unminted rows
			// skipped. A phrase in sessionID would poison the replica
			// phrase→phrase (finding 2).
			_, _ = w.Write([]byte(`{"sessions":[
				{"sessionID":"ses_live_1","shortID":"amber-falcon-glides"},
				{"sessionID":"ses_live_2","shortID":"calm-otter-naps"}
			]}`))
		case "/internal/v1/session-archived":
			// Byte-shape PodSessionArchiveHandler.ArchivedSet emits:
			// HARNESS ids (finding 3) — phrases here never match the
			// list rows and archived sessions leak back (#1735).
			_, _ = w.Write([]byte(`{"archived":["ses_arch_gone"]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer platform.Close()

	token := filepath.Join(t.TempDir(), "sa-token")
	require.NoError(t, os.WriteFile(token, []byte("tok"), 0o600))
	t.Setenv("WORKSPACE_ID", "ws-seam")
	t.Setenv("LLMSAFESPACE_API_URL", platform.URL)
	t.Setenv("LLMSAFESPACE_BOOTSTRAP_TOKEN_FILE", token)

	store := withShortIDStore(t)
	archivedSetCacheVal, archivedSetCacheAt, archivedSetCacheFetchedAt = nil, time.Time{}, time.Time{}
	t.Cleanup(func() {
		archivedSetCacheVal, archivedSetCacheAt, archivedSetCacheFetchedAt = nil, time.Time{}, time.Time{}
	})

	require.True(t, pullSessionShortIDs(context.Background()), "the pull must succeed against the live-shaped platform")

	// Replica resolves phrase→harness (case-insensitive) and reverse.
	id, ok := store.resolveShortID("Amber-Falcon-Glides")
	require.True(t, ok, "the pulled phrase must resolve")
	assert.Equal(t, "ses_live_1", id, "sessionID carries the harness id — a phrase here would resolve phrase→phrase")
	assert.Equal(t, "amber-falcon-glides", store.shortIDFor("ses_live_1"))

	// The archived filter consumes the SAME id domain: the set's
	// harness ids drop list rows keyed by harness ids.
	body := []byte(`[{"id":"ses_arch_gone","title":"Gone"},{"id":"ses_live_1","title":"Live"}]`)
	got := filterArchivedFromSessionList(context.Background(), body)
	assert.Contains(t, got, `"ses_live_1"`, "the live row survives")
	assert.Contains(t, got, "Live")
	assert.NotContains(t, got, `"ses_arch_gone"`, "the archived row must drop — the set and the rows share the harness-id domain")
	assert.NotContains(t, got, "Gone")
}
