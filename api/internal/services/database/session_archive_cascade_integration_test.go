// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package database

// #1755: archive/unarchive cascades over the parentID subtree — the
// SEMANTICS against real Postgres. The sqlmock pins assert the SQL
// text; this suite asserts the sticky marker contract:
//
//   - archive(target): target archived individually; UNARCHIVED
//     descendants archived with archived_by_cascade=true. Already-
//     archived descendants are skipped — individual intent survives
//     the parent round-trip.
//   - unarchive(target): target released; descendants released ONLY
//     when cascade-held (archived_by_cascade). Individually-archived
//     children stay archived.
//   - The recursive CTE uses UNION (dedupe) so a corrupted parentID
//     cycle terminates — tree-by-construction, pinned anyway.
//   - The update-only 404 contract is preserved: an absent target
//     stamps nothing and creates no phantom row.

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/testharness"
)

const cascadeWS = "int-ws-archive-cascade"

// seedCascadeTree builds P → C → G (parent, child, grandchild) plus an
// unrelated top-level row L, all unarchived.
func seedCascadeTree(t *testing.T, h *testharness.Harness) {
	t.Helper()
	pool := h.Pool()
	_, _ = pool.Exec(context.Background(), "DELETE FROM session_index WHERE workspace_id = $1", cascadeWS)
	_, err := pool.Exec(context.Background(), `
		INSERT INTO session_index (workspace_id, session_id, title) VALUES
		('`+cascadeWS+`', 'ses_P', 'Parent worker'),
		('`+cascadeWS+`', 'ses_C', 'Child probe'),
		('`+cascadeWS+`', 'ses_G', 'Grandchild explore'),
		('`+cascadeWS+`', 'ses_L', 'Unrelated live')
	`)
	require.NoError(t, err, "seed rows")
	_, err = pool.Exec(context.Background(), `
		UPDATE session_index SET parent_session_id = 'ses_P'
		WHERE workspace_id = $1 AND session_id = 'ses_C'
	`, cascadeWS)
	require.NoError(t, err, "link child")
	_, err = pool.Exec(context.Background(), `
		UPDATE session_index SET parent_session_id = 'ses_C'
		WHERE workspace_id = $1 AND session_id = 'ses_G'
	`, cascadeWS)
	require.NoError(t, err, "link grandchild")
}

type cascadeRow struct {
	Archived          bool
	ArchivedByCascade bool
}

func readCascadeRow(t *testing.T, h *testharness.Harness, sessionID string) cascadeRow {
	t.Helper()
	var row cascadeRow
	err := h.Pool().QueryRow(context.Background(),
		`SELECT archived, archived_by_cascade FROM session_index
		 WHERE workspace_id = $1 AND session_id = $2`, cascadeWS, sessionID).
		Scan(&row.Archived, &row.ArchivedByCascade)
	require.NoError(t, err, "read row %s", sessionID)
	return row
}

// TestIntegration_SessionArchiveCascade_ArchiveStampsDescendantsSticky:
// archiving the parent archives the whole subtree — the parent is
// individually held (marker=false), its descendants cascade-held
// (marker=true), and the unrelated row is untouched.
func TestIntegration_SessionArchiveCascade_ArchiveStampsDescendantsSticky(t *testing.T) {
	h := testharness.New(t)
	defer h.Close()
	seedCascadeTree(t, h)
	ctx := h.NewContext()

	svc := &Service{DB: h.SQLDB()}
	require.NoError(t, svc.SetSessionArchivedStatus(ctx, cascadeWS, "ses_P", true))

	assert.Equal(t, cascadeRow{Archived: true, ArchivedByCascade: false}, readCascadeRow(t, h, "ses_P"),
		"target archives individually")
	assert.Equal(t, cascadeRow{Archived: true, ArchivedByCascade: true}, readCascadeRow(t, h, "ses_C"),
		"child archives cascade-held")
	assert.Equal(t, cascadeRow{Archived: true, ArchivedByCascade: true}, readCascadeRow(t, h, "ses_G"),
		"grandchild archives cascade-held (recursive, not just direct children)")
	assert.Equal(t, cascadeRow{Archived: false, ArchivedByCascade: false}, readCascadeRow(t, h, "ses_L"),
		"unrelated row untouched")
}

// TestIntegration_SessionArchiveCascade_UnarchiveReleasesCascadeHeld:
// unarchiving the parent releases the subtree it archived.
func TestIntegration_SessionArchiveCascade_UnarchiveReleasesCascadeHeld(t *testing.T) {
	h := testharness.New(t)
	defer h.Close()
	seedCascadeTree(t, h)
	ctx := h.NewContext()

	svc := &Service{DB: h.SQLDB()}
	require.NoError(t, svc.SetSessionArchivedStatus(ctx, cascadeWS, "ses_P", true))
	require.NoError(t, svc.SetSessionArchivedStatus(ctx, cascadeWS, "ses_P", false))

	for _, id := range []string{"ses_P", "ses_C", "ses_G"} {
		assert.Equal(t, cascadeRow{Archived: false, ArchivedByCascade: false}, readCascadeRow(t, h, id),
			"%s released with marker cleared", id)
	}
}

