# Worklog: NNNN_ #2107 row-loss verdict + the gone-row audit trail

**Date:** 2026-11-18
**Session:** The row-loss root-cause investigation chartered as a session (the board story kept vanishing — instances #5/#6 were the investigation story itself): verdict on the epic-auto-complete-purge theory, then the tombstone read fix.
**Status:** Complete

---

## Objective

Prove or refute **"epic auto-completion purges its non-terminal sibling stories"** via code reading, the board_events audit trail, and a repro pin; file the issue; ship the fix PR iterated to APPROVED (never self-merged).

## Verdict — REFUTED, three independent legs

1. **Code (exhaustive)**: exactly two `DELETE FROM tasks` statements exist in pkg/board (`delete.go:73` DeleteTask, `delete.go:173` DeleteEpic). Every epic-completion path — `epicCompletionCheckPlain` (wrapup.go:149), `wrapUpAwareEpicCompletion` (the single drain seam, wrapup.go:183), `completeWrapUpEpicTx` (wrapup.go:826), the RC4 net (reconciler.go:1693) — is a guarded `UPDATE epics SET status='completed'` and nothing else. The enabled branch INSERTS a wrap-up row (the opposite of a purge). No sweep/prune/orphan-cleanup over stories exists.
2. **Whole-workspace emission audit** (this workspace's session store, 239 sessions, every tool part since Oct 26): exactly ONE `board_delete_story` ever emitted (tsk-cloven-harvest-greets, Nov 15, unrelated). No local actor deleted any lost row.
3. **Timeline (reverse causation)**: with exact `stage_entered_at` stamps, the lost rows' epics could not have completed before the losses — e.g. linear-wasabi-reviews' last open story went done 19:26:43Z Nov 18, AFTER stirred-harbor-hums (gone by 17:45Z) and ebony-tunnel-waits (gone by 19:10Z) vanished; quixotic-javelin-heralds was its epic's SOLE open story (its own existence blocked completion). The charter's correlation had the arrow reversed: the drain predicate (≥1 done, zero non-done) means epics complete BECAUSE open rows vanish. Counter-instance: the Nov 17 loss was a whole new epic+5 stories with zero done rows — an epic that can never auto-complete.

Issue #2107 filed with the full evidence table. The true actor is platform-side and unnamed pending trail access — which is the fixable defect this PR addresses.

## Work Completed

### The fix — the gone-row audit trail (PR #2108)

`TaskEvents` resolved the task row first, so a deleted task's board_events timeline 404'd — the audit trail was sealed exactly when an investigation needs it. The delete contract already preserves history (delete events embed the full row snapshot); the read path just never reached it.

- `pkg/board/reads.go` — `TaskEvents` gone-row fallback + `TaskEventsTombstone` adapter arm (trail + the project it names = the ownership perimeter's anchor when there is no row). No anchor ⇒ the honest 404 (never-existed vs below-store stays distinguishable).
- `api/internal/handlers/board.go` — the REST route derives its perimeter from the trail's project on the gone-row arm; foreign trails are plain 404s.
- `api/migrations/000061_board_events_tombstone_indexes` — r1 review's blocking finding: the anchor was a guaranteed seq-scan of the append-only audit table firing on every 404-class probe. Two partial indexes (item_id; `lower(payload->>'short_id')`) + the anchor split per arm (UUID vs phrase) so each stays sargable.
- Handler pins (board_events_rest_test.go): tombstone-owner 200 (live seam never fires), foreign-trail 404 (nothing disclosed), no-trail honest 404.
- Integration pins (pkg/board/row_loss_2107_integration_test.go): the charter's repro held open — two →done transitions + the RC4 sweep over an epic holding open siblings; the epic refuses to complete, rows survive verbatim, zero delete events; all-done completion keeps every row. Plus the tombstone read (create+delete events, embedded snapshot, both address forms) and the honest-404 fork.

## Verification

- `go test -tags integration ./pkg/board/ -count=1` — ok (local PG 16.9, all 60 migrations)
- `go test ./pkg/board/ ./api/internal/handlers/ -count=1` — ok
- `go build ./...`; `gofmt -l`; `golangci-lint run` (v2.15.0) on both packages — 0 issues

## Follow-ups

- The true deletion actor remains unidentified: platform-side board_events query for delete_task events on the lost short ids (needs DB or owner-REST access — now answerable via this tombstone read for FUTURE losses).
- Consider exposing the tombstone read on the automation/agent surface (parity) if workspace-side investigators need it directly.
