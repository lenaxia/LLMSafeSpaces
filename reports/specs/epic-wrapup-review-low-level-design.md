# Epic wrap-up review — low-level design (implementation contract)

**Status:** Design (2026-11-15) — the HOW companion to
`reports/specs/epic-wrapup-review-high-level-design.md` (the WHAT/WHY, owner-settled).
Anchors verified against `de99d5361` (main, 2026-11-15). The four implementation stories
(wrapup-engine, wrapup-watermark-diff, wrapup-config-persona, wrapup-gap-recursion) carry
these exact anchors in their patched charters; where a story name appears in brackets below,
that story owns the change.

---

## 1. Schema — TWO additive migrations (story-independent landings)

Landing as **two** migrations so `wrapup-config-persona` (keys) and `wrapup-engine`
(columns) never share a file and can land in either order (the 000042→000049 precedent:
vocabulary growth and row-shape growth are separate deliberate migrations). Files follow
the 000054 additive pattern, mirrored under `helm/migrations/`.

### 1a. `000055_board_wrapup_config_key` [wrapup-config-persona]

```sql
-- wrapUpReviewer joins BOTH tiers (user defaults + project overrides).
ALTER TABLE board_config_defaults DROP CONSTRAINT board_config_defaults_key_check;
ALTER TABLE board_config_defaults ADD CONSTRAINT board_config_defaults_key_check
    CHECK (key IN ('reviewColumn','autoAssignReviewer','defaultWorkerSlots','stagePersonas','wrapUpReviewer'));
ALTER TABLE board_project_config_overrides DROP CONSTRAINT board_project_config_overrides_key_check;
ALTER TABLE board_project_config_overrides ADD CONSTRAINT board_project_config_overrides_key_check
    CHECK (key IN ('reviewColumn','autoAssignReviewer','stagePersonas','wrapUpReviewer'));
```

### 1b. `000056_epic_wrapup_review` [wrapup-engine]

```sql
-- tasks: the engine-owned row-class marker. 'story' is the default and the
-- only value user-created rows can hold (service layer never writes 'wrapup'
-- outside the engine mint); 'wrapup' marks engine-minted wrap-up rows.
-- Self-exclusion (drain + completion predicates) and the dispatch carve-outs
-- (ClaimNext / dispatchDrive candidate scans) key off this column.
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS kind text NOT NULL DEFAULT 'story';
ALTER TABLE tasks DROP CONSTRAINT IF EXISTS tasks_kind_check;
ALTER TABLE tasks ADD CONSTRAINT tasks_kind_check
    CHECK (kind IN ('story','wrapup'));
CREATE INDEX IF NOT EXISTS idx_tasks_kind ON public.tasks USING btree (epic_id, kind);

-- epics: the on/off flag + the wrap-up lifecycle state. Explicit columns,
-- never derived (D2).
ALTER TABLE epics ADD COLUMN IF NOT EXISTS wrap_up_review boolean NOT NULL DEFAULT false;
ALTER TABLE epics ADD COLUMN IF NOT EXISTS last_wrapped_at  timestamp with time zone;
ALTER TABLE epics ADD COLUMN IF NOT EXISTS wrapup_count     integer NOT NULL DEFAULT 0;
ALTER TABLE epics ADD COLUMN IF NOT EXISTS wrapup_task_id   uuid;
ALTER TABLE epics ADD COLUMN IF NOT EXISTS wrapup_ack_pending boolean NOT NULL DEFAULT false;

-- reference integrity for the in-flight pointer (wrap-up row deletion clears
-- the pointer, same transaction; ON DELETE SET NULL is the physical floor)
DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'epics_wrapup_task_id_fkey') THEN
        ALTER TABLE public.epics
            ADD CONSTRAINT epics_wrapup_task_id_fkey
            FOREIGN KEY (wrapup_task_id) REFERENCES public.tasks(id) ON DELETE SET NULL;
    END IF;
END $$;
```