// TestIntegration_SessionArchiveCascade_StickyIndividualChild: a child
// archived INDIVIDUALLY before the parent cascade stays archived when
// the parent round-trips — individual intent survives.
func TestIntegration_SessionArchiveCascade_StickyIndividualChild(t *testing.T) {
	h := testharness.New(t)
	defer h.Close()
	seedCascadeTree(t, h)
	ctx := h.NewContext()

	svc := &Service{DB: h.SQLDB()}
	// Child archived individually first (the pre-existing-archived-row
	// posture: marker reads false — conservative).
	require.NoError(t, svc.SetSessionArchivedStatus(ctx, cascadeWS, "ses_C", true))
	// Parent cascade: C is already archived → skipped, NOT re-stamped.
	require.NoError(t, svc.SetSessionArchivedStatus(ctx, cascadeWS, "ses_P", true))
	assert.Equal(t, cascadeRow{Archived: true, ArchivedByCascade: false}, readCascadeRow(t, h, "ses_C"),
		"individually-held child keeps marker=false across the parent archive")
	assert.Equal(t, cascadeRow{Archived: true, ArchivedByCascade: true}, readCascadeRow(t, h, "ses_G"),
		"grandchild still rides the cascade")

	// Parent unarchive: C is not cascade-held → stays archived.
	require.NoError(t, svc.SetSessionArchivedStatus(ctx, cascadeWS, "ses_P", false))
	assert.Equal(t, cascadeRow{Archived: false, ArchivedByCascade: false}, readCascadeRow(t, h, "ses_P"))
	assert.Equal(t, cascadeRow{Archived: true, ArchivedByCascade: false}, readCascadeRow(t, h, "ses_C"),
		"sticky: individually-archived child survives the parent unarchive")
	assert.Equal(t, cascadeRow{Archived: false, ArchivedByCascade: false}, readCascadeRow(t, h, "ses_G"),
		"cascade-held grandchild released")
}

// TestIntegration_SessionArchiveCascade_UnarchivedDescendantNotTouched:
// the archive cascade only flips UNARCHIVED descendants; the unarchive
// cascade only releases cascade-held ones — a descendant the user
// unarchived individually mid-cycle keeps its state (idempotent guard,
// no resurrect churn).
func TestIntegration_SessionArchiveCascade_UnarchivedDescendantNotTouched(t *testing.T) {
	h := testharness.New(t)
	defer h.Close()
	seedCascadeTree(t, h)
	ctx := h.NewContext()

	svc := &Service{DB: h.SQLDB()}
	require.NoError(t, svc.SetSessionArchivedStatus(ctx, cascadeWS, "ses_P", true))
	// User unarchives the child individually (direct target → released).
	require.NoError(t, svc.SetSessionArchivedStatus(ctx, cascadeWS, "ses_C", false))
	// Re-archive the parent: the child (currently unarchived) is
	// swept up again as cascade-held.
	require.NoError(t, svc.SetSessionArchivedStatus(ctx, cascadeWS, "ses_P", true))
	assert.Equal(t, cascadeRow{Archived: true, ArchivedByCascade: true}, readCascadeRow(t, h, "ses_C"),
		"re-archived child re-stamped cascade-held")
}

// TestIntegration_SessionArchiveCascade_LeafTarget: archiving a leaf
// (no descendants) degrades to the single-row behavior.
func TestIntegration_SessionArchiveCascade_LeafTarget(t *testing.T) {
	h := testharness.New(t)
	defer h.Close()
	seedCascadeTree(t, h)
	ctx := h.NewContext()

	svc := &Service{DB: h.SQLDB()}
	require.NoError(t, svc.SetSessionArchivedStatus(ctx, cascadeWS, "ses_L", true))
	assert.Equal(t, cascadeRow{Archived: true, ArchivedByCascade: false}, readCascadeRow(t, h, "ses_L"))
	for _, id := range []string{"ses_P", "ses_C", "ses_G"} {
		assert.Equal(t, cascadeRow{Archived: false, ArchivedByCascade: false}, readCascadeRow(t, h, id),
			"%s untouched by a leaf archive", id)
	}
}

