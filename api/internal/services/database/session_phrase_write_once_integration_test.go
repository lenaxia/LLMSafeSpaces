// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package database

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/shortid"
)

// #1850 persistence-by-construction — the integration tier. Three
// pins, all red against pre-#1850 main:
//
//  1. WRITE-ONCE: migration 000044's trigger refuses any UPDATE that
//     changes or NULLs a minted short_id (the phrase is immutable once
//     minted; re-generation is hard-refused at the DB level).
//  2. PHRASE-BEARING BIRTH: every row-creating upsert births the row
//     WITH a phrase — a post-spawn write landing on an absent row
//     (row death, lost registration race) never settles phrase-less
//     and never needs a lazy backfill mint that re-issues identity.
//  3. ORIGINAL-PHRASE SURVIVAL: the charter's concurrent post-spawn
//     write + phrase read — a registration minted at creation, then
//     concurrent message/title/context writes and phrase reads, must
//     keep resolving the ORIGINAL phrase forever after.

func TestIntegration_ShortIDWriteOnce(t *testing.T) {
	svc, h := newIntegrationService(t)
	ctx := h.NewContext()
	wsID, sesID := "int-ws-1850-writeonce", "ses_"+h.ID()

	minted, err := svc.EnsureSessionRegistered(ctx, wsID, sesID, "opencode")
	require.NoError(t, err)
	require.True(t, shortid.IsShortIDForm(minted), "minted %q must be a phrase", minted)

	// Changing a minted phrase: REFUSED by the 000044 trigger.
	_, err = svc.DB.ExecContext(ctx,
		`UPDATE session_index SET short_id = $3 WHERE workspace_id = $1 AND session_id = $2`,
		wsID, sesID, "fallow-mist-rises")
	require.Error(t, err, "an UPDATE changing a minted short_id must be refused (write-once)")

	// NULL-ing a minted phrase: REFUSED too (a phrase may never be
	// un-minted back into the lazy-settlement window).
	_, err = svc.DB.ExecContext(ctx,
		`UPDATE session_index SET short_id = NULL WHERE workspace_id = $1 AND session_id = $2`,
		wsID, sesID)
	require.Error(t, err, "an UPDATE NULL-ing a minted short_id must be refused (write-once)")

	// Re-writing the SAME phrase: allowed (idempotent writes stay
	// legal; the trigger pins the value, not the write).
	_, err = svc.DB.ExecContext(ctx,
		`UPDATE session_index SET short_id = $3, updated_at = NOW() WHERE workspace_id = $1 AND session_id = $2`,
		wsID, sesID, minted)
	assert.NoError(t, err, "re-writing the identical phrase must pass")

	// The NULL-backfill (legacy rows) stays legal: seed a NULL row the
	// way pre-#1756 rows exist, then backfill it.
	legacy := "ses_legacy_" + h.ID()
	_, err = svc.DB.ExecContext(ctx,
		`INSERT INTO session_index (workspace_id, session_id, short_id, message_count, updated_at)
		 VALUES ($1, $2, NULL, 0, NOW())`, wsID, legacy)
	require.NoError(t, err)
	_, err = svc.DB.ExecContext(ctx,
		`UPDATE session_index SET short_id = $3, updated_at = NOW()
		 WHERE workspace_id = $1 AND session_id = $2 AND short_id IS NULL`,
		wsID, legacy, "amber-lynx-sprints")
	assert.NoError(t, err, "backfilling a NULL (legacy) short_id must stay legal")
}