Down migrations drop in reverse. Row-shape additions (Go): `Task.Kind string` (rows.go, `json:"kind,omitempty"` — story
omits), `Epic.WrapUpReview bool`, `Epic.LastWrappedAt *time.Time`,
`Epic.WrapUpCount int`, `Epic.WrapUpTaskID *string` (rendered in reads as the live
wrap-up short id — join at read time, never stored denormalized),
`Epic.WrapUpAckPending bool` (rows.go:49-73 extension). `EpicInput` (service.go:276-280)
gains `WrapUpReview *bool` (nil = unchanged on PATCH; create defaults false).
`ValidateEpicInput` (textguard.go:74) needs no change (bool pointer).

## 2. The drain/deferral seam — `wrapUpAwareEpicCompletion` [wrapup-engine]

`epicCompletionCheck` (store.go:449-463) is replaced by a two-function shape in a NEW file
`pkg/board/wrapup.go`; the old name stays as the disabled-branch implementation so the
five call sites (stage.go:516, review.go:141, service.go:866, reconciler.go:1308,
reconciler.go:1517) each change exactly one token:

```go
// wrapUpAwareEpicCompletion is the single completion seam. Runs inside the
// caller's transaction. Returns (completed, mintedToken, err).
func (s *Service) wrapUpAwareEpicCompletion(ctx context.Context, tx pgx.Tx, epicID string, n *nudge) (bool, string, error)
```

Body, in order (all anchors verified):

1. `SELECT … FOR UPDATE OF e` the epic row (status, wrap_up_review, wrapup_task_id,
   wrapup_count, wrapup_ack_pending) — the serialization point for concurrent drains (D1).
   Absent row → no-op (deleted epic).
2. `status <> 'active'` → no-op (today's `e.status='active'` guard, store.go:455-457).
3. `wrap_up_review = false` → run today's statement VERBATIM (store.go:452-463 body;
   extract to `epicCompletionCheckPlain`) and return. [D9 regression pin lives here]
   **The disabled branch is the seam's ONLY completion path.** An enabled epic NEVER
   completes through the drain seam — completion of an enabled epic happens exactly
   once, in the PASS verdict transaction (§4), under the same row lock discipline. This
   is what keeps PASS a terminus rather than a fresh mint: after the settle SQL clears
   `wrapup_task_id`, a seam re-run on a drained enabled epic would walk to step 7/8
   (ack-wait or an empty-delta mint) — so the verdict path MUST NOT re-run the seam;
   it completes directly (§4).
4. Enabled: drain check over story rows only:

```sql
SELECT EXISTS(SELECT 1 FROM tasks t WHERE t.epic_id=$1 AND t.kind='story' AND t.stage='done'),
       EXISTS(SELECT 1 FROM tasks t WHERE t.epic_id=$1 AND t.kind='story' AND t.stage<>'done')
```

   Not drained (no done story, or any non-done story) → no-op. (A wrap-up epic with zero
   stories never completes — today's `EXISTS(done)` rule preserved.)
5. Drained + `wrapup_task_id IS NOT NULL` → defer (no-op; the in-flight row owns the epic).
6. Drained + `wrapup_ack_pending` → no-op (the human holds the gate).
7. Drained + `wrapup_count >= s.cfg.WrapUpConsecutiveCap` → ack-wait arm:
   `UPDATE epics SET wrapup_ack_pending=true WHERE id=$1 AND NOT wrapup_ack_pending` +
   `recordEventTx(... "wrapup_ack_required", payload{epic_id, wrapup_count, cap})`. No mint.
8. Else MINT (§3) inside this transaction; set `epics.wrapup_task_id`; the post-commit
   spawn rides the caller's `nudge` (`nudge{taskID, token, spawn:true, events:[]}` — the
   stage.go:225-233 pattern).

`EngineConfig` (board.go:44) gains `WrapUpConsecutiveCap int` (default 3, beside
`MaxNodesPerEpic`).

