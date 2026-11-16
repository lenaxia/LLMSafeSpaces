// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

// wrapup_settle_internal_test.go — the wrapup-watermark-diff story's pins
// for advanceWatermarkTx (epic wrap-up review, low-level design §4's settle
// SQL; high-level D4). In-package because the primitive is unexported: its
// only production caller is RecordWrapUpVerdict (the wrapup-gap-recursion
// story's verdict verb) — these pins land the contract first, so the
// sibling lands against a pinned seam.
//
// Born-red mapping (README-LLM rule 0):
//
//   - [0] the settle: last_wrapped_at takes the wrap-up row's MINT time
//     (created_at — captured at mint, NOT close time), wrapup_count
//     increments, wrapup_task_id clears (RED before: no primitive).
//   - [0] the guard: a replayed close (second call, same ids) and a raced
//     close (the epic's pointer names a different row) are CLEAN NO-OPS —
//     0 rows, ledger and watermark untouched.
//   - [0] both-or-neither: the advance rides the caller's transaction — a
//     rollback leaves BOTH the row's →done and the settle unwritten; a
//     commit lands BOTH. (The full failpoint pin at the settle SQL inside
//     the verdict transaction lands with wrapup-gap-recursion's
//     RecordWrapUpVerdict; this is the primitive-level atomicity pin.)
package board

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAdvanceWatermarkTx(t *testing.T) {
	pool := wrapupReviewerPool(t)
	defer pool.Close()
	ctx := context.Background()

	// Deterministic uuids (raw-seed shape, the arbitration-test precedent).
	const owner = "wrapup-settle-user"
	const wsID = "55555555-5555-4555-8555-555555555551"
	const projID = "55555555-5555-4555-8555-555555555552"
	const epicID = "55555555-5555-4555-8555-555555555553"
	const wrapupID = "55555555-5555-4555-8555-555555555554"
	const otherID = "55555555-5555-4555-8555-555555555555"

	seed := func(t *testing.T) (mintAt time.Time) {
		t.Helper()
		_, err := pool.Exec(ctx, `TRUNCATE TABLE board_events, board_transitions, board_dependencies, board_sweeps, tasks, epics, projects, workspaces CASCADE`)
		require.NoError(t, err, "truncate")
		_, err = pool.Exec(ctx, `
			INSERT INTO users (id, username, email, password_hash, active, role)
			VALUES ($1, $1, 'settle@example.com', 'hash', true, 'user')
			ON CONFLICT (id) DO NOTHING`, owner)
		require.NoError(t, err, "users row")
		_, err = pool.Exec(ctx, `
			INSERT INTO workspaces (id, user_id, name, namespace, created_at, updated_at)
			VALUES ($1, $2, 'settle-ws', 'default', now(), now())`, wsID, owner)
		require.NoError(t, err, "workspace")
		_, err = pool.Exec(ctx, `
			INSERT INTO projects (id, short_id, user_id, name, target_workspace_id, worker_slots, tags)
			VALUES ($1::uuid, 'prj-settle01', $2, 'settle-proj', $3::uuid, 2, '{}')`, projID, owner, wsID)
		require.NoError(t, err, "project")
		// The epic seeds pointer-less (the in-flight pointer's FK requires
		// the wrap-up row to exist first; the pointer is set below).
		_, err = pool.Exec(ctx, `
			INSERT INTO epics (id, project_id, short_id, title, project_name, wrap_up_review, status)
			VALUES ($1::uuid, $2::uuid, 'epc-settle01', 'settle epic', 'settle-proj', true, 'active')`,
			epicID, projID)
		require.NoError(t, err, "epic")
		// The wrap-up row: minted 90 seconds in the past — MINT time is
		// what the watermark must take, and backdating makes "mint, not
		// now" observable (a close-time settle would read ~now()).
		var mint time.Time
		err = pool.QueryRow(ctx, `
			INSERT INTO tasks (id, project_id, epic_id, short_id, title, description, stage, kind,
			                   project_name, epic_name, tags, acceptance_criteria, created_at)
			VALUES ($1::uuid, $2::uuid, $3::uuid, 'tsk-settle01', 'Epic wrap-up review: settle epic', '',
			        'in_review', 'wrapup', 'settle-proj', 'settle epic', '{wrapup}', '[]', now() - interval '90 seconds')
			RETURNING created_at`, wrapupID, projID, epicID).Scan(&mint)
		require.NoError(t, err, "wrap-up row")
		_, err = pool.Exec(ctx, `
			INSERT INTO tasks (id, project_id, epic_id, short_id, title, description, stage, kind,
			                   project_name, epic_name, tags, acceptance_criteria)
			VALUES ($1::uuid, $2::uuid, $3::uuid, 'tsk-settle02', 'other', '', 'in_review', 'wrapup',
			        'settle-proj', 'settle epic', '{wrapup}', '[]')`, otherID, projID, epicID)
		require.NoError(t, err, "other wrap-up row")
		_, err = pool.Exec(ctx, `UPDATE epics SET wrapup_task_id=$2 WHERE id=$1`, epicID, wrapupID)
		require.NoError(t, err, "in-flight pointer")
		return mint
	}

	type epicSettle struct {
		LastWrapped *time.Time
		Count       int
		Pointer     *string
	}
	readEpic := func() epicSettle {
		t.Helper()
		var st epicSettle
		require.NoError(t, pool.QueryRow(ctx,
			`SELECT last_wrapped_at, wrapup_count, wrapup_task_id::text FROM epics WHERE id=$1`, epicID).
			Scan(&st.LastWrapped, &st.Count, &st.Pointer))
		return st
	}

	t.Run("settle advances to mint time", func(t *testing.T) {
		mintAt := seed(t)
		tx, err := pool.Begin(ctx)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback(ctx) }() // leak-proof on assertion failure
		settled, err := advanceWatermarkTx(ctx, tx, epicID, wrapupID)
		require.NoError(t, err)
		require.True(t, settled, "the owning close settles")
		require.NoError(t, tx.Commit(ctx))

		st := readEpic()
		require.NotNil(t, st.LastWrapped, "last_wrapped_at set")
		assert.WithinDuration(t, mintAt, *st.LastWrapped, time.Second,
			"D4: the watermark takes the wrap-up row's MINT time (created_at, captured at mint) — NOT close time")
		assert.Equal(t, 1, st.Count, "wrapup_count increments")
		assert.Nil(t, st.Pointer, "the in-flight pointer clears")
	})

	t.Run("replayed close is a clean no-op", func(t *testing.T) {
		mintAt := seed(t)
		tx, err := pool.Begin(ctx)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback(ctx) }() // leak-proof on assertion failure
		_, err = advanceWatermarkTx(ctx, tx, epicID, wrapupID)
		require.NoError(t, err)
		require.NoError(t, tx.Commit(ctx))

		// Replay: the same close lands again (client retry after commit).
		tx2, err := pool.Begin(ctx)
		require.NoError(t, err)
		defer func() { _ = tx2.Rollback(ctx) }()
		settled, err := advanceWatermarkTx(ctx, tx2, epicID, wrapupID)
		require.NoError(t, err)
		assert.False(t, settled, "the pointer guard makes the replay 0 rows")
		require.NoError(t, tx2.Commit(ctx))

		st := readEpic()
		assert.Equal(t, 1, st.Count, "no double increment")
		assert.WithinDuration(t, mintAt, *st.LastWrapped, time.Second, "no double advance")
	})

	t.Run("raced close on a foreign pointer is a clean no-op", func(t *testing.T) {
		seed(t)
		// The epic's in-flight pointer names the OTHER row (a contender
		// won the mint race): the stale advance for wrapupID must refuse
		// cleanly, and exactly one settle — the pointer's owner — lands.
		_, err := pool.Exec(ctx, `UPDATE epics SET wrapup_task_id=$2 WHERE id=$1`, epicID, otherID)
		require.NoError(t, err)

		tx, err := pool.Begin(ctx)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback(ctx) }() // leak-proof on assertion failure
		settled, err := advanceWatermarkTx(ctx, tx, epicID, wrapupID)
		require.NoError(t, err)
		assert.False(t, settled, "the stale contender refuses cleanly")
		require.NoError(t, tx.Commit(ctx))

		st := readEpic()
		assert.Equal(t, 0, st.Count, "the refused close touched no ledger")
		require.NotNil(t, st.Pointer, "the pointer survived the stale contender")

		tx2, err := pool.Begin(ctx)
		require.NoError(t, err)
		defer func() { _ = tx2.Rollback(ctx) }()
		settled, err = advanceWatermarkTx(ctx, tx2, epicID, otherID)
		require.NoError(t, err)
		require.True(t, settled, "the pointer's owner settles")
		require.NoError(t, tx2.Commit(ctx))

		st = readEpic()
		assert.Equal(t, 1, st.Count, "exactly one settle won")
		assert.Nil(t, st.Pointer)
	})

	t.Run("crash between row-done and settle is both-or-neither", func(t *testing.T) {
		seed(t)

		// ROLLBACK leg: the row's →done and the advance ride ONE
		// transaction; aborting it leaves neither observable.
		tx, err := pool.Begin(ctx)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback(ctx) }() // leak-proof on assertion failure
		_, err = tx.Exec(ctx,
			`UPDATE tasks SET stage='done', claim_token=NULL, lease_expires_at=NULL WHERE id=$1 AND stage='in_review'`, wrapupID)
		require.NoError(t, err)
		settled, err := advanceWatermarkTx(ctx, tx, epicID, wrapupID)
		require.NoError(t, err)
		require.True(t, settled)
		require.NoError(t, tx.Rollback(ctx))

		var stage string
		require.NoError(t, pool.QueryRow(ctx, `SELECT stage FROM tasks WHERE id=$1`, wrapupID).Scan(&stage))
		assert.Equal(t, StageInReview, stage, "rollback unwound the row's done")
		st := readEpic()
		assert.Nil(t, st.LastWrapped, "rollback unwound the watermark advance")
		assert.Equal(t, 0, st.Count)
		require.NotNil(t, st.Pointer, "the pointer survived the aborted close")

		// COMMIT leg: both land together.
		tx2, err := pool.Begin(ctx)
		require.NoError(t, err)
		defer func() { _ = tx2.Rollback(ctx) }()
		_, err = tx2.Exec(ctx,
			`UPDATE tasks SET stage='done', claim_token=NULL, lease_expires_at=NULL WHERE id=$1 AND stage='in_review'`, wrapupID)
		require.NoError(t, err)
		_, err = advanceWatermarkTx(ctx, tx2, epicID, wrapupID)
		require.NoError(t, err)
		require.NoError(t, tx2.Commit(ctx))

		require.NoError(t, pool.QueryRow(ctx, `SELECT stage FROM tasks WHERE id=$1`, wrapupID).Scan(&stage))
		assert.Equal(t, StageDone, stage)
		st = readEpic()
		assert.NotNil(t, st.LastWrapped, "commit landed the advance")
		assert.Equal(t, 1, st.Count)
		assert.Nil(t, st.Pointer)
	})
}