func TestIntegration_RowCreatingUpsertsBirthWithPhrase(t *testing.T) {
	svc, h := newIntegrationService(t)
	ctx := h.NewContext()
	wsID := "int-ws-1850-birth"

	readPhrase := func(ses string) string {
		var got string
		require.NoError(t, svc.DB.QueryRowContext(ctx,
			`SELECT short_id FROM session_index WHERE workspace_id = $1 AND session_id = $2`,
			wsID, ses).Scan(&got))
		return got
	}

	// Each upsert births a row that must carry a canonical phrase
	// from birth — no phrase-less window, no lazy settlement.
	sesMsg := "ses_msg_" + h.ID()
	require.NoError(t, svc.UpsertSessionMessage(ctx, wsID, sesMsg, time.Now()))
	assert.True(t, shortid.IsShortIDForm(readPhrase(sesMsg)),
		"UpsertSessionMessage birthed a row without a phrase (lazy settlement)")

	sesTitle := "ses_title_" + h.ID()
	require.NoError(t, svc.UpsertSessionTitle(ctx, wsID, sesTitle, "t"))
	assert.True(t, shortid.IsShortIDForm(readPhrase(sesTitle)),
		"UpsertSessionTitle birthed a row without a phrase")

	sesCtx := "ses_ctx_" + h.ID()
	require.NoError(t, svc.UpsertSessionContextUsed(ctx, wsID, sesCtx, 99))
	assert.True(t, shortid.IsShortIDForm(readPhrase(sesCtx)),
		"UpsertSessionContextUsed birthed a row without a phrase")

	sesSeen := "ses_seen_" + h.ID()
	require.NoError(t, svc.UpdateSessionLastSeen(ctx, wsID, sesSeen))
	assert.True(t, shortid.IsShortIDForm(readPhrase(sesSeen)),
		"UpdateSessionLastSeen birthed a row without a phrase")

	// Review r1 finding 3: the two upserts that were unit-pinned on
	// SQL shape only get integration birth pins too — every birth
	// path, no tier gap.
	sesCount := "ses_count_" + h.ID()
	require.NoError(t, svc.UpsertSessionMessageCount(ctx, wsID, sesCount, 9))
	assert.True(t, shortid.IsShortIDForm(readPhrase(sesCount)),
		"UpsertSessionMessageCount birthed a row without a phrase")

	sesParent := "ses_parent_" + h.ID()
	require.NoError(t, svc.UpsertSessionParent(ctx, wsID, sesParent, "ses_parent_up"))
	assert.True(t, shortid.IsShortIDForm(readPhrase(sesParent)),
		"UpsertSessionParent birthed a row without a phrase")

	// And the existing-phrase-wins idempotency: after the registration
	// seam minted P1, a message upsert must NOT change it.
	sesReg := "ses_reg_" + h.ID()
	p1, err := svc.EnsureSessionRegistered(ctx, wsID, sesReg, "opencode")
	require.NoError(t, err)
	require.NoError(t, svc.UpsertSessionMessage(ctx, wsID, sesReg, time.Now()))
	require.NoError(t, svc.UpsertSessionTitle(ctx, wsID, sesReg, "t2"))
	assert.Equal(t, p1, readPhrase(sesReg), "a post-mint upsert changed the phrase (re-generation)")
}

// TestIntegration_OriginalPhraseSurvivesConcurrentPostSpawnWrites is
// the charter's red test: create_session's mint (the registration
// seam) runs concurrently with the post-spawn write storm (message
// SSEs, title fetch, context usage, last-seen) and phrase reads; the
// ORIGINAL phrase must survive every interleaving — pre-#1850, the
// phrase-less INSERT legs meant any write that won the row-birth race
// forced a later backfill re-mint (the observed re-phrase).
func TestIntegration_OriginalPhraseSurvivesConcurrentPostSpawnWrites(t *testing.T) {
	svc, h := newIntegrationService(t)
	ctx := h.NewContext()
	wsID, sesID := "int-ws-1850-race", "ses_"+h.ID()

	original, err := svc.EnsureSessionRegistered(ctx, wsID, sesID, "opencode")
	require.NoError(t, err)
	require.True(t, shortid.IsShortIDForm(original))

	const racers = 8
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, racers)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			switch i % 4 {
			case 0:
				errs[i] = svc.UpsertSessionMessage(ctx, wsID, sesID, time.Now())
			case 1:
				errs[i] = svc.UpsertSessionTitle(ctx, wsID, sesID, "worker")
			case 2:
				errs[i] = svc.UpsertSessionContextUsed(ctx, wsID, sesID, int64(1000+i))
			case 3:
				_, errs[i] = svc.FindSessionByShortID(ctx, wsID, original)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	for i := range errs {
		require.NoError(t, errs[i], "racer %d", i)
	}

	// The ORIGINAL phrase still resolves to the session, and the row
	// still carries it — identity continuity is the whole contract.
	got, err := svc.FindSessionByShortID(ctx, wsID, original)
	require.NoError(t, err)
	assert.Equal(t, sesID, got, "the original phrase must keep resolving to its session")

	var onRow string
	require.NoError(t, svc.DB.QueryRowContext(ctx,
		`SELECT short_id FROM session_index WHERE workspace_id = $1 AND session_id = $2`,
		wsID, sesID).Scan(&onRow))
	assert.Equal(t, original, onRow, "the row must still carry the original phrase")
}

// TestIntegration_ListSessionIndexCarriesCreatedAt pins the grace
// window's feed: ListSessionIndex must surface created_at so the reap
// guard can tell a fresh spawn-window row from an aged ghost.
func TestIntegration_ListSessionIndexCarriesCreatedAt(t *testing.T) {
	svc, h := newIntegrationService(t)
	ctx := h.NewContext()
	wsID, sesID := "int-ws-1850-created", "ses_"+h.ID()

	_, err := svc.EnsureSessionRegistered(ctx, wsID, sesID, "opencode")
	require.NoError(t, err)

	rows, err := svc.ListSessionIndex(ctx, wsID)
	require.NoError(t, err)
	found := false
	for _, r := range rows {
		if r.InternalID == sesID {
			found = true
			require.NotNil(t, r.CreatedAt, "ListSessionIndex must populate CreatedAt (the reap grace's feed)")
			assert.WithinDuration(t, time.Now(), *r.CreatedAt, 5*time.Minute)
		}
	}
	require.True(t, found, "the registered row must be listed")
}