**Disabled-path byte-identity pin** (born-red): an integration test asserting the SQL
executed for `wrap_up_review=false` epics is the plain statement — pinned by behavior
( epic completes on last story done; no wrap-up row ever minted; `wrapup_count` stays 0).

## 3. The mint — `mintWrapUpTx` [wrapup-engine, charter payload from wrapup-watermark-diff]

Inside the caller's transaction, mirroring `CreateTask`'s write shape (service.go:947-995)
without the owner-facing guards:

1. Resolve the reviewer persona: `s.wrapUpReviewerForProject(ctx, p)` (§6). nil → **defer**:
   the mint is skipped, `slog` warn + `recordEventTx("wrapup_mint_deferred", reason:
   "reviewer-unresolved")`. The drain re-fires on the next qualifying transition or sweep —
   fail-closed, never a role-less spawn (the applyPersonaRole degradation discipline,
   persona.go:218-248).
2. Phrase id: `mintUnique(ctx, txQ{tx}, "tasks", PrefixTask, taskMintOwnerScope, ownerUserID)`
   (store.go:378 — the shared phrase space; wrap-up rows are tasks).
3. INSERT the row — `stage='in_review'`, `kind='wrapup'`, `tags='{wrapup}'`,
   `title = "Epic wrap-up review: "+epic.Title` (+ `" (delta N+1)"` when `wrapup_count>0`),
   `description = RenderWrapUpCharter(…)` (§5), engine-seeded `acceptance_criteria` (§5),
   `epic_name`/`project_name` denormalized per the create path.
4. Persona claim in the same tx: `personaClaimMintTx(ctx, tx, taskID, StageInReview,
   persona.Label())` (persona.go:276) + `personaClaimEventTx(...)` (persona.go:294) — the
   stage.go:225-233 entry-mint pattern, origin `"wrapup-mint"`. `claim_persona` stamps the
   wrap-up reviewer's label; `attempts` untouched (the personaClaimSQL discipline).
5. `UPDATE epics SET wrapup_task_id=$2 WHERE id=$1 AND wrapup_task_id IS NULL` — 0 rows =
   raced mint (converged; unreachable given step-1 lock, belt anyway).
6. `recordEventTx(id: "wrapup:mint:"+taskID, action:"wrapup_mint", payload{epic_id, task_id,
   window_from, window_to, story_count, wrapup_count})` — deterministic id (the mint's
   exactly-once audit anchor).
7. Caller's `nudge` carries the spawn (post-commit, r1-C5).

Node-cap note (D, open question 3): the mint deliberately does NOT check
`MaxNodesPerEpic` — engine state, not author graph growth.

## 4. The close — `RecordWrapUpVerdict` [wrapup-gap-recursion] {#close}

New `pkg/board/wrapup.go` service method + surfaces (§7). Signature:

```go
type WrapUpVerdictInput struct {
    Verdict   WrappingVerdict // "pass" | "gaps" — closed enum, reject otherwise
    ReportURL string          // required, both verdicts (the →done transition's evidence)
    GapRefs   []string        // required non-empty iff verdict=gaps; short ids or UUIDs
    Notes     string          // optional, size-capped (textguard)
    Reviewer  string          // display identity (mirrors VerdictInput.Reviewer)
}
func (s *Service) RecordWrapUpVerdict(ctx context.Context, actor Actor, taskID, shortIDOrID string, in WrapUpVerdictInput, requestID string) (*Task, *Epic, error)
```

Guards, in order — each failure is a loud `GuardError`:

1. Resolve task; `t.Kind == "wrapup"` else `GuardValidation` ("not a wrap-up row").
2. `t.Stage == in_review` (the live-claim stage; anything else is a stale replay — return
   the current row, 200-with-original discipline).