// TestIntegration_SessionArchiveCascade_AbsentRow404NoPhantom: the
// update-only 404 contract survives the cascade — an absent target
// errors AND stamps nothing (no phantom row, no partial cascade).
func TestIntegration_SessionArchiveCascade_AbsentRow404NoPhantom(t *testing.T) {
	h := testharness.New(t)
	defer h.Close()
	seedCascadeTree(t, h)
	pool := h.Pool()
	ctx := h.NewContext()

	svc := &Service{DB: h.SQLDB()}
	err := svc.SetSessionArchivedStatus(ctx, cascadeWS, "ses-ghost", true)
	require.Error(t, err, "absent target must 404")

	var n int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM session_index WHERE workspace_id = $1 AND archived`,
		cascadeWS).Scan(&n))
	assert.Zero(t, n, "no row was stamped or created by the refused call")
}

// TestIntegration_SessionArchiveCascade_CycleCorruptionTerminates:
// parentID is a tree by construction, but a corrupted cycle (P→C→P)
// must not hang the statement — the UNION dedupe terminates the walk.
func TestIntegration_SessionArchiveCascade_CycleCorruptionTerminates(t *testing.T) {
	h := testharness.New(t)
	defer h.Close()
	seedCascadeTree(t, h)
	pool := h.Pool()
	ctx := h.NewContext()

	// Corrupt: make P's own parent its child — a 2-cycle.
	_, err := pool.Exec(ctx, `
		UPDATE session_index SET parent_session_id = 'ses_C'
		WHERE workspace_id = $1 AND session_id = 'ses_P'
	`, cascadeWS)
	require.NoError(t, err, "corrupt parent link")

	svc := &Service{DB: h.SQLDB()}
	require.NoError(t, svc.SetSessionArchivedStatus(ctx, cascadeWS, "ses_P", true),
		"the recursive walk terminates on a corrupted cycle")

	assert.Equal(t, cascadeRow{Archived: true, ArchivedByCascade: false}, readCascadeRow(t, h, "ses_P"))
	assert.Equal(t, cascadeRow{Archived: true, ArchivedByCascade: true}, readCascadeRow(t, h, "ses_C"))
	assert.Equal(t, cascadeRow{Archived: true, ArchivedByCascade: true}, readCascadeRow(t, h, "ses_G"))
}

// TestIntegration_SessionArchiveCascade_GranularityEdge: documented
// single-marker granularity — in P→C→G, archiving C individually
// stamps G cascade-held; a later parent round-trip releases G even
// though C (individually held) stays archived. Individual intent
// survives; cascade-of-cascade attribution does not (would need
// per-holder tracking — judged complexity for the edge).
func TestIntegration_SessionArchiveCascade_GranularityEdge(t *testing.T) {
	h := testharness.New(t)
	defer h.Close()
	seedCascadeTree(t, h)
	ctx := h.NewContext()

	svc := &Service{DB: h.SQLDB()}
	require.NoError(t, svc.SetSessionArchivedStatus(ctx, cascadeWS, "ses_C", true))
	assert.Equal(t, cascadeRow{Archived: true, ArchivedByCascade: true}, readCascadeRow(t, h, "ses_G"),
		"grandchild rides the child's individual archive as cascade-held")

	require.NoError(t, svc.SetSessionArchivedStatus(ctx, cascadeWS, "ses_P", true))
	require.NoError(t, svc.SetSessionArchivedStatus(ctx, cascadeWS, "ses_P", false))

	assert.Equal(t, cascadeRow{Archived: true, ArchivedByCascade: false}, readCascadeRow(t, h, "ses_C"),
		"individually-held C stays archived")
	assert.Equal(t, cascadeRow{Archived: false, ArchivedByCascade: false}, readCascadeRow(t, h, "ses_G"),
		"cascade-held G released by the parent round-trip (documented granularity)")
}

// TestIntegration_SessionArchiveCascade_WorkspaceScoping: the cascade
// never crosses workspace boundaries — a same-ID session in another
// workspace is untouched.
func TestIntegration_SessionArchiveCascade_WorkspaceScoping(t *testing.T) {
	h := testharness.New(t)
	defer h.Close()
	seedCascadeTree(t, h)
	pool := h.Pool()
	ctx := h.NewContext()

	const otherWS = "int-ws-archive-cascade-other"
	_, _ = pool.Exec(ctx, "DELETE FROM session_index WHERE workspace_id = $1", otherWS)
	_, err := pool.Exec(ctx, `
		INSERT INTO session_index (workspace_id, session_id, title, parent_session_id) VALUES
		('`+otherWS+`', 'ses_P', 'Other parent'),
		('`+otherWS+`', 'ses_C', 'Other child', 'ses_P')
	`)
	require.NoError(t, err, "seed other-workspace rows")

	svc := &Service{DB: h.SQLDB()}
	require.NoError(t, svc.SetSessionArchivedStatus(ctx, cascadeWS, "ses_P", true))

	var n int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM session_index WHERE workspace_id = $1 AND archived`,
		otherWS).Scan(&n))
	assert.Zero(t, n, "the cascade is scoped to the target workspace")
}
