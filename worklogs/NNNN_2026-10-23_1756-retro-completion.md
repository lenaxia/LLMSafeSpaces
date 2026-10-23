# Worklog: #1806 post-merge residuals — r4 completion (7th surface) + record corrections

**Date:** 2026-10-23
**Session:** lane-1756-retro (post-merge remediation completion),
branch `fix/1756-retro-completion`.
**Status:** Complete — PR iterating review.

---

## Objective

PR #1806 merged with three verification-layer residuals named by the
formal retro verdict (final review on the PR thread, against head
`974530b6`):

1. r4 (`86b9b9c8`, branch tip of `fix/1756-post-union-remediation`)
   fixes most of them but never landed on main — verified content
   sitting on a dead branch.
2. The compact **busy** branch (`mcp_tools.go:1193`) is asserted by
   NO test even at r4 — r4's "all seven sites held" claim was
   6-of-7 accurate. Six of the seven `session_id_deprecated` blocks
   were deletable green.
3. Two records misdescribe shipped behavior: worklog 1161
   finding-9 (attributes the `session_id_deprecated` cold marker to
   `session_metadata` rows — the code emits `id_deprecated` there,
   `mcp_tools.go:1034`, and `session_read`'s raw-body path emits no
   marker at all) and the MT1 test comment (`mcp_tools_test.go`
   "covers every `if idDegraded` block" while exercising only
   compact's idle branch).

This lane lands all three: cherry-pick r4 to main's lineage, close
the 7th surface, carry the record corrections forward.

## What landed

1. **r4 cherry-picked clean** — `86b9b9c8` onto `origin/main`
   (`d5cce3c8`): `TestMCPEmission_SessionIDDeprecatedMarker_AllSurfaces`
   (five sibling surfaces: rename, archive, delete, send_message
   session_id, abort — each cold-flagged/warm-phrase), the MT1
   comment scope correction, and the worklog 1161 finding-9
   precision (single-id results carry `session_id_deprecated`; rows
   carry `id_deprecated` — session_list parity). Auto-merge, no
   conflicts (test+records only, as the verdict predicted).
2. **The 7th surface** — sixth subtest `compact busy` in
   `TestMCPEmission_SessionIDDeprecatedMarker_AllSurfaces`
   (`mcp_tools_test.go`): inline fixture (the send_message subtest's
   precedent — the busy leg needs `f.busySet[s1]=true`, which the
   shared `run()` helper doesn't build). Pins marker PRESENT on a
   cold busy replica (`"status":"scheduled"` asserted first so the
   busy leg — not idle — is provably the one exercised) and ABSENT
   (phrase emitted) on a warm one; joins the detached summarize
   goroutine before test end (CompactBusyFullStack convention).
3. **Comments made true** — the AllSurfaces header now names the
   sixth site; the MT1 header says "compact idle only" + "SIX other
   sites". With the busy case added, "together the two tests hold
   all seven `if idDegraded` blocks" is now 7-of-7 accurate —
   the verdict's "six-of-seven until then" condition is discharged
   by closing the gap, not by rewording around it.

## Test evidence

- `go test ./cmd/workspace-agentd/ -run 'TestMCPEmission_SessionIDDeprecatedMarker|TestMCPSessionMetadata_WarmReplica' -v`
  — all green, 6/6 subtests incl. `compact_busy`.
- **Mutation-verify (busy branch)**: replacing the busy-branch
  `if idDegraded` with `if false && idDegraded` (`mcp_tools.go:1193`,
  the "block never emits" mutant) fails EXACTLY the
  `AllSurfaces/compact_busy` subtest; the five siblings and the idle
  test stay green — matching the reviewer's own mutation
  methodology from the r4 verification.
- Full package suite run pre-push (see PR checks); no production
  code touched by this lane (test + worklog only).

## Boundaries

- Fleet no-touch respected: no `api/internal/services/sessionindex/`,
  `database/`, `pkg/board`, or frontend e2e files. The cherry-pick
  carried only `mcp_tools_test.go` + worklog 1161 — inside scope.
- Merging is the orchestrator's; this PR iterates to APPROVED only.