3. **Claim-token guard** (the criteria_met.go:70-73 pattern): actor carries the row's live
   `claim_token` — the wrap-up reviewer's own token, the in-flight guard from its charter
   envelope.
4. **Done-guard parity**: `criteriaAllMet(t)` (store.go:487) + `ReportURL != ""`. The
   reviewer checks its own criteria via `board_update_story {setCriteriaMet}` — the #1946
   reviewer grant applies unchanged (it holds the row's claim).
5. GAPS: resolve each ref (did-you-mean resolution, ids.go); every ref must be a
   `kind='story'` row in the SAME epic with `'gap' = ANY(tags)` and `created_at >=
   t.created_at` (filed since mint). Any miss → refuse with the offending ref.
6. Replay: `recordEventTx` id is deterministic — `"wrapup:verdict:"+t.ID` (one verdict per
   wrap-up row, ever) + the eventWriter `requestID` replay path (the CreateTask
   service.go:905-913 shape).

Transaction body (single tx, D4):

```sql
-- the row's →done (the completeReviewTask statement shape, review.go:113-120)
UPDATE tasks SET stage='done', stage_entered_at=now(), claim_token=NULL, lease_expires_at=NULL,
  claim_persona=NULL, failure_count=0, last_attempt_failed=false, pending_input=false,
  nudge_id=NULL, last_heartbeat_at=NULL WHERE id=$1 AND stage='in_review'
-- the epic settle, guarded by the in-flight pointer (replay/race = clean no-op)
UPDATE epics SET last_wrapped_at=(SELECT created_at FROM tasks WHERE id=$2),
  wrapup_count=wrapup_count+1, wrapup_task_id=NULL
WHERE id=$3 AND wrapup_task_id=$2
```

then: `writeTransition(done, evidence=ReportURL)`; `recordEventTx("wrapup_verdict",
payload{verdict, report_url, gap_refs, notes, window:{from,to}, story_count})`; on PASS —
**direct completion, never a seam re-run** (§2 step 3's note): the verdict transaction
holds the epic's logical turn (the settle SQL just consumed the in-flight pointer), so the
PASS branch executes the explicit guarded completion against the enabled-epic drain
predicate in the SAME transaction:

```sql
UPDATE epics SET status='completed'
WHERE id=$epic AND status='active' AND wrap_up_review
  AND wrapup_task_id IS NULL AND NOT wrapup_ack_pending
  AND EXISTS(SELECT 1 FROM tasks t WHERE t.epic_id=epics.id AND t.kind='story' AND t.stage='done')
  AND NOT EXISTS(SELECT 1 FROM tasks t WHERE t.epic_id=epics.id
                 AND t.kind='story' AND t.stage<>'done')
```

(0 rows = a story reopened under the verdict — the epic stays open by the ordinary
predicate; the gaps-filed-late shape, the honest outcome. A deterministic
`"wrapup:epic_complete:"+epicID` event rides the same tx.) The cap does NOT gate PASS
completion: the cap bounds the MINT loop (a pedantic gap-filer), and a PASS is the loop's
terminus — the reviewer approved the aggregate, so the ledger stops mattering (reset on
ack; completion makes it moot). On GAPS — nothing further (the non-done gap rows hold
the epic by the drain predicate alone). Late gaps after a PASS hold the epic open
through the same predicate if anything reopens before completion commits.
`unblockDependents` is NOT needed (wrap-up rows enter no dependency edges — they're born
edge-free and #1747's guard makes them edge-refusable: `insertDependencyTx` rejects
`kind='wrapup'` endpoints — one-line guard [wrapup-engine]).

Manager override posture: the raw manager `move_stage→done` on a wrap-up row stays legal
(existing manager-grade powers) but does NOT run the settle SQL — a wrap-up closed without
a verdict leaves `wrapup_task_id` set, which the next drain sees as in-flight → the epic
parks until ack. Fail-visible, never wedged: ack clears `wrapup_task_id` too (§8).

## 5. The charter renderer — `RenderWrapUpCharter` [wrapup-watermark-diff]

`pkg/board/wrapup.go`, deterministic, pure function of (epic, window, stories, cap state).
Rendered ONCE at mint into the description (frozen on the row — a re-claimed reviewer reads
the same window; convergence, not drift):

```
Epic wrap-up review — holistic, delta-scoped, against the epic's outcome.

Epic: <short_id> "<title>" (project <project short/name>)
Outcome (the bar this epic exists to achieve):
  <outcome verbatim>

COVERAGE WINDOW (UTC, both ends inclusive-bound):
  from <RFC3339 — coalesce(last_wrapped_at, epic.created_at)>
  to   <RFC3339 — this wrap-up row's created_at>
Stories in this window (<N>):
  - <short_id> "<title>" — done <at>; evidence: <evidence_url or "(none recorded)">
Aggregate diff: in the project's target workspace run, after fetching origin,
  git log --no-merges --since='<from>' --until='<to>' origin/main
  (plus each story's PR head where its evidence URL points at an unmerged PR).
  The window bounds the review; the story list is the union to read.

THE REVIEW (engine-owned procedure — the role owns judgment, never the reverse):
1. Read the aggregate diff as ONE change against the outcome above — coherence
   across stories, integration seams between them, dead/orphaned code,
   built-but-never-wired features.
2. LENSES, not checklists: evidence verification (PRs merged, issues closed) and
   wiring (registered/mounted/reachable; flag states are decisions, not defaults)
   run INSIDE the holistic read.
3. Close ONLY via board_record_wrapup_verdict (claim token from your envelope):
   - pass  {verdict:"pass",  reportUrl:"<the wrap-up report>"}   — the epic completes.
   - gaps  {verdict:"gaps",  reportUrl:"<report>", gapRefs:["tsk-…",…]} — the epic
     stays open. File gap stories FIRST via board_create_story (tag "gap"; DEDUPE
     against existing rows — board_list the epic before creating; a gap is a real
     story with real acceptance criteria, never a note). Then record the verdict
     referencing them.
4. Check off this row's criteria with board_update_story {setCriteriaMet} as you
   verify them (#1946: your own claimed row); the verdict requires ALL met +
   reportUrl (the Done guard applies).
5. Recursion awareness: this is consecutive wrap-up <count+1> of <cap> for this
   epic; at the cap a human ack is required before another mints.

If a BOARD LEASE NUDGE arrives, call board_heartbeat ONCE immediately, then continue.
The claim token in your envelope authorizes in-flight writes for this task only.
```

Engine-seeded `acceptance_criteria` (born on the row, reviewer-checkable):

- `[0]` "The aggregate diff for the coverage window was read as a whole, against the epic's
  outcome, with the evidence and wiring lenses applied"
- `[1]` "A wrap-up report exists at a reachable URL and is referenced by reportUrl in the
  recorded verdict"
- `[2]` "The verdict was recorded via board_record_wrapup_verdict; on gaps, every gapRef
  resolves to a deduped gap-tagged story in this epic"

**Delta story list** (the mint's SELECT, wrapup-watermark-diff): story rows of the epic
whose LAST `→done` transition falls inside the window:

```sql
SELECT t.* FROM tasks t
WHERE t.epic_id=$1 AND t.kind='story' AND t.stage='done'
  AND EXISTS(SELECT 1 FROM board_transitions tr
             WHERE tr.task_id=t.id AND tr.to_stage='done'
               AND tr.at > $2)   -- $2 = coalesce(last_wrapped_at, '-infinity')
ORDER BY t.created_at
```

(`board_transitions` is append-only evidence, migration 000036 §5 — `at` is the done time;
`updated_at` is not trustworthy for completion ordering.) First run: `$2 = '-infinity'`
→ all done stories ever — "first run covers all". Evidence URLs come from the transition
row (`evidence_url`), falling back to the latest verdict's `evidenceUrl`.

## 6. Config + seeded role [wrapup-config-persona]

**Slot.** pkg/board/config.go: key const `ConfigKeyWrapUpReviewer = "wrapUpReviewer"`
(:48-55 block). `BoardConfig` gains `WrapUpReviewer string` (:196 block — resolved
role-ref string, empty = default); `BoardConfigOverride` gains `WrapUpReviewer *string`
(:217 block, omitempty). `DefaultBoardConfig()` (:285) sets
`DefaultWrapUpReviewerPersona.RoleID` (the slug). `MergeBoardConfig` (:304) inherits
user→project exactly like `ReviewColumn`. `ValidateBoardConfigKey` (:337) decodes a bare
string (the PersonaConfig single-ref discipline — `{roleId}` object also decodes;
name/instructions reject). `validatePersonaRoleRefs` (:670) gains the key: the ref must
resolve in the RoleDirectory at SET time — unresolved refuses the whole write (D8).
`BoardConfigView`/`buildBoardConfigView` (:785-853) render the slot with
`ConfigSource*` annotations; no `StagePersonasSet`-style set-ness flag (a bare-string key
with a non-empty default has no opt-out semantics to preserve — empty string means
"default", explicitly out of scope). The `personaEligibleStages` map (:189) is NOT touched
(wrapUpReviewer is not a stage persona).

**Seeded role.** pkg/board/builtin_roles.go: `DefaultWrapUpReviewerRoleSlug =
"wrapup-reviewer"` + `DefaultWrapUpReviewerRoleSystemPrompt` — the judgment-half charter,
sibling of `DefaultReviewerRoleSystemPrompt` (persona.go:75-84). Prose core (pinned by
test, names pinned like the builtin pairs):

> "You are the wrap-up reviewer for an epic: read the aggregate diff of everything the
> epic shipped, as one change, against the epic's stated outcome. Coherence across stories,
> integration seams between them, dead or orphaned code, features built but never wired
> in. Evidence (PRs merged, issues closed) and wiring (registered, mounted, reachable;
> flag states are decisions, not defaults) are lenses inside that read — never a checklist
> replacing it. You own the judgment: depth, lens order, what merits a gap. The procedure
> (verdict verb, gap discipline, the cap) is not yours to change. File gaps as real
> stories, deduplicated; never merge, never push."

Seeding: `seedBuiltinWrapUpReviewerRole` in api/internal/app/board_role_directory.go
(beside `seedBuiltinReviewerRole` :101 — idempotent platform-scope seed, slug check) +
wired in app.go's startup block (:1678-1698, beside the existing seeds).

**Persona resolution.** `func (s *Service) wrapUpReviewerForProject(ctx, p *Project)
*PersonaConfig` in wrapup.go: `effectiveConfigForProject` → `WrapUpReviewer` → default
persona → `applyPersonaRole` (:218, canonicalize + degrade-to-nil). Three call sites:
mint (§3), reconciler re-claim pass (§7 reconciliation), `spawnFor` (§7 dispatch).

**Epic flag.** `CreateEpic`/`UpdateEpic` (service.go:604/674) pass `WrapUpReview` through;
`EpicActivate` needs no change (the recheck at service.go:866 funnels through the wrap-up
seam — an activated finished epic with wrapUpReview mints instead of completing).

## 7. Dispatch, reconciliation, and lifecycle carve-outs [wrapup-engine]

- **`ClaimNext`/`claimOnce` (engine.go:76, candidate CTE)**: candidate predicate gains
  `AND t.kind='story'` — wrap-up rows are NEVER served to worker spawns. Same predicate
  in `dispatchDrive`'s scan (reconciler.go:1062) and `TaskStart` refuses `kind='wrapup'`
  targets loud (`GuardValidation`: wrap-up rows are engine-driven only).
- **`spawnFor` (engine.go:675)**: before the stage-persona branch —
  `if t.Kind == TaskKindWrapup { persona = s.wrapUpReviewerForProject(ctx, p); … }`
  (nil → return; the reconciler re-claim heals, D3). The `Claim` needs no new field
  (`Persona` carries the reviewer; `Stage` stays `in_review`).
- **Reconciler in_review re-claim pass (reconciler.go:~940-1045)**: the per-row persona
  resolution becomes `personaOf(t)`: `t.Kind=='wrapup'` → wrapUpReviewerForProject, else
  `stagePersona(StageInReview)`. A lapsed/dead wrap-up reviewer converges here: re-minted
  claim (personaClaimSQL, attempts untouched), fresh spawn, same frozen charter.
  Escalation at the attempts cap applies unchanged (failed/escalated — the human exit).
- **`rearm` (rearm.go)**: wrap-up branch — a `failed` wrap-up row returns to `in_review`
  (not backlog), attempts reset, `wrapup_task_id` on the epic stays set (it is still the
  in-flight pointer); the reconciler pass re-claims. The backlog exit (manager pending
  re-queue) instead CLEARS `wrapup_task_id` (a manager un-parking a wrap-up row to backlog
  aborts the wrap-up cycle; the next drain mints fresh) — belt: both exits converge.
- **`delete` (delete.go)**: deleting the in-flight wrap-up row clears
  `epics.wrapup_task_id` (the FK ON DELETE SET NULL floor + the service-level event);
  the next drain re-mints. Depose (depose.go) works unchanged (a deposed wrap-up attempt
  hands its salvage pointer to the next reviewer claim — inherit, don't restart).
- **`doneCascade` (stage.go:504-520)**: the epicCompletionCheck call token-swaps to
  `wrapUpAwareEpicCompletion` and threads the `nudge` (one-line each; the mint's spawn
  rides the same post-commit fire).

## 8. Cap/ack state machine [wrapup-gap-recursion]

States (explicit columns, D2/D7; derived view in reads):

| state | columns | entry | exit |
|---|---|---|---|
| idle | `wrapup_task_id IS NULL ∧ wrapup_count=0` | epic create / ack / completion | drain+enabled → mint |
| in-flight | `wrapup_task_id = <row>` | mint (§3) | verdict close (§4); row delete/re-park → idle |
| ack-wait | `wrapup_ack_pending = true` | drain at `count ≥ cap` (§2 step 7) | `board_ack_epic_wrapup` → idle + immediate re-drain |

`AckWrapUp(ctx, actor, epicRef, requestID)`: manager authz (`requireManagerAction`,
service.go:319); tx: `UPDATE epics SET wrapup_count=0, wrapup_ack_pending=false,
wrapup_task_id=NULL WHERE id=$1 AND (wrapup_ack_pending OR wrapup_task_id IS NOT NULL)`
(guarded; 0 rows = nothing to ack → original echo), `recordEventTx("wrapup_ack")`, then
re-run `wrapUpAwareEpicCompletion` (an acked, still-drained epic mints immediately).
Surfaces: `POST /api/v1/board/projects/:pid/epics/:id/ack-wrapup`
(router.go:1397-1402 block), external MCP `board_ack_epic_wrapup`
(pkg/mcp/board_tools.go), agent tool (cmd/workspace-agentd/mcp_tools.go:2590 tool set).
`board_health` (reads.go:400) gains an `ack_waits` section; `Epic` reads carry
`wrapup: {count, cap, ack_pending, in_flight_short_id, last_wrapped_at}`.

Events vocabulary (board_events): `wrapup_mint` (deterministic id per row),
`wrapup_mint_deferred`, `wrapup_verdict` (deterministic id per row),
`wrapup_epic_complete` (deterministic `"wrapup:epic_complete:"+epicID` — the PASS
path's completion event, §4; rides the verdict transaction),
`wrapup_ack_required` (deterministic `"wrapup:ackreq:"+epicID+":"+count`),
`wrapup_ack` (requestID id). Payloads carry the window + counts (audit ≡ replay anchors).

## 9. Verdict + charter on the wire [wrapup-gap-recursion, wrapup-watermark-diff]

REST (handlers/board.go + router.go, the board_record_verdict shape):
`POST /api/v1/board/tasks/:id/wrapup-verdict` — body `{verdict, reportUrl, gapRefs,
notes, reviewer}`, claim token in the actor (the worker-surface pattern). External MCP:
`board_record_wrapup_verdict` (same handler, customer authz + claim token). Agent tool:
same name (mcp_tools.go board tool set). OpenAPI (`sdks/openapi.yaml`) + SDK regen per the
contract tier; frontend BoardPage: epic drawer wrap-up chip (in-flight / ack-wait /
count) + the verdict action on wrap-up cards.

## 10. Test map (born-red discipline per README-LLM rule 0)

wrapup-engine:
- disabled-epic byte-identity: last story done completes the epic, zero wrap-up rows (red
  first: mint fires without the branch).
- enabled drain mints; concurrent double-done serializes to ONE row (epic FOR UPDATE);
  in-flight defers second drain; wrap-up row excluded from drain math (a done wrap-up row
  + open story ≠ drained).
- ClaimNext never serves kind='wrapup'; TaskStart refuses; dependency insert on wrap-up
  endpoints refuses.
- mint-deferred on unresolvable reviewer role (warn event, no row, re-fires).

wrapup-watermark-diff:
- first run covers all stories (window from epic.created_at); charter carries window +
  story list + evidence; stories completing mid-review fall in the NEXT window
  (advance-to-mint-time pin); crash between verdict and advance = both-or-neither
  (failpoint at the settle SQL); delta list from board_transitions honors re-done rows
  (latest done in window).

wrapup-config-persona:
- set-time validation: unresolved wrapUpReviewer refuses user AND project writes; both
  tiers inherit correctly; get_config source annotations correct; seeded role idempotent,
  slug/prompt pinned; custom role swaps in; epic-level reviewer override ABSENT (pin the
  refusal).

wrapup-gap-recursion:
- PASS completes epic + advances watermark atomically (failpoint mid-settle);
  GAPS validates gapRefs (wrong epic / untagged / pre-mint / empty-on-gaps all refuse),
  holds epic open; gap completion → next mint scoped to delta; cap at 3 → ack-wait event,
  no 4th mint; ack resets + re-mints; verdict replay (deterministic id) converges;
  claim-token guard (wrong/absent token refuses); manager raw-done on wrap-up row leaves
  the pointer set → ack clears it.
- PASS IS A TERMINUS (born-red, the §2/§4 contract): PASS at/below cap completes the
  epic — NO fresh mint, NO ack-wait (a seam re-run would mint an empty-delta wrap-up;
  pin that the verdict path completes directly and never re-runs the seam); PASS at
  exactly the cap ALSO completes (cap bounds the mint loop, not the terminus); a story
  reopened under the PASS verdict (0-row completion) leaves the epic open with no
  mint (the drain seam re-arms on its own schedule); ack-wait arms ONLY on drain-mint
  paths, never from a verdict.

All integration tests ride the existing suite runners (`pkg/board` + api handler suites,
`-race`, `-timeout` per rule 0).

## 11. Story split + build order (the patched charters carry this)

`wrapup-config-persona` → `wrapup-engine` → `wrapup-watermark-diff` → `wrapup-gap-recursion`
(dependencies: engine on config-persona; watermark on engine; gap-recursion on engine +
watermark). The engine story lands the schema + seam + mint with the window payload
rendered minimally (window + story list inline); the watermark story owns the delta-query
correctness + the renderer's contract pins; the close/verdict lands last. Each story is
independently green mid-sequence: before wrapup-gap-recursion lands, wrap-up rows close via
manager moves with the pointer parked (fail-visible, §4) — never wedged.
