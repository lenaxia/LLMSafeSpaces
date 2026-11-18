# Salvage-prefer terminal: exhaustion deposes with salvage

**Date:** 2026-11-18
**PR:** #2103 (`salvage-prefer-terminal`, worktree off `origin/main` @ `0aedbaf0a`)
**Story:** tsk-retro-textile-picnics (wait-salvage-terminal, the claim wait lifecycle epic)
**Designs:** PR #2101 — `reports/specs/claim-wait-lifecycle-{high,low}-level-design.md` §4/§5 (patched text governs; issue #2100 referenced, counters untouched)

## Why

Attempts-exhaustion at the two worker-claim sites (the dispatch claim's escalate arm and
`task_start`'s) wrote `stage='failed'` unconditionally — a row whose lapsed attempts left a
perfectly good open PR behind read as a failure (the #2064 shape). The S5 depose/rearm/inherit
machinery already existed for the manager depose lever; the exhaustion path bypassed it.

## What landed

- `pkg/board/salvage.go` — the POOR injectable seam (the AssigneeLivenessSource house pattern):
  `SalvageSource.SalvageFor(ctx, *Task) *SalvageSignals` + `SalvageSignals{Branch, Head, OpenPR,
  Uncommitted}` + `DifferFrom` (the loop bound) + `Pointer` (composes the S5 record with the
  `exhaustion-salvage` State prose) + `WithSalvageSource` (Nil = inert = today's terminal, pinned)
  + the ONE shared policy `salvageDeposeAtCap` both doors ride. **pkg/board imports nothing new**
  beyond stdlib `strconv` — the poverty line holds.
- `pkg/board/engine.go` — the policy diff at both exhaustion sites, probed under the row lock
  (the `deposeShowsLife` precedent; SKIP LOCKED keeps the fleet moving). No-salvage / inert /
  identical-facts / raced ⇒ the untouched terminal below the probe, byte-identical.
- `api/internal/app/board_salvage.go` — the production probe on the spawner's own seams
  (EnsureActive + agentd sessionless script node, read-only python): repo discovery bounded and
  deduped by git-dir; lane = the pointer's branch when present, else recency-ordered discovery
  with topical preference; the three questions = `rev-list --not --remotes` / `gh pr list` /
  `status --porcelain`. Wired in `app.go` from hoisted shared activator/executor locals.

## Decisions (the ones a future session will ask about)

1. **Probe inside the claim tx.** The row lock is held for ≤ the 2s budget (engine-side clamp
   `salvageProbeBudget`, adapter clamps itself identically). SKIP LOCKED means a slow probe delays
   only its own claim round — the same trade `deposeShowsLife` makes under the row lock.
2. **No `board_transitions` row.** The row is already `backlog` at both sites; a backlog→backlog
   transition row is noise (the depose backlog-arm precedent — the `salvage_depose` audit event
   is the record). Event id is nanos-keyed (the reconciler persona-escalate precedent): attempts
   RESET in this write, so an id keyed on the count would collide across salvage episodes.
3. **Rearm posture + backoff, depose's cooldown discipline.** The write is rearm.go's column set
   (attempts=0, overlay cleared, pending_origin NULL, episode slots cleared) plus
   `cooldown_until=now()+failureBackoff(failure_count+1)` (depose.go's magnitude, computed on the
   pre-reset count) plus the pointer. The typed-wait columns don't exist on this branch (story 1
   owns them); the composition point is stated in the write's comment.
4. **The DEPOSED directive is assignee-first, implementer-fallback, no-op when identity-less.**
   The exhausted row's reclaims cleared assignee/implementer in the common path; the directive
   fires only when an identity survives (best-effort, post-commit, self-healing on the next
   token-bearing write — depose.go's contract verbatim).
5. **Discovery honesty (first sighting).** Without a pointer the probe prefers topical
   (short-id-token) branch matches, else recency-ordered branches with facts. A wrong first
   sighting costs one depose whose pointer says VERIFY; a lost PR costs the incident over again.
   Stated in the adapter comment as the v1 posture.
6. **Loop bound on {Branch, Head, OpenPR} only.** Uncommitted is a presence fact riding State
   prose, not identity — two all-uncommitted rounds with the same (empty) triple read identical ⇒
   terminal, per §5.2's "identical {Branch, Head, OpenPR}".

## Tests

- Pins (`salvage_terminal_integration_test.go`): ExhaustionWithOpenPR at BOTH sites,
  Nothing/StaysFailed, SamePointer/Terminal, NilSalvageSource/ByteIdentical,
  PersonaEscalation/Unchanged (the scoped-out reconciler site, probe-count 0 asserted).
- Unit: seam purity (`salvage_test.go`), adapter shape/mapping/failure legs
  (`board_salvage_test.go`). The rendered probe script: `py_compile` clean + functionally
  exercised against this workspace's real repos (discovery found a sibling lane's uncommitted
  work; branch-pinned found this branch's worktree; missing branch returned `{}` ⇒ nil).
- CI round 1 caught a TEST-FIXTURE bug (the directive pin's forced re-pool left the assignee
  stamped, so the directive — correctly, assignee-first — went there instead of the seeded
  implementer); fixed by mirroring the real post-reclaim shape (assignee/claim cleared).
  Everything upstream of that assertion had passed against real PG.

## Verification honesty

This sandbox has no Postgres server (client-only, no root/docker): local verification = unit
lanes green, `go build ./...`, `make fmt-check`, `go vet -tags integration` (pins compile), the
script checks above. The PG-backed pins ran in CI (`secrets-integration.yml`'s
`-tags=integration ./pkg/board/...` lane) — green on the final head, with the AI reviewer's
APPROVED verdict pinned to it.
