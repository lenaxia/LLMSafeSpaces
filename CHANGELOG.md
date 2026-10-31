# Changelog

All notable changes to LLMSafeSpaces are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Fixed — the SECOND undeletable-trigger live-fire: statement-cache rotation (SQLSTATE 26000) retried on trigger per-id ops (2026-10-30 22:00–22:40, tsk-crystal-flatboat-tramps)

- **The incident**: trigger `5da42f34` ("v04411-release-watch", cron `*/5` with a marker-file inline gate script, **created hours AFTER the #1995 fix shipped**) 500ed on every per-id verb through the v0.44.11 release+rollout window — `fires`/`update` → "failed to fetch trigger", `delete` → "failed to delete trigger" — while LIST read the same row fine, `trigger_create` worked mid-window, and every per-id verb succeeded first-try once the rollout settled (verified live at 22:43–22:47). Root cause, empirically pinned on real PG with the exact live row shape (worklog 1238): when a rollout rotates server-side prepared statements behind pgx's statement cache (pooler recycle / failover-side DEALLOCATE), the next execution of a cached per-id statement fails ONCE with SQLSTATE 26000 `prepared statement "stmtcache_…" does not exist`, and the immediate re-run re-prepares and succeeds. #1995's `isTransientStoreErr` covered TCP-class faults (PG 08/53/57/58 + net.Error) but let the deterministic-LOOKING class 26 through un-retried — a per-(connection, statement) one-shot blip surfaced as the hard 500s (LIST survived on connections whose statement had already been revalidated; the scheduler's own claim-statement burn shows as the 10-second catch-up lag at 22:40:27 shared by two triggers to the microsecond). The row's creation time was a red herring: the failure is condition-dependent (rollout window), not row-shape-dependent — deterministic causes were excluded by store symmetry, live probes, and the benign missing-`tz` in its sourceConfig.
- **The fix**: class 26 joins the retryable set in `isTransientStoreErr` (one switch case + the live-fire rationale) — the existing single 200 ms retry absorbs the one-shot rotation blip on every trigger fetch/update/delete arm. Retry is safe: the statement never executed server-side (no uncertain-outcome hazard; strictly safer than the already-retried connection classes), and a genuinely invalid statement name just fails identically 200 ms later. A sustained rotation storm still 500s — deliberately — but now with the class-26 cause and trigger id logged (#1995's diagnosability arm, exercised on its first second event).
- Pinned by `triggers_stmtcache_test.go` (the exact live row shape — cron `*/5`, no-`tz` sourceConfig, the verbatim marker gate script, `autoDisableAfter` 12: one-shot 26000 → update/disable/delete/fires all retried to 200 with 2 store calls, persistent 26000 → 500 with the logged cause, and the full fires-audited → disabled → deleted round-trip through class-26 blips), the `TestIsTransientStoreErr` classification cases (26000 and wrapped 26000 → transient), and `TestTriggerStatementCacheRotation_Class26IsOneShot` on real PG (warm the cached per-id statement, `DEALLOCATE ALL` server-side, assert the verbatim 26000 error class, assert the immediate re-run heals — on a dedicated session so the suite pool is never poisoned). All handler pins fail on the old classifier (mutation-verified). Live: `update {enabled:false}` → 200 and `delete` → `{"deleted":true}` against the stuck row at 2026-10-31 01:06Z; the every-5-minute poll is silenced.

## [0.44.11] - 2026-10-30

### Added — "How the sprint board works": a help drawer on the board (owner ruling 2026-10-30, tsk-glassy-shovel-feels)

- **Two entry points, one drawer**: the board toolbar carries a HelpCircle icon affordance (accessible name "Learn more: how the sprint board works") and the empty-board first-run state ("No boards yet.") carries a text entry point beside *Create your first project* — both open `BoardHelpDrawer`, a right-side static-prose drawer (the app's existing Radix side-drawer pattern: Escape closes, title/description wired, close button). Static TSX copy, no data dependency — the learn-more moment exists before the first project does.
- **The copy teaches the board to a first-time user in five sections**: (1) *What it is* — stories as units of work with acceptance criteria, the Backlog → In Progress → In Review → Done flow, workers that pick stories up on their own, and a review pass that checks every criterion before Done; (2) *The recommended workflow* — six numbered steps (project + outcome-describing epic; one-sitting stories with CHECKable criteria; descriptions written for a reader with no memory of the conversation; let stories run; read the audit timeline; Done = every criterion verified with evidence); (3) *Writing good acceptance criteria* — "improve performance" (uncheckable) vs "the p95 query time is under 200ms, shown in the benchmark output" (checkable), criteria as the contract between you, the worker, and the reviewer; (4) *Worked examples* — six compact cards (writing code, research, creative writing, data analytics, data post-processing, batch content operations), each a story title + 2-3 criteria + what the evidence looks like; (5) *When not to use the board* — quick questions belong in a direct conversation; the board pays off when work has a definition of done and benefits from an independent review pass.
- **Infra-neutral by construction (binding owner rule)**: every example names ARTIFACTS and VERIFICATION OUTCOMES — "the suite passes", "the diff", "the report", "the cleaned dataset plus the reconciliation table" — never tooling or vendors, so the help reads usable whatever stack the workspace runs on; pinned by a test that scans the drawer's rendered text against a vendor blocklist (GitHub/GitLab/Actions/Jenkins/CircleCI/Jira/Slack/Notion/Excel/Google/OpenAI/Copilot).
- **Vocabulary**: the drawer speaks in *stories*/*sprint board* (the task→story rename lane's vocabulary — it never says "task board", pinned by a case-insensitive test on the whole drawer) so it lands in either order with the rename.
- Pinned by `BoardPage.helpdrawer.test.tsx` (7 tests): the toolbar affordance renders with its accessible label; the empty-state entry point renders and opens the same drawer; the drawer opens/closes via Escape and the Close button; all five section headings present, all six example cards present with their use-case labels TEXT-pinned; the vocabulary and infra-neutrality pins. Plus the real-browser tier (`frontend/tests/e2e/board.spec.ts`, "2026-10-30 help drawer", 4 tests): the drawer must actually RENDER visible in a real browser (portal/z-index/overlay layout) from both entry points — seeded board and the empty-board first run — Escape/Close must RELEASE the board underneath (search takes a click+type and filters — no overlay trap), and the icon affordance plus the drawer's width-clamped geometry hold at a 375px mobile viewport.
### Changed — the Task Board is the Sprint Board; tasks are stories in every human-facing surface (owner ruling 2026-10-30, tsk-orient-lantern-speeds)

- **PRESENTATION-LAYER rename, wire untouched**: "Rename the task board to sprint board. And rename tasks to stories/story. Task conflicts too easily with other features such as subtasks for opencode." Every user-visible surface now reads Sprint Board / story / stories — the board page h1 and the Sidebar nav ("Sprint Board"), the swimlane + epic-picker + drawer "New story" buttons, "Story <title>" create/edit dialog (placeholder "Story title"), "Edit story" / "Delete story" affordances, the card tooltips ("Story details…", "Story start (task_start)…"), the epic-delete cascade notices ("2 stories cascaded"), the audit-timeline action labels ("story created/updated/deleted/paused/resumed"), the search aria-label, the FAILED-filter and dependency/delete-dialog copy, and the BOARD_GUARD_MESSAGES 409 hints ("The story holds a live worker claim…", "A story cannot depend on itself…"). IDENTIFIERS ARE BYTE-IDENTICAL: the tasks table, `tsk-` short ids, REST paths (`/me/tasks`), SDK methods, MCP tool names, and every `data-testid` keep their `task` spellings — the pending_input flag-not-stage precedent (the UI vocabulary moves, the wire contract doesn't). Stage names, "Acceptance criteria", and epic/project vocabulary are unchanged. The board suites are re-pinned to the new labels (a new vocabulary pin asserts the "Sprint Board" heading, the "New story" button text/title, and that "New task" never renders; delete-flow menuitems, e2e board specs, and the Settings absence pin follow).
- **MCP tool descriptions in BOTH catalogs are story-first** (agents are exactly the audience that trips on task-vs-subagent-task): the agentd manager+worker catalog (`board_list` "List EVERY story (the wire/API term is 'task')…", `board_create_task` "Create one story…", `board_read` "Read ONE Sprint Board story — your claimed story's full worker view", the config pair "the Sprint Board's configuration", the dependency pair "story→story", bulk/plan/pause/rearm/delete prose) and the external platform projection in `pkg/mcp/board_tools.go` — with the wire-term note written once per surface ("story (the wire/API term is 'task')"), never at every mention; parameter NAMES stay `taskId`/`task_ids`. The heartbeat description's recovery line now reads "re-read it with board_read" and its ToolDescriptionGuidance drift pin moved in the same commit (the #1415/0059 class).
- **OpenAPI + SDK doc-comments, prose only**: the board tag description is "Sprint Board (formerly task board) — projects, epics, stories (the wire/API term is 'task')…", board summaries/descriptions/schema prose read story-first, and the Go/TypeScript/Python/Java SDK doc-comments carry the one-time note — zero path/operationId/field renames (`sdks validate` + router/schema parity green prove the wire untouched). `docs/api/mcp.md` retitles its section "Sprint Board (design 0067)" with the "formerly the Task Board" discoverability line and story-first tool-table prose.

## [0.44.10] - 2026-10-30

### Added — the workspace event bus is ARMED for production: controller flags + sidecar env + helm values (design 0072 rollout wiring, tsk-weathered-urn-ambles)

- **The bus ships dormant no more**: two controller flags — `--workspace-eventbus-addr` (host:port or a credentialess `redis://` URL) and `--workspace-eventbus-password-secret` (`NAME:KEY` of an existing secret, e.g. `llmsafespaces-credentials:redis-password`) — compose into the agentd process in BOTH pod topologies (the #1332 both-modes convention): the MAIN container in single-container mode (the helm default, where the notifier + bus consumers run) and the sidecar container in sidecar mode — whose process now runs the SAME notifier + consumer wiring as the single-container boot path (`wireSidecarNotifierBus`, which also closes the latent 0071 gap of a sidecar serving the MCP subscription surface without its delivery engine). `AGENTD_EVENTBUS_ADDR` rides as a plain value; `AGENTD_EVENTBUS_PASSWORD` as a secretKeyRef — credentials never enter a pod spec, and the credentialess invariant is ENFORCED, not documented: the controller refuses any userinfo-bearing address at boot (and the helm chart refuses it at render) — an operator reflexively passing `redis://:password@host` gets a refusal naming the secret flag, never a leaked password in pod specs or controller logs. Unset (the default) renders no eventbus env at all: the exact pre-0072 dormant posture. Boot validation also refuses a malformed `NAME:KEY` or a secret with no address (an address with no secret is legal: an authless Redis, logged); the helm chart carries the install-time twin (`controller.workspaceEventbusPasswordSecret` without `controller.workspaceEventbusAddr` refuses to render).
- **agentd's password leg**: `AGENTD_EVENTBUS_PASSWORD` composes onto host:port redis options only when the parsed options carry no credentials of their own (a credentialed URL keeps what it embedded — and now WARNS on the both-set smell instead of winning silently); pinned over an authed miniredis — the right password wires the bus, a wrong one disables to nil (never a half-authed client). The credential never reaches workspace-visible env on ANY path (review round 3's cross-tenant fix): sidecar mode strips both bus vars from the uid-1000 supervisor container (the #887 D5.1 admin-token precedent — the supervisor seeds opencode's spawn env from `os.Environ()`, and the password is the DEPLOYMENT-WIDE bus Redis credential with cross-workspace reach), and the spawn seam (`scrubAdminEnv`, every `buildEnvFrom` path) scrubs it for single-container mode, where agentd's own env legitimately holds it but no tool child may `printenv` it. The intended production values (the talos-ops-prod bump is a SEPARATE follow-up, not this change): the `llmsafespaces-valkey` svc address plus the platform credentials secret's `redis-password` key the API already uses.
- Pinned by: the agentd password-composition + both-set-warn pins and the sidecar wiring parity pins (armed-from-env / nil-bus-when-unarmed); the sidecar AND main-container env pins in the controller (composed / addr-only / absent-when-unconfigured — the main-container leg at the full buildPod level) plus the sidecar-mode SUPERVISOR-EXCLUSION pin (armed bus + sidecar enabled → the uid-1000 supervisor carries NEITHER bus var, the sidecar carries both) and the spawn-seam scrub pin (the bus password never rides the opencode spawn env on any buildEnvFrom path); the flag-surface pins (parse-into-returned-struct via the `Into` seam — the #1548 orphaning posture — plus defaults-dormant); the `parseEventbusWiring` boot-validation table (dormant / authless-legal / NAME:KEY split / credentialess-refusals / four malformed refusals); 5 helm render pins (both flags verbatim, addr-only, absent-by-default, secret-without-addr fails, credentialed addr fails); and the ARMED-posture envtest — a real apiserver through `SetupControllers` → reconciled pod → both env vars in their exact shapes, in its own package (controller-runtime's controller-name registry is process-global) with its explicit CI step.


### Fixed — the 2026-10-29 synchronize-drop window mapped to GitHub, and the #1844 run-watchdog upgraded to catch it (tsk-shaded-sturgeon-ambles)

- **The window, mapped with evidence (not a shrug)**: 2026-10-29 19:28–21:25Z, FIVE heads across #1983/#1984 got zero `pull_request`-event runs (`616f8b9c`, `882398c3`, `6437426f`, `3353d878` — recovered by manual CI dispatches — and `c455f193`, never recovered at all), while `issue_comment`, `schedule`, `workflow_dispatch`, and one `pull_request.opened` (#1985, 21:23:49) delivered normally in the same minutes, and merge-of-main synchronizes at 21:46/21:47 fired instantly. Event-type-scoped GitHub-side loss of `pull_request.synchronize` — the #1844 class, now with a full evidence register in **`docs/automation/ci-event-delivery.md`** (per-head run tables, the config clearance, the live probe, the residual asks, and a zero-checks playbook). The task record's fourth incident is corrected in the register: #1986's round-1 head `87df47c7` fired all four workflows in 23s — the "No CI exists on this head" quote belongs to #1983 round-2 (20:06:50Z reviewer comment).
- **Our config is cleared by construction AND by live probe**: no `paths:` filter exists on any of the four PR-triggered workflows; base-branch filters matched; pr-review.yml has no `workflow_run`/`issue_comment` trigger indirection; and concurrency `cancel-in-progress` groups produce CANCELLED-VISIBLE runs, never absence — proven live (probe PR #1991: a superseded head carries four `conclusion=cancelled` runs; the dropped heads carried zero). The decisive shape: four independent `on:` blocks produced zero runs on each dark head while the same workflow files ran on `workflow_dispatch` in the same minutes — a lost event, not a config fault.
- **The dead-man's check landed as an UPGRADE of run-watchdog (not deferred)**: threshold 15→**10 minutes** (healthy synchronize→check-run latency measured 5–50s across the probe and tonight's healthy controls; #1983's final head sat dark 42 minutes — inside any delivered sweep's catch with a 10-min trigger age), and a new **`issue_comment: [created]` trigger** — the out-of-band lane that demonstrably delivered through the drop window (AI Commands ran at 19:31/20:30×2/21:24 while synchronize was lost), so every PR comment now buys a full sweep at exactly the moments a human is looking at a possibly-dark PR. Recursion-safe: fault comments posted with GITHUB_TOKEN do not re-trigger `issue_comment` workflows. Pinned by `TestRunWatchdogTriggerSurfacePinned` (trigger surface, threshold literal, cron).
- **What is GitHub's is documented with the mitigation chosen**: the Actions-side delivery record for the window (owner-only webhook/audit trail — the repo PAT reads 403/404), a support-ticket ask reconciling `pull_request.synchronize` deliveries 19:25–21:45Z against run creations, and a second ask for measured **scheduled-event scarcity** (Run Watchdog's `*/10` cron yields 5–15 runs/day of 144 nominal ticks, 2026-10-23→30 — the starvation that let five dark heads pass between the 19:39 and 23:34 sweeps). In-band detection stays best-effort against an event-delivery fault by nature; the register states this honestly.

### Fixed — trigger delete/update 500s on a live-listed trigger: transient store faults now retried, logged, and correctly classified (2026-10-30 live-fire, tsk-swarthy-bloom-triumphs)

- **The incident**: trigger `7df2f7d4` ("release-v0449-watch", cron `*/3` with a never-proceed inline gate script) became undeletable and undisablable for 8 minutes during the v0.44.9 rollout — `DELETE` → 500 "failed to delete trigger" (×3), `PUT {enabled:false}` → 500 "failed to fetch trigger" (×2) — while `LIST` read the same row fine and both verbs succeeded again after the window. Root cause: a transient connection-class store fault (the scheduler's own fire cadence shows catch-up lag across the exact window); the CODE defects were that the 500 arms discarded the underlying error entirely (zero diagnostic trace — no logger on the handler), retried nothing (one blip = a hard 500 to the agent caller), and mapped client-canceled contexts into the same 500s. Deterministic row-shape causes were excluded by audit (no cache layer, no soft-delete, same owner-scoped predicates and row scanner as list) and by a real-PG pin that delete-with-fire-history is a plain CASCADE round-trip.
- **Diagnosability**: `TriggersHandler.SetLogger` (deferred injection, nil-safe — the pod-bootstrap precedent), wired for user+org in app.go; EVERY generic 500 arm on the trigger surface now logs the underlying error with verb + trigger/owner fields + the error's concrete type — the incident arms (fetch/get/update/delete/list_fires) and, per review r1, the previously-silent remainder: create, create-webhook + its encrypt arm, the workflow-mapping fetch (UPDATE-reachable via every mapping-touching patch), quota setting/count, target-workspace resolve/check/authorize (create + update), and rotate's encrypt + secret-write arms. The two remaining raw 500s ("cross-workspace targeting is not configured", "encryption provider not configured") are nil-capability config answers with no underlying error to swallow.
- **Transient resilience**: one bounded retry (200 ms, request-context-yielding) on connection-class faults only — PG classes 08/53/57/58 plus wrapped `net.Error` (the admin/crash-shutdown family a rolling deploy produces); deterministic SQL faults and context cancellations are deliberately not retried. Applied to the shared trigger fetch (update/get/listFires/rotate), the workflow-mapping fetch (UPDATE-reachable), the store update (single atomic patch statement — idempotent re-apply), and delete with **idempotent-complete semantics**: when an uncertain transient first attempt is followed by a retry that finds the row gone, the answer is 200 `{"deleted":true}` (the goal state holds), never the 404-after-500 "undeletable" trap; a plain first-attempt miss stays 404. Create/insert arms are logged but deliberately not retried (an insert is not idempotent — a retry after an uncertain commit would surface the duplicate-key 23505, not the original fault).
- **Client-canceled classification**: `context.Canceled` with a done request context now answers 499 (client closed request) with an Info-level CLIENT-event log line — per review r1, never an Error-level "fault behind a 5xx" for a client cancel — and genuine server-fault message strings are unchanged (wire-compat).
- **Pins**: handler-level (CI-runnable) — transient-once retried to 200 across all five surfaces, persistent transient → 500 with the underlying error + fields captured in the log, deterministic fault not retried, client-cancel → 499 (with the Info client-event log pinned — no Error line), delete transient-then-gone → 200 + Warn, plain miss → 404, the mapping-patch workflow fetch (transient retried to 200 / persistent 500 + logged cause) and the create arm (persistent → 500 + logged cause, no retry), and the full round-trip (fires visible → disable → delete → gone) over a trigger with fire history; store-level — `TestTriggerDelete_WithFireHistory_CascadeRoundTrip` on real PG recreates the exact repro shape (cron `*/3` + never-proceed gate script + skipped fires) and proves the FK cascade lets delete-with-history through.
- **Deferred (proposal in the PR, owner call)**: the immortal-skipped-tick litter question — `autoDisableAfter` counts failures only, so an all-skipped healthy poller ticks forever BY DESIGN (that is the #1812 feature). Proposal: surface `consecutiveSkips`/`lastProceededAt` in `trigger_list` (pure observability) first; an opt-in `autoParkAfterSkips` threshold only if real litter accumulates. The incident's own litter is moot — the live trigger is disabled and preserved as the repro row.

## [0.44.9] - 2026-10-30

### Added — gate-script worked patterns: a source doc shipped in the agentd image, materialized at boot, pointed at by the tool description (owner ruling 2026-10-29, tsk-arctic-wardrobe-verifies)

- **The first real inline-gate trigger took four attempts, and every failure was the script's EXTERNAL contract, not the #1812 machinery**: the anon GitHub API returns `review_decision: null` unauthenticated (the truth is `GET /pulls/{n}/reviews`, last element's `state` + `submitted_at`), states are lowercase, `urlopen` THROWS `HTTPError` on 304 when ETags are opted into, and a saved ETag can pin stale CDN content permanently (a verdict becomes invisible). The settled delivery: agents need worked patterns, LAZILY, without prompt bloat.
- **`docs/automation/gate-script-patterns.md`** is the source of truth — the canonical forms the live-fire validated: normalized external polling (never compare against invented constants; the GitHub-anon contract verbatim; the shared per-IP 60/h anon budget with the `calls-per-fire × fires-per-hour ≤ ~40` cadence rule), conditional requests (304 arrives as `HTTPError`, catch `e.code == 304` as the no-change path; for CDN-fronted anon APIs plain fetches + marker dedup beat ETag diffing), the marker-file signature idiom for state dedup across fires (fires have NO memory; signature-compare-skip prevents condition-true spam), the failure-posture choice (unhandled exceptions fail closed and count toward auto-disable; catch-all → return the chosen verdict — `proceed=false` quiet, the common poller case, or `proceed=true` fail-open — with the choice stated in the handler), and self-contained action prompts (the LLM leg is a fresh context: exact relay text, exact destination). A full worked example — the PR-review watcher itself — composes all five.
- **Delivery is the agentd image** (rebuilt every release, already in every pod, carrying the MCP tools whose descriptions point at the doc — one release unit, zero cross-component coordination): the doc is `go:embed`ded into the agentd binary (a byte-identical copy at `cmd/workspace-agentd/gate_script_patterns.md`, drift-pinned equal to the source — `go:embed` cannot reach outside the package dir), and the `init-fs` boot phase materializes it idempotently to `/sandbox-runtime/docs/gate-script-patterns.md` (identical file untouched — stable mtime; stale or absent file rewritten atomically; best-effort by design: a failed copy logs loudly but never blocks boot). Boot-time copy on purpose — init-fs runs before any app container, so the doc precedes the first agent session; this CREATES the platform→pod file rail properly (Runtime Base was the wrong layer, the Image Factory adds ceremony, MCP resources are not universally surfaced — files are).
- **Discovery is a pointer, not content**: `trigger_create`'s trivial worked example is REPLACED in all three description sites (the agentd MCP catalog, the api-side `pkg/mcp` surface, and the trigger-body schema blurb) by one line — worked gate-script patterns (external polling, conditional requests, state dedup across fires, failure postures, self-contained action prompts) live at `/sandbox-runtime/docs/gate-script-patterns.md`. Net-smaller on every site (−59, −15, and −25 chars respectively, decoded description strings): the depth moves out of the prompt and into the lazy read. Drift pins in BOTH catalog tests hold the pointer (the #1415/0059 class — this surface has lost description text silently before) plus a NotContains pin that the in-description example stays gone; the existing posture-sentence pins pass unchanged; the materialization is pinned end-to-end by the init-fs subprocess suite (fresh boot, idempotent re-run) and the embed's content pins name every live-fire trap.

### Changed — stagePersonas is stage → ROLE reference, full-stack, ZERO migration: no custom behavior definitions in the board layer (owner ruling 2026-10-29, tsk-spurious-auction-triumphs)

- **Owner rulings (verbatim): "Board stages should ONLY ever have roles configured, there is no custom definition in the board itself. ROLES are the way to define behavior for board stages." / "We do not need migration, we have never created any personas ever."** Supersedes #1980's compat posture at the model layer: the board stops being a persona-definition surface entirely. It REFERENCES platform agent roles; behavior definition (model, tools, system prompt — including the reviewer's verdict-channel charter) lives in the agent-roles system, nowhere else.
- **The data model collapses to role references**: `stagePersonas: {"in_progress": "<roleId|slug>", …}` — the bare string is the canonical value; the `{roleId}` object remains decodable but carries nothing else. `PersonaConfig.name` and `PersonaConfig.instructions` are gone from the board layer — no decode, no encode, no wire presence (openapi re-specced to the oneOf; the frontend types drop the dead fields). A stored non-role shape fails set loud and never decodes (400).
- **ZERO migration machinery (owner ruling)**: no compat decode of legacy shapes, no lazy name→role resolution, no backfill — #1980's `migratePersonaRoleRefs`/`MigratePersonaRoles` code paths and the suites that pinned them are DELETED. The set-path role-reference validation (id or slug against the available set, `validatePersonaRoleRefs`) stays: a mismatch refuses, a role-blind deployment refuses role-bearing sets.
- **The default-reviewer composition moves INTO the reviewer role**: the platform SEEDS a built-in `reviewer` agent role (platform scope, idempotent at startup when the slug is absent) carrying the default-reviewer verdict-channel prose (board_record_verdict, the criteria walk, the Done guard) VERBATIM as its system prompt — role seeding, never a persona migration. The default reviewer (`DefaultReviewerPersona`, the slug reference) resolves to a role deterministically; at spawn, slug references canonicalize to the role's id on the dispatch and an UNRESOLVABLE reference (deleted role, blind directory, read failure) degrades to persona-less — the transition lands byte-identical to pre-persona, never a role-less reviewer, warn-logged with the stored ref still visible in the config view until the re-arm heals. An org/user overriding the reviewer points `in_review` at their own role.
- **The spawner dispatches persona'd stages only via `spec["agent"] = <role>`** (the opencode agentID seam): the role's model/tools/system-prompt are the single source of truth; the charter names the role but composes NO behavior (the verdict-channel contract rides the role's system prompt); no name/instructions reach the dispatch. Persona-less stages dispatch with no agent key — byte-identical to pre-persona.
- **The role actually reaches the spawned worker (review round 2, PR #1990 F1 — the A8 seam, end-to-end)**: opencode NEVER errors on an unknown `agentID` — it silently falls back to the `build` agent (NODE-EXECUTE-CONTRACT §A8, validated probe), so a role reference alone would run every persona'd spawn behavior-less while appearing successful. Two halves close the trap. MATERIALIZATION: the pod-bootstrap response now carries the workspace's addressable agent-role vocabulary (platform scope + the workspace's org — the same composition the board role directory serves; `SetAgentRoleReader` on the handler, wired in app.go), the agentd bootstrap subcommand stages it verbatim to `/sandbox-runtime/agent-roles.json` (tmpfs, the admin-prompt lifecycle; env-relocatable `LLMSAFESPACES_AGENT_ROLES_PATH` for sidecar splits), and the opencode ConfigWriter materializes each entry as a NAMED agent (`agent.<roleID>` — prompt/model/mode/tools from the role, sibling fields of a pre-existing entry preserved; freshness = next pod boot, the admin-prompt contract). PRE-VALIDATION: agentd's agent-node executor resolves opencode's live registry (`GET /agent`, cached per password) BEFORE the session create and rejects an unknown agent name with `agent_not_found` in the 200 envelope instead of letting opencode swallow it; the board spawner reads that code and degrades ONCE to the persona-less dispatch (fresh `-np` RunID — #1957's mux drops re-used charter keys; one-shot by construction; warn-logged, stored ref still visible, next bootstrap heals). Other error codes (`agent_list_failed` included) keep the fail-by-lease contract — transient outages never silently downgrade a worker. Pinned by: the spawner degrade suite (persona-less re-dispatch, one-shot, other-codes-fail), the A8 agent-node suite (unknown rejects, known dispatches on it, empty skips the gate, list-failure fails loud, cache-per-password), the full-chain materialization test (bootstrap JSON → named agents in the rendered config; ids only, no slug aliases), the ConfigWriter role suite, the pod-bootstrap composition suite (platform+org, org-failure non-fatal, unwired omitted), and the app-level reader-wired guard.
- **Advisory parity and comment truths (review round 2, F2/F3 + missing tests)**: `board_validate_file`'s set_config semantic check now trim-rejects a whitespace-only BARE-string role reference (the schema's `minLength` cannot see whitespace — admission is identical to the platform decoder on both shapes); the reviewer-role seed comment states the real idempotence rule (a PLATFORM-scope namesake suppresses; an org-scoped one does not — org roles are invisible to boards outside that org); the spawner and RoleDirectory comments describe the role-only world (no DefaultReviewerPersona instructions, no legacy decode). The user-tier twin of the stored-legacy-shape drift pin lands (`board_config_defaults` — the decode path is shared but the belts are separate).
- **Docs/pins**: MCP `board_get_config`/`board_set_config` descriptions and `board_validate_file`'s set_config schema (rule renamed `persona-role-ref`) state role-only admission; pkg/board suites re-pinned (role-ref admission, the legacy shape a must-REJECT, degradation arms); frontend pins re-based to the role-only wire (the editor round-1 suppression guards carry over — the unpicked draft row is the only role-less form left anywhere).

### Changed — the persona editor is ROLE-ONLY: one select per stage, every other authoring affordance removed (owner ruling 2026-10-29, tsk-pallid-pivot-wiggles)

- **Owner rulings (verbatim): round 1 — "We don't need backwards compatibility because we never created any personas." Round 2 (stronger, supersedes) — "We can remove any editor feature for the DX. It's not role-first, it's role-ONLY."** The v0.44.8 editor's empty state steered toward the deprecated shape: an absent persona override map rendered per-stage cards defaulting the role Select to "No role (legacy name + instructions)" with the free-form name input + instructions textarea as the primary affordance. The editor now offers exactly ONE authoring surface per stage: the role select + Remove (both tiers).
- **Removed from the DOM, not deprecated**: the `PERSONA_NO_ROLE` option, the free-form name input, and the instructions-override textarea (instructions are not an editor surface anymore — the backend's default-reviewer composition and the stored-shape decode are the only writers/readers of them; the editor never shows them). The per-row role note folds into one section-level hint. "Add persona" opens the role picker directly (the `ui/Select` wrapper gains an optional `placeholder` — Radix's own `value === ""` seam — "Pick a role…" on an unpicked row).
- **The drop-on-save rule**: an entry with no role reference is not a persona — `persistablePersonaMap` (applied at both tiers' save bodies) drops added-but-unpicked rows AND stored legacy `{name, instructions}` shapes from the outgoing map; entries carrying a roleId ride VERBATIM (name/instructions included — the wire shape is untouched, and the REPLACE-never-wipes-a-stored-map protection holds for every persistable persona). A stored legacy entry renders as an unpicked picker — no legacy editing lane remains.
- **Backend untouched**: stored `{name, instructions}` / `{roleId, instructions}` shapes still decode and ride wire, the default-reviewer composition (role + charter-prose override) is server-side and unchanged, the set-path RoleID validation is unchanged — no migration, no wire/schema change. The suppression warning's one-click import still adds the reviewer ROLE persona (roleId-first; the no-reviewer-role fallback lands as an unpicked picker row — no roleId, so it persists only once a role is picked, and the warning stays up until then).
- **Review round 1 (PR #1988, findings F1–F3 — one root cause, fixed)**: the suppression warning, the import's dismissal, and the empty-map note read the RAW draft map while the save bodies persist the role-filtered `persistablePersonaMap` — three paths let an explicit save silently suppress the in_review persona with the warning masked (a stored legacy `in_review` entry with the composition live; the no-reviewer-role import; an all-role-less draft hiding the opt-out note). All three guards now read the PERSISTABLE map — exactly what the save sends: a role-less in_review entry (stored legacy shape, unpicked added row, or import fallback) can no longer mask the suppression the warning exists to catch; an import that lands role-less KEEPS the warning up until a role is actually picked; an all-role-less explicit draft renders the empty-map note describing the `{}` the save materializes. The stale "display continuity" comment (there is no legacy lane to display the fallback in) is corrected. New pins: the three guard paths at both tiers (composition-live legacy entry fires the warning AND the save really sends `{}`; no-reviewer-role import keeps the warning until a pick, then saves the picked role; the all-role-less note), plus the unresolvable-roleId ride (renders as its own select option, persists verbatim — behavior preserved from main, previously unpinned). Full frontend suite 2576/2576 green, tsc clean, eslint 0 errors.
- **Tests re-pinned** to the role-only shape: the persona section carries NO text inputs of any kind (`input, textarea` count zero), Add-persona = picker (unpicked rows never persist, both tiers), role-bearing stored maps carry back verbatim (including stored instructions overrides), and the reviewer-import pin holds roleId + prose on the wire with no name input or textarea rendered. Full frontend suite 2571/2571 green, tsc clean, eslint 0 errors.



### Fixed — board-worker spawns return identity immediately; the charter delivery is detached (the 2026-10-29 post-0.44.8 live-fire)

- **The incident**: two 0.44.8 board workers claimed tasks and ran as anonymous orphans — no `assignee_session` on their rows (no card link, no busy indicator), no Workers-folder classification, the lease ladder blind to them. Root cause: BOTH identity stamps (assignee + the session_origins worker mark) were gated on the spawner's return, the spawner's executor carries a 15-minute client timeout, and agentd's agent-node handler blocked on opencode's message endpoint — which answers at TURN COMPLETION. First turns over 15 minutes = client timeout = both stamps silently skipped with a fully-working session alive server-side. Never deterministic (short first turns stamped fine, every suite green) — tonight's two long-first-turn workers exposed it.
- **The fix (agentd, board-worker NodeID only)**: after the session create, the charter message dispatches in a DETACHED goroutine (`dispatchAgentMessageDetached`: background context with a 2h bound, the same #1327 dedupe key so a re-drive upserts the same transcript message, failures log and lease-lapse heals exactly as before) and the endpoint answers IMMEDIATELY with the session id — identity is never hostage to turn completion. Real DAG agent nodes keep the blocking contract (their results feed the graph). Pinned three ways over a parked fake opencode: the spawn returns identity in <2s while the turn runs (mutation-verified RED — the disabled arm deadlocks on the parked leg, the incident's exact shape), a non-board agent node still blocks, and a failing delivery never rewrites the spawn's outcome.
## [0.44.8] - 2026-10-29

### Added — the board's session refs click straight through to the session (owner ruling 2026-10-29, tsk-teal-vocalist-orders)

- **Owner ruling (verbatim): "Please add a task for adding a link/ref to sessions so from the board task info cards we can click straight through to the session. And via api mcp sdk we can look up the session ref if we need to fetch it."** The task drawer's Claim/lease assignee and the task card's assignee area are now React Router links to `/chat/{workspaceId}/{sessionId}` — the same target as clicking the session in the sidebar; the drawer (the declutter ruling's ids popup) additionally renders the #1959 IMPLEMENTER identity as a second link (during review the assignee is the reviewer — the implementer names the session that did the work). The read edge already serves the #1922 phrase form, so the display text IS the short 3-word id and the route segment in one value; the cold-window degrade (raw harness id + `assignee_session_deprecated`/`implementer_session_deprecated`) still links — never a broken href (`data-deprecated` marked), and an archived/deleted session renders the link with the chat page owning its not-found state. Isolated renders without a workspace id degrade to plain text.
- **`implementer_session` is exposed on every task read** (owner ruling: "the info is already there just need wire" — it was, except this one field): the #1959 bounce identity (captured at the in_progress→in_review forward exit, preserved through the review lifecycle) loses its `json:"-"` and rides REST reads/write echoes, the SSE `board.event` snapshots, the Go/TS SDKs, and openapi.yaml — translated at the read edge by the SAME #1922 phrase seam as `assignee_session` (one `session_index` consult per workspace covers both fields; stored rows stay canonical; `implementer_session_deprecated` is the cold-window flag). The MCP tool descriptions (`board_read`, `board_list`, the api-side board tool) now state that the session fields are RESOLVABLE session refs — pass either form to `session_read`/`session_metadata` to inspect the worker behind a claim. No new endpoint: the field IS the session ref.
- **Review round 1 hardening**: the REST task-events read (`GET /board/tasks/:id/events`) now translates the delete/cascade payload snapshots' session fields through the same phrase seam (`TranslatePayloadAssignees` rides `translateEventReads` — one consult covers actors AND snapshots; raw `implementer_session`/`assignee_session` never serve unflagged off any read edge), and the click-through behavior is pinned at every tier: vitest CLICK tests (card + drawer navigate to `/chat/…`; the card's `stopPropagation` contract — the link never opens the edit dialog), a Playwright real-browser pin (card leg + drawer leg land on the chat route), and a PG integration pin for the events-read snapshot translation (warm phrase / cold raw+flagged on both fields; stored audit rows stay canonical).

### Changed — the #1812 gate-script failure posture is stated in the tool descriptions (the 2026-10-29 live-fire follow-up)

- The first real inline-gate trigger (a PR-review watcher) surfaced a wording gap, not a feature gap: the catalogs said failures fail CLOSED but never said an author can CHOOSE a different posture. `trigger_create`'s descriptions (the agentd MCP catalog, the api-side `pkg/mcp` surface, and the trigger-body schema blurb) now carry the contract verbatim: "Unhandled exceptions count toward auto-disable; a handler wanting a different failure posture on error — quiet (proceed=false) or fail-open (proceed=true) — must catch its own exceptions and return that verdict itself." The platform behavior is untouched (fail-closed on the unhandled case remains); this documents the author-facing rule the live-fire validated — the invariant first, the policy choice second, no cause enumeration.

### Fixed — the lease wedge verdict never kills a mid-turn worker (the 2026-10-29 busy-gate, live-fire amendment to the seamless lease)

- **The live-fire**: three healthy board workers died holding PR #1982 — each inside a `sleep N && gh pr checks` CI-poll loop, killed by the WEDGE verdict while their turn was still RUNNING. A long silent tool call is observationally identical to a wedged session on the frame stream (a tool-call START stamps once, then minutes of nothing), the nudge queues behind the busy turn and can never be answered, and the sentinel's ordinal position never advances — the ladder's machine verdict fired on a worker that was alive and working the whole time. The fix is one fact the harness already held: the turn is BUSY.
- **`AssigneeObservation.Busy`** (pkg/board/liveness.go): the fourth POOR question — "is the session mid-turn (the #1574 derived busy: streaming or an in-flight tool call, never a raw status string)?" Busy ⇒ ALIVE at every rung: the ladder ORs it into the renewal arm, so a mid-tool-call worker renews however silent its frames. The zero value (false) is INERT — a source that cannot see busy state never blocks a verdict, and the Nil-source contract plus every existing pin pass unchanged. The layering the verdict now respects: the harness's tool TIMEBOX bounds a busy turn (a hung command ends, the turn ends, the queued nudge delivers); the wedge ladder owns IDLE silence (a real wedge); nudge→wake→response renews an idle-but-healthy worker. Busy defers the verdict for exactly as long as the turn runs — it never cancels it: when the turn ends, post-turn quiet re-opens (or opens) a nudge episode and a full window of true silence still lands the verdict (pinned both ways).
- **The adapter's busy leg** (api/internal/app/board_liveness.go): the hub gains a `boardBusySource` wired (app.go) to the usagestream Consumer's gate truth — `Consumer.DerivedBusy(workspace, harnessID)` reads the consult-maintained `g.derived` map (the same #1602 flip baseline the bridge reads; re-seeded from the authority snapshot on every reconnect, torn down with its gate). The answer is honored only for a live row this process has SEEN a frame for (the PART_START that flips a session busy also stamps the hub's frame index — busy-with-no-frame-ever is an answer about a session this replica never observed, distrusted in the safe direction: today's semantics). One map read per sweep candidate; no new RPCs, no new state.
- **Pinned by**: `TestPin_Lease12_BusyMidTurnNeverWedges` (busy answers renew through the exact wedge shape, three windows, zero wedges; busy clears → the episode re-opens from the NUDGE rung — never wedging on a dead episode's ordinal question — and only full post-turn silence verdicts), `TestIntegrationLeaseLiveness_BusyGateLeg` (the four arms: honored / frame-distrusted / unknown-never-busy / deleted-never-busy, plus the unwired inert contract), `TestIntegrationLeaseLiveness_ProductionWiring_BusyWorkerNeverWedges` (the live-fire's exact shape end-to-end: a real engine + production hub + production nudger composition, three frame-silent mid-turn windows renewed with the nudger NEVER called, then post-turn quiet opens the episode and the verdict lands), and `TestDerivedBusyReadsGateTruth` (the consumer read: truth / unknown arms).
- **The deferral is BOUNDED engine-side (review round 1's blocking finding)**: a source latched busy-true forever — the traced pathological mode is a pod dying mid-turn with recreation stalled while the workspace stays Active: the gate's `g.derived` holds stale-true, never reseeds, never drops, and the ladder would renew the lease forever, silently disabling failure detection for the row. `tasks.busy_renews` (migration 000053) counts CONSECUTIVE busy-ONLY renewals per claim epoch — incremented when Busy alone renewed, zeroed by any stronger alive evidence (movement renewal, heartbeat response) and every claim mint; at the cap (`boardBusyRenewCap`=3 ≈ 2¼ h of frame-silent busy claims on the 45 m lease) Busy evidence is EXHAUSTED for the epoch and the ladder proceeds as if busy were false — the ordinary quiet arms (nudge, then verdict) own the row again. The exhaustion deliberately PERSISTS through the nudge window (the nudge write does not reset the counter — otherwise a stuck source would cycle cap→nudge→reset forever). Legitimate mid-turn shapes never approach the cap: the harness timebox bounds a turn and the next tool call's PART_START is movement evidence, which resets the count. Pinned by the bounded arm (cap renewals → nudge → verdict on the SAME stuck-busy silence) and the consecutive-not-cumulative arm (movement restarts the count) of the PIN 12 suite; the reviewer's mutation check redden-verified the core arm, and the two round-1 pins are red against an unbounded implementation by construction.

### Added — session idle-subscriptions v1 (design 0071): subscribe/unsubscribe/list + the notifier

- **The push primitive polling cannot replace**: a session can WATCH another session in the same workspace and be woken when the watched session's turn completes (idle) or ends in error — a short notification lands in the subscriber's thread as its next turn, so a delegating manager learns "worker finished" without polling `board_list`/`session_metadata`. The notifier is a sibling in-process consumer of the sessionstate authority's ordered frame stream (the same seam the API usagestream consumer dials — every state-changing path crosses `applyLocked → fanoutLocked`, so one subscriber sees every busy→idle transition regardless of origin, including the synthetic #1722 evidence settles a harness-SSE hook would miss); derived busy per #1574 (never raw status strings), the saw-busy latch (unknown→idle never notifies; a busy observed this generation or persisted across restart settles through the gates), the pending-input gate (blocked-on-question is not "complete"), and subtask exclusion on BOTH sides (ParentID off the opencode session list, refreshed on a 60s ticker).
- **Suppression (§4.4, already-notified)**: per broadcaster, the last COMPLETED `send_message` to a target vs the last `edit`/`write`/`bash` part end (completed OR errored — an errored bash may still have mutated) — the broadcaster's last word to the subscriber postdating its every mutation suppresses the wake-up (the subscriber already holds the final state). 5s debounce per broadcaster (`AGENTD_NOTIFY_DEBOUNCE`); idle→busy flutter cancels; one notification per busy episode.
- **Delivery (ruling 4, at-least-once with duplicates tolerated)**: detached V1 `POST /session/{id}/message` via `context.WithoutCancel` (a busy subscriber queues server-side at its turn boundary — the queue-wait IS the wake-up; no artificial timeout), body stamped with the v1 provenance sentinel in its additive "notify" mode; the intent is written to a persisted outbox BEFORE the POST, cleared on success, retried exactly ONCE at boot after the first successful harness session-list pull; per-subscriber 60/h token-bucket storm backstop; one `llmsafespaces_session_notifications_total{outcome=…}` counter family carries every design-named outcome.
- **MCP surface**: `subscribe_session` (guards: no self, no cycles, no subtasks either side, cap 32 per broadcaster, did-you-mean on unknown targets; idempotent re-subscribe updates the event set; phrase-first id emission), `unsubscribe_session` (idempotent), `list_subscriptions`, and `create_session` gains a `subscribe` bool (the caller watches the child; ignored with a loud warning for subagent callers). Caller identity is the origin plugin's `lsp_injected_session` injection ONLY (the #1849 send_message posture — absent injection refuses loudly, never a guessed edge); the injection set grows the three tools + `create_session`, lockstep-pinned both directions.
- Registry durability: one JSON file at `/sandbox-runtime/session-subscriptions.json` (atomic write + fsync), carrying rows, latches, and the pending-notification outbox across restarts and pod suspend/resume.

### Added — the workspace event bus (design 0072): one Redis Stream per workspace, many consumer groups

- **A bus, not a protocol** (the owner's "per tenant MQTT almost?" — YES conceptually): the frame stream's derived events formalize into a TOPIC VOCABULARY — `session.{id}.idle`, `session.{id}.error`, `task.{id}.stage_changed`, `lease.{task_id}.nudged`, `lease.{task_id}.wedge_verdict` — published to one Redis Stream per workspace (`workspace:{id}:events`, `XADD MAXLEN ~ 10_000`: the retained-replay window and the memory bound in one knob). The vocabulary is a convention, not an enum: subscriptions are PATTERNS (`*` one segment, trailing `**` the rest, empty list = all), and new topics join by producer↔consumer agreement. New `pkg/eventbus`: `Publish` (MAXLEN-capped XADD), `Consume` (consumer groups — XREADGROUP/XACK, own-pending drain on same-name restart, XPENDING+XCLAIM reclaim of crashed consumers' entries with the PEL's durable delivery count, poison cap 5 attempts with a loud drop, OnError/OnPoison observability hooks), new-group start at `$` (live) or `0-0` (replay the retained window — the late-subscriber path); fully miniredis-tested (round-trip, fan-out, redelivery, reclaim, replay, MAXLEN, poison, hooks).
- **The notifier's delivery layer evolves; the logic survives unchanged**: when `AGENTD_EVENTBUS_ADDR` wires a Redis, a settle publishes ONE `session.{id}.{idle|error}` topic event (the producer leg rides the notifier's settle fold — the frame→topic mapping for session topics; publish failure falls back to the direct v1 leg, a lost wake-up being the failure that matters), and the new `notifier` consumer group turns it back into the SAME per-subscriber deliveries — identical suppression, rate bound, compose, subtask exclusion; the ONLY change is the transport, with consumer-group ack/pending replacing the hand-rolled outbox's write-before-send (a hard delivery failure stays pending and is REDELIVERED — the retry the outbox owned, now free). Unset, unscoped, malformed, or unreachable → nil bus → v1 point-to-point verbatim (the default and the fallback; every v1 test passes unchanged). New metric outcome `bus_published`.
- **The `lease` consumer group — the NudgeSender generalization**: `lease.{task_id}.nudged` events become the lease-nudge sentinel via SessionSend — the exact marker grammar + directive prose the API's nudge ladder composes (byte-parity pinned by test), so the API's ladder can switch from direct-sending to PUBLISHING (roadmap; the consumer stands ready and pinned). `wedge_verdict` topics log only — the verdict reclaims the row at the engine; messaging a wedged worker is noise.
- **MCP**: `subscribe_session` gains the optional `topics` alias (`session.<id>.idle|error|*` addressing the same broadcaster, unioned into the `events` set; foreign topic families refuse loudly — arbitrary pattern subscription is a future `subscribe_events` tool). Existing tools stay compatible; defaults unchanged.
- Deployment posture (honest): Redis is API-side today; workspace pods get the bus via `AGENTD_EVENTBUS_ADDR` + Redis routing (a helm/env change, no code change) — until then v1 point-to-point is the default. API-side consumers (audit → board_events, SSE → UI live updates, the nudge-ladder publisher) join the same stream as their own groups next.

### Changed — stage personas REFERENCE platform agent roles instead of redefining them (owner ruling 2026-10-29, tsk-chalky-guitar-supports)

- **`stagePersonas` is role-first**: each stage maps to an agent-role reference — the compact bare-string form (`{"in_review": "reviewer"}`, id or slug) or `{roleId, name?, instructions?}` — and the referenced role (the platform's agent-roles system: full CRUD, org-level + workspace-scoped) is the single source of truth for the spawned worker's model, tool surface, and system prompt. `instructions` becomes the OPTIONAL override layered on the role's system prompt (most stages need none); `name` becomes the display label. The legacy `{name, instructions}` shape still decodes verbatim from storage and wire (backward compat), and stored name-only personas LAZILY migrate at read time — a name matching an available role (slug, then name) renders with the resolved `roleId`, unresolvable names keep the instructions as a role-less override, and the stored row itself is untouched until a client writes the resolved shape back.
- **The default reviewer composition POINTS AT the built-in reviewer agent role** when the owner's vocabulary carries one (slug or name "reviewer"): the spawned reviewer runs on the role's model/tools/system-prompt with the board's verdict-channel charter prose layered on as the override. No reviewer role in the vocabulary → the legacy prose persona stands byte-identically (the ruling's fallback).
- **The board spawner passes the role through to the agent dispatch**: a persona carrying `roleId` rides the agent-node spec's `agent` key (the opencode agentID seam — the same one the workflow engine's routine fires use). Role-less and persona-less dispatches are byte-identical to pre-role.
- **Set-path validation**: `board_set_config` (and the REST config PUTs) validate every explicit role reference against the owner's available agent-role set (id or slug; a role-blind deployment refuses role-bearing payloads rather than storing an unresolvable reference); name-only personas write unvalidated (the legacy vocabulary stays legal). `board_get_config` renders role references with the unchanged provenance annotations.
- **The persona editor (BoardConfigPanels) is a role dropdown + optional override**: the per-stage base behavior selects from the member-readable `GET /me/agent-roles` vocabulary (NEW — platform + acting-user org roles, member-gated like the org prompt read; openapi + SDK surface updated); the free-form name input renders only for role-less stored personas (backward compat); the one-click reviewer import prefers the reviewer role with the mirrored prose override. agentd's `board_validate_file` set_config schema and the MCP config tool descriptions carry the role-first contract.
- Pinned by: pkg/board persona-roles unit + PG integration suites (shape admission, storage round-trip, resolution precedence, lazy migration + stored-row immutability, default-reviewer role/fallback arms, set-config validation), the spawner dispatch pin (`spec["agent"]`), ListForMe handler tests, agentd validate-file role shapes, and the re-pinned BoardConfigPanels frontend suite.

### Changed — the board's filter pills collapse into a filter drawer (owner ruling 2026-10-29, tsk-noble-spiral-notices)

- **Owner ruling (verbatim): "I think those pills need to be in a collapsed drawer. They eat up a lot of screen real estate esp on mobile."** The board toolbar's inline filter pill row (the #1813 FAILED overlay toggle + the tag facets — the real filters the 2026-10-28 ruling 1 kept) is gone: a funnel button (`ListFilter`) in the toolbar opens a COLLAPSIBLE filter drawer holding every filter control. The toolbar keeps only its essentials — the board title, project-focus select, search, the filter button, Board defaults, and the New epic/New project buttons — with no pills inline. (The ruling's "stage filter" is the #1813 FAILED overlay — the one stage-class filter that exists; no new stage filter is invented.)
- **Active filters stay VISIBLE with the pills folded away**: the filter button carries a count badge (`1` per live filter — the FAILED overlay and/or the selected tag facet; search and project focus are not drawer filters and never count; no badge at zero). Clicking the button reveals the drawer with the CURRENT selections rendered (selected states live); a **Clear all** affordance lives INSIDE the drawer (disabled at zero active filters) and resets every filter at once. Escape collapses the drawer without touching the applied filters; the button toggles it both ways (`aria-expanded`/`aria-controls` wired).
- **Mobile zero-height posture**: the drawer is conditionally rendered — closed means NOT in the DOM (zero height, nothing overlapping the swim lanes), and the board content starts immediately below the toolbar (pinned by geometry e2e: `board-grid.top === board-toolbar.bottom` at a 375px viewport, drawer absent, no inline pills). Open, the drawer is an in-flow slide-down panel that pushes the lanes down rather than covering them — the same shape serves desktop.

## [0.44.7] - 2026-10-29

### Changed — the EXISTING sidebar Workers group is provenance-populated with real nesting; worker provenance on the session list payload (rulings 6+7, 2026-10-28 owner rulings, tsk-polished-finch-paints)

- **The sidebar's EXISTING worker group ("Workers") is populated by worker PROVENANCE, never parentage (ruling 6: the workers group is the group to use — no new group)** — the #1966 `origin === 'worker' || parentId` classifier's parentId arm is gone ("the worker subtree is for board workers": board workers are spawned parentless/top-level by the wakeability invariant, so what nested under a session were that session's genuine Task-tool subagents, not workers). One tree is built over the full live set and partitioned: origin-class `worker` roots leave the flat top-level roots for the collapsed Workers group (current AND past workers, idle included, sorted by recent activity with alpha fallback, busy/idle per row via the shared row renderer); a worker's genuine subagent children nest BENEATH it inside the group (working chevrons, shared expansion state); subagents of user sessions nest under their top-level parent in the main tree again (ruling 7 — the reported nesting bug: the main tree was built from user sessions only and the folder rendered a flat `children: []` map, so a subagent could never nest under its parent anywhere); orphan-by-parentage rows follow the same split (non-worker orphans promote to top-level main-list rows; orphaned workers root the Workers group — visible either way, the synthetic "Orphaned subtasks" group stays dead); the group hides entirely when the workspace has no board workers, and archived workers still row under Archived (retirement outranks the class).
- **Worker provenance now rides the session LIST payload end-to-end**: `ListSessionIndex` LEFT JOINs `session_origins` onto every row (the spawn-path mark the board spawner already stamps at spawn — origin `worker`, kind `session`), so `SessionListItem.origin` is wire-visible on `GET /workspaces/:id/sessions` (SDK Go/TS types + openapi.yaml updated; the frontend's `/session-origins` merge stays as a compatibility fallback). Pinned by a sqlmock unit test (join + scan) and a build-tagged Postgres integration test.
- Tests: `sessionWorker` pins re-pinned provenance-only (parentId is NOT a worker signal) + the activity sort; `Sidebar.workers` re-pinned (group renders workers with busy badge and activity order, hides when empty even with subagents present, workers' subagent children nest, no worker remains a flat root, payload-carried origin suffices without the fallback endpoint); hierarchy/forcestop/indicator suites re-pinned to the restored nesting; `tests/e2e/sidebar-hierarchy.spec.ts` re-pinned to the provenance contract.

### Changed — board view declutter + mobile layout (2026-10-28 owner rulings, tsk-polished-finch-paints)

- **The epic pills are gone from the board toolbar** — only real filters render there (the #1813 FAILED overlay, tag facets); epic association is discovered via the epic rows' click-highlight. The per-project epic sections render ONE row per epic (title, progress counts, paused badge, kebab) with NO nested task list underneath — the row's click remains the epic→task navigation (highlight the epic's cards across the stage columns, dim the lane's others, scope New task). The epic list is CAPPED at 4 visible rows (`EPIC_LIST_MAX_VISIBLE`) with overflow scrolling inside the list's own region (never page growth), ordered by recent activity first (the newest of the epic's and its tasks' `updated_at` — data already in the view layer) with alpha as the tiebreak/quiet fallback.
- **Short ids are out of the chrome**: no `prj-`/`epc-`/`tsk-` ids on lane headers, epic rows, task cards, the project-focus options, notices, or prompts — the human title is the primary label everywhere (wrap/truncate with tooltip). Ids render ONLY in the click-through metadata popups (task details drawer, epic drawer, delete/task dialog titles as the secondary mono ref); `data-testid`/`data-*` attributes keep them for tests.
- **Card wrapping + responsive columns**: task cards are `min-w-0` with the title `break-words` inside the card and the action cluster (details/start/kebab) `shrink-0` — buttons can no longer bleed into neighboring columns at narrow widths; the stage grid collapses to 2 columns on small screens (`grid-cols-2 md:grid-cols-4`). Pinned by the new `BoardPage.declutter.test.tsx` (8 tests), re-pinned board suites, and two new e2e rows in `tests/e2e/board.spec.ts` (real overflow/scroll metrics on a 9-row capped list; 375px-viewport wrap/containment geometry).

### Added — board config parity: the config surfaces are first-class on every tier (UX relocation + persona editor + MCP config tools)

- **Owner ruling (verbatim): "The taskboard settings should not be in 'Settings' — that doesn't make sense for something as dynamic as the board. Configuration should be alongside the projects and on the board itself."** The Settings → 'Task Board' tab is REMOVED (relocated, not duplicated): board config now lives ON the board — a "Board defaults" affordance on the board header opens the user-tier defaults dialog (review column, auto-assign reviewer, default worker slots, stage personas), and every project swimlane header (active lanes AND the archived drawer) carries a Configure gear opening that project's configuration (worker slots via the project PATCH, review column + auto-assign reviewer override/inherited with provenance badges, and the persona editor over the effective merged view per `GetProjectBoardConfig`). `/settings/board-config` redirects to `/board?config=defaults`, and the board auto-opens the defaults dialog off that param — muscle memory lands on the config, never a dead end; every config control is reachable from the board view in ≤1 interaction. (The live finding behind the ruling: the only config UI hid under a Settings tab unreachable from the board, and carried no persona editor at all.)
- **The stage-persona editor (both tiers, in the new location)**: per persona-eligible stage (`in_progress`, `in_review` — the stages a live claim can exist in), name + multiline instructions; add/remove; UNLISTED stages render explicitly "no persona (the explicit map owns every stage it does not list)" — the explicit-key-owns-all-stages semantics made visible, never a silent inheritance. The `{}` opt-out affordance at the project tier ("override with empty map = no personas for this project"); the legacy-reviewer-suppression warning when an explicit map about to be saved carries no `in_review` entry while in_review currently runs on something (the built-in auto-reviewer under the `reviewColumn=auto` + `autoAssignReviewer` composition, or an inherited persona), with a one-click import of exactly what would be suppressed (the inherited persona, else `DefaultReviewerPersona` — byte-identical to the engine's, pinned by a Go-side mirror test). The verbatim round-trip wipe-protection semantics of the old BoardConfigTab (the user-tier `stagePersonasSet` rule and the project-PUT verbatim carry) are preserved in the new panels' save bodies and pinned by regression tests. Also fixed live: the config PUTs no longer stamp `requestId` (the strict key-gate 400'd every save).
- **MCP config tools over the in-workspace agent seam** (the "api/sdk/mcp/ux are all first class citizens" ruling): `board_get_config` — READ, open to any in-workspace session (#1747 §5 repeal), optional `projectId` (no project → the user tier's board-wide defaults), rendering the EFFECTIVE merged config with SOURCE annotations (per key: `code_default | user_default | project_override`), per-stage persona provenance included (`project_override | user_default | default_reviewer | none_explicit | none`, the persona rendered); `board_set_config` — the tier-selected REPLACE write (`tier=user|project`, design-0070 file-payload discipline: the config override JSON rides `configFile`, validated by the new `board_validate_file` op `set_config`, tier/projectId stay inline, stray file keys and inline config keys refused, the consumed file deleted only after success). The platform seam (`GET/PUT /internal/v1/automation/board/config`) validates verbatim through the same `DecodeBoardConfigOverride` key-gate as REST and enforces the same 64 KiB body cap as the REST twin (sized and chunked bodies alike) — agentd re-models nothing. The seam's `GetBoardConfigView` is DB-backed-pinned end to end (raw maps, per-key sources, persona provenance, guards). SDK/openapi unaffected (no /me wire shape changed). The #1832 item-6 parity matrix's agent cells for the four config actions go live; external MCP stays config-free by design.

### Changed — `board_heartbeat` is repurposed as the lease-nudge RESPONSE channel (ruling 2026-10-28: "when we nudge, we also require the call to board_heartbeat, rather than having to history dive")

- **The PERIODIC self-report discipline is DEAD; the tool survives as the concrete reply to a lease-nudge.** A heartbeat on a row carrying no live nudge NEVER extends the lease (liveness is platform-observed — the movement ladder renews); the arrival is still recorded (new row column `last_heartbeat_at`, server clock) because the wedge predicate reads it, and the 409 `claim_token` arbitration is unchanged. The agentd catalog, charter trailers, and bounce prose stop suggesting periodic heartbeats; the #1847 future-clamping STAYS (worker stamps are response events now, not liveness input — the future direction still never stores).
- **A heartbeat on a NUDGED row is the response the nudge prose demands**: the lease renews, the nudge slot clears, the wedge clock dies — the worker may then run long commands safely. The nudge sentinel's prose carries the full contract: RESPOND first (call `board_heartbeat` once), then continue working / advance the stage / declare failure — never restart.
- **The wedge predicate is three-legged**: nudged ∧ no post-sentinel events ∧ no post-nudge heartbeat ⇒ wedge. The heartbeat arrival is a strong ALIVE signal OR'd in at EVERY rung of the RC1 ladder, and at the no-evidence rung (Nil source, degraded adapter, unindexed session) it is the ONLY wedge detector — deliver-a-message + call-a-tool is the minimal harness capability (the harness-agnostic wedge floor), so that rung now runs nudge → heartbeat-or-wedge whenever a sender is wired (previously it always fell to the wall-clock reclaim; pin 6's honest lapse now applies only when there is no sender either).
- **The stillborn turn-evidence consultation leaves `Heartbeat`** (a zero-turn session cannot call tools, so it can never answer a nudge — the wedge verdict owns corpses; the `turnEvidence` seam survives for the #1959c bounce re-dispatch). Consequence recorded in the worklog: un-nudged `in_review` persona claims no longer heartbeat-renew (the ladder observes and nudges `in_progress` rows; reviewers keep the 45m window + the re-arm pass with #1813 accounting — reviewer-side nudge machinery is deliberately out of scope for this branch).
- Pins 9-11 (`lease_pins_test.go`): nudge → heartbeat response ⇒ renewed (at every rung, including the no-evidence rung where it is the only detector); nudge → silence on BOTH channels ⇒ wedge (including the sender-only floor); periodic-only heartbeat with no nudge ⇒ no extension. The #1847 pins re-pinned to the response semantics (the future-stamp rejection and the mild-skew clamp survive; the evidence-gate/heal pins die with the gate — extension no longer consults the watermark).
- **Review rounds 2-4 (worklog §9-§11)**: 000052's constraint swaps re-ordered for POPULATED databases (drop-old → rehome → add-new, both directions — pinned on a throwaway DB seeded with the pre-000052 world INCLUDING the origin-less failed row class the downgrade rehomes to backlog, `api/migrations/migration_000052_populated_test.go`: up, down, and operator-retry re-up); the nudge/heartbeat episode slots reset at every claim-lifecycle write (a dead episode's sentinel never inherits into a fresh claim — pinned); the send→stamp race is safe by Postgres transaction-clock semantics and PINNED (a heartbeat fired mid-delivery lands as the response); nudge/wedge audit events key on the sentinel (episode-unique); the worker claim path's flagged-row predicate pinned; `AssigneeObservation.NudgeMarkKnown` (an unmeasured ordinal answer re-nudges, never wedges); sentinel delivery is one bounded-concurrent batch; the hub TTL-evicts send-marks, caps its maps, and exports a degraded-sweep gauge. Round 3 closed the RC2 re-entry flag-blindness (the constraint-violating poison-pill sweep, born-red pinned — it initially shipped as a commit-message claim without the code, caught by review round 3 and landed for real) and restored the openapi semantic content a formatting re-base had silently deleted (`BoardTask.pending_input` + the 000050 descriptions), now guarded by a spec↔wire schema-parity gate (`TestOpenAPISchemaParity_BoardTask`) so spec regeneration can never again silently drop a serialized field.

### Changed — the seamless lease + `pending_input` as a FLAG (not a stage): a busy worker's claim never lapses mid-work, and attempts exhaustion lands a real `failed` terminus

- **The board lease is no longer a silent mid-work reaper.** Three mass-churn cycles (2026-10-27/28) burned live workers mid-verification to lease lapse — the 15m window expired under them while they ran long tool calls (the progress watermark stamps completed activity only; a tool-call START never counted), the rows were re-fed to fresh claimants, attempts hit the cap, and the exhaustion escalated wearing an owner-question costume (`pending_input`); a pod memory-reap once reclaimed all four in-flight rows at the same wall-clock microsecond with no session-death evidence. The redesign (owner rulings quoted in `reports/specs/seamless-lease-design.md`): the reconciler's RC1 ladder RENEWS a claim whenever the harness observed the assignee session do ANYTHING since the last sweep (a same-source delta — never a cross-process timestamp); on expiry-without-movement it sends exactly ONE nudge (a nullable `nudge_id` slot on the row, latest-only); on the next expiry it asks the ORDINAL question — any session events after the sentinel's transcript position, or any post-nudge heartbeat (the response channel above)? Silence on BOTH channels after a positioned sentinel is a machine WEDGE verdict (even a thinking model emits turn-start): the row lands `failed` with wedge evidence and is reclaimed; any post-sentinel activity renews and clears nudge state. The sentinel rides the existing agentmessage-v1 additive grammar with the full nudge contract ("RESPOND first via board_heartbeat; then continue, advance, or declare failed — never restart") — no attestation machinery per ruling (forgery at worst burns a recoverable attempt), and no timestamp is ever compared across processes (the ordinal answer lives in the harness's totally-ordered event log; the sentinel solves clock skew by POSITION, not time).
- **The engine stays harness-agnostic: `pkg/board.AssigneeLivenessSource`** — an injectable three-fact seam (movement-delta, existence where false means PROVABLY gone, and the ordinal events-after-nudge answer); Nil = inert and every engine guarantee holds at Nil (honest wall-clock semantics with the sender-wired heartbeat floor, pinned). The production adapter composes two same-source legs read-only over sessionstate: the 0069 frame stream (`usagestream.Config.OnFrame`, late-bound via `ProxyHandler.SetFrameObserver` — a PART_START tool-part is movement the instant it happens, so a worker mid-command longer than the lease window renews; the decisive production-wiring pin renews an expired claim off a single PART_START frame) and session_index message-count deltas (durable, stream-less pods, cross-replica). Honesty mapping: `deleted_at` is dead-evidence (fast reclaim, no nudge wasted); an unindexed session is NO-evidence — absence is never death; a failed batched read degrades the whole sweep to no-evidence, loudly. The `NudgeSender` delivers the sentinel over the V1 route with the bounce seam's fast-fail window so a dead session's 4xx returns synchronously.
- **Lease default 15m → 45m (2700s), rationale recorded**: wall-clock is the DEATH detector, not the liveness detector — movement renews and the quiet case gets the nudge ladder, so the window only bounds how long a provably-gone assignee's row stays pinned; 45m covers the longest observed legitimate silent stretch (a 10+ minute integration suite mid-command) with full ladder cycles inside a working window. Two knobs only (window + escalation ceiling); rejected alternatives with reasons — recency-based renewal (skew-fragile), three knobs (over-knobbling), affinity state machines (re-create stuck rows), nudge-grace tiers (a mid-turn nudge is undeliverable), attested sentinels (over-engineering per ruling), per-episode nudge bounds (superseded by latest-only). Changed in `DefaultEngineConfig`, the viper default, and the helm values + template fallback.
- **`pending_input` is a FLAG riding the row's real stage — never a stored stage — and `failed` is the failure terminus.** Escalation (attempts exhausted) lands `failed` + origin `escalated` (the re-arm's exact target); the lease wedge verdict lands `failed` with evidence; a worker question or drift-review park sets the flag on the row WHERE THE WORK LIVES, with no return-to-original-stage machinery (the row never moved) and claim predicates excluding flagged and failed rows (pinned — a missed flag check would feed an owner-blocked row). The WIRE keeps the `pending_input` literal as the flag-op alias (filter = flagged rows, move = set the flag; the engine maps it — `StageInVocabulary`/`WireStageVocabulary` are the single sources), so existing callers keep working. Migration `000052_seamless_lease` reconstructs existing rows (escalated → `failed`; deliberate parks keep their real stage + flag) under a check constraint admitting exactly the paired flag form plus the terminus. Surfaces re-pinned across the stack: api handlers (filter/toStage vocabularies + errors), pkg/mcp + agentd MCP tool enums and descriptions, the frontend (the awaiting-input badge rides the real stage; `failed` takes the side column; drag affordances via `stageDragKey`), and the sdks (ts/go types + openapi enums and descriptions).
- **The 8 design pins + ladder-contract pins** (`pkg/board/lease_pins_test.go`) land with deterministic RC1 invocation (the `rc1.post` failpoint + the expireLease clock seam): live-worker-never-lapses across 3× the window; exactly-one nudge; the wedge verdict; post-sentinel-activity renewal; dead-evidence fast reclaim; honest Nil-source lapse; claims-issue-only-to-spawner-minted-sessions (the wakeability invariant); and 409 `claim_token` arbitration untouched. The 32 old-semantics tests re-pinned to the flag/failed shapes — and the re-pinning surfaced four real engine defects now fixed (a migration constraint that forbade the escalation terminus, stage-machine flag-exit gaps, a phantom escalation transition row, and latent `secs::$n` syntax errors in the reconciler's lease extensions).


## [0.44.6] - 2026-10-27

### Added — abiclient seq-resume + usagestream billing closure: the design-0069 consumer half (L3 windows 1–2 closed, window 3 measured-unaddressed)

- **The reference Go ABI client (`pkg/abi/abiclient`) now PERSISTS its fold across connections and presents the fold's last applied seq as `EventsRequest.since` on every reconnect** — the internal redials (server-side slow-consumer drop, reseed, wedge) and later `Stream` calls on the same `Client` alike. A served cursor replays the authority's retained frames through the same client discard rule (no gap, no duplicate application, by construction); an unservable cursor (below the ring's low-water mark, ahead of its current seq, or a pre-0069 agentd that ignores the field) degrades to today's snapshot-first protocol, recognized by the `SnapshotFrame` that opens the connection — mixed-version rolls need no version negotiation. The resume cursor is in-memory (lives as long as the `Client`); the durable ws_state-store variant was judged not worth the coupling — a lost cursor degrades to one snapshot, never to loss (the tradeoff is documented at the `Client` type). A `ReseedNotice` inside the replayed range is delivered in seq position and triggers the mandatory re-snapshot redial WITHOUT `since` (replaying post-reseed deltas onto a pre-reseed fold would diverge it; replaying the notice again would loop the redial) — frames replayed BEFORE the notice stay applied, which is precisely the billing window the resume closes. First-frame wedge detection: the 2s budget applies only while a snapshot is owed; a resumed connection that is caught-up-and-idle legitimately hears nothing (steady-state posture). New `WithResumed`/`ResumedOf` seams mirror `WithResynced` (fired when a connection resumes; `WithResynced` fires when a snapshot lands) so consumers can tell a lossless resume from a fallback.
- **The API's usagestream consumer reuses one abiclient per gate** (rebuilt only when the resolved pod endpoint or credential changes) — across stream retries the persisted fold turns every reconnect into a ring resume instead of a full snapshot. On a resumed connection the #1884 snapshot-seed machinery (derived-baseline rebuild, #1666 busy corrections, `StreamResynced` → the busy-snapshot authority flight) is suppressed via `WithResumed`: nothing was lost, so nothing reconciles — the resync flight is now exactly the unservable-cursor (below-low-water or client-ahead)/reseed fallback it should be. Billing correctness rests on redelivery being safe by construction: the deterministic `(workspace, message, seq)` idempotency keys make the DB's unique constraint the dedupe (key determinism re-pinned at `TestRecordStepUsage_DeterministicKeys`).
- **Fault-injection proof that the billing windows close** (`api/internal/services/usagestream/billing_replay_test.go`, kill-window suite precedent): (1) agentd-side slow-consumer drop — the fold stalled mid-billing, the subscriber evicted server-side (a deterministic armed-drop wrapper pins the eviction's exact wire shape: clean stream end; no kernel-buffer dependence), MESSAGE_ENDs published in the drop window are recovered by the internal redial's ring replay and billed exactly once; (2) transport gap — the stream dies mid-flight (server write abort, no close frame), billables publish while dead, the RUN-LOOP redial on the REUSED client resumes from the ring, bills exactly once, converges the lost busy frame via the replayed event, and fires NO `StreamResynced` (the no-double-fire pin); (3) duplicate redelivery produces byte-identical `Usage` values, so the idempotency key absorbs it (already billed, no double row). The #1884 e2e rows are re-based onto the honest fallback (tiny-ring authority + filler events evicting the cursor past low-water during the dead window) — every seed that remains fires the flight, and no resume ever does. The gate-closed window (L3 window 3: a turn starting entirely inside the pod arms no gate; MESSAGE_ENDs before the ≤60s reconciler re-arm stay unbilled) is **measured-but-unaddressed by design** — the investigation's recommendation is to measure the sub-tick miss rate before building the agentd→API "gate poke" coupling.
- **The discard-rule property fuzz extends to the resume path** (`pkg/abi/abiclient/client_resume_test.go`): randomized disconnect/publish/reconnect cycles on ONE client against the real authority pin that the applied-seq multiset stays duplicate-free, every published event applies exactly once across the cycles (count equality), and every MESSAGE_END lost in a disconnect window is recovered; plus the served-resume pin (replay delivers exactly `since+1..`, no snapshot, `WithResumed` once), the below-low-water fallback pin (snapshot-first, subsumed seqs never fire applied-events, live continues from stamp+1), and the reseed-inside-replay pin (notice in seq position, re-snapshot redial settles, no loop).

### Added — agentd authority replay ring + `Events.since` resume (design 0069, amending design 0055 M1)

- **The ABI Events stream can now redeliver what a reconnecting consumer missed — closing the only event-loss window with money attached and no correction path (billing on MESSAGE_END; the snapshot carries no cost data, and billing inserts are already dedupe-safe by construction).** Design 0055 M1 ruled "no ring buffer, no `since=` cursor, no replay machinery"; that ruling is amended (design 0069) after the SSE delivery-guarantees audit found snapshot reconstruction covers every state consumer but not the per-event billing callback (slow-subscriber eviction, transport flap, or a reseed redial silently skipped billing for frames published in the gap). The authority now retains a bounded replay ring of every published frame (events, reseed notices, orphan-sweep aborts — anything that consumes a seq), evict-oldest under BOTH a 1024-frame cap and a 16 MiB byte budget (the SSE 16 MB line-buffer precedent for large patch/delta frames; newest frame always retained), appended at the `fanoutLocked` seam under the authority lock. `EventsRequest` grows an optional `uint64 since` (additive wire change; the op set stays five — schema gates untouched): a client presenting its last applied seq ≥ the ring's low-water mark AND ≤ the authority's current seq is resumed with the retained frames `seq > since` and then live, gapless and duplicate-free by construction (replay-tail copy + live registration under ONE lock hold — I2's atomic cut generalized); a cursor below low-water (always after a process restart — the ring is memory, the seq counter is durable) OR beyond the current seq (the client-ahead guard: a cursor from a foreign/reset seq space would otherwise be served as an empty resume and silently wedge the client's discard rule on the renumbered stream) degrades honestly to today's snapshot-first protocol, with the first frame on the wire (snapshot vs event) disambiguating for the client. Absent `since` is byte-identical today's behavior — existing consumers (contract-stream manager, S1 comparator, abiclient) unaffected until the follow-up consumer work opts in. Ring observability in `Metrics()` (frames, bytes, low/high water marks, fallback count). Property tests (randomized disconnect/resume cycles, flood-under-race), eviction/byte-budget boundary tests, the kill-9 honest-fallback pin, the cursor-bounds (caught-up vs client-ahead) pin, and the reseed-notice-in-replay pin land in `sessionstate/replay_test.go`.

### Fixed — user-stream replay: a client-ahead Last-Event-ID now emits resync (SSE L4a, the P0 row)

- **The only SILENT loss signal on the platform's strongest SSE leg is closed.** The user event stream's replay ring is in-process per replica, so after an API roll the fresh replica's ring restarts at id 1; a browser reconnecting with a `Last-Event-ID` from the dead replica's id space (≥ the fresh ring's `nextID`) previously got: no gap detected (the rule only caught `lastID < oldestID`), zero replayed entries, no resync — silently stale until the next reconnect wipe. `replayBuffer.sinceLocked` gains the second gap arm (`lastID >= nextID`), and the review round closed the window at its narrowest too: a user with NO ring state at all on this replica (`Replay`'s old nil-return, `sinceLocked`'s empty-ring return — the most common shape right after a roll, before any event publishes) also gaps on any presented cursor. One outcome everywhere: the `resync` marker + snapshot-reconstruction, never a silent empty replay. Pinned end-to-end on the wire (`TestStreamUserEvents_ReplayClientAheadEmitsResync` — resync marker, zero `id:` lines) and at the broker (`TestUserBroker_ReplayGapRuleCompleteTable` — the behind-oldest / client-ahead / no-ring / in-range / cursor-0 truth table, including cursor-at-newest = up-to-date, not a gap).
### Changed — the #1747 §5 manager grant is REPEALED: board manager tools open to ANY in-workspace session (ruling 2026-10-27, tsk-copper-refectory-generates)

- **Owner ruling (verbatim): "The concept of a manager grant needs to go away. This is a sharp edge that will cause pain. Why is there even a concept of RBAC here. We have access to the whole system and if a task is going to impersonate another session that's the user's prompting issue not mine."** A workspace pod is single-tenant (every session shares one credential — an identity-grant layer over a shared credential is theater, not authorization), and the live failure was incoherent: the session that created a project/epic/tasks could create/update but was refused `board_rearm_task`/`board_list`/`board_health` ("spawned workers and unidentified callers hold no manager grant, ruling #1747 §5"), stranding completed rows at `pending_input` and forcing successor-task workarounds.
- **Both grant tiers removed**: agentd's `boardManagerSession` gate (required-identity fail-closed + spawned-worker registry refusal, pre-wire) is reduced to identity RESOLUTION for audit attribution — an absent, mistyped, or unresolvable identity degrades to empty attribution and the call proceeds (loudly noted); the platform seam's belt (`resolveManager`'s required-session 403 and the `IsWorkerSession` live-assignee refusal, removed from `pkg/board`) is gone. Manager-grade dispatch (token-less `board_move_stage`, `board_rearm_task`, `board_list`, `board_health`, bulk, CRUD) serves any in-workspace session, spawned workers included; the resolved session id still rides the audit actor.
- **Deliberately unchanged (correctness machinery, not RBAC)**: claim-token arbitration (409 `claim_token` — concurrency fencing), stage-machine and Done guards, the worker surface's denial of the pending_input exits, and #1946's reviewer-persona `setCriteriaMet` scoping — a spawned TASK worker's token-less self-certification still refuses (agentd's `boardWorkerSelfCertificationPin`, the one surviving registry check). External REST/MCP surfaces keep user-token authn (real tenancy). Design 0067 §10.10 records the ruling; every tool description/comment citing "#1747 §5"/"manager grant"/"orchestrator-class" amended.
- Tests: every manager-gate/worker-refusal pin re-pinned to the open behavior (agentd dispatcher + catalog guidance + phrase-identity suites; API handler belt suites `ManagerGrantRepealed`; engine criteria-met/persona pins), integration pins updated including the **regression for tonight's failure — the creating session re-arms its own attempts-exhausted rows through the manager seam** (backlog, attempts reset, audit attributed).


## [0.44.5] - 2026-10-27

### Added — board file-backed CRUD: bodyFile/patchFile/planFile, board_validate_file, auto-cleanup (design 0070)

- **Payload files are the ONLY payload path on the five payload-bearing board manager tools** (`board_create_task`/`board_create_epic`/`board_update_task`/`board_update_epic`/`board_import_plan`) — a straight cutover against the model-emission corruption class (#1832 dogfood: duplicated argument fragments, stray file keys, unparsable parameter streams on multi-KB structured JSON). The tool call carries short scalars (routing + control-plane) and one path; all structure rides a JSON file written via the write tool. Inline payload keys are refused loudly with the workflow hint, the payload file is REQUIRED, and the tools' schemas drop the inline payload properties entirely (the unsafe shape is unexpressible). The **#1837 `descriptionFile`/`outcomeFile` params are removed** (superseded by body/patch files); the #1837 read discipline survives in the shared reader (regular-file gate, stat+read caps, UTF-8; 256 KiB payload cap).
- **Workspace-root path anchoring**: relative file-param paths resolve against the PVC workspace root (`/workspace`), not agentd's process cwd — the live `open reports/charters/…: no such file` failure class.
- **Auto-cleanup**: a successful CRUD call deletes its consumed payload file (a delete failure warns via a `cleanup_warning` field, never failing the call); `board_import_plan` keeps the file across `dryRun` (a dry run mutates nothing). A retry referencing a deleted file gets the file-not-found hint pointing at `board_list`/`board_read` to confirm the landing.
- **`board_validate_file`** — the local, advisory, zero-wire pre-submit check (write → validate → submit), and **the SAME strongly typed validator gates every CRUD call** (santhosh-tekuri/jsonschema/v6, one compiled schema per op, `additionalProperties:false`, property sets pinned to the platform request DTOs by a reflection parity test — DTO drift fails CI; semantic layer: #1849 atomic-create, the placeholder-class scan via the ONE shared vocabulary, plan-key uniqueness, `exactly-one-of` epic arms). Findings carry JSON instance paths (`tasks[2].acceptanceCriteria[0].text`), rule, severity, problem/fix, expected/actual; error findings refuse the wire call with byte-identical feedback at both entry points (dual-entry equivalence pinned); warnings never block.
- **The unified plan document**: `board_import_plan`'s `planFile` covers any granularity — a NEW epic with all its tasks (`{epic:{title,outcome,tags}, tasks:[1..N]}`) or N tasks attached to an EXISTING epic (`{epicId:"epc-…", tasks:[1..N]}`); exactly one epic arm (both/neither → one clear finding); all-or-nothing in every mode; dependsOn addresses payload keys. Platform: `ImportPlan` gains the epicId attach arm (exactly-one-of validation, epic-belongs-to-project guard, the #1948 terminal-epic guard mirrored from create-task including the in-transaction row-lock re-check, and the epic-create step skipped in the one transaction); seam + REST handlers bind the typed `board.PlanInput` (which gains `Epic *PlanEpicSpec`/`EpicID`); `sdks/openapi.yaml`'s `BoardPlanRequest` documents both arms. The legacy `epicTitle` create-arm spelling stays wire-accepted for the REST/external surfaces.

## [0.44.4] - 2026-10-27

### Fixed — board bounce re-dispatch: phrase→raw resolution through the WHOLE bounce path (#1959 root cause 2, PR #1967)

- **A phrase-addressed bounce no longer falls back to a fresh worker** (the token drain): the engine resolves the implementer ref upstream of the raw-keyed turn-evidence pre-check, and the api-side V1 seam resolves phrase→raw before the POST (platform registry consult inside the fast-fail window; raw `ses_`/legacy refs pass through byte-identical). Halt/resume directives and in-flight-claim aborts resolve through the same leg; a bare adapter no longer satisfies `boardV1Sender` (compile-level narrowing). Mutation-verified pins; the live phrase-in-row cause remains an honestly-recorded open question with pre-registered probe discriminators.

### Added — sidebar Workers folder + worker-session hard cap: oldest-first retention (#1966, PR #1968)

- **Worker sessions no longer flood the sidebar**: board/engine-spawned workers and subagent sessions folder into a collapsed "Workers" group (count badge, one-click expand, rows fully usable; classification is presentation-only, riding the new `worker` origin mark + parentage).
- **Past a per-workspace cap of 50 worker sessions, the OLDEST workers are hard-deleted on every create** (retention, not archiving): worker-only eviction re-verified by a fresh single-row store read immediately before the delete; mid-lease workers (< 1h grace floor) and the just-created worker are never deleted. Config: `LLMSAFESPACES_BOARD_WORKERSESSIONCAP` (default 50), `LLMSAFESPACES_BOARD_WORKERSESSIONGRACESECONDS` (default 3600).

### Added — board UI: archived projects collapse into a bottom drawer (#1964, PR #1965)

- **Archived projects leave the active board surface**: active-lane-only dropdown/chips/search derivations, a collapsed bottom drawer holds archived projects (name-only until clicked, full swimlane inside), a focused lane that archives anywhere resets scope to All, and an all-archived board shows an explicit "No active projects" hint. Presentation-only; the #1755 archive semantics are untouched.

## [0.44.3] - 2026-10-26

### Fixed — board: bounce re-dispatch + halt/resume directives ride the live V1 delivery route (#1959 follow-up, #755)

- **The bounce now re-dispatches the ORIGINAL implementer for real**: the #1959 seam was wired through opencode's dead V2 queue (delivery:queue, never drained on 1.18.x) — every bounce errored at delivery and fell back to a fresh spawn (full context re-acquisition, the token drain). Both worker-comms seams (re-dispatch + halt/resume directives) now deliver via the V1 /session/:id/message route, DETACHED (context.WithoutCancel + bounded budget), with a 10s fast-fail window preserving the fresh-worker fallback on dead sessions. The dead path is compile-blocked from the constructors (boardV1Sender narrowing); wiring pins run against a stub opencode mirroring 1.18.x (V1 live, V2 404).

## [0.44.2] - 2026-10-26

### Fixed — Test hygiene + one client classification: the three release-blocking race families de-flaked (v0.44.1 pipeline abort + PR #1958 red/green pair on identical heads)

- **`TestUsageBridgeStreamResynced_NonBlockingAndCoalesced` — the #1927 recurrence, closed structurally** (test-side; `api/internal/handlers`). The v0.44.1-era CI red (`expected: 1, actual: 2`, 0.11 s) was the shared-key TOCTOU #1927's marker-clear could not close: every harness in the package shared ONE workspace id (`busySnapWS`), the flight-coalescing marker (`busyFlightsInFlight`) is keyed by workspace id, and a leaked predecessor flight's deferred marker `Delete` (which runs strictly after its final publish — potentially after its test returned) deleted the KEY while the successor's first cue had already `LoadOrStore`d its own marker under it, unlocking the successor's rapid re-cues to spawn a second authority fetch. Fix: each harness mints a UNIQUE workspace id (`busySnapWS-<seq>`) — a leaked flight's deferred Delete can now only ever remove its own harness's key; the #1927 construction-time clear stays as a loud guard, and `TestBusySnapHarness_MarkerKeysPerHarness` pins the uniqueness so the shared-key channel cannot be reintroduced silently. Red-first: a planted exact-leak simulation reddens pre-fix main deterministically with the byte-identical CI signature (0.11 s, `expected: 1, actual: 2`); green ×5 post-fix under the same plant.
- **`TestWorkflowExecute_ScriptForwardTimeoutMaps504` — client-side timeout classification made deterministic** (production, `cmd/workspace-agentd`; `controlClient.ScriptExec`). The release-run red (`got 200 script_env_unavailable … read script_exec: read tcp …: i/o timeout`) was the two-clock race UploadApply's §3.3 classification was built to avoid: when the conn arm is DERIVED from the caller's ctx deadline, the parked read's `os.ErrDeadlineExceeded` and the ctx timer fire at the same absolute instant — classifying by `ctx.Err()` alone lets a loaded runner surface the read error while the timer callback has not run, misreading the script-timeout class as transport (HTTP 200 instead of the 504 the mux contract promises). Fix: adopt UploadApply's discipline — `callDeadline`'s `deadlineFired` on a derived arm IS the timeout class by construction; the `ctx.Err()` leg remains for timer-first wakeups. New pin `TestScriptExec_ClassifiesDerivedDeadlineAsTimeout` (accept-but-never-answer supervisor + 300 ms budget) holds the contract.
- **`TestUploadApply_ConcurrentWallTime` — wall-ratio discriminator replaced by seam overlap** (test-side; `cmd/workspace-agentd`). The 20:53Z CI red (`concurrent 4 applies took 195.8ms — near the serialized baseline 231.4ms`) false-failed on a runner whose -race CPU contention slowed the four concurrent 1 MiB copies ~4× while the applies were in fact fully parallel — the ratio bound made the serialized and concurrent phases load-biased in OPPOSITE directions. The pin now measures what it exists to prove: the rename seam's CONCURRENCY high-water mark (each apply holds the seam 50 ms — pure timer parking, no CPU), which is exactly 1 under the #1539 applyMu serialization signature and ≥2 when applies overlap, immune to copy-speed scaling. Mutation-verified: re-adding a serialization lock around `Apply` reddens deterministically (`max concurrent seams: 1`).
### Fixed — board bounce round-trip (#1959)

- **Verdict-driven bounce refusal (mint→stamp window)**: a fail verdict on an
  in_review row holding a live reviewer-persona claim returned
  `applied:false, refused:stage_transition` whenever it fired before the reviewer's
  async assignee spawn-stamp landed (~90s in prod) — and since the #1947 lapse-clear
  that window recurs on every reviewer lapse→re-arm cycle, so prompt reviewers (the
  normal case) NEVER bounced. The manual in_review→in_progress move refused through
  the same guard. The bounce now reads `tasks.implementer_session` (migration
  000051): the ORIGINAL implementer captured at the in_progress→in_review forward
  exit, preserved through the whole review lifecycle, immune to the reviewer
  claim's mint/stamp/lapse churn.
- **Bounce-target semantics (owner ruling)**: a fail verdict re-dispatches the
  ORIGINAL worker — the fresh claim token + the embedded verdict chain (findings as
  iteration history) delivered to the implementer's EXISTING session via the
  session-message seam (`board.WithBounceRedispatcher`, SendAsync — never a
  session:new spawn). A fresh-implementer spawn fires ONLY when the original is
  gone/unresponsive: never-turned (zero turn evidence), delivery failure
  (archived/reaped), or no captured identity. The mint stamps the implementer in
  the same statement (never a token-less in_progress row); attempts stay unchanged
  (a bounce is an iteration, not a failure). #1958's episode-unique RunIDs hold
  across bounce rounds (pinned).

## [0.41.0] - 2026-10-23

### Added — board dispatch durability + stage-machine hardening (the #1832 lane)

- **Board re-arm + stillborn guard** (#1869, #1870, feat/board-rearm /
  feat/board-stillborn-evidence): the reconciler re-arms reclaimed claims
  immediately and records stillborn evidence for zero-message spawn
  failures, so a dead spawn no longer burns a full lease window invisibly.
- **Platform-derived liveness** (#1862, the #1847 ruling): LLM heartbeats
  retired as the primary lease-liveness mechanism; liveness reads
  platform-observed session activity.
- **Board reads criteria** (#1864) and **board tool hardening** (#1863):
  criteria surface on board reads; manager-tool guardrails tightened.

### Fixed

- **#1878** — trigger inline scripts + workflow script nodes execute in the
  session environment (sidecar lacked /tmp, interpreters, gh) (#1885).
- **#1886** — sidebar row cap must not evict live unarchived sessions
  (#1902); deterministic ordering keeps NULL-recency dispatch rows visible.
- **#1882** — delete propagation (#1887).
- **#1842** — upload stream flake: product-race vs test-flake stop-rule
  resolved, race suite de-flaked (#1888).
- **#1844** — run watchdog + secrets dispatch re-register (#1874, #1876,
  #1894).
- **#1846** — chat textbox up-arrow recall no longer fires mid-text (#1875).
- **#1850** — create_session workers keep their minted phrase ids —
  mint-in-create-transaction, DB-column persistence (#1881).
- **#1884** — usage stream resync (#1899).
- **#1832** — cascade integration (#1893).
- **#1860** — e2e harness hygiene: lifecycle error coverage + dead route
  purge (#1903).
- **#1756** — retro completion r4: 7th surface + record corrections (#1900).
- **#1856** — ai-workflows v0.2.24 reapply (#1896).


## [0.44.1] - 2026-10-25

### Fixed — board: episode-unique workflow RunIDs for worker claims (#1957)

- **Re-claimed tasks no longer spawn stillborn workers** (#1957 BUG A,
  live-proven): the spawner's workflow-agent RunID for a plain worker
  claim was the BARE task id — agentd's workflow-agent mux keys charter
  delivery on the (workflow, run) pair, so after a task's first
  dispatch every re-claim (bounce, cooldown re-arm, lapse re-feed)
  re-used the key and the charter was silently dropped: a session
  spawned with zero tokens and lapsed at +15min, forever. RunIDs are
  now unique per episode by construction (task + stage tag + the fresh
  claim token); the dispatch closure is pinned end-to-end over
  interface seams (the reviewer's bypass mutation verified RED).
  BUG B (the reviewer persona's missing toolset) was the stale
  digest-pinned agentd image — rolled to the v0.44.0 build
  (talos-ops-prod#2854).


## [0.44.0] - 2026-10-25
### Fixed — board: creation into terminal epics refuses loud (#1948)

- **`board_create_task` / `board_import_plan` no longer land rows in a
  terminal target silently** (#1948): a task created into a COMPLETED
  epic succeeded with no error and then sat permanently unfed — the
  claim set gates on `e.status='active'` (candidateSQL) /
  `p.status='active'` (eligibleProjectsSQL), none of which surfaced at
  create time; the live evidence sat 20+ minutes before manual
  inspection caught it. Now `CreateTask` refuses (409
  `epic_state`/`project_state`, error naming the epic/project and its
  state, with the remedy — reopen/unarchive first) for terminal epics
  (completed, archived) and archived projects; `ImportPlan` refuses the
  WHOLE transaction for an archived project (its terminal target — the
  tool mints a fresh draft epic, so a terminal target EPIC is
  structurally unreachable there) before any write, all-or-nothing
  preserved. PAUSED epics still accept creates (pause is a scheduling
  lever; resume re-feeds — pinned explicitly), as do DRAFT ones
  (create-then-activate is the documented flow). The guards are
  two-layer — pre-write fast-fail AND an in-transaction re-check under
  row locks (`FOR UPDATE`, the task_start precedent) so a CONCURRENT
  completion/archive between the read and the commit still refuses
  (pinned via deterministic failpoint races); `board_create_epic` into
  an archived project refuses the same way (the sibling seam). MCP tool
  descriptions
  document the guard. Fixture-only adaptations in two kill-tests that
  seeded rows into terminal epics incidentally (intent preserved:
  task_start refusals; requestId idempotency). Reopen-with-audit
  (auto-reactivating the epic on create) was considered and deliberately
  left as a follow-up — refuse-only is the minimal, truthful semantics.
### Fixed — stage-persona spawn defects (#1947)

- **Reviewer persona charter no longer carries the implementer
  trailer** (#1947, the probe-2 live evidence): a persona-bearing
  charter closes on the persona's outcome — the shipped trailer
  ("Work the task to completion … move the task to in_review when
  the work is done") rode the reviewer's charter too, and the
  spawned reviewer executed the task's work and called
  board_move_stage instead of recording its verdict. The
  persona-less charter is byte-identical to today.
- **The verdict channel for spawned reviewers** (#1947): the
  reviewer persona the engine spawns is a WORKER session —
  `board_record_verdict` now exists in the agentd worker registry,
  rides the pod automation seam
  (`POST /internal/v1/automation/board/tasks/verdict`), and is
  guarded by the in_review row's LIVE claim token at the engine
  (the reviewer's own; a wrong or missing token refuses; the
  owner/REST surface is unchanged).
- **Persona lease-lapse now clears the dead claim's assignee** (rc1
  parity; #1947's 03:49Z finding): the in_review lapse pass left the
  corpse assignee recorded while rc1's in_progress reclaim clears
  it — and the #1832 WS4 stillborn guard reads the row's assignee,
  so every re-armed reviewer claimant's heartbeats consulted the
  dead prior worker's zero-turn evidence and were refused (fresh
  claimant every 15 minutes, none renewable). The lapse's accounting
  branch now clears `assignee_session`; the unclaimed no-persona
  convergence branch keeps today's retain-identity semantics.
- **Race pins for the production async spawn shape** (#1947): the
  charter dispatch blocks for the spawned session's first turn, so
  a freshly-granted task sits claimed-but-unstamped and every
  persona mint opens a mint→stamp window — none of which the sync
  spawner tests modeled. New PG-harness pins (gated spawner):
  no-mechanical-flip without a token-bearing move (S1),
  immediate-move containment — one reviewer-persona spawn on
  in_review entry, the parked implementer's late stamp aborted
  (S2), the re-arm heartbeat window (S5), and the verdict-channel
  token guard (S6); all mutation-verified.
### Added — reviewer-persona criteria-met grant (#1946, owner ruling 2026-10-25)

- **Reviewer personas can record criteria-met on their claimed row**:
  ruling #1747 §5's blanket surface separation caught the auto-review
  reviewer persona in the task-worker net — it could verify acceptance
  criteria but never RECORD them met (setCriteriaMet was
  manager-surface-only), so auto-review could not close a task
  end-to-end (the done guard R2 refused the reviewer's →done until a
  user-collaborator hand-set the flags). The owner ruling ("Why is set
  criteria met manager only. It should not be.") grants it, scoped on
  all three axes: **who** — only a reviewer-persona claim,
  distinguished by the #1941 mint's provenance made durable
  (`tasks.claim_persona`, migration 000050 — non-NULL only while a
  persona episode's token holds the row; every non-persona mint and
  token-clearing write NULLs it); **what** — only setCriteriaMet's
  per-criterion flag walk, audited per-criterion (action
  `criteria_met`: index, flag, criterion text, persona name); **where**
  — only the caller's OWN claimed row (live claim-token arbitration
  under the row lock). Surfaced as a worker route on the pod seam
  (`POST /internal/v1/automation/board/tasks/set-criteria-met`) with a
  strict loud vocabulary (any field beyond addressing + token + the one
  check refuses by name), and agentd's `board_update_task` dual-routes
  on claimToken presence (the #1832 WS2 shape). Task-worker
  self-certification REMAINS forbidden — the token-less manager shape
  keeps the tier-1 #1747 §5 refusal byte-identically, and an
  implementer's token-carrying call refuses with the same error class
  (`manager_surface`, the §5 sentence preserved verbatim in the new
  engine refusal text). The default reviewer persona's charter now
  teaches the loop: verify → record flags → move done, zero manager
  touches.
### Added — relay-router aged-arg compaction, the poisoned-context emission cure (#1949)

- **Aged `llmsafespaces_*` prior-call argument stubbing on the BYO resolve
  path** (#1949, the #1849/#1929 follow-up): chat-completion request
  bodies passing through the relay-router get tool calls older than a
  recent window (last N tool calls of any tool, default 30,
  `BYO_AGED_ARG_RECENT_TOOL_CALLS`; 0 = stub all, negative = kill
  switch) rewritten — bulk-first old-shape arguments (the pattern-
  continuation poison #1929 replay-proved: 12/12 runaway; production
  confirmations 2026-10-25) replaced by a one-field note. Call identity,
  tool name, and tool RESULTS are preserved; the recent window is
  untouched; the on-disk session store is untouched (the router never
  persists bodies). Passthrough-safe (anything unexpected forwards
  byte-identical) and idempotent across re-sent turns. Every fire is
  loudly observable: metadata-only log line +
  `llm_relay_byo_aged_arg_{compactions,calls_stubbed,bytes_saved}_total`
  metrics. Ships with the checked-in replay harness
  (`scripts/1949-aged-arg-replay.py`: synthetic poisoned transcript +
  exact-context mode + signature scorer, per the #1849 adjudication's
  regression plan) and the Go discriminator pin
  (`TestAgedArgSyntheticReplayCure`: recorded production runaway shapes
  → compacted → scorer-clean). Live validate-first evidence (13 runs,
  5 legs, glm-5.3): the un-stubbed runaway could NOT be re-fired on
  today's serving path — documented in the worklog as an evidence limit
  of the live leg, with the deterministic pins carrying the regression.
### Added — session bulk variants (#1836, partial)

- **Bulk session archive/delete with per-item result arrays** (#1836's
  session siblings — the lived pain: archiving ~48 sessions cost ~48
  context-resending tool round-trips): `session_archive_bulk` and
  `session_delete_bulk` on the agentd MCP surface (session_archive's
  and delete_session's batch twins — up to 50 refs in ONE call, every
  item one full single-tool delegation: `resolveSessionRef`
  phrase/alias/raw dual-accept, then the pod-identity platform write).
  Contract: per-item `{id, status: done|failed, archived_visibility?,
  error?}` — partial failures per item, never all-or-nothing; one
  malformed id sinks only its own item; batch capped at 50
  (`board.BulkMaxItems`, single-sourced). Idempotent retry: repeat
  archive returns done (the platform seam's root-row write is
  idempotent); delete follows the destructive ruling recorded on #1836
  — absence ≡ miss ≡ foreign, all three the same bare `not_found`
  item. The self-delete guard is per item (`self_delete_refused` —
  the batch keeps processing); done archive items carry #1879's
  `archived_visibility` from ONE shared post-write confirmation read.
  The origin plugin stamps the delete bulk for the guard's injection
  half (lockstep pin extended); the #1816 §2 deprecation notice scans
  array args.
- **Fixed (review r1 F1 + r2 F1)**: the pod-identity internal delete
  route (`POST /internal/v1/session-delete`) classifies the agent-side
  session-not-found verdict as 404 in BOTH authority regimes — the
  adapter's `Adapter.DeleteSession` now wraps `agent.ErrSessionNotFound`
  on a delete-route 404 (the #1340 definitiveness the session-READ
  routes already carried; the Act regime's connect `not_found` was
  already classified) instead of the route flattening every failure to
  502 — live absence renders the bare `not_found` shape the
  destructive-variant retry contract keys on; every other failure keeps
  the honest 502. Both bulk forms carry origin-plugin e2e legs
  (`TestOriginE2E1836_*`, CI-selected).

### Added — board bulk delete variant (#1836, partial)

- **Bulk task delete with per-item result arrays** (#1836's
  delete_task(s) batch — the issue's first-listed board sibling):
  `BulkDeleteTasks` at the engine seam (each item ONE full
  single-task DeleteTask — its own transaction, snapshot-bearing
  audit event, orphan events + `is_blocked` rederive, post-commit
  worker abort; ONE shared `reconnect` flag), rendered on REST
  (`POST /api/v1/me/tasks/bulk-delete`), the agentd manager surface
  (`board_delete_tasks` — delete IS a manager action; only
  claim/create carry the owner-only boundary), and the external
  platform MCP (`board_delete_tasks`). Delete-variant retry
  semantics: absence ≡ miss ≡ foreign — all three render the same
  bare `not_found` item (never disclosed, never a conflict on
  re-submit); done items echo `orphaned_dependents`/`reconnected`.

### Added — board bulk claim/complete variants (#1836)

- **Bulk task claim/complete with per-item result arrays** (#1836):
  `BulkTaskStart` + `BulkMoveStage` at the engine seam (each item ONE
  full single-task delegation — its own transaction, audit event,
  `board_transitions` row, spawn/directive side effects), rendered on
  REST (`POST /api/v1/me/tasks/bulk-start`, `…/bulk-move-stage`), the
  agentd manager surface (`board_move_stage_bulk` — the bulk claim
  deliberately stays off it: task_start is owner-surface only,
  round-3 #2), and the external platform MCP (`board_start_tasks`,
  `board_move_stage_bulk`). Contract: per-item `{id, status:
  done|failed, escalated?, error?}` with partial failures reported,
  never all-or-nothing; idempotent retry (already-landed items return
  done, not conflict); one malformed id sinks only its own item; batch
  capped at 50. Bulk projections carry NO claim tokens (#1864 ruling
  extension); foreign rows render the same `not_found` item a miss
  renders (never-disclose parity). Validate-first deltas vs the spec
  documented in the worklog.

### Fixed — Test hygiene: the #1922 phrase pins revived after the #1849 atomic-create contract (#1922 lane, validate-first)

- **Three of the five #1930 raw-ID-leak pins were dead on main** (RED on
  the Secrets Integration job at the #1933-merge push, run 40090453099):
  #1904's atomic-create contract (description + ≥1 acceptance criterion
  REQUIRED at create, #1849) landed after #1930 the same morning and its
  "12 direct CreateTask sites" fixture sweep missed the four CreateTask
  fixtures inside #1930's pin files — the guard rejects the seedless
  fixtures before any phrase assertion runs, so the pins die at setup
  and the #1922 leak guards stop executing. Fixtures repaired (charter
  description + one criterion each, asserting nothing new): the
  replay-echo pin (`pkg/board/phrase_read_integration_test.go`,
  CI-visible red) and both SSE pins
  (`api/internal/handlers/board_sse_phrase_integration_test.go` — its
  CI step was skipped behind the board kill-test red, so latent).
  Translation itself verified INTACT on main (all five pins pass once
  fixtures satisfy the contract); mutation-verified the revived pins
  still bite (replay-echo tail translation disabled → `amber-falcon-
  glides` assertion red; SSE actor emission disabled → `amber-heron-
  waits` assertion red). Test-only; no production changes.

### Fixed — Test hygiene: the usagestream resync/flight family no longer reddens on byte-identical content (#1927)

- **Cross-test contamination through two package-global state cells
  rooted out, test-side only** (the #1910/#1916 discipline extended to
  the whole family). The `TestUsageBridgeStreamResynced_*` /
  `TestStreamUserEvents_ConnectPathFlightNotCoalesced` family reddened
  ~2/5 in the wild because (A) the per-IP SSE connection budget
  (`sseConnCounts`, 10/min per loopback IP) is shared by every
  SSE-connecting test in one test process — in-process stress
  (`-count>1`) and fast family+neighbors runs exhaust it, and the 429
  JSON body reads to an SSE collector as "no events" (silent 0.00 s
  reds), and (B) the on-demand flight-coalescing marker
  (`busyFlightsInFlight`, keyed by the shared `busySnapWS` id) is
  deleted only in the flight goroutine's defer — a flight finishing
  after its test returns leaks a marker that swallows the next test's
  first cue ("resync flight incomplete before deadline; got []").
  Fixes: `newBusySnapHarness` clears the workspace's stale marker at
  construction and wipes its loopback budget consumption on teardown;
  `collectSSE` starts each connect from a live budget and fails loudly
  on any non-200 (a budget regression now names its status instead of
  masquerading as a missing flight); `ConnectPathFlightNotCoalesced`
  gains #1916's defer-based idempotent park-release (a fatal inside a
  collector while fetches are parked no longer strands the stub's
  `srv.Close` to the 10-minute timeout). Red-first: pre-fix
  origin/main redden 15/20 at `-race -count=20` (exactly the connects
  past the 10/min budget); mutations (neutered budget reset with a
  planted consumed budget; neutered marker clear with a planted stale
  marker) redden deterministically with the named signatures; post-fix
  270 in-process runs + 20 fresh-process runs across GOMAXPROCS 1/2,
  `-race`, and the CI `-short -covermode=atomic` shape: 0 reds.

### Added — Task Board: atomic-create seed validation (#1849 items 1+6)

- **A task CREATE requires a charter and criteria**: non-empty
  description (the worker's charter) and at least one acceptance
  criterion are mandatory at create on every surface (service-layer
  guard; import_plan dry-run refuses seedless nodes the same way —
  the approval moment never blesses one). Updates stay exempt (patches
  clear fields deliberately).
- **Placeholder-class charters reject at the boundary** (#1820/#1814
  discipline, shared vocabulary): whole-value placeholder tokens
  (placeholder, placeholder-removed, <placeholder>, todo, tbd, asdf,
  qwerty — trimmed, case-folded) reject with `text_rejected` in title,
  description, or criterion text; prose that merely mentions a token
  passes, short legitimate payloads pass. send_message's guard now
  delegates to the ONE shared set (pkg/board).
- Composes with #1837's guardrails: caps and corruption scans run
  first, the seed contract second — a corrupted OR seedless create
  never stores. agentd's board_create_task description teaches the
  contract (Layer 2).
### Fixed — Automation: auto-disable disarms webhook triggers; the routine path's circuit breaker stops wedged-outage hammering (#1834)

### Fixed — Board reads: assignee/actor session ids emit the phrase at the read edge, never the raw harness id (#1922)

- **Task Board public reads translate `assignee_session` and
  `board_events.actor.session_id` to the #1756 session phrase at the
  platform service read edge** (#1922, the #1919 Part-2 audit's filing):
  task drawer/read/list/search, health leases (both perimeters), the
  agent-surface projections (`board_list`/`board_health`/worker
  `board_read` — the manager MCP pass-through rides the same seam),
  stage-move/verdict/pause/resume/update echoes, the task audit timeline
  (`GET /me/tasks/:id/events`), and the SSE `board.event` actor. Stored
  rows stay canonical forever — `scanTask` and the internal consumers
  (bounce aborter, reconciler, stillborn guard, halt/resume messenger)
  keep the raw harness id; `board_events.actor` rows are never rewritten
  (read-time translation on the response copy only).
- **Degraded posture follows the #1817/#1821 SSE precedent**: phrase
  when the `session_index` knows the session (project target workspace,
  bounded ≤1 consult per distinct workspace); the raw id + additive
  `assignee_session_deprecated` flag while the design-0066 registration
  seam has not yet observed the session (never worse); the actor's flag
  is `session_id_deprecated` in `markDeprecatedSessionID`'s exact shape.
  No `internal_assignee_session` — no consumer demonstrated correlation
  need.
- **Review round 2 closings**: requestId-replay echoes (CreateTask's
  outcome-id path, RearmTask's tail reload) and the TaskStart echo's
  embedded task copy translate identically; the SSE `board.event`
  payload leg (delete/cascade task snapshots) translates alongside the
  actor in one bounded consult.

### Fixed — Session-ID emission: the agent-message sentinel carries the phrase, never the raw harness id (#1919)

- **The delivered `agent-message-v1` sentinel's `fromSession` emits the
  platform 3-word phrase** when the agentd short-id replica knows the
  mapping — the last un-migrated id emission in the injected-origin
  flow. The send_message result fields already phrase-emitted
  (#1756/#1806); the sentinel still carried the resolved canonical
  `ses_…` into the recipient's transcript and the frontend origin
  badge regardless of replica warmth. Validation (by-ID probes),
  the self-send guard, and the #1530 recovery comparisons stay
  canonical; a phrase-valued `fromSession` remains a valid reply
  address (resolution accepts short ids). The raw form survives only
  the degraded cold-replica window, flagged by the result's
  `origin_deprecated` — the #1756 §1 contract every other emission
  already follows. The origin plugin's wire attestation
  (`lsp_injected_session`) keeps the canonical form per the #1920
  ruling (agentd-internal, never model-visible).
- **Audit**: the accompanying sweep classified every raw-ID surface —
  SSE/REST/MCP emissions clean (phrase edge + additive internal ids +
  deprecated flags); board surfaces (`Task.assignee_session`,
  `board_events.actor.session_id`) leak raw and are filed as #1922
  (fix carries design choices); `X-Session-Ref-Resolved` and the
  engine↔agentd envelopes are by-design canonical.

- **A disabled trigger's webhook deliveries are absorbed, never
  executed.** The cron claim already filtered `enabled=false`, but the
  webhook receiver minted executing fires for disabled triggers
  regardless — so auto-disable (the `consecutiveFailures` /
  `autoDisableAfter` circuit) was decorative on the webhook route.
  During the 2026-10-21 routine-path outage (01:20–22:54Z, the
  headless opencode invocation surface wedged post-OOM across
  workspaces), an auto-disabled trigger (pr-monitor-sweep: ~30 failed
  fires, `cf=0` under the pre-#1878 accounting) kept dispatching the
  wedged path on every signed delivery — the platform's own breaker
  could not stop the bleeding it exists to stop. The receiver now
  checks `trigger.Enabled` after HMAC/skew/dedup (auth-first: no
  unauthenticated oracle for trigger state) and records a
  self-explaining `skipped` fire (`{"reason":"trigger_disabled"}`) with
  a 202 — a non-2xx would make GitHub-style senders retry-storm a
  trigger the platform just disarmed.
- **The pending-fire drain skips disabled triggers too** — a fire
  minted before the disable (a delivery racing the auto-disable, or
  pre-fix residue) no longer rides `processPendingRoutineFire` into
  the agent leg. The gate-closed discipline applies: absorbing is not
  failing, so no failure accounting, no counter movement.
- **The workflow shape's residue is gated at run execution** — a run
  minted before the disable claims into the Reconciler, which now
  re-checks the owning trigger and CANCELS the run (named reason
  `trigger_disabled`, before workspace activation — the expensive leg)
  with the audit mirrored onto the run's fire row. A trigger row that
  cannot be confirmed disabled (store fetch error, trigger gone) is
  not a disable verdict: the run executes as before.
- **Re-enable-while-pending semantics (stated per review):** a
  delivery absorbed while the trigger was disabled — at the receiver
  gate, or as a pending fire skipped at the drain — is consumed, never
  replayed after a later re-enable. Stale deliveries executing late
  would be worse; owners re-arming a trigger send a fresh delivery.
- **The outage's error shape is pinned fail-loud**: a retry-exhausted
  `script_failed` / `"opencode returned 500"` envelope (every attempt,
  the exact 15–25s class the investigation recorded) fails the fire,
  carries the code+detail verbatim into the audit, and counts toward
  auto-disable exactly once — the #1441/#1878 accounting family,
  extended to the retry-exhausted agent-leg shape.
### Fixed — Longhorn RecurringJob snapshot cron invalid day-of-week; deploy-tree cron validity guard (#1905, cross-repo)

- **The `default-snapshot` RecurringJob's cron `0 1 * * 8` is corrected
  to `0 1 * * *` (daily 01:00)** in lenaxia/talos-ops-prod
  (`kubernetes/apps/storage/longhorn/app/recurring-jobs.yaml`,
  PR talos-ops-prod#2795). Day-of-week 8 is outside the valid 0-7
  range — `8` being the missed-Shift form of `*` (Shift+8) — so
  Longhorn's robfig parser rejected the job and scheduled snapshots
  never ran as declared. The daily cadence matches the job's own
  `dailySnapshot` label (sibling convention: `0 0 * * 1` ↔
  `weeklyBackup`); intent verification posted to the issue. Owner
  ruling: the Longhorn recurring-jobs/backup posture is intentionally
  dormant (prod backups run on Kopia via the kyverno
  snapshot-cronjob-controller path) — this is latent correctness of
  the declared manifest, no re-enable.
- **A red-first deploy-tree cron validity guard** now parses every
  literal cron expression under the ops repo's `kubernetes/` tree
  (`cron:`/`schedule:` string fields) against a 5-field validator with
  standard ranges (minute 0-59, hour 0-23, dom 1-31, month 1-12, dow
  0-7) — the same class as the platform trigger validator. Six-field
  CloudNative-PG schedules validate seconds 0-59 plus the standard
  five; `@`-shorthands form-checked; sops ciphertext and `${...}`
  templates excluded; unparseable files frozen behind an explicit
  allowlist; walker-regression pins (harvest floor + longhorn
  coverage) prevent vacuous passes. Red pin `733bce3d` (guard alone:
  1 failed, 55 passed — failing exactly on the dow 8), green
  `6d9a5aa3` (56 passed).
### Fixed — carrier-reap follow-ons: kind discriminator, drain tombstone visibility, restart posture (#1901)

- **The #1882 carrier reap bypass now admits ONLY platform-deleted
  carriers.** The blanket "any `session_origins` mark admits a
  history-bearing row past the reap guard" verdict could not tell an
  ephemeral carrier (reap-admissible: its absence from the harness is
  platform policy) from a PRESERVED session's persistent mark
  (`preserveSession: always` / `on_failure`-failed) — a harness store
  reset would have reaped the preserved session's history row, the
  exact `kept_for_operator` scenario for it. Migration 000046 adds
  `session_origins.kind` (`'carrier' | 'session'`, DEFAULT `'session'`
  — the fail-safe backfill: stale pre-migration carrier marks stop
  being reaped rather than risk wrong deletion); the engine records
  the session's fate at mark time (`propagateCarrierDelete` →
  carrier, `recordRoutineSessionArtifacts` → session) and the store's
  carrier-marks lookup feeds both reap paths (reconcile plan +
  read-path) — preserved marks are unreachable at the guard.
- **The session-index drainer no longer re-mints deleted sessions'
  rows.** A queued message-count event whose session was deleted
  between enqueue and drain previously applied unconditionally: the
  phrase-bearing INSERT leg birthed a fresh visible ghost row
  (`message_count ≥ 1` → reaped never — the #1882 harm at drain-race
  rate). The drain now consults the wsstate tombstone (both legs —
  live and shutdown flush; nil-unwired keeps pre-fix semantics) and
  drops tombstoned events as a queryable
  `session_index_events_total{outcome="drain_skip_tombstoned"}`
  outcome; the consult inherits the Redis store's fail-closed outage
  posture (an outage suppresses re-mints rather than minting).
- **Restart posture pinned.** API restarts wipe the in-memory
  tombstones and the known-registered LRU; a late pod event can
  re-mint a dead carrier's row (accepted per the store's design). The
  durable `kind='carrier'` mark survives the restart and the reconcile
  reaps the re-born row — the converging backstop, now pinned.

### Fixed — session_index.message_count drift to 0 on live sessions: listing serve-through, loud skips, and rebuild rotation (#1880)

- **The sidebar's session list now serves the HARNESS-walked message
  count for the drift class** (board/dispatch-spawned sessions whose
  message stream never traverses the API proxy — `message_count = 0`
  with a full harness history, the 10-row live evidence). The listing
  route walks the count BY ID (immune to the #1452 list-visibility
  gap), serves it on the wire in place, and persists it through the
  rebuild seam — converging across listing polls (budget 3/request,
  never-walked-first rotation, 5-minute unchanged-row cooldown); a
  failed walk serves the index value and re-arms (no tracker entry),
  a failed persist still serves the truth and retries. Reaching every
  row is unconditional: #1886's uncapped `ListSessionIndex` removed
  the 100-row page cut that previously hid NULL-`last_message_at`
  rows past the budget.
- **Every rebuild skip is now attributable.** `count_pass_ran` (the
  denominator — its absence under sidebar traffic means the pass is
  never scheduled, the issue's third skip class),
  `count_skip_not_present` (list-invisible count-zero rows, the #1452
  gap), `count_skip_pass_no_list` (harness list failure), walk
  failures Debug→Warn with row identity, and the rebuild rotation:
  the pass window advances `(passIndex × K) mod N` per 30s cadence
  bucket, repairing the stationarity defect where the same first-K
  NULL rows were re-walked every pass while the starved tail never
  converged.

### Added — zero-fill corruption: detection, push guard, and flush belt (#1808)

- **`POST /api/v1/workspaces/:id/fs-audit` — fsck-style detector for
  the zero-fill corruption class** (files silently replaced with NUL
  bytes at preserved length; the second sandbox-filesystem corruption
  class after #1793, which reached the REMOTE via a pushed commit).
  The engine (`pkg/fsaudit`, executed by agentd on the PVC view —
  same topology as #1601 disk-recover, including the sidecar
  control-socket leg) flags a file only on the exact signature:
  `size > 0` AND 100% of content bytes NUL AND non-sparse
  (`st_blocks*512 >= size` — ftruncate/seek-created all-NUL files
  report `sparse`, never flagged: the false-positive trap a naive
  zeros-grep walks into). Response-only (the #1601 ruling, inherited:
  the scan writes nothing); bounded (4KiB-chunk reads with early exit
  — clean files cost one chunk; per-file 64MiB / total 256MiB read
  budgets, defaults overridable per request); honest about
  incompleteness (an all-NUL file larger than the per-file cap reports
  `suspect` — 100%-NUL unconfirmed — and an exhausted total budget
  sets `truncated` rather than masquerading as a clean tree). Request
  roots are grounded INSIDE the workspace (deny-by-default: no root
  can point the scan elsewhere). Zero-filled bytes are unrecoverable —
  the report's job is visibility (rebuild the dead files from source
  or a fresh clone), not repair.
- **Pre-push guard: all-NUL blobs no longer reach the remote.** The
  runtime base image installs an executable `pre-push` hook via git's
  template dir (`/usr/share/git-core/templates/hooks/pre-push`), so
  every NEW `git init`/`git clone` in a workspace gets it active
  (existing clones untouched — new-clone-forward rollout). The hook
  scans the pushed range's blobs (`git rev-list --objects` +
  `cat-file`) with the same all-NUL signature — git cannot notice the
  class itself (an all-NUL blob is valid zlib data). Doctrine mirrors
  the #1793 belt's never-worse rule, both directions pinned: fail
  CLOSED on positive detection (the dead-file list is printed with
  recovery guidance; the remote is the last unclean copy), fail OPEN
  on any scanner/tool error, and `LLMSAFESPACE_PREPUSH_AUDIT=0` is
  the documented opt-out for legitimate all-NUL files.
- **Shutdown flush belt (the H1 window).** The suspend path (pod
  delete → bounded graceful termination → unmount → CSI detach) never
  proactively flushed dirty pages — the platform-controllable slice
  of the ext4 delayed-allocation hypothesis (an unclean stop can
  persist i_size while data blocks never land: zero-fill at preserved
  length on the next mount). Both agentd shutdown paths that hold the
  RW PVC view (single-container `runShutdown` and the sidecar-mode
  supervisor) now trigger a filesystem-wide writeback flush as their
  last act, bounded at 10s — the kernel completes a triggered
  writeback asynchronously even after the triggering process exits,
  so the bound can never hang shutdown.

### Fixed — board heartbeat: server clock bounds client-observed stamps (issue #1847 PR1, the C1 incident class)
- **A future-stamped `observedProgressAt` rejects outright**
  (`invalid_input`)**.** During the 2026-10-22 board dogfood a worker
  hallucinated a timestamp ~7.5h into the future; the server accepted
  it as progress evidence and stored it as the lease's progress
  watermark — which then became unrenewable by construction (every
  honest later observation failed the moved-watermark evidence gate),
  locking a live worker out of its own claim. Stamps more than
  `heartbeatFutureTolerance` (2m) ahead of the server clock are
  estimates, not evidence, and no longer touch the watermark or the
  lease.
- **Mild future skew clamps to the server clock** (a future stamp is
  never stored — the poison direction only); honest clients with
  drifting clocks keep extending. Past-skewed stamps remain
  client-trusted until the #1847 rework retires worker-declared stamps
  as the liveness input entirely.
- **Already-poisoned watermarks self-heal, one-shot**: a stored
  watermark reading future (pre-fix damage, or DB-ahead/app-behind
  clock skew) reads as stale, so an honest present-t observation
  extends once and overwrites it; a re-sent identical stamp does not
  extend again — the C1 lockout cannot recur against repaired rows.

### Fixed — Automation: platform-deleted sessions stop haunting the sidebar (#1882)

- **Every session delete now propagates.** Routine-trigger fires used
  to mint an ephemeral "carrier" session per fire (deleted seconds
  later) whose deletion announced itself nowhere — no
  `session.status: deleted` event, no tombstone, no `session_index`
  cleanup — while the live event stream indexed the carrier during
  its brief life. With watch triggers live that was ~75 ghost sidebar
  rows per hour that vanished only when clicked (and history-bearing
  rows survived even clicks). All platform-initiated deletes (agentd's
  ephemeral teardown, the engine's authorized deletes: PreserveOnFailure
  success cleanup, superseded retry attempts, DAG node-retry cleanup)
  now run the same effect sequence as the user delete route, with
  agentd reporting the deleted session's id on the node envelope
  (`deletedSessionId`) so the engine can act on it; the carrier is
  also origin-marked first so a partially-failed cleanup still
  self-heals via the reconcile. Belt-and-braces on the read path: a
  new durable `session_index.deleted_at` death mark (migration 000045)
  keeps a lingering row out of the session list even when the row
  delete itself fails. The #1340 reconcile now reaps
  `session_origins`-marked carrier rows even with history (the
  harness-store-reset exception does not apply to platform carriers —
  the #1850 by-ID probe confirmation still gates every reap), so
  accumulated marked residue drains without manual surgery.

### Fixed — chat UX

- **Mobile is no longer stuck on the first of several queued agent
  questions** (#1845): every pending ask used to render as a stacked
  card in the message stream, which on a phone viewport surfaced only
  the topmost card with no indication more were queued — and the stack
  orders newest-first, so the surfaced card was the NEWEST ask. Mobile
  now presents ONE ask of each kind at a time, the OLDEST pending
  (first-seen order — the provider's standing semantics; asks seeded
  from one fold snapshot now carry strictly-monotonic first-seen stamps
  so same-batch asks can never tie and leak insertion order through the
  newest-first sort), with a "Question 1 of N — answer to see the next"
  queue-depth line; answering or dismissing advances to the next ask
  automatically. Desktop rendering is unchanged.
- **Up-arrow history recall no longer destroys the in-progress draft**
  (#1846): the Composer's Up-recall guard checked only the caret's LINE
  (any column of line 1 qualified), so pressing Up with a non-empty
  single-line draft swapped the entire draft for the previous message —
  silent user-data loss. Recall now fires only from a collapsed caret at
  the very beginning of the buffer (position 0 of line 1 — the opencode
  TUI buffer-boundary rule the guard was always meant to mirror); any
  other caret position moves the cursor and leaves the text untouched.
  A selection, even one starting at position 0, collapses rather than
  recalls. The empty-input recall gesture and the caret-at-0 recall are
  unchanged; Down-navigation is unchanged.

### Added — workflow_dispatch outage levers for PR-triggered workflows (#1844, PR 1 of 2)

- **`pr-review.yml` is now dispatchable — with a guard, not a bare
  trigger.** The reusable ai-workflows review workflow at v0.2.23 keys
  every step on `github.event.pull_request.*`; a naive dispatch would
  run it with EMPTY PR context (burned LLM round, verdict check against
  an empty SHA). The reusable job is therefore pinned
  `if: github.event_name == 'pull_request'` and dispatches land in a
  new `dispatch-recovery` job: resolve the PR (input or auto-detect
  from the dispatched ref), noop if the head already carries a bot
  verdict, else post the #1844-namespaced fault comment (deduped per
  head SHA on the `<!-- run-watchdog: fault sha=... -->` marker shared
  with the automated sweep) and re-dispatch `ci.yml` on the branch —
  the recovery verified live during the 2026-10-23 branch-scoped
  suppression. Forcing an actual AI review round from inside this repo
  remains impossible at v0.2.23 (documented in the workflow header);
  the levers stay an admitted `/review` comment or an empty
  `ci: retrigger` push.
- **`secrets-integration.yml` gains a bare `workflow_dispatch`** — its
  changes-gate keys on `event_name != 'pull_request'`, so a dispatch
  always runs the full suite, exactly what an operator forcing
  recovery wants. `ci.yml` and `posture-gate.yml` already had dispatch
  (ci.yml's is the verified lever); e2e-pr.yml is noted for its owner
  lane.

### Added — run watchdog: automated detection + recovery for dropped synchronize events (#1844, PR 2 of 2)

- **A zero-run PR head no longer stalls silently.** Every 10 minutes
  the new `run-watchdog` sweep enumerates open PRs; a head ≥15 min old
  carrying ZERO total check-runs (pr-review.yml is the universal
  check-runs producer — every pull_request, no branch/paths filter)
  and no Actions skip marker is a #1844 delivery fault → namespaced
  comment on the PR (tagging #1844 so instances accumulate as
  evidence; deduplicated per head SHA via the `<!-- run-watchdog:
  fault sha=... -->` marker — the identical literal pr-review.yml's
  dispatch-recovery shim posts once that PR merges) +
  `workflow_dispatch` of ci.yml on the branch — the recovery verified
  live during the 2026-10-23 branch-scoped suppression. Auto-retrigger
  is OPT-OUT (label `watchdog:no-auto-retrigger`) and HONEST about its
  cost: a dispatched ci.yml run pushes a sha-/ts- image set from the
  unmerged branch, so amplification is capped at exactly-once per head
  SHA (the dispatch consults the same per-SHA marker as the comment),
  and the predicate clears once the dispatched run registers
  check-runs. The default stays ON because the fault is silent and
  GitHub-side — an opt-in default would have caught zero of the four
  documented instances.
- **The detection predicate lives in tested Go, not bash**
  (`pkg/runwatchdog`, red-first: the skip-ci, dedup, label, and
  comment-contract tests were verified failing against the naive
  age+zero-runs baseline before the full predicate landed;
  `cmd/runwatchdog` is a thin stdin→stdout caller; the workflow only
  gathers inputs and executes the decision — the numbering-gate
  pattern). Dry-run against live repo data caught the still-suppressed
  probe branch (PR #1873, head 75 min, zero runs) as a fault on its
  first pass.

### Fixed — Session phrase IDs are persistent by construction: write-once column, phrase-bearing row births, spawn-window reap grace (#1850)

- **create_session workers can no longer be re-phrased after spawn.**
  Five reproductions showed platform-spawned workers losing the
  3-word phrase minted at creation after their first turn —
  `id_deprecated` on the original id, send_message to it failing
  with 500, and the session's addressable identity collapsing onto
  its 2-word harness slug. Root cause was lazy settlement in three
  cooperating layers: six row-creating session_index upserts inserted
  WITHOUT the phrase (any write landing on an absent row birthed a
  phrase-less row, later backfilled with a FRESH mint); the #1340
  reaper's message_count==0 guard was armed for exactly the spawn
  window (a platform-spawned worker's prompt delivery is a pod-local
  POST the API never sees, so the count stays 0 through the whole
  first turn — two harness-list misses reaped the LIVE row and
  orphaned the phrase); and the post-death resurrection re-minted a
  different phrase. The fix is persistence-by-construction: every
  row-creating upsert now carries a Go-minted phrase in its INSERT
  leg (the phrase is born in the same statement/transaction as the
  row; the ON CONFLICT legs never touch short_id, so an existing
  phrase always wins), migration 000044 pins short_id WRITE-ONCE at
  the DB level (a trigger refuses changing or NULL-ing a minted
  phrase; identical rewrites and the legacy NULL-backfill stay
  legal; helm mirror byte-identical), and the reap guard refuses
  count-zero rows younger than 15 minutes (a fresh session is not a
  ghost; aged rows keep the #1340 hygiene verdict — stillborn rows
  now converge one grace period later, a deliberate, documented
  trade). Phrase minting keeps the registration seam's ruling-3
  collision discipline (deterministic counter suffix) on every birth
  path. Known effect: the grace window shelters genuinely stillborn
  rows for the window's duration. The id_deprecated marker remains
  only for legacy rows and the degraded emission window.

### Fixed — Trigger inline scripts + workflow script nodes execute in the workspace container (#1878)

- **Scripts run in the environment the sessions run in (owner ruling,
  2026-10-23).** Both trigger inline scripts (the `script` gate and the
  legacy `script_path` pre-script) and workflow script nodes executed
  inside agentd's own process — in sidecar mode
  (`controller.agentdSidecar.enabled`) the FROM-scratch uid-2000
  container with no `/tmp`, no interpreters, no `gh`, and a read-only
  `/workspace`: workflow nodes failed `script_env_unavailable` and the
  deployed trigger path failed SILENTLY (probe fire `delivered` in
  0.77s, sentinel never created). Script execution now runs in the
  WORKSPACE container in both topologies: single-container executes
  in-process; sidecar mode forwards the handler over the design-0051
  control socket (`script_exec` method — the `upload_apply`/
  `disk_recover` long-held pattern) to the workspace-container
  supervisor, which runs it via `scriptwrap` with the opencode spawn
  environment (`opencodeBaseEnv()` — secrets-env sourced, redact PATH
  prepend — plus the live spawn delta), so interpreters, mise shims,
  `gh`, the credential belt, and `GH_TOKEN` resolve exactly as they do
  for session tool children. `scriptwrap` gains `ExecuteWithEnv` (env-
  explicit exec); the local EnvCheck text now names the single-container
  cause.
- **Script-only fires never mint a carrier session.** The #1812
  sessionless early-return tested `ScriptLanguage` only, so a
  `script_path`-only trigger with an empty prompt fell through to the
  agent leg with an empty prompt — one ephemeral carrier session per
  fire (the ~75 ghost-sessions/hour churn #1882 measured). The
  condition now covers every script-bearing, prompt-less shape.
- **Failures are loud in every topology.** The `script_exec` closed
  error enum (`bad_request`, `script_env_unavailable`, `script_failed`,
  `script_timeout`) maps onto the workflow error envelope with
  fidelity (`bad_request` folds into `script_failed` — the local path's
  deterministic-validation outcome, behavior parity); a dead
  supervisor surfaces `script_env_unavailable` naming the control
  socket; the #1812 gate carries script errors into the fire audit's
  `actionResult.gate.error` — never a silent `delivered`. The legacy
  pre-script leg was ErrorCode-blind (HTTP-200 error envelopes read as
  success); it now fails the fire LOUD (error log + accounting), and
  records the cause in `actionResult.script.error` without clobbering
  an open gate's audited outcome (review F1/F-A/F-C — without this, a
  failed `script_path`-only trigger laundered to `delivered` + a
  failure-counter reset).
- The #1455/#1812 nightly e2e contracts flip from "sidecar fails loud
  with `script_env_unavailable`" to "the script executes in BOTH
  modes" (marker + in-workspace side effect asserted). The execution
  environment contract is documented in `docs/api/mcp.md`.

### Fixed — Provisioned SSH keys: the delivery writer validates + repairs the trailing-space corruption class (#1793)

- **SSH git operations no longer die on formatting-corrupt stored
  keys.** A live provisioned key delivered with ONE trailing space on
  every line (PEM markers included) made stock OpenSSH fail with the
  misleading `error in libcrypto` — killing every SSH git operation in
  fresh workspaces while the key material itself was intact. The
  agentd delivery writer now runs a belt at the staging seam
  (`pkg/agentd/secrets`): a structurally clean, parse-valid key (or an
  OpenSSH-format passphrase-protected one — x/crypto reports those as
  `PassphraseMissingError`, which is validity, not corruption) is
  staged byte-identical; anything else gets one strict repair
  (per-line whitespace trim + CRLF normalize) staged only if it parses
  or is OpenSSH-format passphrase-protected — reproducing the clean
  PEM byte-for-byte for the padding-only corruption class (encrypted
  OpenSSH keys included). Other encrypted PEM envelopes (PKCS#8
  `ENCRYPTED PRIVATE KEY`) parse as unsupported in x/crypto and stage
  verbatim under never-worse; garbage likewise. Detection is
  STRUCTURAL, because Go's parser tolerates exactly the
  trailing-space class OpenSSH rejects. The frontend save path
  normalizes ssh-key values on create/update (paste/transport reflow
  cannot reintroduce the class), defense in depth with the belt.

### Added — Task Board: kebab menus + delete flows (#1832 item 3)

- **Kebab menus at all three board levels.** Each project swimlane
  header, epic band header, and task card carries a kebab menu
  (distinct a11y labels; triggers stopPropagation — a kebab click
  never toggles band selection nor opens the task editor).
- **Task delete modal with the 'reconnect dependents to upstream'
  checkbox** (#1747's optional flag): dependents are surfaced
  pre-flight from the read model; unchecked (default) they are simply
  unblocked (each gets an `unblocked_by_delete` audit event), checked
  they inherit the deleted task's direct upstream edges (deduplicated,
  cycle-guarded server-side). Both semantics are pinned at the wire in
  e2e (`?reconnect=true` presence/absence exact-matched) and in
  component tests (API-client args asserted). The delete result
  surfaces as a page notice (orphans listed / `orphan→upstream` pairs).
- **Epic delete surfaces in-flight workers with refuse/force-confirm
  matching the API**: the modal lists the epic's in-flight claims
  (in_progress + assignee, live read model) and its honest cascade
  count; the first attempt never sends force — the engine's 409
  `epic_state` renders inside the modal, and only that real refusal
  reveals the acknowledge-gated "Force delete" (force deletes, then
  aborts each in-flight worker post-commit).
- **Project kebab is archive/unarchive ONLY** (#1747: project delete
  is eliminated) — the menu structurally offers exactly one lifecycle
  item; the absence of any delete item is pinned in both test layers.
- This lane's e2e runs at **E2E_PORT=18521** (registry discipline:
  18517/18519 claimed by sibling lanes; nightly PORTFWD 18083–18093
  untouched).

### Fixed — worklog/migration renumber bot: root fix for the next-free assignment race (#1838)

- **Assignment consults the open-branch taken-set** (the soft layer):
  `repolint`'s worklog numbering pass (`-fix-worklogs`, the post-merge
  bot, `.githooks/post-rewrite`, `make pre-commit-fix`) now skips
  numbers claimed by OTHER open PRs' heads — discovered via
  `gh pr list` + the git-trees API — in addition to the local tree and
  origin/main. Two branches assigning inside one bot-lag window can no
  longer both mint the same number and land it via file-wise auto-merge
  (the 1161x2 / 1164x2 / 1173x2 / 1175x2 class). Discovery degrades to
  the previous main-only behavior offline; the #1807 uniqueness
  invariant stays as the net.
- **New `numbering-gate` workflow** (the hard layer, both artifact
  classes): on every push to main touching `worklogs/**` or
  `api/migrations/**`, re-evaluates every open PR's MERGE tree
  (`refs/pull/N/merge`) with `repolint -numbering-only` (migrations
  sequence + worklog uniqueness — the same gates ci.yml's Lint runs,
  but against the CURRENT tip, closing the stale-green window that
  landed migration 41x2). A PR whose merge tree collides gets a red
  namespaced `numbering-gate` check run on its head (title + summary
  name the collision and the healing lever — never the PR's own lint);
  once healed, a success run replaces the red (clear-on-heal, pinned in
  `gate_test.go`). The decision matrix is pinned in `pkg/repolint`
  (`NumberingGateDecision`), not in bash.
- **The renumber bot is serialized and tip-based** (the CAS layer):
  `worklog-renumber.yml` gains a `worklog-renumber` concurrency group
  (queued, never cancelled), checks out `ref: main` (a queued run
  numbers the CURRENT tip, including the previous run's numbering
  commit — the stale-SHA checkout is how two runs minted 1164 twice),
  and its push is a compare-and-swap: on rejection it rebases onto the
  new tip, re-runs the fix pass (interleaved merges may have added new
  sentinels), amends, and retries (≤3). Git's fast-forward check on
  main IS the lock — no manifest file to conflict or drift. After a
  successful push the bot explicitly dispatches the numbering-gate
  sweep (its own `[skip ci]` marker suppresses the push trigger): in
  the degraded mode where gh discovery was unavailable, the bot may
  have minted a sibling-held number, and the dispatched sweep catches
  it in minutes instead of at the 6h cron.
- **New `repolint -next-numbers`**: prints the next free worklog and
  migration number given local state, origin/main, and open-PR claims —
  the soft layer for hand-numbered migrations (the 41x2 class had no
  bot in the loop; authors get knowledge instead of automation).
- Red-first suite: `pkg/repolint/numbering_race_test.go` simulates two
  (and three) sibling branch trees assigning inside one window and
  asserts the merged tree passes the uniqueness gate; the control test
  pins that claims-blind assignment still collides (the documented
  degraded mode the invariant nets).

### Changed — Task Board: stillborn turn-evidence guard on heartbeats (issue #1832, lane board-stage-hygiene WS4)

- **A heartbeat extension now requires ACTUAL turn evidence when the
  evidence source is installed**: a POSITIVE zero-message report for
  the task's assignee session refuses the extension (the
  lease-keeper-renews-a-stillborn-spawn class — spawn-moment
  `updated_at` alone is not liveness); the refusal carries a `reason`.
  The wall clock reclaims the stillborn's lease through the ordinary
  RC1 path. Source errors fail open (logged) — the narrow fix only;
  the platform-derived liveness rework is #1847's lane.

### Added — Task Board: re-arm, the attempts-exhaustion re-pool (issue #1832, lane board-stage-hygiene WS3)

- **`POST /api/v1/me/tasks/:id/rearm`** (and `board_rearm_task` on
  both MCP surfaces, the pod seam manager route, and the agentd
  manager catalog): a task parked at `pending_input/escalated`
  (attempts at the cap — the engine withheld the grant) returns to
  `backlog` with attempts reset to 0 — a fresh-judgment ledger whose
  pre-reset count rides the `rearm` audit event. The failure overlay
  and cooldown clear (a re-arm is not a failure) and the row is
  claimable immediately. Only escalated rows; other stages and
  origins keep their enumerated transitions.
- **UI deferral is a flagged ticket, not a silent gap** (standing
  directive #20): the REST surface is live for a swimlane affordance.

### Added — Task Board: manager-grade pending_input exits (issue #1832, lane board-stage-hygiene WS2)

- **The pod seam's manager projection gains a manager-grade move
  (`SurfaceAgentManager`)**: `POST /internal/v1/automation/board/
  tasks/:id/move-stage` behind the existing manager belt
  (harness-attested session identity + live-assignee worker refusal,
  fail-closed on omission) — the orchestrator-class in-workspace
  session can now exit `pending_input` rows (the tsk-3do6n458 freeze
  class: → in_progress re-claims, → backlog re-queues with the attempts
  reset), audited with its session identity and a distinct surface
  vocabulary.
- **`board_move_stage` on the agentd toolset dual-routes**: a claim
  token rides the worker projection (wire unchanged); a token-less
  call with an orchestrator-class identity rides the manager path.
  Ordinary workers are unaffected and cannot reach the manager path
  (both gates refuse them).
- **The workspace binding extends to the manager grade**: a foreign
  workspace's rows stay untouchable (403 workspace_binding).

### Added — Task Board: delegated-review completion coupling (issue #1832, lane board-stage-hygiene WS1)

- **A task whose job is to review another task's artifact anchors to
  its subject with the `review-of:<subject-short-id>` tag** (the pr:N
  discipline: the only form the engine consumes, workspace-scoped;
  malformed anchors are inert tags, never guessed).
- **When the subject reaches done — manager move, approve verdict, or
  RC2 external-truth drift — the engine settles its review tasks in
  the same transaction**: criteria ALL met → done (the Done guard is
  never bypassed), with an audited transition plus a deterministic
  `review_complete` event; chained delegation (a review of a review)
  recurses safely over a visited set.
- **Criteria UNMET → NEVER auto-done**: the in_review review task
  parks to `pending_input/drift_review` with a `review_park` audit
  event (the tsk-zbdeufiw class: subject merged, review stranded —
  the pending_input column is the owner-visible surface instead of an
  invisible in_review rot).

### Added — Task Board: agent overview reads + setCriteriaMet safety (issue #1837 P2)

- **`board_list` + `board_health` on the agent manager surface**: one
  project's every task in a single call (stage, claim/assignee, lease
  expiry, paused, blocked, attempts, cooldown columns) with the derived
  READY set (backlog + unblocked + unpaused + not cooling), and the
  workspace's board health snapshot (active leases with expiry,
  cooldowns, pending origins, reclaims, per-project slots). New
  manager seam routes `GET /internal/v1/automation/board/projects/:id/tasks`
  and `GET /internal/v1/automation/board/health` — manager-gated
  (session identity + worker refusal), workspace-scoped (a foreign
  workspace's project is a plain 404).
- **`setCriteriaMet` is `{index, met}`**: check off AND uncheck (the
  legacy bare-integer form still decodes as `{index, met:true}` wire
  compat). Combined use with an `acceptanceCriteria` replace is
  refused ("send one or the other"); out-of-bounds indices are loud
  errors (both were silent before); a CREATE payload carrying
  setCriteriaMet is refused loudly on every surface (never silently
  dropped); create/update responses echo the mutated criterion
  (`mutated_criterion`: index, met, text). All three surfaces (REST,
  external MCP, agent seam) share the service semantics.
- **board_list never carries live claim tokens** (ruling, #1864
  review): the bulk projection renders assignee + lease columns only —
  the claim token stays on the per-task read where the claimant's own
  flow needs it.

### Added — Task Board: emission-corruption defenses + file-backed long fields (issue #1837 P1)

- **Server-side text rejection heuristics on every board write surface**
  (REST, external MCP, agent seam — enforced at the Service layer):
  string-field byte caps (title 512 / description 128 KiB / outcome
  64 KiB / name 256 / criterion 8 KiB / tag 64 ×16), degenerate-
  repetition rejection (a fragment tiling the value, or one line
  repeated, ≥50 times), and raw JSON structural splice rejection
  (`","key":"` adjacency inside string values — the #1832 dogfood's
  silent-corruption artifact; junk-wrapped repeats, line-start
  artifacts, artifacts after unterminated fences, and the tag channel
  included). New guard code `text_rejected` (400 class;
  `content_echo_diff` on task/epic/project create/update responses
  flags stored != submitted, never silent). TERMINATED fenced code
  blocks and balanced inline JSON containers are exempt — legitimate
  markdown/JSON/code is pinned un-rejected by tests; the repetition
  heuristic covers fragments up to 512 bytes (documented bound).
- **File-backed long fields on the agent manager surface**:
  `description_file` (board_create_task / board_update_task) and
  `outcome_file` (board_create_epic / board_update_epic) read a
  workspace file agent-side (loadImages discipline: regular files
  only, O_NONBLOCK open, stat+read caps, UTF-8 gate) and carry its
  content to the platform verbatim — long charters never ride the
  emitted-inline-argument path where corruption lives. Mutually
  exclusive with the inline field; a `*_file` key on the wrong tool
  is refused loudly.

### Added — Task Board: task audit timeline + drawer (issue #1832 item 4)
- **`GET /api/v1/me/tasks/:id/events` renders a task's FULL audit
  timeline** — every `board_events` row scoped to the task (stage
  transitions, claims who/when, review verdicts when present,
  dependency-graph changes, orphan/unblock events), chronological,
  behind the same owner perimeter as the task read (foreign rows are
  plain 404s); optional `?limit=N` keeps the newest N (default 200,
  max 500).
- **`board_task_events` mirrors the timeline on the external MCP
  server** (construction parity — the #1747 rule that every board
  surface lives on every surface): read-only tool, `task_id` +
  optional `limit`, identical rows.
- **The task-detail drawer surfaces the timeline in the UI**: header
  completeness (ids, stage, owner perimeter), criteria, dependencies,
  and the full event feed on one panel.
### Changed — Task Board short ids flip to word phrases (issue #1831, the session-ID playbook)
- **Fresh `prj-`/`epc-`/`tsk-` ids are word phrases** over the #1756
  frozen session lists (same generator via `pkg/shortid`, not a fork):
  `tsk-amber-falcon-glides`, with a deterministic counter suffix
  (`-2`, `-3`, …) arbitrating mint collisions. The prefixes (the
  closed kind map) are kept, keeping board ids pairwise disjoint from
  session phrases (bare 3 words), workspace phrases (2 words + digit),
  and harness slugs. The pre-#1831 random 8-char form
  (`tsk-ab12cd34`) is never minted again.
- **One-release dual-accept window**: legacy random-form rows keep
  resolving on every surface (case-insensitive, as today) and render
  `short_id_deprecated: true` at scan time — every read path carries
  the flag (REST/MCP/agentd render the row verbatim). A later
  migration narrows the CHECK once no legacy row remains.
- **Uniqueness widened to owner-wide for `epc`/`tsk`**: the phrase
  space and the resolution perimeter (workspace-scoped `board_read`,
  user-scoped search) make design 0067's per-project scope
  insufficient — a same-phrase row in a sibling project would resolve
  silently wrong. The per-project UNIQUE constraints remain as DB
  backstops; migration `000043` widens the shape CHECK to both forms.
- **did-you-mean ranks phrase candidates over WORD edit distance**
  (bound 2; the issue's acceptance case `tsk-meek-pony-rides` →
  suggests `tsk-meek-poncho-rides` — character distance 4 was out of
  bound); legacy random ids and titles keep the character ranker (word
  distance degenerates on a single random token). The frontend's
  `suggestBoardItems` mirrors the word ranker.
- **Tool schemas/docs updated**: `pkg/mcp` + agentd registry describe
  the phrase form first (`tsk-crimson-finch-blooms`), legacy forms
  noted as deprecated-but-resolving; `docs/api/mcp.md` `board_get`
  row, `design/0067` §4.1 supersession note, TaskDialog dependency
  placeholder.
### Added — busy-snapshot handshake + EventID-ordered deltas + frontend busy lease (#1821 pieces 2-4)
- **The user-stream connect path emits a `session.status.snapshot`
  flight per Active workspace** (`snapshot_begin` → one snapshot frame
  carrying `{cursor, sessions: [{session_id(phrase),
  internal_session_id, status, derived_busy}]}` → `snapshot_complete`
  with `snapshot_ok`), sourced from the pod's statusz — the same
  authority `GetAuthoritativeActiveSessions` consults, now with an
  ok-typed read so an authority failure reads "keep your state"
  (D10), never an authoritative all-idle. Kills the delta-only fragility
  (class 1): a subscriber joining after a busy flip — or a page refresh
  with no `Last-Event-ID` — rebuilds busy in the connect window, and
  the per-replica replay limitation becomes benign (state converges
  without the delta log). Rows are phrase-keyed through the #1824
  translation discipline (`sessionIndex.Register`, degrade flagged
  `session_id_deprecated`), and the frame carries the broker cursor
  (`UserEventBroker.LastEventID`, read before the authority fetch).
- **`POST /workspaces/:id/sessions/status-snapshot`** — the on-demand
  twin (RequestInputSnapshot pattern, 202 + flight on the user
  stream): the frontend busy lease's authoritative recheck surface.
- **EventID-ordered deltas (frontend)**: session.status deltas apply
  in broker id order against a watermark seeded by the snapshot's
  cursor — a replayed delta at or below the watermark is skipped,
  making application idempotent across snapshot/replay overlap
  (#1781's emission legs unchanged in substance).
- **90s busy lease + one authoritative recheck (frontend)**: latched
  busy entries carry `lastConfirmedAt` (any busy-bearing delta or
  snapshot row refreshes it); expiry after 90s (≈3× the 25s
  heartbeat, above the 60s liveness window) triggers exactly ONE
  recheck per lease window via the status-snapshot trigger — the
  flight's D10 marker decides keep-vs-clear, so an unreachable
  authority holds the badge as unknown, never flips it to idle. The
  ok=true snapshot commit is the authoritative per-workspace
  reconciliation (entries superseded at the cursor clear; fresher
  deltas survive), replacing the REST overlay as the re-seed source
  on reconnect/resync/refresh (the overlay stays as the degraded
  fallback).
- **Review-hardened (PR #1848 r0)**: the statusz session-list
  authority signal (`sessions_ok`, pointer — absent on older pods
  reads ok) so a 200 whose ListSessions failed (agentd's swallowed
  empty slice) reads non-authoritative instead of clearing latched
  busy; delta ordering is now DEDUP-BY-APPLIED-SET + snapshot-cursor
  subsumption instead of a watermark (the broker delivers after
  releasing the shard lock — out-of-order arrivals of never-applied
  deltas must still apply); a stale overlapping flight (cursor below
  the applied evidence high-water) is discarded instead of
  re-latching; REST-seeded and cursor-0 entries are orderable-at-0
  (governable by any snapshot — no permanently-protected classes);
  degraded harness-keyed rows heal client-side through the alias
  seam; the connect-path flight Registers only busy rows (cold-replica
  budget) and coalesces per workspace (one in-flight flight per
  trigger window).
- **Review-hardened round 2 (PR #1848 r1)**: every ordering bound is
  PER-WORKSPACE (a snapshot covers only its own workspace's rows —
  global bounds suppressed other workspaces' never-applied deltas and
  discarded their flights); resync/reconnect RESET the ordering state
  (the broker's id space is per-replica, restarting at 1) and resync
  additionally requests one authoritative busy-snapshot flight per
  affected workspace (the authority re-seed, not just the REST
  overlay); a flight with unhealed degraded rows adds busy but never
  removes (its id join is partial); agent_died / non-Active phase
  invalidate the workspace's open flights (no post-terminal re-latch);
  agentd's ListSessions treats non-2xx as an error (the status-blind
  empty-array decode could still report sessions_ok=true).
- **Review-hardened round 3 (PR #1848 r2)**: the on-demand trigger's
  coalescing moved OUT of the emitter (the connect path always emits
  its own per-subscriber flight — a mid-flight joiner has no backlog
  and was left unseeded); delta ordering is enforced PER-SESSION (an
  out-of-order older transition for the same session can no longer
  overwrite the newer one), and overlapping-flight staleness is
  resolved per-entry (every authority-answered session's watermark
  moves to the flight's cursor — an unrelated session's delta can no
  longer starve the recheck's rows); a degraded busy row blocks
  removal of exactly the entries it may correspond to (matched by the
  additive harness id, now carried on latched entries) instead of the
  whole workspace's removals; a non-authoritative (ok=false / dropped)
  flight schedules ONE authority retry per lease window (the
  post-wipe re-seed previously left no lease to recover with); the
  route is documented in sdks/openapi.yaml (the router↔spec parity
  gate); Playwright e2e pins the connect-path badge (refresh-mid-busy,
  abort-reconnect, down-then-recovered).
- **Review-hardened round 4 (PR #1848 r3)**: the snapshot commit
  reconciles INSIDE the state updater against its fresh `prev` (SSE
  frames dispatch synchronously per chunk — a render-closure read was
  stale within the chunk and could delete a same-chunk newer busy
  re-confirm); idle rows are REGISTERED server-side too (every row
  joins the phrase id space — a harness-keyed idle row could not
  advance a phrase-keyed session's ordering watermark); the degraded
  row blocks removal only of entries it could BE (own harness id,
  reverse alias of the phrase, or a session with no resolved answer) —
  a resolved answer always clears; terminal events (agent_died /
  non-Active phase) also drop the pending recheck and the wipe memory
  (no 90s retry loop for dead workspaces); a dropped flight
  (ok=true, staged missing) schedules nothing; and the charter's
  unknown-hold is delivered end-to-end: the reconnect/resync wipe
  REMEMBERS what it cleared, and an ok=false answer to the post-wipe
  re-seed re-latches it as unknown (never idle) with the lease owning
  a once-per-window recovery cadence — pinned in vitest (fake timers)
  and at e2e fidelity (authority-down never clears a live badge).
- **Review-hardened round 6 (PR #1848 r5)**: the reconnect/resync wipe
  memory is stored orderable-at-zero (a pre-wipe confirm id belongs to
  the old replica's event-id space and could block the new replica's
  authoritative clears forever); a post-wipe session.status delta
  prunes the memory (an ok=false can no longer resurrect state the
  authority already cleared); latching busy evidence hands the recheck
  cadence to the lease (exactly one recheck POST per 90s window — the
  no-evidence pending retry and the lease no longer double-fire); and
  the authority-down e2e now proves the unknown-hold itself (the busy
  delta rides the initial connects only; reconnects answer ok=false
  alone), plus a resync-driven hold e2e.
- **Review-hardened round 5 (PR #1848 r4)**: the resync recheck
  capture rides the state-updater queue (a render-closure read was
  stale within one SSE chunk — a same-chunk busy delta followed by
  resync was never re-checked); a late ok=false after agent_died or a
  non-Active flip arms nothing (no 90s POST loop for a dead workspace;
  Active re-arms service); a degraded harness-keyed idle row beats an
  older phrase-keyed busy flight (the busy row's latch checks its own
  harness-id watermark too); idle snapshot rows are pinned
  phrase-keyed at the wire; and the lost-idle lease recheck is pinned
  at e2e fidelity (open-stream authority, fake-clock lease expiry).
### Added — Task Board: config UI section (issue #1832 item 6, part 2)
- **Settings → Task Board tab**: the user's board-wide defaults editor
  (reviewColumn `off|manual|auto`, autoAssignReviewer,
  defaultWorkerSlots ≥ 1) over `GET/PUT /api/v1/me/board/config`, with
  client-side validation mirroring the server (enum + integer gates
  block the send before any request).
- **Per-project override editors** over `GET/PUT
  /api/v1/me/projects/:id/config`: reviewColumn + autoAssignReviewer
  only (defaultWorkerSlots is a user-level key the project tier
  rejects), Inherit = the key's absence (REPLACE semantics — Save
  sends the override set, `{}` clears to full inheritance).
- **Effective view with provenance**: each project row shows the
  merged effective config, each key marked override vs inherited from
  the stored overrides.
### Added — Task Board: review auto-assign + verdict chain in the next charter (issue #1832 item 5, part 2)

- **Config-gated reviewer dispatch**: when a task enters in_review and
  the project's EFFECTIVE config is `reviewColumn=auto` AND
  `autoAssignReviewer=true`, a reviewer is dispatched DIRECTLY (a
  dedicated reviewer session — never queued behind the global worker
  FIFO; the single-reviewer latency multiplier was the design driver).
  `manual`/`off`/`auto`-without-assign keep today's behavior exactly —
  no event, no dispatch (5-combo gating matrix pinned in the PG
  harness). The intent records a `review_assign` event; dispatch
  failure logs and leaves the manual path intact.
- **The verdict chain rides the next claimant's charter**: after a
  fail-bounce, a fresh claim's spawn carries every prior verdict +
  findings (`Claim.VerdictChain` → the charter prompt renders "PRIOR
  REVIEW VERDICTS" with severity/pointer/criterion-index) — iteration
  history, never a failure count.
- **The reviewer's charter** carries the adversarial-review contract:
  verify each criterion, record ONE structured verdict through the
  forge-agnostic core (`board_record_verdict`), never merge or hold a
  claim.

### Added — Task Board: structured review verdicts (issue #1832 item 5, part 1)
- **The forge-agnostic verdict core**: a validated payload
  (`{verdict: approve|fail, findings: [{severity blocker|major|minor,
  pointer, criterionIndex?}], reviewer}` — criterionIndex
  bounds-checked against the task's criteria; a fail requires at least
  one finding) persisted as an ORDERED per-task record (the verdict
  chain) and applied through the EXISTING guarded stage machine. Forge
  adapters (PR reviews) map INTO the core; they never re-implement it.
- **approve → done STILL passes the Done guard** (ALL criteria met +
  evidence URL). A refused transition is a RECORDED outcome, not an
  error: the verdict persists with `applied=false` and
  `transition="refused:<code>"` — the chain keeps the evidence.
- **fail → in_progress rides the bounce**: a fresh claim is minted for
  the recorded assignee and **attempts are UNCHANGED** — a review
  bounce is an iteration inside the verdict chain, never a
  failure-ledger entry (the amended #1832 semantics).
- **Surfaces (construction parity)**: `POST /api/v1/me/tasks/:id/verdict`
  + `GET /api/v1/me/tasks/:id/verdicts`; external MCP
  `board_record_verdict` + `board_task_verdicts`; OpenAPI paths +
  schemas. Idempotent per `requestId`.
### Added — Task Board: failed-state attribute overlay (issue #1813)
- **A task whose worker attempt failed is now VISIBLE**: every
  lease-lapse reclaim marks `last_attempt_failed` (+1 consecutive
  `failure_count`, `last_failure_at`). NEVER a sixth stage — the
  five-stage vocabulary is untouched; the failed row stays in its
  stage (usually backlog) and carries the marker.
- **Auto-return-to-ready with backoff**: the reclaim parks the row
  behind the existing cooldown gate with doubling backoff per
  consecutive failure (base `board.failureBackoffSeconds`, default
  60s; capped at 1h) — redispatch is automatic, never a manager
  action. A crash-looping task is both visible and rate-limited.
- **Reset on success**: any completed turn (in_review / done entry)
  and the pending_input human exit clear the overlay and count with
  the attempts reset.
- **Truthful marker semantics**: the overlay means an attempt
  actually failed, and ONLY the reclaim writes it (the invariant
  last_attempt_failed == (failure_count > 0) holds through every
  reset). An all-success exhaustion (attempts preserved through
  in_review, cap reached with zero failures) escalates WITHOUT the
  marker — `FAILED ×0` cannot render; the exhaustion signal is
  pending_input with `pending_origin=escalated`, attempts at cap, and
  an `exhausted=true` audit event.
- **Surfaces**: `GET /me/projects/{id}/tasks?failed=true|false`
  (tri-state filter); `board_read`'s `failed` argument (MCP); the UI
  task card carries a `FAILED ×n` badge plus a FAILED filter toggle;
  the overlay renders on every task read (REST/SDK/MCP) and in
  reclaim audit events (escalate events carry exhausted=true).

### Fixed — create_session workers get re-phrased after spawn; 3-word ids must be persistent (#1850)

- **The reconcile reap no longer deletes LIVE sessions off a harness
  list gap.** The #1340 convergence pass treated two consecutive
  harness-list absences as deletion evidence, but the list is not a
  liveness authority for harness/Act-path sessions (#1452: live and
  readable by ID while never listed — exactly the create_session
  spawn class). A fresh spawn's `session_index` row sits at
  `message_count == 0` for its whole first turn, so the row was reaped
  mid-turn; the first turn-boundary event then re-registered the
  session and the single-mint seam minted a NEW phrase — the reported
  re-phrase (old phrase dead: `send_message` to it 500s mid-lane; the
  session re-emits flagged `id_deprecated`). Every reap is now
  confirmed by the by-ID probe first (the send_message `SessionExists`
  precedent — immune to the list-visibility gap): alive-by-ID vetoes
  the reap and resets the miss counter (`reap_vetoed_live`); any
  indeterminate outcome (transport/5xx error, or a sessionless 2xx
  answer) defers it to the next pass (`reap_deferred_probe`); only a
  definitive by-ID not-found reaps, as before.
- **The agentd short-id replica applies registry pulls as a MERGE,
  not a replace.** `putAll` rebuilt the whole map from each pull's
  snapshot: a pull in flight at spawn (the emission-warm worker's 1s
  cadence) returned without the just-registered row and WIPED the
  fresh phrase→harness mapping — the spawned session degraded to
  harness-id emission + `id_deprecated` (worker panes stopped
  rendering it) and phrase-addressed sends lost resolution until a
  later pull. Pulls now upsert (a pull never removes entries): absent
  entries are stale snapshots or deleted sessions — the latter resolve
  to a dead id and fail the `SessionExists` probe cleanly with the
  did-you-mean clause, never a hard error. This is also the honest
  reading of the replica's own contract ("only ever RECEIVES entries"
  — phrases are immutable once minted).
- **Phrase-as-identity contract pinned red-first** on both tiers: the
  spawned session keeps its original 3-word phrase across its first
  turn boundary AND across a stale registry pull (e2e over the real
  create_session + delivery path); a `send_message` addressed to the
  ORIGINAL phrase still resolves and delivers after rename and
  compaction paths; the by-ID reap veto/defer/confirm matrix is pinned
  at the reconcile orchestration. Test-isolation fix (fail on
  unmodified main at `-count>1`): the on-miss pull TTL timestamp now
  resets per test.

### Fixed — builder GOTOOLCHAIN=auto carry + health probes exempt from rate limiting (#1825)
- **The five service builders set `ENV GOTOOLCHAIN=auto`** alongside
  #1819's 1.26.10 digest re-pins (main already carries those, the
  go.mod bump, vite 8.1.6, and every workflow go-version pin): the
  digest remains the pinned base image; auto makes go.mod's minor
  requirement govern the compile, fetching the toolchain through the
  module proxy/GOSUMDB only when it exceeds the image's.
- **/livez and /readyz are exempt from the global rate limiter.**
  The SDK rate-limit canary pins "not rate-limited" for both, and a
  429 on a k8s liveness probe restarts healthy pods — the probes only
  passed pre-#1825 by refill-boundary luck after the canary's login
  burst. Pinned red-first (an exhaustion control leg proves the
  budget is spent — 429 observed on an ordinary route — before both
  probes are asserted: /livez exactly 200, /readyz its typed 503).
### Changed — repolint worklog uniqueness invariant (#1795, ruling c)
- **Duplicate worklog numbers now FAIL `make repolint`** — the new
  `runWorklogUniqueness` check, matching ANY numbered-worklog filename
  (`WorklogNumberPattern`) so historical uppercase-slug files count
  too (two live duplicate pairs — 0498, 0500 — shipped invisibly under
  the lowercase-only slug class; healed here by renumbering the
  non-series member of each pair — final numbers 1170/1167 after the post-rewrite hook bumped one past main's new 1166). Gaps stay warnings:
  historical holes nothing backfills. The post-rewrite renumber-all semantics stay
  (pinned end-to-end through the real hook: every sentinel numbered,
  main-inherited included, all unique) and the falsified "sentinels
  never appear on origin/main" / "collisions cannot occur" comments
  are corrected. The invariant caught one real collision in-flight
  (its own PR's 1161 vs main's lag-window assignment); a second
  (1162) was pre-empted by a manual leapfrog before it could redden.
  Companion security bumps shipped separately: 43a7ba29 (Go
  toolchain), #1828/57495a4c (vite), #1819 (the five builder digests).
### Fixed — agentd send_message rejects placeholder-class payloads (#1814)
- **The delivery boundary now refuses literal placeholder tokens.** The
  mid-write emission-corruption mode (observed across worker sessions
  and the orchestrator) replaces a drafted body with a placeholder
  token as the call fires — the sender rarely notices, and every such
  delivery costs the recipient a turn to diagnose. `send_message`
  validates BEFORE delivery and rejects the narrow exact-match set
  (trimmed, case-insensitive): `placeholder`, `placeholder-removed`,
  `<placeholder>`, `todo`, `tbd`, `asdf`, `qwerty` — with the
  re-compose guidance in the error. NOT a length heuristic: short
  legitimate payloads (`ok`, `x`), probe traffic (`test`, `ping`), and
  sentences merely CONTAINING a placeholder word pass untouched
  (pinned). The tool description teaches the same contract: send the
  complete intended content; re-compose if emission corruption ate the
  draft.
### Changed — platform MCP surfaces flip to phrase session IDs (#1816, the #1756 §5 remainder)
- **The external MCP server (pkg/mcp / cmd/mcp) accepts phrase
  addressing for real.** `session_message` with a 3-word phrase target
  previously worked only by accident: the client's contract-stream
  filter never matched the frames. Post-#1824 the frames are
  phrase-keyed on refreshed pods (harness id additive as
  `internal_session_id`) and harness-keyed on heal-window/legacy pods,
  so the client now admits a frame keyed by EITHER space
  (`sessionMatchKeys`), learning the resolved canonical id from the
  session-ref middleware's `X-Session-Ref-Resolved` response header
  when the edge rewrote the ref — phrase-targeted sends complete from
  live frames in every pod regime, and the deprecated full-id form
  stays functional too (it adopts phrase-keyed frames via the additive
  id). The header is correlation on a response the caller already
  receives under authz (not a session-bearing payload surface) and
  retires with a phrase-only ABI. The pending-question result's
  session id is likewise rewritten to the caller-facing phrase — the
  other id space never leaks into tool output on the phrase path.
- **§2 deprecation notices reach the MCP surfaces.** The external
  server's `session_message`/`session_history` attach the one-release
  notice as a second content block on full/hashed-id inputs — on the
  success path, the send/history error paths, AND the early validation
  errors (missing message/workspace, invalid attachment) — and the
  workspace agentd's tools/call envelope does the same for every
  session tool — including array-shaped `session_read` and
  `session_metadata` scopes that had no object field to carry it, and
  `send_message`'s declared ORIGIN ref (the id the recipient re-uses
  to reply). Parity with the REST `Deprecation`/`Sunset` headers.
- **Degraded rows surface flagged on the external server too**:
  `session_create`'s registration-degraded window now emits
  `session_id_deprecated: true` + the address-by-phrase guidance
  (agentd parity — never a silently-raw harness id).
- **Emission closure on the agentd `llmsafespaces_*` handlers**: the
  #1530 duplicated-argument recovery warning (with a canonical-to-
  canonical origin comparison — a fragment whose embedded origin is
  the same session in the other id space is not a mismatch), the
  multiple-busy refusal, did-you-mean clauses (phrase-only rendering —
  the harness id no longer parenthesizes the canonical address), and
  metadata/compact/send error paths render session ids as phrases
  whenever the replica knows them (ranking stays keyed on the internal
  id). The #1530 recovery itself now accepts 3-word phrase targets —
  phrases are the canonical address and the emission slip follows the
  form callers see. Board manager tools' declared `sessionId` fallback
  resolves phrase refs like every other id-taking input.
- **e2e (the issue's acceptance)**: phrase-targeted `send_message`
  and `abort_session` proven through the real chain — pinned opencode
  binary, real origin-injection plugin, real tool dispatch, warm
  phrase replica — with the harness id asserted absent from results;
  the full-id window still accepts with the notice.
### Added — Task Board: swimlane creation buttons + collapsible epic bands (#1832 items 1-2)
- **Contextual creation per project swimlane.** Every swimlane header
  now carries its own New epic / New task buttons; the New-epic dialog
  is retargeted to the swimlane it was opened from (no dropdown
  selection required — the global toolbar button remains but is no
  longer the only path, and is still greyed at All-projects scope).
- **Collapsible epic bands inside each swimlane**, tasks grouped
  beneath their band (title, short id, per-lane derived progress,
  collapse toggle; default expanded). The band header row is the
  selection surface: clicking it highlights the epic's task cards and
  dims the lane's others (display-only — never filters, never crosses
  swimlanes); double-click opens the epic drawer.
- **The selected epic scopes task creation.** The swimlane's New task
  lands inside the selected epic; with no selection an epic picker
  opens (with a create-epic escape hatch for empty projects).
- **Epics are sourced from the engine's list seam (review round).**
  Bands, the create-task picker, and the scope-switcher chips read
  `GET /me/projects/:id/epics` (#1699's `ListEpics` engine read — the
  client used to infer epic existence from task rows, so a zero-task
  epic vanished after reload and invited duplicate creation). A failed
  epics read renders the board load error, never a silent empty lane;
  task-row derivation remains as the mid-load fallback and this
  session's created epics bridge the POST→refetch window. Filtered
  bands distinguish "No tasks match the current filter." from a true
  "No tasks yet."; creation flows gained unhappy-path unit/e2e rows
  (failed epic/task POSTs keep the dialog open, error rendered, fields
  preserved) and reversion tests for the band-header hit surface and
  the per-lane task routing.
- **E2E port isolation**: the Playwright dev-server port is
  `E2E_PORT`-driven (default 5173, `--strictPort`) so concurrent
  sibling worktrees cannot latch a foreign server; this lane runs
  18519 (nightly PORTFWD_PORT registry 18083–18093 untouched).
### Added — task-detail drawer with the full audit timeline (#1832 item 4)
- **The board's task cards open a detail drawer that renders the task's
  full history.** Header (title, short id, stage, attempts, claim/lease
  with assignee and lease-until, blocked-by with live dependency stage
  chips), the charter and acceptance criteria with met state, and the
  full audit timeline: stage transitions (from→resulting, reclaimed
  flag), claims (who/when via the engine claim row's worker + attempt),
  manual task_start dispatches, structured review verdicts with
  per-finding severity/pointer/criterion when one rides a review-bounce
  move, dependency-graph changes, and orphan/unblock events — plus
  explicit loading/empty/error-with-retry states. The card's primary
  click keeps its edit-dialog behavior; the drawer opens from a
  dedicated ⓘ details affordance.
- The history needed a read seam: the §4.6 SSE board.event projection
  only carries post-subscribe commits, and no REST read exposed
  board_events. `GET /api/v1/me/tasks/{id}/events` returns the task's
  rows chronological ascending behind the same owner perimeter as every
  task read (foreign rows are plain 404s), with a newest-N window
  (`?limit=`, default 200, max 500 — lease heartbeats write a row per
  call, so long-lived claims stay bounded). OpenAPI schemas and the
  frontend client ship with it.
### Fixed — CI flake: TestRequestSessionStatusSnapshot_Coalesces coalescing precondition now established, not hoped for (issue #1907)

- **The `-short`+coverage-suite flake (run 39914818351, "got 2")** was a
  test-design flaw, not a product race: the on-demand snapshot
  trigger's coalescing guard (`busyFlightsInFlight`, an atomic CAS)
  coalesces a second request only **while a flight is running** for
  that workspace — after a flight completes, a new trigger legitimately
  starts a new flight. The old test fired two POSTs "nearly
  simultaneously" and hoped both guards landed inside the first
  flight's sub-millisecond window (the statusz stub answers instantly);
  under CI load the second POST's dial+handler latency exceeded that
  window, the second trigger legitimately spawned its own flight, and
  the monotonic fetch counter read 2 with `assert.Eventually(==1)`
  unable to ever recover (the 2.01 s signature).
- **The rewrite parks the first authority fetch in the stub and fires
  the second trigger only once that fetch is provably mid-flight** —
  the map entry cannot be gone yet (Delete runs only after the fetch
  returns), so the second guard MUST coalesce. Four independent nets
  assert it: the parked-phase fetch count, a stub-side flag on any
  second fetch whenever it lands, a pre-release `snapshot_complete`
  drain (impossible while the only flight is parked), and a
  post-settle cleanup verdict. Mutation-verified red-first at 613238f3
  under BOTH causal classes — a neutered guard and an early Delete
  (the steady-state analogue of what CI load did transiently) — each
  deterministic 3/3 under `-race`. No production code changed; no
  sleeps/retries/skips added.
- Companion verification (no diff): the #1842 upload-stream fix
  (2a300c9d) re-verified on current main — `-race -count=50` green and
  its mutation recipe (threshold → `cap1MiB`) re-confirmed to redden
  both streaming tests deterministically.
- Follow-up hardening of the coalesce pin (lane-1907, PR #1916): (a)
  the parked fetch's release is closed idempotently in a `defer` — a
  require/fatal firing while flight 1 is parked (trigger-2 assert,
  drain verdict, or the park wait itself) otherwise strands the
  harness's stub-server `Close` cleanup until go test's 10-minute
  timeout (proven: injected early-fatal on the merged body panics
  `test timed out after 20s`; hardened body fails cleanly in 0.01 s);
  (b) the pre-release drain now also counts `snapshot_begin` — a
  second flight's begin is published in-process BEFORE its authority
  fetch, so `begins > 1` catches a begin-only mutant flight the
  complete-only drain never sees. Both causal mutations re-verified
  red 3/3 under `-race` on the hardened body (neutered guard; early
  `Delete`), 0.01 s exits; still no production changes and no
  sleeps/retries/skips.
### Fixed — reap-net coverage gap: engine-authorized and DAG-leg carrier deletes origin-mark before effects (#1915)

- **Every effects-bearing session delete the engine authorizes or
  observes now runs the mark→effects→clean discipline** previously
  unique to the Scheduler's `propagateCarrierDelete` (#1882/#1901).
  Pre-#1915, two leg families ran the platform delete effects with NO
  origin mark: `deleteSessionAuthorized` (the caller-neutral core
  behind the routine PreserveOnFailure-success cleanup, the
  retry-intermediate cleanup, and the DAG reconciler's node-retry
  cleanup) and the DAG reconciler's `propagateDeletedSession` (both
  envelope legs — errorCode and delivered). The effects' error return
  is void, so when the effects' session_index writes failed (or the
  effects tail died mid-sequence) the ghost row carried **no carrier
  mark**: the #1901 reap bypass (`ListCarrierOriginMarks` —
  kind='carrier' rows only) could not admit it, and a history-bearing
  kept_for_operator ghost survived until operator review — a
  self-heal coverage gap, not a wrong-reap risk (these are
  policy-deleted sessions, never preserved ones).
- **`ReconcilerStore` gains the origin seam** (`RecordSessionOrigin` /
  `DeleteSessionOrigin` — the structural cause was their absence from
  the reconciler's store interface); the concrete store already
  implemented both, so no store-package change. The authorized-delete
  core takes a deliberately NARROW `SessionOriginStore` (the two
  origin verbs, not a full store interface) so the caller-neutral
  dependency stays minimal; nil degrades to the pre-#1915 unmarked
  behavior for unwired/embedded engines.
- **Fail-safe semantics, mirroring the carrier tail:** the kind=
  'carrier' mark is best-effort BEFORE the effects (a failed mark
  logs; the effects still run — only the reconcile self-heal
  degrades), and the clean after is best-effort too — when the same
  DB outage that failed the effects' index writes fails the mark
  clean, the mark SURVIVES as the reap bypass's admission data
  (pinned). The #1901 item-b invariant is untouched: a refused delete
  (the session survived) writes no carrier mark, and preserved
  sessions' kind='session' verdicts stay invisible to the bypass.

## [0.43.0] - 2026-10-25

### Added — generalized stage-persona claim-then-spawn (owner directive 2026-10-24; #1941)

- **One mechanism, any stage, any persona**: entering a stage with a
  persona configured claims the row (re-claim-class mint: fresh token +
  lease, attempts unchanged) and spawns a fresh session whose charter
  carries the configured agent persona. The dormant parallel
  auto-review assignment path (#1832's token-less, lease-less
  ReviewAssigner) is replaced entirely.
- **Config-driven via `stagePersonas`** (user + project tiers,
  migration 000049; per-KEY merge; an explicit `{}` owns the stage
  mapping outright and is the opt-out the flipped default floor
  needs). Legacy `reviewColumn=auto` + `autoAssignReviewer=true`
  composes as the default reviewer persona — existing stored configs
  keep their meaning.
- **Board semantics preserved**: claim-before-spawn, token-gated
  forward writes, liveness = new progress evidence, FIFO +
  dependencies-only blocking, human-only pending_input exits.
- **Dead-persona-worker healing**: RC1 persona pass with doubling
  backoff, fresh-worker re-arm under the pure zero-config default
  posture, attempts-cap escalation to pending_input; pause doctrine
  respected at scan and mint. The #1832 WS4 stillborn guard composes
  (zero-turn persona workers lapse into the healing loop).
- **Wire surface**: `stagePersonasSet` set-ness field on BoardConfig
  (openapi documented) so settings save-paths never materialize or
  wipe stored maps.

### Fixed

- **#1922 pin deflake**: PayloadAssigneeIDs map-order flake made
  order-insensitive (set compare).

## [0.42.0] - 2026-10-24

### Changed — Task Board: auto-review defaults flipped to on (owner ruling 2026-10-24)

- **The code defaults for review automation are now `reviewColumn=auto`
  + `autoAssignReviewer=true`** (was `manual`/`false` — the #1832
  landing default that left auto-review dormant for every
  freshly-created board project). A fresh project with zero config rows
  now dispatches a reviewer directly on →in_review.
- **Explicit stored opt-outs still win** (back-compat): the flip changes
  only the inheritance floor of the code → user → project read-time
  merge — a deliberate `reviewColumn=manual|off` or
  `autoAssignReviewer=false` at either tier (board-wide defaults or
  per-project overrides) keeps auto-review off; clearing the map
  (REPLACE `{}`) re-inherits the new default. Pinned in the PG harness
  (`TestReview_AutoReviewDefaultOn`,
  `TestReview_ExplicitOptOutSurvivesDefaultFlip`).
- OpenAPI `BoardConfig` documents the new defaults; the Settings
  pre-data display fallbacks match the new floor.

## [0.40.1] - 2026-10-21

### Fixed — v0.40.0 regression: user-stream session events carry the phrase id (#1817, #1821 piece 1)

- **The busy-signal/live-streaming id join is fixed at the user-stream
  and contract-stream edges, within the heal window.** The
  #1756/#1775/#1806 phrase re-keys flipped the frontend's session
  identity (sessions list, history REST) to the phrase space while
  user-stream `session.status` / `agent.question` /
  `agent.question.resolved` events kept emitting the harness id — the
  join missed for every minted row, killing live busy tracking
  (sporadic busy only on degraded rows) and live streaming.
  The API now reverse-resolves at the emission surface: the wire
  `sessionID` is the PHRASE, with the harness id preserved additively
  as `internal_session_id`; agentd's contract-stream frames carry the
  same phrase + additive harness id. A degraded index (no phrase
  minted, or an un-refreshed pod that has not yet learned the phrase)
  passes the harness id through unchanged — never worse, but the join
  is only complete once that heal window closes.
  The API-side consumer also routes the additive harness id through
  every seam (status, resolved input, context usage) so a refreshed
  pod's already-translated frames never re-register the phrase (no
  phantom index rows, context occupancy lands on the harness row).
  agentd's replica warm was redesigned as ONE sync.Once worker (not a
  goroutine per emission) fed by a bounded pending set (hard cap 4096;
  an id already pending is kept, overflow drops the newest id, and a
  dropped id re-enqueues only on its next emission — so a quiet session
  may stay harness-keyed until then or until its next refresh), with
  per-id exponential backoff (1s → 30s cap), a GLOBAL 1s floor between
  warm pulls (distinct newly-seen ids batch instead of pulling once each,
  while a new session still warms within a second), and a single-pass
  prune of the idle TTL and backoff ceiling (the pending cap is enforced
  on enqueue). The worker is bound to the
  process background context and is cancelled at shutdown, where
  runShutdown waits on it for up to 5s; the pull's own 10s context bounds
  the remainder.
  Coverage now spans the dual-id fixture class (mock index rows keyed
  phrase + ABI events keyed harness id, T0.1 phrase-contract pins, the
  T1.6 cross-surface join tripwire), live-goroutine/global-pull-floor/
  backoff pins, and new Postgres-backed integration levels: translated
  authority stream → real consumer → real `usageBridge` → real
  `sessionindex`, the create-via-API registry round trip, the
  legacy-frame phrase translation, and the agentd replica warm against
  the real registry contract. A translation miss on the user-stream edge
  flags the passthrough harness id as `session_id_deprecated` (map-shaped
  `session.status` / `agent.*.resolved` payloads only; the struct
  `agent.question` payload is left intact), and the lifecycle clear call
  sites (idle reconcile, delete, archive, admin abort) are pinned through
  their real paths.
- **Frontend heal-window id seam (#1817, additive).** The app now learns
  per-workspace (harness → phrase) aliases from the additive ids
  (`data.internal_session_id` on user-stream events, `internalSessionId`
  on contract frames), bounded at 512 aliases per workspace with FIFO
  eviction, identity when no pair is known (never worse), with an
  equal-id decoy guard and late re-key of fold rows that arrived before
  the pair. This closes the API-new/agentd-old and API-old/agentd-new skew
  windows. It cannot help when BOTH streams are fully pre-fix (no additive
  id anywhere) — those workspaces still need a refresh/bounce. The
  red-first E2.1/E2.6 Playwright pins drive pre-fix wire shapes on the
  surface under test (composer send + sidebar collapsed-workspace busy
  count, harness-keyed idle clear, reconnect lost-idle wipe, pre-fix
  contract frames in the phrase view, and a non-vacuous same-id decoy);
  they fail on `origin/main` at the predicted assertions and pass with the
  seam.
- **Late-learned aliases now re-key provider activity; snapshot learning is
  batched (#1817, #1824 r10).** When a harness→phrase pair is learned after a
  harness-keyed user event has already latched, `SessionActivityProvider`
  re-keys its session-keyed activity maps (busy, unread, aborted, pending
  actions and prompt content, and the cleared-session ref) through the
  changed workspace's alias table, so the sidebar badges follow the phrase
  without waiting for a reconnect. The contract stream learns only applied
  frames, re-keys the fold only when the alias version moves (PART_DELTA
  stays off the O(all sessions) path), and collapses a multi-pair snapshot
  into a single alias notification and publish via
  `beginSessionAliasBatch`/`endSessionAliasBatch`. The r11 companion commit
  also hoists provider ref mutations out of state updaters so StrictMode's
  double-invoked updaters stay pure and resolves staging maps at commit.

## [0.40.0] - 2026-10-21

### Changed — trust `ai-workflows-propagation[bot]` for AI review + slash commands (#1810)

- `ai-workflows-propagation[bot]` (the ai-workflows propagation pipeline's
  own bot; authenticates with that repo's app credentials) is added to the
  canonical trusted-app-bot allowlist (`canonicalTrustedAppBots`,
  tests/ghaworkflows/gha_callers_test.go) and every gate copy:
  - `pr-review.yml` `trusted_app_bots` — its authored sync PRs (e.g. #1809)
    become AI-reviewable without a per-PR human `/review`.
  - `ai-comment.yml` comment-admission arm + `trusted_app_bots` — **trust
    expansion disclosure (#1710 discipline): this grants the bot the
    repo's slash-command surface** (`/review`, `/fix`, `/implement`, …)
    on PR and issue threads, not just PR review. The bot only authors
    sync PRs today (it does not post comments), so the practical new
    reach is nil; the canonical single-source contract requires all gates
    to carry the identical list, which is why the command gate moves too.
  - ai-workflows intends to default-trust this bot fleet-wide
    (ai-workflows#65); this list then becomes redundant on the next sync.

### Added — Task Board: agent manager tools + the #1747 amendment set (issue #1747)

### Added — workspace phrase IDs (#1757): 2 words + 1 digit, fleet-unique

- **Every workspace gains a minted phrase ID** (`amber-river-7`):
  two words from the #1756 frozen lists + one digit, FLEET-unique via
  the global partial unique index with check-and-retry (a collision
  re-draws — the digit slot is the disambiguator; there is no counter
  suffix). The UUID stays the internal key everywhere (migration
  `000038`, api + helm synced; creation mints, reads lazy-backfill
  pre-phrase rows). Namespace rule PINNED: shared wordlists with
  session phrases, disjoint BY SHAPE from them (3 words) and from the
  harness's 2-word no-digit slugs (the digit).
- **Addressing:** the workspace phrase is the canonical `:id` form on
  every workspace route — resolved to the UUID before the access gate;
  phrase misses answer a structured 404. UUIDs pass through
  byte-identical; unambiguous workspace NAMES rewrite during the
  #1756-mirroring deprecation window (owner-scoped, ambiguity never
  resolves, Deprecation/Sunset headers on the rewritten response).
  Routine triggers' `targetWorkspaceId` accepts the phrase (resolved
  FIRST; the #1729 gate and the stored key both use the UUID — user
  AND org trigger routes).
  `phraseId` surfaces on workspace metadata, the GET response, AND
  the list API (`WorkspaceListItem` + every SDK model: Go, Python,
  TypeScript, Java, openapi.yaml — the Python dataclass previously
  hard-rejected the GET field, caught by the live canaries).
  §4.3 anti-enumeration masking: unknown and foreign phrases answer
  byte-identical 404s on both the route and trigger surfaces (pinned
  as ONE JSONEq pairing on the same ref).
  Legacy rows converge on the first AUTHORIZED read (the user-scoped
  read, post-verifyOwner — the DB read is pure; no sweep job; the
  registration UPDATE never touches soft-deleted rows).
- **Deferred (explicit; completion tracked in #1792):** frontend
  display surfaces (dashboard, pickers, board refs — zero frontend
  changes in this PR; the list API now feeds them) and the admin
  `:workspaceId` route groups; the {workspace-phrase,
  session-phrase} cross-workspace SSE/SDK pair composes on these
  primitives and rides the #1756 §5 SDK-major follow-up. A workspace
  legitimately NAMED `word-word-digit` is addressed by the PHRASE
  interpretation (shape precedence — pinned in
  router_workspace_ref_test.go).

### Added — word-phrase session IDs; short-only emission; full-id input deprecation (#1756, supersedes #1745's format)

- **Session short IDs are 3-word grammatical phrases** (adjective-
  noun-verb from frozen curated lists, e.g. `amber-falcon-glides`):
  no hash characters, no harness prefix, no checksum word — words are
  high-probability LLM tokens, truncation is visible, and did-you-mean
  recovers word near-misses far better than hash fragments. A
  deterministic counter suffix (`-2`, `-3`, …) arbitrates mint
  collisions (never a random re-roll). The lists are frozen with
  pinned sizes/membership (160×192×64 ≈ 2M phrases/workspace;
  enumeration is ruled a non-issue — ids are references under authz)
  and exclude the harness slug vocabulary; session phrases, harness
  slugs (2 words), and the upcoming workspace phrases (2 words + 1
  digit, #1757) are disjoint by shape.
- **Outputs go short-only**: the session list's `id`, parentId (via a
  parent self-join), `EnsureSessionResponse.sessionId`, the did-you-
  mean suggestions' re-copy affordance, and the agentd MCP tool
  outputs (`session_list` row ids, `create_session` result, rename/
  archive/delete/abort results) now carry the phrase as THE
  identifier; the harness id moves to internal-only fields
  (`deprecatedSessionId` appears only in the registration-degraded
  window; unmapped `session_list` rows carry `id_deprecated` until
  the replica converges). The #1745 hash short ids stop being minted
  but stay addressable as input.
- **Full-id input is deprecated (one release)**: addressing a session
  by its `ses_…` full id or a #1745 hash id still resolves (truncated
  ids still never silently resolve) and now attaches the notice —
  `Deprecation: true` + `Sunset` headers on the API, a `deprecation`
  note in agentd tool results — naming the phrase as the address.
  SDK/SSE/frontend value-flips coordinate with the ABI short_id M2
  major (#1756 §5).

### Fixed — #1756 post-union remediation: the un-audited ID re-key consumers, §2 notice, and mint determinism (#1756/#1775 post-union review; PR #1806)

- **Five production regressions from the re-key, all re-keyed to the canonical harness id (`InternalID`):** the agentd registry pull no longer poisons the replica phrase→phrase (unminted rows are skipped — the replica install rejects empty short ids); the archived set matches harness row ids again (the #1735 leak is re-closed); session-gone reap verdicts fire (no more blanket "uncertain"); `RebuildMessageCount` no longer mints phrase-keyed ghost rows; the `context_used` self-heal fires (and the workspace CRD stops being fetched on every session-list call). `PlanReconciliation`'s error-path miss-carriers are keyed by `InternalID` too (reap convergence survives transient list errors).
- **§2 deprecation notice now fires for the PRIMARY deprecated form:** `ses_…` full-id passthrough (and a #1745 hash-id miss) set `DeprecatedInput`, so API routes attach `Deprecation: true` + `Sunset` — now an RFC 8594 HTTP-date (`Tue, 01 Dec 2026 00:00:00 GMT`, the v0.41.0 removal), not free text. Did-you-mean candidates carry the harness id for the ses_-form edit-distance leg (near-miss full ids suggest again; degraded rows get a ranking identifier).
- **Mint determinism per the ruling:** the base is drawn ONCE; a 23505 retry suffixes the SAME base (`-2`, `-3`, …) — two concurrent minters of one base converge, as the record always claimed.
- **agentd §1 surfaces completed:** `send_message` results (including `origin`), `session_metadata` rows, `session_read` bodies (session-id values rewritten best-effort), and `compact_session` results emit the phrase, flagged when the replica is cold — `session_id_deprecated` on single-id results, `id_deprecated` on `session_list` AND `session_metadata` rows (parity across every surface) and the input-deprecation note when addressed by a full/hashed id. The tool catalog no longer advertises the retired `oc-…` short ids.
- **Test hygiene:** the birthday-bomb mint test (P(collision) ≈ 6.2%/run, reproduced 3/40) is replaced by prefix-coverage + distinct-fraction asserts sized by Monte Carlo (P(fail) ≈ 1.4e-4/run); the mandated levels added — middleware Deprecation/Sunset pin, registry pull + archived set handler pins (emission contract; the agentd replica consumes both), reap/ghost/heal unit pins, `EnsureSession` minted/degraded paths, e2e happy+unhappy phrase addressing.

### Added — Task Board: the four #1699 engine-seam closings (design 0067)

- **Board search + health + epic listing — the board_read projections
  Lane 2 could not surface.** `GET /api/v1/me/board/search?q=…` (short
  ID exact, case-insensitive, or title substring; project/epic/stage/
  tag filters; zero-hit near-misses carry 0066-shape did-you-mean
  suggestions that never cross the ownership perimeter; bounded to 50
  results), `GET /api/v1/me/board/health` (the §4.8 health panel
  snapshot: active leases with expiry, the 20 most recent reclaims,
  task and project-slot cooldowns, pending_input by origin, per-project
  worker slots in use), and `GET /api/v1/me/projects/{id}/epics` (every
  row carries DERIVED progress — one aggregate join, no N+1; Done over
  ALL tasks per the #1747 owner ruling, which supersedes the original
  design's cancelled-exclusion denominator). Search results and the
  epic list render in creation order — the FIFO read-side mirror the
  same ruling sets (priority fields leave the schema with PR #1763). All three surface through the four SDKs, the hurl contract
  suite, and the external MCP projection (`board_read` grows `q` /
  `kind=epics` / `health=true`; a dedicated `board_health` tool joins
  the set — 19 board_* tools).
- **The `board.event` SSE publisher (design 0067 §4.6) — the board's
  live-update feed lights up.** Committed board events fan out on the
  existing user + workspace SSE channels (user copy = the project
  owner, ruling R7; workspace copy = the project's target) as flat
  `board.event` frames carrying the `board_events` row — the shape the
  merged frontend already consumes (invalidation + reconnect reseed;
  the e2e reclaim arm's `action=reclaim` streams from the reconciler).
  Publishing is strictly post-commit on BOTH write paths (user/engine
  eventWriter transactions and the reconciler's deterministic-id
  writes — a replayed event id publishes nothing) and fail-open: no
  broker or a failed project lookup logs and drops the projection; the
  UI's #1646/#1687 reseed belt is the consumer's safety net.
- **Lifecycle replay protection completed at every tier.** The six
  lifecycle actions (archive/unarchive project, activate/pause/archive/
  unarchive epic) now thread `requestId` into their event writers (the
  engine gap PR #1728's review flagged), remove-dependency carries the
  replay key as an optional `requestId` query parameter (DELETE has no
  body), the MCP lifecycle tools grow `request_id`, and the shared
  error mapping renders a `DuplicateError` replay as 200-with-original
  — closing a live-tier gap where a replayed requestId answered 500 on
  every surface. `replayExceptions` in the parity matrix is empty:
  every mutating action asserts the full idempotent-replay contract on
  both the REST and external-MCP columns.

### Changed — Review infra: admit the agentic-actor-coder app bot past the reviewer's startup assert (ai-workflows #51/#53; callers pinned v0.2.18)

- **Both AI callers pin `lenaxia/ai-workflows@v0.2.18` and pass
  `trusted_app_bots: '["agentic-actor-coder[bot]"]'`.** App-bot accounts
  can never be repository collaborators (the collaborators API
  structurally returns `none` for them), so the opencode CLI's startup
  `assertPermissions()` refused every PR our worker pods authored and
  every `/review` they posted before any review pass — the #1775 outage
  class (three dead synchronize runs + a dead `/review` fallback).
  Upstream v0.2.17 (#51) added the opt-in `trusted_app_bots` input
  (listed actors run in TOKEN mode — assert bypassed for them ONLY;
  every human and unlisted actor still passes the collaborator assert
  unchanged; fail-closed on malformed input); v0.2.18 (#53) fixed the
  bypass delivery after the first live rollout caught that step-level
  env never reaches the composite action's inner CLI step — TOKEN now
  rides `GITHUB_ENV` (validated end-to-end on this very PR's own
  review: run 39373330689 died at the assert on v0.2.17, run
  39374196693 executed the LLM on v0.2.18). The literal must match
  `canonicalTrustedAppBots` in `tests/ghaworkflows/gha_callers_test.go`
  (single-source ruling #1710, extended to the new input — comment
  lines stripped and `(?m)`-anchored so a commented-out or mid-block
  input cannot false-pass), and both callers' pin tests accept any
  `vX.Y.Z` tag instead of version literals (the propagate sed bumps
  pins without touching tests — the shape is the contract, not the
  literal; same relaxation applied to the renovate-analysis pin test).

- **The agent-internal board surface gains the FULL manager set**
  (design 0067 §4.3c superseded in the manager direction): 21 manager
  tools at project/epic/task level — create/update/archive/unarchive +
  pause/resume (projects), create/update/activate/pause/archive/
  unarchive/delete (epics), create (inline `dependsOn`)/update/delete/
  pause/resume + add/remove dependency (tasks), and atomic
  `import_plan` with a dry-run validate mode (graph + warnings +
  initial ready set — the user's plan-approval moment). Every manager
  route on the `/internal/v1/automation/board/*` seam is pod-identity
  authorized and workspace-bound by construction (the caller's own
  workspace only; agent-side project retargeting refused). Audit
  events attribute every row to the creating session
  (`board_events.actor.session_id`). `claim_next` stays engine-internal
  forever and `task_start` is dropped from agent surfaces — execution
  is epic/project activation + engine claiming.
- **Grant differentiation (#1747 §5)**: manager board tools attach to
  user-collaborator sessions ONLY. agentd fails closed without a
  harness-attested session identity, refuses sessions its spawn-time
  registry recorded as board workers (the `board-worker` node
  dispatch), and the seam belt re-checks the engine's live-assignee
  answer (`IsWorkerSession`) — a spawned worker never reaches a
  manager action.
- **Pause at project/epic/task — PAUSE MUST PAUSE**: a flag (never a
  stage; the machine is now FIVE stages). Pause blocks claiming (one
  query-scoped predicate at the choke points), forward stage-writes
  (409 `task_paused`), and every claim mint; it never cuts a
  generation — the in-flight worker receives a HALT directive over the
  platform's own session-message machinery (turn-boundary landing),
  and resume delivers RESUME. Lease accounting during pause is LAPSE
  (never frozen): the evidence gate needs no special case; a long
  pause reclaims naturally and the flag parks the row.
- **Cancel is ELIMINATED — delete replaces it**: no `cancelled`
  terminal stage; `delete_task` (clean abort first when in-flight) and
  `delete_epic` (force required while claims live) remove rows whose
  delete events embed the full final-state snapshot (per-task on
  cascade). Orphaned dependents are listed in the response and each
  gets an `unblocked_by_delete` audit event; NO auto-reconnect
  (`reconnect=true` optionally inherits upstream edges, cycle-guarded).
  Dependants unblock in place — blocked is "deps not done", full stop.
- **Ordering is dependency-only FIFO (final ruling)**: the P0-P3
  priority fields are REMOVED everywhere (schema, REST, SDKs, UI);
  claim order is deterministic creation order over the ready set,
  implemented in exactly one place (pkg/board's candidate ORDER BY).
- **DAG spec**: edges cross epics within a workspace, never
  workspaces; duplicate edges are idempotent no-ops; blast-radius
  caps (16 edges/node, 64 tasks/epic — platform knobs
  `board.maxEdgesPerNode`/`board.maxNodesPerEpic`).
  `epic_activate` refuses dependencies entering inactive epics unless
  acknowledged and reports the initial ready set.
- **Backpressure from day one**: per-user (default 8) + per-instance
  (default 64) concurrent-claim caps at the claim choke point —
  over-cap is never an error; reads surface
  `waiting_on: "concurrency_limit"` (`board.userClaimCap`/
  `board.globalClaimCap`). Resource backpressure: agentd reports pod
  CPU/memory/disk (static thresholds + hysteresis) over the new
  `/internal/v1/automation/board/resource-pressure` route; the engine
  stops claiming into pressured workspaces and surfaces
  `backpressure: "disk 92%"` on rows.
- REST + external MCP mirror the full vocabulary (parity matrix
  green on all three surfaces); SDKs (Go/Python/TypeScript/Java) and
  the OpenAPI spec updated; migration 000039 drops the cancelled
  stage + priority columns, adds the pause flags, relaxes the
  dependency guard to workspace scope, and keeps transition history
  alive across deletes.

### Added — Platform: the Go build-cache economy moves off the PVC (#1758)

- **Workspace pods now carry a dedicated disk-backed emptyDir for the
  regenerable Go caches.** `GOCACHE=/sandbox-cache/go-build` and
  `GOTMPDIR=/sandbox-cache/gotmp` are injected with the platform env
  block, and an 8Gi `SizeLimit` emptyDir (no `Medium` — capacity, not
  RAM) backs both paths via subPath mounts on every container that runs
  platform tooling (the main container and the `workspace-setup` init,
  whose read-only root filesystem would otherwise fail `go install`).
  Pre-#1758, Go's defaults landed the build cache in
  `$HOME/.cache/go-build` and build temp in `/tmp` — both subPaths of
  the workspace PVC, which is never resized — producing the #1744
  incident economy (55%→93% in minutes under five concurrent Go lanes,
  ~5G/hour regrowth; caches dying with the pod is the correct
  lifecycle). `GOPATH` deliberately stays PVC-mounted: the module cache
  is network-expensive to rebuild and valuable across suspend/resume
  (the disk-recovery lever sweeps it under pressure). Storage docs
  updated with the sixth volume and its trade-offs.

### Changed — Agent permissions: the three PVC mount points are read-write by default (#1759)

- **`/workspace`, `/tmp`, and `/home/sandbox` no longer permission-prompt.**
  The opencode config writer's platform permission floor now carries
  default-allow rules for all three PVC mount points (bare key +
  children glob each — the wildcardless keys shield the bare paths from
  the `/home/*` parent deny), replacing the pre-#1759 ambient-ask carve
  on the home root and the narrow subpath pre-allows
  (`.cache`/`.config`/`.local` — superseded by the root allow and
  removed as dead rules). Every credential and system deny keeps
  winning by the documented sort-order precedence
  (`'*' 0x2A < '.' 0x2E` — each deny sorts after the root allow glob
  and is the last matching rule for its paths): `.ssh`, `.secrets`,
  `.git-credentials`, `.local/opencode/auth.json`,
  `/sandbox-runtime/rt/auth.json` + `rt/secrets`, and the system denies
  (`/proc`, `/sys`, `/run`, `/etc`, `/dev`, `/root`, `/var`, `/home`).
  The full verdict matrix — allows present, denies still biting — is
  pinned in `permissiontiers_test.go`; the deny-preservation pin
  asserts every deny entry survives the amendment.

### Added — Automation: agentdDelivery updatePolicy — workspace pods no longer roll on platform upgrades (#1746)

- **`agentdDelivery.updatePolicy: Rolling | OnRefresh` (default
  `OnRefresh`) on the Workspace CRD + controller** (owner ruling
  2026-10-19: the user owns when their sessions are interrupted). Under
  `OnRefresh` a desired agentd digest change NEVER touches a running
  pod: the desired digest is recorded on the Workspace status
  (`agentdPendingDigest`) and applies at the next natural pod
  recreation — user compute refresh (`restartGeneration` bump),
  suspend/resume, or any controller-initiated recreation. `Rolling`
  (per-workspace spec, or instance-wide via
  `controller.agentdDelivery.updatePolicy` → `--agentd-update-policy`)
  recreates pods on digest drift drain-aware per #761 (deferred behind
  busy sessions until idle or stalled; `workspace_drain_deferred_total
  {reason="agentd_update_roll"}`). Resolution order: spec → instance
  flag → `OnRefresh`; a typo'd flag value fails controller startup and
  the Helm render. Drift compares the digest PORTION only — a repo/tag
  re-pin of the same digest never churns the fleet.
- **`agentdDelivery.forceRoll` escape hatch** (spec, any policy): roll a
  workspace IMMEDIATELY on digest drift, session drain BYPASSED — for
  security-critical agentd fixes; the operator setting it declares the
  consent to cut in-flight turns (the #1505 posture). Level-triggered:
  no drift is a no-op; clear it after use. Emits the
  `AgentdUpdateForcedRoll` event and
  `workspace_drain_forced_total{reason="agentd_update_roll_forced"}`.
  The new deletion site is classified in the #1724 deletion-path
  inventory (`upgrade_rollout_kill_test.go`).
- **Status drift visibility**: Workspace status carries
  `agentdEffectiveDigest` (digest the current pod was built with) and
  `agentdPendingDigest` (desired, unapplied — "applies on next
  refresh"), exposed through `GET /workspaces/{id}/status`
  (`agentdEffectiveDigest` / `agentdPendingDigest`) for the dashboard.
- **N-1 compatibility discipline for the worker-tool seam**: the frozen
  wire contract of `/internal/v1/automation/board/*`
  (`board_heartbeat` / `board_report_throttle` / `board_move_stage` /
  `board_read`, captured from agentd v0.39.0) lives in
  `pkg/board/worker_compat.go` as a tested skew matrix — the frozen
  sets replay verbatim against the CURRENT API
  (`board_worker_compat_1746_test.go`) and the live agentd client must
  emit exactly the newest set (`loopback_board_wire_1746_test.go`).
  Wire changes are additive (a new frozen set or `/internal/v2`),
  never v1 mutations.
- Docs: `docs/operator/agentd-delivery.md` (policy matrix + escape
  hatch + N-1 discipline), `docs/operator/storage.md` (platform-delivered
  volumes pointer), `docs/architecture/components.md` (controller policy
  + the versioned board seam contract).

### Added — Disk recovery: the #1744 regenerable classes + open-handle/floor/staleness gates (#1744)

- **The workspace pod's disk-recovery allowlist now covers the caches
  that actually fill the PVC.** Nine classes covered ~2G of the
  typical footprint while the biggest regenerables went unchecked —
  the incident pod logged three ENOSPC episodes in a day (one broke a
  Go test run at 100%) and needed four manual orchestrator sweeps for
  97%→60% swings. New classes (with observed live sizes): playwright
  browsers `~/.cache/ms-playwright` (641M), the session caches
  `/tmp/opencode/{gocache,goocache,npm-cache}` (2.4G live; the new
  `/tmp/opencode` root is a parent root — live session scratch and
  GOBIN tools under it stay untouchable, only the three cache dirs are
  entries), stale Go temp scratch `~/.cache/gotmp*` incl. `gotmp-w*`
  worker dirs (the 120s corpse rule), the golangci-lint cache (103M),
  npm's `_cacache`/`_npx` (260M; the existing `npm-cache` class is
  NARROWED from the whole `~/.npm` root to `_cacache` — `npm cache
  clean`'s own target), and the kubebuilder-envtest assets (163M) —
  regenerable but under the protected `/workspace/.local` package-home
  tree, allowlisted via an exact-directory root (the protected list
  itself is untouched).
- **Three safety gates make those classes reapable without pulling
  them out from under a live process.** (1) An open-handle scan — one
  `/proc/*/fd`+`exe`+`cwd` snapshot per sweep: any class (or prefix
  child) with an open path under it is never a deletion candidate and
  reports the new `in_use` status with its measured bytes visible. A
  `/proc` that cannot be enumerated at all fails the scan closed
  (everything `in_use`); individually unreadable tables (foreign-uid
  processes, mid-execve transients, non-dumpable session helpers like
  `(sd-pam)`) are skipped by documented policy — none exist inside the
  workspace container's pid namespace where the engine runs (verified:
  7/7 tables readable, one uid).
  (2) Per-class size floors calibrated to the observed full-set sizes —
  below the floor a tree is a partial download or debris and reports
  `not_present` with the floor in the reason. (3) An optional
  staleness gate on directory classes holds freshly-written (hot)
  caches whole. The frontend strip renders the new classes and the
  `in_use` verdict (pinned bytes shown, excluded from the reclaimable
  total); the REST contract documents the classes and gates.

## [0.39.0] - 2026-10-19

### Added — Automation: routine triggers can target other workspaces, owner-scoped (#1729)

- **`trigger_create` (agentd automation surface) accepts an optional
  `targetWorkspaceId` for routine triggers.** The routine then runs IN
  the target workspace, with that workspace's tool surface and pod
  identity — the deploy-resume choreography (one orchestrator session
  waking several workspaces) no longer needs manual per-workspace
  setup. Authorization is against the trigger owner's rights on the
  target via the workspace service's ownership check (creator or org
  admin): 403 without rights, 404 for a target that does not exist
  (the cross-workspace disclosure policy). On the org REST route the
  gate authorizes the ACTING user's rights on the target (the org id
  is not a user; pre-r1 that arm could only ever 403). Omitting the
  param keeps the previous behavior exactly — the creating pod's
  workspace is force-stamped (#1412), delegated body byte-identical.
  The `trigger.create` audit event now records both ends
  (`targetWorkspaceId` + `creatingWorkspaceId` when created from a
  workspace). Workflow-targeting rules are unchanged
  (`workflowId` + `targetWorkspaceId` is rejected), the key is
  create-only (`trigger_update` and the REST PATCH surface both reject
  it with one shared message — #1426 keeps retargeting scoped), and an
  unwired ownership gate fails closed (500). The user REST API's
  `CreateTriggerRequest` carries the same optional field under the
  same gate.

### Added — agent-internal Task Board worker tools (#1701)

- **The worker's board surface is live in the agentd MCP registry**
  (design 0067 §4.3c): exactly four tools — `board_heartbeat`,
  `board_report_throttle`, `board_move_stage`, `board_read` — riding
  the `/internal/v1/automation/board/*` seam with the pod SA token,
  bodies passed through verbatim ("the platform validates"). No claim
  logic lives in agentd: claiming stays engine-side (R9
  claim-then-spawn), manager actions stay on the owner's surfaces, and
  the parity matrix's agent column now enforces BOTH tiers — the seam
  route table and the agentd registry source (a `board_*` tool added
  without a matrix row, or a manager/`claim_next` tool registered at
  all, reddens `TestBoardParity_AgentRegistryColumn`).
- **Claim-token semantics on the worker surface**: the tool schemas
  mark `claimToken` required on every in-flight stage/lease write and
  the dispatcher refuses a token-less call pre-wire; platform guard
  classes (409 `claim_token`, 404 + did-you-mean) surface verbatim in
  tool errors. Pinned end-to-end against a real BoardService at the
  seam: token-less/wrong-token refusal, single-use (a forward exit
  consumes the token) + expiry (a reclaimed claim's token is dead; the
  re-claim mints a fresh one), attempts accounting (the claim is the
  only increment; a live 429 rolls it back), the foreign-task 404-class
  (a pod never resolves another workspace's tasks — the real token
  buys nothing), and the pending_input self-exit refusal (a valid token
  does not unlock a manager-surface exit).

### Added — Task Board frontend UI: kanban/swimlanes, drag, scope switcher, SSE feed (#1702, design 0067 Lane 4)

- **BoardPage at `/board`** (nav beside Workflows/Triggers): kanban with
  columns = stages (Pending Input + Cancelled as collapsible side
  columns) and swimlanes = projects, over the #1699 REST surface
  (`/api/v1/me/projects|epics|tasks`, wire shapes verbatim).
- **Drag = server-arbitrated `move_stage`**: optimistic move with
  snap-back; the engine's 409 guard codes (`done_guard`,
  `stage_transition`, `claim_token`, …) render verbatim; a → done drop
  collects the evidence URL and previews unmet acceptance criteria
  without vetoing (ruling R2's Done guard stays the server's call).
- **Scope switcher**: project focus, epic chips (derived from task rows —
  display inheritance, R2), tag filters, and short-id/title search with
  did-you-mean suggestions (0066 shape).
- **SSE live feed on the #1687-hardened user stream**: `board.event`
  payloads invalidate the board read model; reconnect reseeds it; a 30s
  reconciliation belt backs the stream (design §4.6 ii).
- **Create/edit surfaces**: project (one target workspace, R8), epic
  (outcome = the steering wheel), task (criteria editor with reviewer
  check-off, structural dependency add/remove rendering the 409+code
  and 404+suggestions shapes with clickable suggestion chips, R3/R4),
  all with client-side OR4 `pr:N` anchor validation
  (`^pr:[1-9][0-9]*$`, submit-blocking).
- Cards carry the mono short id, priority chip, tags, the structural
  blocked badge (🔗 N with live dependency stage chips, R3), the
  evidence-backed assignee busy indicator, and the attempts counter with
  its escalation preview (⚠ at the R6 documented cap default — the engine
  stays the authority).
- **task_start affordance on backlog cards** (the designed UI exit —
  backlog stage moves are not machine-enumerated): manual one-off
  dispatch rendering the engine's Claim honestly, escalation notice
  included.
- **Epic drawer activity feed** (§4.8): live board.event activity
  filtered to the epic's tasks via an in-page fan-out from the single
  user SSE connection (`lib/boardFeed.ts`).

### Changed — archived sessions are opt-in on session listings (#1735)

- **agentd MCP `session_list` and `session_metadata` default to
  non-archived sessions** (long-lived orchestration workspaces
  accumulate an unbounded archived set — 60+ after a week — that every
  listing shipped as ambient context noise). `include_archived:true`
  is the explicit opt-in on both tools; a single-ID `session_metadata`
  lookup of an archived session still resolves (the filter applies to
  listings/aggregates only), the per-entry `archived` marker stays,
  and the filter is freshness-gated and advisory — a platform outage
  degrades it to the unfiltered list within one cache window (~15s;
  the last-known set never filters past its freshness), never a failed
  tool.
- **API `GET /workspaces/{id}/sessions` excludes archived rows by
  default**; `?include_archived=true` restores the full list. The
  filter is response-shape only — index reconciliation and
  single-session routes still see every row. Documented in
  `sdks/openapi.yaml` and `docs/api/rest.md`; the Go, Python
  (sync+async), and TypeScript SDKs expose the flag.
- Callers migrated: the frontend sessions queries (Sidebar + ChatPage,
  which render the Archived group) opt in explicitly (pinned); the
  e2e route mocks match the opted-in query; the #1627 metadata
  annotation test pin opts in explicitly.

### Added — external platform MCP server tools for the Task Board (#1700)

- **18 customer-facing `board_*` tools on the platform MCP server**
  (design 0067 §4.3b): project/epic/task create + update, lifecycle
  (epic activate/pause/archive/unarchive, project archive/unarchive),
  `board_move_stage`, `board_start_task` (manual dispatch), `board_cancel`,
  `board_add_dependency`/`board_remove_dependency`, and the read tools
  (`board_list_projects`, `board_get`, `board_read`). The tools ride the
  owner's REST projection (`/api/v1/me/*`, ruling R7), so guard outcomes,
  idempotent replay (same `request_id` → original outcome, through the
  tool path), audit-row stamping, and the read perimeter (foreign rows =
  plain 404) are identical to the REST column by construction — each
  pinned live in the board parity matrix's external column, which flips
  from pending to live (worker-only rows — heartbeat/report_throttle, and
  engine-internal claim — pinned absent by design; external actors hold
  no claim tokens).

### Added — harness-abstracted short session IDs + alias layer + did-you-mean (#1691, design 0066)

- **Sessions gain a platform-minted short address** (`oc-ab12cd34`:
  harness prefix + 8 chars over `[0-9a-z]`, per-workspace unique,
  case-insensitive at resolution) beside the 30-char harness id:
  migration `000037` (nullable `session_index.short_id` + partial
  unique index `(workspace_id, short_id)`), the single registration
  seam `EnsureSessionRegistered` (crypto/rand mint in Go, check-and-
  retry on the unique index, NULL-backfill) wired into all six
  row-creating index paths + `POST /sessions/new` + the reconcile
  walk's bounded backfill sweep. The harness id becomes an internal
  detail at the edges; internal joins stay keyed on it (additive only).
- **The keyword alias layer (design 0066 ruling 5)**: a session's
  current title — and, in the agentd tools, its harness slug
  (`sunny-wolf`) — is a first-class resolvable alias, unambiguous-only
  within the workspace. Aliases resolve at request time against the
  live name holders; they are never persisted as references. Every
  id-taking API route (message/history/get/abort/delete/queue/title/
  seen/archived/admin force-abort) and every id-taking agentd MCP tool
  (send_message target + origin, session_read, session_metadata,
  rename, abort, archive, delete, compact) accepts short ids and
  unambiguous aliases; a shared precedence (short id > legacy id
  byte-identical > alias) never rewrites legacy ids and never probes
  them first — full-ID addressing is byte-identical with zero added
  round trips, and truncated ids never silently resolve (#1663).
- **Did-you-mean on every resolution surface** (the #1667
  generalization): one shared deterministic ranking (bounded
  Levenshtein over short + legacy ids for id-form input;
  word-boundary prefix / token overlap over titles + slugs for
  alias-form input; last-active DESC tiebreak; top-3, always — the
  cross-workspace tail included) serves the API's structured
  `404/410 + suggestions:[{title, shortId, sessionId, workspace,
  lastActive}]` payloads (the typed session_gone 410, the archived
  404, admin force-abort, alias ambiguity) and the agentd clause
  (candidates = replica short ids + harness ids + titles + slugs).
  Workspace-scoped first; only when nothing scores and the requester
  is known does one bounded exact-match query cover their OTHER
  authorized workspaces. Ambiguity is never auto-resolved;
  authz/transport/5xx never suggest. On a transient alias-lookup DB
  error, resolution degrades by shape: seam-charset-valid refs pass
  through unchanged (never a false 404), while refs the seam could
  never accept as ids answer a new honest-availability **503**
  ("session resolution temporarily unavailable") on the id-taking
  routes — no suggestions, and no write ever sees title text as a
  session id (no ghost-row mint). The #1340 session-not-found error
  mapping stays scoped to the session-READ routes exactly as its
  covenant documents.
- **The agentd replica + control plane**: `/sandbox-runtime/
  session-shortids.json` (memory-backed class, 0600, atomic rename)
  populated by boot pull, TTL-limited on-miss pulls, and
  create_session's register round trip over the new pod-identity
  endpoints (`GET /internal/v1/session-registry`, `POST
  /internal/v1/session-register`); agentd never mints. create_session
  returns `short_id` (omitted on control-plane timeout, self-heals);
  session_list decorates rows with `shortId` and stays byte-verbatim
  when the replica is empty; `GET /sessions` gains the additive `q`
  filter (title substring / short-id prefix / legacy-id prefix; an
  empty id-like miss answers the suggestions payload; no `q` ⇒ the
  bare array byte-identical); `SessionListItem`/`EnsureSessionResponse`
  carry `shortId` (omitempty); OpenAPI additive updates.
- **Out of this PR** (sequenced per design 0066): the ABI `short_id`
  fields (`Session` 13 / `ActionResult` 12) + regen and the frontend
  URL flip. Sequencing note (design §7 inverts here deliberately):
  this PR lands the M3 addressing/acceptance surfaces ahead of the
  M2 ABI fields because the REST/tools layer is what the orchestrator
  lanes address sessions through today, the ABI fields only carry the
  same ids as data, and both orders converge on the design's Phase C
  road — the ABI regen lands with the SDK/OpenAPI flip that consumes
  it. Two tracked follow-ups: design 0066 §4.3(c) push-on-mint along
  agentpush (the replica converges via boot + on-miss pulls in the
  meantime — the design's own "advisory, nothing is ever unaddressable"
  discipline), and the send surface's bare-502 on id-form misses — a
  deliberate exception to ruling 8's everywhere-suggests intent forced
  by the #1340 covenant (the session-not-found classification stays on
  the read routes), to be revisited with a typed verdict from the
  harness.

### Fixed — api/relay rolls no longer wedge in-flight sessions (#1732)

- **Fleet relay-router drains in-flight generations on deploys** instead
  of hard-cutting them after 10s (the v0.38.0 incident's halt leg):
  SIGTERM now drains within `DRAIN_GRACE` (default 10m, the #1078 BYO
  parity sizing), and the chart adds preStop + `terminationGracePeriodSeconds: 630`
  (`controller.inferenceRelay.router.{drainGrace,terminationGracePeriodSeconds}`).
- **Roll-classified turn auto-resume (agentd)**: a provider stream cut
  mid-generation (assistant `finish: unknown`, no `session.error` —
  wire-proven distinct from user abort's `MessageAbortedError`) fires
  ONE bounded re-prompt per interrupted turn (audit-tagged, cooldown +
  hourly cap, `AGENTD_TURN_AUTO_RESUME=0` kill-switch,
  `workspace_agentd_turn_resumes_total`). User aborts never resume.
- **Usage-gate re-arm after an api roll**: the api's state reconciler
  (immediate first pass + 60s tick) re-arms busy-gated pod streams for
  Active workspaces with live sessions in the roll-surviving ws_state
  store — the platform's persistent subscriptions (billing + user-stream
  state bridge) return without new client activity. D1-B holds: idle
  workspaces still open nothing on a deploy.
- Runbook: post-roll re-establishment verification table in
  `helm/MONITORING-OPERATIONAL.md`.

## [0.38.1] - 2026-10-18

### Fixed — Platform-upgrade blast-radius contract pinned + rollout kill-tests (#1724)

- **The v0.38.0 incident's suspected mechanism is refuted and the real
  invariant is now pinned.** Code-path analysis (posted on #1724)
  showed overlay digests enter the workspace pod spec only at BUILD
  time (`wireAgentdOverlay` / `wireOpencodeOverlay`) and no reconcile
  path ever compares a running pod's overlay against the configured
  delivery — an overlay-digest bump cannot and does not delete running
  pods (the #761 drain never fired because no deferrable deletion ran;
  the identical-shape v0.37.0 bump had no incident). Red-first
  kill-tests (the #1719 pattern, extending the #761 suite) now pin the
  contract: an overlay digest change with busy in-flight sessions
  produces zero pod deletions, zero drain events/metrics, and no
  in-place spec mutation; idle pods are not churned; a pod recreated
  for an unrelated reason picks up the new digest (the lazy roll
  rolls); and a source-guard inventory test pins every classified
  `deletePodByName` call site to its #761/#1507 drain ruling — it fails
  CI when a deletion appears in a new or renamed function (a call added
  inside an already-classified function or a raw `r.Delete` bypass is
  the behavioral kill-tests' layer, not the guard's). A blast-radius
  runbook section
  (helm/MONITORING-OPERATIONAL.md) documents which Helm fields touch
  workspace pods (digests: new pods only; api tag: SSE re-connect;
  org status: the one fleet-wide no-drain kill path) plus the
  discriminating metric series for rollout-incident triage. The live
  v0.38.0 trigger remains under owner telemetry review; the D20
  org-suspension drain question is escalated for ruling on the issue.

### Fixed — stuck busy after an aborted turn (busy-truth machinery)

- **A session no longer shows busy forever after a user aborts a
  long-running tool call mid-turn** (#1722): wire-proven against the
  real harness (opencode 1.18.10 + 1.18.15 probe), the abort path DOES
  emit its busy-clear burst — `session.error` + `session.status idle` +
  `session.idle` + the aborted tool's terminal update — but the whole
  burst lands in ~30ms on agentd's RAW `/event` subscription, which is
  replay-less and recycled every 5 minutes (`sseConnectionTimeout`)
  plus 2s-backoff reconnects. A burst lost to a recycle gap wedged
  THREE layers: the `sessionStatusTracker`'s raw mark stuck busy
  forever (no evidence settle existed — blocking config-apply restarts
  and suspends via `trackerHasBusyOrUnknown`, skewing the busy gauge
  and the #1685 evidence clock); the sessionstate authority healed from
  store evidence on its 15s reconcile pass but **mutated silently** (no
  seq, no fold, no fanout), so the API usagestream consumer never
  consulted, never flipped, and the user stream's `session.status`
  badge stayed busy until the next turn's events — the reported
  symptom, now also an alerting symptom post-#1685 (a genuinely stuck
  busy flag trips the evidence-gated nudge). The fix defines the abort
  case's settle signal explicitly — *store evidence says the turn ended
  AND the harness's live registry (`/session/status`, busy throughout
  tool execution, cleared by the abort) holds no live turn* — and makes
  it an ordered publication: `clearBusyFromEvidence` folds the
  transition through `applyLocked` (seq-assigned, projected, fanned
  out), the wiring's store reader fills the new `SessionSeed.LiveBusy`
  gate (conservative on registry failure), the composition
  (`cachedState`) settles tracker marks the authority's one busy
  definition proves idle (projection-known, no pending user ask, not
  registry-floored — and stamps `idleObservedAt` so a stale registry
  cannot re-busy later renders, #1672 r2), and the bridge's
  SESSION_STATUS leg now carries ERROR verdicts too (an errored turn is
  over, not busy). Red-first pins at every layer: 5 sessionstate, 3
  agentd composition, 3 bridge e2e, and a real-opencode integration
  repro (long bash tool aborted mid-turn through a burst-dropping
  proxy) asserting busy=false after turn end at the tracker, the
  projection, and the publication seam — now CI-registered beside the
  origin e2e. Review r1: the live-registry settle gate now covers BOTH
  reconcile legs (the `rederiveStatuses` sibling published IDLE
  mid-turn for silent turns past the 30s lease bound — reviewer-
  reproduced, red-first-pinned, fixed), the wiring's conservative
  registry-failure mapping is pinned, and a cursor-outage no longer
  overcounts `BusyCleared`.

### Added — Task Board REST API + openapi + 4 SDKs (Lane 2)

- **The board has its product contract tier** (#1699, design 0067 §4.3a;
  engine #1698/#1719 unchanged underneath): the REST projection at
  `/api/v1/me/projects|epics|tasks` — 23 operations, user-level only
  (ruling R7), item addressing by `prj-/epc-/tsk-` short id or UUID
  (case-insensitive, did-you-mean 404s where the engine scopes them).
  Projects CRUD + archive/unarchive; epics create/update with DERIVED
  progress (never stored, ruling R2), activate/pause/archive/unarchive
  and cancel (epic cancel lands paused — the enum has no cancel value);
  tasks create/update (criteria check-off rides the PATCH), the
  server-arbitrated `move-stage` (done = ALL criteria met + evidence),
  manual `start` (task_start: claim + fresh spawn, draft-epic-allowed),
  terminal `cancel`, and structural `dependencies` add/remove (the only
  blocking mechanism, rulings R3/R4; violations are 409 + did-you-mean).
  The adapter renders the engine's rows verbatim and maps error classes
  identically to the internal worker seam (the parity-matrix REST
  column). `sdks/openapi.yaml` grows the board tag with schemas,
  pattern-constrained short-id params, 404+suggestions /
  409+guard-code shapes, and the OR4 ruling encoded: the PR-drift
  anchor is the EXPLICIT `pr:N` tag form only
  (`BoardPrAnchor`, pattern `^pr:[1-9][0-9]*$`) — no free-text guessing,
  no URL scraping; the structured-field form is deferred additive. The
  four SDKs (TypeScript, Python, Go, Java) gain full board surfaces,
  wire-level tested; `s-board-crud` canaries join all three canary
  suites and a 34-arm hurl contract suite pin happy + unhappy + authz
  per endpoint (CI-registered — the live-API suites run in CI, not
  locally); the §4.3/§6 parity-matrix harness pins the REST column
  live, the agent seam live, the external-MCP column pending Lane 3
  (absence enforced by source-scan), and the declared asymmetries as
  absent-cells. Deliberately absent pending engine seams (flagged on
  #1699): epic listing, board search, board health.

### Added — Task Board dispatch engine + reconciler (Lane 1)

- **The board backend runs dark: Postgres-arbitrated dispatch, leases,
  fairness, and a self-healing reconciler with zero external surface**
  (#1698, design 0067 / #1696): migration `000036_task_board` (projects →
  epics → tasks per rulings R1-R9: structural `board_dependencies` as the
  only blocking mechanism with an acyclic-DFS write guard, terminal-stage
  immutability trigger, claim/lease/attempts columns with the
  claim-provenance flag, partial ready-set/reclaim indexes, the
  `board_events` idempotency log, and the reconciler watermark). The
  dispatch engine (`pkg/board`) is one atomic claim statement — the p CTE
  locks the least-served candidate-eligible project in the caller's
  workspace (a candidate-less project can never win the lock and starve
  others), the candidate CTE orders (priority, vft, created_at) under DRR
  quanta, the grant is the ONLY attempts increment, escalation withholds
  the grant at claim N+1, and two-level round rollover is bounded. Every
  write landing a row in progress sets/re-arms the lease in the same
  statement; heartbeats extend only on progress evidence (#1685); the 429
  path requeues without attempt penalty and cools both the task and the
  project's slots. The reconciler (RC1-RC5) reclaims expired leases
  exactly once under concurrent sweeps, converges PR-merge drift (the
  Done guard never bypassed; closed-unmerged re-claims), recomputes
  blocked state, completes epics event-fed, and never writes terminal
  rows — with deterministic event ids so a crash between sweeps replays
  to the same state. All self-healing invariants pinned red-first as
  real-Postgres kill-tests (concurrent claims single-winner; reconciler
  killed mid-operation leaves no orphans; expired leases reclaimed
  exactly once). The internal automation seam
  (`/internal/v1/automation/board/*`) carries the fresh worker's
  heartbeat/throttle/move-stage/read actions behind the pod-identity
  resolve; platform-deploy knobs per ruling R6 ride the helm → env →
  config chain (`board.*` values). REST/SDKs, MCP projections, and the
  frontend are #1699's lanes.

### Fixed — Chat streaming auto-tail vs user scroll (#1720)

- **Scrolling away during a streaming response no longer snaps back to
  the bottom.** The chat timeline's scroll handler recomputed the
  auto-follow ("tail") flag from a near-bottom heuristic on EVERY
  scroll event — including the events generated by the auto-follow's
  own snaps — so each snap re-armed the tail and the next token batch
  snapped again: a self-sustaining snap-back loop while reading history
  mid-stream (intermittent, since it depended on event interleaving).
  A stream starting also force-snapped and re-armed unconditionally,
  yanking users who had scrolled away. Per the owner spec in #1720:
  any genuine user scroll away now disarms tailing durably; tailing
  continues only until the user scrolls away or streaming stops; the
  near-bottom re-arm is forbidden while a stream is active (idle
  convention preserved); resume is explicit via the existing
  "Resume tailing" control; and all programmatic scrolls are routed
  through a guarded helper so the user-intent detector never mistakes
  them for user input. Follow-up (#1720 r2): the stream-follow
  observer now lives for the whole stream instead of attaching only
  when the tail is armed at stream start — previously, resuming via
  the control after a stream had started while scrolled away jumped to
  the bottom but did not follow subsequent batches, and an empty
  session whose log mounted after the streaming flip never followed at
  all, and — the same attachment gap on the listener side — a fresh
  session (log mounted mid-stream after starting empty) never saw
  user-scroll intent at all, so scroll-away could not disarm and every
  batch snapped; the scroll-intent listener now attaches when the log
  comes into existence. Regression coverage: deterministic red-first jsdom pins for the
  loop and the resume-follows-again compositions, source-shape pins on
  the scroll controller, WCAG contrast pins for the resume control
  (light + dark), and a Playwright acceptance spec (mocked SSE) for
  scroll-away-holds / explicit-resume-that-follows / stream-end-settle.

## [0.38.0] - 2026-10-17

### Fixed — Light color scheme contrast (user-facing)

- **The "message from session X" provenance badge is readable in the
  light color scheme again** (#1716): the badge rendered inside the
  user chat bubble — an inverted surface (dark navy bubble, near-white
  text) — while borrowing muted-surface colors, so in light mode its
  background composed to nearly the same gray as its text
  (**1.05:1** — invisible; dark mode was merely sub-AA at 3.9:1). The
  badge now borrows the bubble's own ink (≥10:1 in both schemes).
  Same release, wider light-scheme WCAG AA fixes: secondary/muted text
  (`muted-foreground`) on muted surfaces 4.34→4.88:1, error text
  (`destructive`) on white 3.76→6.43:1 and on the red error-banner
  tint 3.29→~5.5:1, destructive buttons 3.60→6.15:1, and the
  assistant-bubble timestamp 2.58→4.88:1. The dark palette is
  unchanged (pinned byte-identical by test). Remaining lower-priority
  light-scheme pairs are enumerated in #1717.
- **The failed-send queue pill is readable again** (#1716 r1): the
  "queued message failed" pill in the chat composer diluted its text
  (opacity-70/80) over a red tint — 3.06–3.58:1 in light and as low as
  **1.46:1 in dark** (nearly invisible — worse than the sentinel).
  Dark-scheme ink is now `red-400` (5.8:1) and the opacity dilution is
  gone (light 4.59:1; the strikethrough alone already marks the unsent
  text). A new browser-level e2e gate also reads the sentinel badge's
  real computed colors in Chromium (light and dark) and enforces WCAG
  AA ≥4.5:1, catching future drift between the design tokens and what
  the browser actually renders.

### Fixed — AI command fallback from the platform delivery bot

- **The AI Commands caller gate no longer silently skips commands from
  the platform's delivery app bot** (#1710, P1): `.github/workflows/
  ai-comment.yml`'s authorization `if:` admitted only
  OWNER/MEMBER/COLLABORATOR associations, but app bots
  (`agentic-actor-coder[bot]`) structurally carry `author_association:
  NONE` — they are neither owners nor collaborators — so every AI
  command they posted clean-skipped (conclusion `skipped`, zero
  PR-visible trace). During the 2026-10-16 event-delivery outage
  (#1704/#1707) this made the orchestrator's `/review` fallback on
  #1708 a silent no-op twice (runs 38836736982, 38850671818) — exactly
  the recovery path meant to cover lost `pull_request` events. The gate
  now carries the ai-workflows v0.2.16 caller-contract trusted-bot
  allowlist arm: an exact-login, opt-in list (sole entry:
  `agentic-actor-coder[bot]`), never a blanket association-`NONE` pass —
  each entry is a deliberate trust decision. The list is ONE shared
  definition: workflow-invariant pins (`tests/ghaworkflows/
  gha_callers_test.go`) fail on a dropped arm from any
  `author_association` gate, drift between gate copies, or a
  non-exact-bot-login entry (e.g. `*`). **Operator-relevant:** the
  outage fallback (app-bot `/review` re-fire) is functional again.

### Changed — hung-session alerting

- **The 'busy 15 min may be hung' nudge now requires message-level
  evidence** (#1685, owner ruling): a `session_hung` alert fires only when
  a session is busy past the 15-min floor AND no new messages from either
  side — an LLM-COMPLETED message (final `message.updated` upsert carrying
  `info.time.completed`) or a user-sent message — have arrived for 30 min.
  Busy-age alone no longer escalates: this fleet's healthy turns regularly
  run 30-60+ min with long tool executions, and the elapsed-time nudge had
  become noise (the owner's canonical no-alert case: last message 20 min
  ago + a long tool running is a HEALTHY long turn). `statusz` gains
  `last_progress_seconds` (additive, omitempty; 0 = evidence unavailable →
  no alert); the SSE alert payload carries it; the banner states the
  evidence workspace-scoped ("no new messages for X min while sessions
  stayed busy", with a busy-minutes fallback for older APIs); alert
  resolution fires when EITHER window clears. Persisted alerts, MCP, and
  SDK surfaces are unchanged in shape (audited).
- **OPERATOR DISCLOSURE — legacy-fleet hung-alert blackout during mixed
  rollout (#1685, ruling R4):** a pre-#1685 agentd never emits
  `last_progress_seconds`, so the API decodes 0 ("evidence unavailable")
  and such workspaces NEVER raise `session_hung` — the D6 safety net is
  silently off for them until their agentd rolls forward. This is the
  sanctioned fail-toward-silence direction (a false negative is
  human-recoverable; the false positive was the dogfooded noise the issue
  filed), but the disablement is deliberate and disclosed, not an
  accident of `omitempty`. The evidence clock is also in-memory: an
  agentd restart mid-turn self-heals at the next message event (same
  direction).

### Fixed — workspace UX

- **Disk-space banner surfacing** (#1686, owner dogfooding): the
  disk-recovery strip rendered at ANY disk level (a permanent "Free up
  disk space" on-demand line whenever metrics were known — training
  users to ignore the safety banner), occupied its own dedicated line
  under the metrics row, and on mobile sat under the Hide/Metrics toggle
  so "hide" never cleared it. The strip now renders ONLY at the critical
  tier (≥95%, mirroring systemnotices' 0.95 — the v0.35.0 surfacing
  spec): below the threshold nothing renders at any usage level; at ≥95%
  a compact single-line banner (inline headline, "Review what can be
  freed", and a per-episode Dismiss that re-arms when usage drops below
  the threshold and re-crosses). The recovery API surface is unchanged
  (presentation-only; no SDK/MCP/API contract changes — audited).

### Changed — org provisioning (BREAKING)

- **All orgs require OIDC; org creation requires the complete SSO
  configuration in the same request** (#1679, design 0065 §4 Option B —
  owner ruling: "All orgs require OIDC. We don't support anything else.").
  **BREAKING CHANGE** for API consumers: `POST /api/v1/orgs` now REQUIRES an
  `sso` object in the request body — `discoveryUrl`, `clientId` and
  `clientSecret` are mandatory (the first-configuration rule: there is no
  stored secret to leave unchanged); `claimedDomains`, `autoProvision` and
  `groupRoleMapping` are optional. Every previously-valid request body is
  now rejected with 400 (`sso.<field>` detail keys, zero side effects)
  until it carries the block; the config is persisted in the same request,
  a failed persistence rolls the creation back (a failed rollback logs an
  ORPHAN ORG alert for operator cleanup), and a deployment whose SSO
  service is not wired refuses creation with 503 instead of producing a
  no-SSO org. Consumers beyond the UI: the hand-written SDKs
  (sdks/{typescript,python,go,java}) do NOT model this endpoint (audited —
  their only `createOrg`-named methods are agent-role helpers on
  `/orgs/{id}/agent-roles`); the canonical contract is `sdks/openapi.yaml`,
  updated in lockstep (`CreateOrgRequest.sso`, new `InitialOrgSsoConfig`
  schema, 503 response). The platform-admin creation UI collects the IdP
  fields inline in the same step.

## [0.37.0] - 2026-10-11

### Added — auth completion

- **Email-verification and password-reset frontend** (#1651, #1670): the
  emailed links now land on real pages — /verify-email (410 expired/used
  and 404 states, resend with prefilled email, token stripped from the
  URL on terminal results, StrictMode double-POST guard) and
  /reset-password (request + confirm, always-202 neutral copy, redirect
  chain with success notice). Routes verified OUTSIDE GuestOnly — the
  discriminating placement: inside it, link-clickers bounce and the
  lockout re-creates itself. Login's email-verified 403 is now
  actionable with a threaded resend link; unverified signups get a
  check-email card. The auth audit's last HIGH gap, closed.
- **Org-scoped login surface, phase 1** (#1669, #1674, design 0064
  option C): optional orgContext {slug, name} on /auth/config
  (Host-derived, present iff the org has SSO — existence oracle equal
  to the start-URL baseline) and a first-class "Sign in with {org}"
  button on the org's login page, with cross-org verified-domain
  buttons suppressed (§6.2, justified from the gap persona's journey).
  The arbitrary-email user arriving at their org's URL now has an
  obvious path.

### Fixed

- **Initial-load busy composition** (#1672, #1673): the sessions
  listing's busy state derived entirely from SSE-event-fed sources that
  miss turns starting during stream-recycle gaps, and a reconcile step
  actively un-busied quiet mid-turn sessions — a fresh app restart
  showed 1-of-3 busy while the platform said 3-of-3. The harness live
  /session/status registry now composes in as an add-only busy floor
  (permission-wait carve-out survives on confirmed pending-ask only;
  staleness-bounded, failure-inert). Owner-observed, triaged to the
  server root with the frontend exonerated by evidence.
- **ProxyHandler/ResolverHost data races** (#1641, #1668): both
  lazy-init frames locked (RWMutex double-checked), all shared-resolver
  writes guarded — caught live by the v0.36.0 release run, completed
  through a reviewer-caught second frame.

## [0.36.0] - 2026-10-11

### Fixed — auth hardening (audit #1649)

- **Registration actually stops when disabled** (#1650, #1657): the
  `registrationEnabled` operator toggle — advertised in /auth/config but
  never enforced — now gates both signup paths (password + passkey
  begin/finish) server-side with 403s; login/recovery stay ungated;
  fail-open on setting-read errors; Turnstile runs before the gate.
  Mutation-verified in all three directions.
- **Duplicate-register no longer leaks account existence** (#1659): the
  409's wire message was "user email already exists" (an enumeration
  oracle) while the router-level pin mocked the service and never saw it;
  now generic, pinned at the wire level.
- **Dead API client surface removed** (#1660): changePassword/rotateKey
  methods pointing at routes deleted long ago.

### Fixed — frontend reliability

- **SSE staleness: busy/session state freezes no more** (#1646, #1662):
  after a silently-dead event stream (e.g. API pod restarts under an open
  tab), the app now converges within one reconciliation window — a 60s
  liveness watchdog force-reconnects dead streams, every (re)connect
  reseeds sessions+busy with causality guards (dataBirth/wipeAt
  ordering-by-construction) that refuse stale re-latches, and a 60s
  refetch floor backstops it all. Six review rounds closed five real
  holes; independent sub-agent verification against query-core source.
- **Mobile viewport regression** (#1648, #1661): h-screen (100vh — the
  LARGE viewport) + viewport-fit=cover made the shell taller than the
  visible screen (header OR composer off-screen). All viewport-height
  shells are now h-dvh with a verified @supports 100vh fallback, Android
  keyboard resizes-content, and a conditional on-screen-keyboard
  viewport guard; zero h-screen invariant; served-artifact e2e.

### Fixed — CI reliability

- **The warn-flood job killer** (#1644, #1658): the control-socket accept
  loop now exits on net.ErrClosed instead of warn-retrying forever — the
  bug that drowned CI jobs in million-line logs (two confirmed job
  deaths incl. a release run). Transient errors keep the recovery path.
- **Four flake families given real fixes** (#1532, #1664): BusyTruth (7+
  CI hits), watchdog-family, DeferSeam, and UploadStorm tests
  restructured to outcome-deterministic waits with red/green evidence
  under an identical SIGSTOP stall schedule; residual production finding
  split to #1666.

### Added — operator tooling

- **Near-miss session-id did-you-mean** (#1663, #1667): all six
  session-addressing tools' not-found errors now suggest the live id
  (levenshtein ≤2, deterministic, SUGGEST-ONLY — never auto-delivers),
  plus a standing 45-creation ID-fidelity guard. Born from this fleet's
  own incident: a one-character transcription slip looked like a lost
  session for an hour.

## [0.35.0] - 2026-10-09

### Added — mechanical disk-space recovery (#1601, #1642)

- **An out-of-band rescue lever for full-disk workspaces**: a
  deny-by-default, compiled-in per-runtime cache manifest (go
  build/module, npm/pnpm/pip/cargo/mise, stale /tmp go-build
  corpses); the agentd executor runs via the control socket so it
  works when opencode itself is wedged — the exact failure mode where
  the old remedy (the agent running go clean) was impossible. The
  owner-authz API facade (POST /workspaces/:id/disk-recover,
  dry-run-first) with free-ENOUGH semantics (reclaim below ~85%, not
  a maximal sweep), honest per-path bytesFreed (aliased or externally
  removed paths count zero), symlink-escape and protected-path
  refusals (PVC roots, $HOME, /tmp, package homes — an ancestor of a
  protected path is refused too), unknown-runtime-base fail-safe
  (empty manifest deletes nothing), and a frontend lever surfaced
  automatically at ≥95% full. Adversarially reviewed through four
  rounds; boundary, dry-run, and fail-safe legs mutation-verified.

### Added — wedge self-heal + agentz liveness (#1632, #1637, #1639)

- **The 2026-10-08 incident class closed end-to-end**: control-
  protocol `vitals` method gives the sidecar CPU evidence (supervisor
  reads /proc + cgroup in its own pidns — FLAT/STARVED distinguishable
  in production topology for the first time; mixed fleets degrade to
  the pre-fix behavior); a bounded UNKNOWN-episode soft kill (continuous
  UNKNOWN > 15m escalates once per episode, rate-limited, marker
  written; STARVED/FLAT/RESPAWN never escalate); supervisor zombie
  reaping (the [python]/[esbuild] defunct accumulation); and the
  bearer-gated `/v1/agentz` sustained-unhealthy-episode HTTP probe
  replacing the kernel-accept-blind tcpSocket liveness — generation-
  epoch CAS prevents mid-poll lost updates; AGENTZ_AGENTZ_SUSTAIN_
  SECONDS env knob floor-clamped at 2m for test lanes.

### Fixed — call_with_model surface (#1598, #1634)

- **Provider validation before dispatch** (unknown/unqualified
  providers 400 with the valid catalog quoted, not opaque 500s), all
  hardcoded provider examples removed from guidance (pinned both
  directions), `list_models` gains a tri-state vision field
  (fail-open join), the vision gate resolves the catalog live per
  call with the frozen-config/restart truth stated in the refusal,
  and no-text-parts errors carry a part-type histogram + bounded
  excerpt.

### Changed — toolchain

- **Go 1.26.9 + x/net v0.60.0** (GO-2026-6617, both surfaces — the
  advisory hit x/net/http2 AND stdlib net/http): go.mod directive,
  all 20 workflow go-version pins, and both digest-pinned golang
  builder images re-pinned (registry-verified GOLANG_VERSION=1.26.9)
  so shipped binaries don't embed the vulnerable stdlib (#1636).

## [0.34.20] - 2026-10-08

### Added — session archiving (#1627)

- **Archive/unarchive across the stack** (three-PR feature): the
  sessions API gains a reversible archived marker (migration + PUT
  endpoint + SDKs) (#1628); archived sessions are read-only end-to-end
  (the proxy 409s sends with session_archived) while history stays
  viewable. agentd carries `session_archive` (REQUIRED direction
  boolean — no default; a malformed call refuses rather than guessing)
  and `delete_session` (HARD delete, both platform index and agent
  store; refuses to delete the caller's own running session — injected
  origin first, single-busy fallback for degraded pods) (#1631);
  `session_metadata` now defaults to the CURRENT session only
  (all_sessions:true preserves the old behavior — a BREAKING default
  change), listings annotate archived state via a 15s stale-tolerant
  cache, and the freeze-pin e2e legs serve the real tool catalog on
  tools/list so origin dispatch is exercised end-to-end. The frontend
  sidebar adds an always-collapsed Archived group with count, kebab
  archive/unarchive actions (alert on failure, no refetch on reject),
  unarchive restores the ABSENT-means-not-archived wire contract on
  the cached row, archived views render a read-only banner with the
  composer hidden, and SSE archived/unarchived events converge every
  tab's cache (#1633). Adversarially reviewed (multiple rounds per PR)
  and merge-gated on mutation-verified pins.

### Fixed — the sidebar edge gesture, done right (#1629, #1630)

- **overscroll-behavior-x: none replaces the 0.34.19 interim
  inward-offset handle**: the CSS-native surrender (Chrome 63+,
  Safari 16+) hands the absolute screen edge to the OS back-gesture
  while the full edge still opens the sidebar — no JS coordinate
  arithmetic, no dead strips, no safe-area coupling. The 0.34.19
  drag-handle strip (owner-reported "horrible experience") is
  superseded; this is the platform-blessed mechanism the original
  implementation should have found.

## [0.34.19] - 2026-10-07

### Fixed — the sidebar swipe/back-gesture conflict (#1623/#1626)

- **The inward-offset gesture zone**: swipe-to-open now engages from a
  drag-handle strip set inside the OS back-gesture zone (safe-area
  aware), with the gesture claimed only at the qualifying horizontal
  move — the absolute screen edge is left entirely to the OS, so
  edge-swipe back-navigation and sidebar-open no longer race (the
  ~50% back-nav regression). The strip is input-transparent: taps pass
  through to the hamburger beneath, and vertical scrolling is
  edge-to-edge including over the strip (the old 30px dead column is
  gone). viewport-fit=cover enabled with real safe-area consumers
  (mobile/portal/drawer headers, composer). Note: merged on owner
  override of the device-QA gate (reviewer pre-committed approval;
  on-device verification rides the production rollout with revert as
  the escape hatch).

## [0.34.18] - 2026-10-07

### Fixed — the queued hardening set

- **applyModelAllowlist delivers provider models named "default"** (#1575,
  #1621): the allowlist check moved pod-side (riding the existing model
  enricher + 24h cache) — a model literally named "default" is delivered
  iff the provider serves it; a default-only allowlist restricts the
  fetched catalog (was: silent fall-through to the FULL unfiltered list);
  relay token scope prefers the attached allowlist (Models-derived scope
  would have minted unrestricted tokens); ValidateModelAllowlist on all
  five credential write paths.
- **Strict decode on the remaining agentd surfaces** (#1565, #1625):
  workflow_execute + user_timezone now carry the #1561/#1564 boundary —
  bounded reads, exactly-one-document, loud diagnostics, 400/413 classes.
- **Cluster-scoped reconcile reads through the direct reader** (#1587,
  #1622): RuntimeEnvironment resolution + StorageClass WFFC detection
  bypass the cache — an RBAC gap 403s immediately and requeues instead
  of silently wedging a worker on a never-syncing informer (#1551's
  defect-1 structural close).

## [0.34.17] - 2026-10-06

### Features — list_models tool, self-correcting call_with_model failures, credential-declared vision metadata (#1624)

- **The `list_models` MCP tool**: the workspace's usable model catalog
  (every model of every connected provider, as provider/model references
  ready for call_with_model's model argument). The seam-level
  `Client.AvailableModels` (GET /provider, raw bytes stay behind the
  Rule-12 seam) replaces the adapter's duplicated parse.
- **call_with_model failures that self-correct**: bare model names get a
  live-catalog did-you-mean ("provider must be included, found matching
  model name: did you mean 'p/text'?"); unknown qualified refs are
  pre-checked against the catalog BEFORE the carrier session exists
  (unknown provider → connected providers named; known provider → its
  model IDs named; case-typo'd refs → case-insensitive suggestions while
  the gate stays case-sensitive); every variant points at list_models.
  The pre-check is fail-open (#1307 direction): a catalog problem never
  blocks a call the wire would accept.
- **Credential-declared vision metadata**: opencode synthesizes an
  all-text-only capabilities block for models absent from its bundled
  models.dev catalog — multimodal custom-endpoint models included
  (probe-verified: config `attachment` lands at
  `capabilities.attachment` in both /config/providers and /provider;
  config cannot reach `input.image`). The credential's per-model
  `attachment: true` declaration is now the truth channel:
  `LLMModelConfig.Attachment` (tri-state) flows FormatOpenCodeConfig →
  catalog, and BOTH resolvers — the seam's ModelInfo (the agent-side
  gates: call_with_model pre-check, transcript repair) and the API's
  `supportsVision` (the picker) — OR-merge it over the synthesized
  false. The refusal message names the declaration path. Resolvers
  agree on every capability-block shape; documented divergence beyond
  it is always in the overstating direction. Recorded-fixture contract
  tests pin the pinned-opencode shapes (both endpoints, happy and
  unhappy); schema-validated config emission; HTTP tools/call
  round-trip pin. Four automated-review rounds (funlen; cross-surface
  semantics split — both resolvers aligned; suggestion-branch coverage;
  comment sharpening).

## [0.34.16] - 2026-10-02

### Features — dev-preview header configuration (design 0062, #1600/#1583)

- **The dev_preview_headers MCP tool**: agents set/clear/list literal
  header name+value pairs for their own dev preview (set/clear/list;
  literal values only — no Secret references, the structural guard;
  ≤20 entries, ≤4KiB values, canonical names, reserved-header denylist).
  State: plain JSON in the memory-backed volume (0600, atomic writes;
  survives agentd container restart, wiped on pod recreation).
  Injection at the dev-preview proxy hop after the tunnel Authorization
  strip — one handler covers both topology modes; agent-set
  X-Forwarded-User delivers clean (the stdlib strips inbound copies
  before the injection point — forward-mode for free). e2e: five arms
  incl. the restart/wipe lifecycle. Note: merged via owner-approved
  override after r1 remediation — GitHub\'s multi-day pull_request
  event failure blocked the r2 review on four PR carriers.

## [0.34.15] - 2026-10-02

### Fixed — the strict-flip counters made trustworthy (#1614/#1615)

- **A frontable provider missing from a PRESENT handoff** (the
  all-mints-failed shape — the residual failure class of the five-day
  #1611 staging outage) previously delivered raw keys UNCOUNTED in
  migration mode (invisible to the 7-day-zero strict-flip criterion)
  and raw in strict mode (a fail-open arm inside fail-closed). Now:
  migration counts it (relay_fallback_deliveries_total + audit, raw
  still delivers); strict mutes it (per-batch degraded counter,
  slug-deduped). One shared stageability predicate
  (RelayFrontableProvider) governs both the controller's staging set
  and the builder's classification.
- **Controller status honesty**: CredentialsStaged=True now counts
  actually-staged tokens and names mint shortfalls (during the outage
  it claimed "1 staged" with zero staged).
- **Catch-up pins**: pre-arming Active workspaces stage on reconcile
  (the disproven event-tie hypothesis, now guarded green), plus the
  trigger source-pin.

## [0.34.14] - 2026-10-02

### Fixed — agentd restart-storm defenses (the 2026-10-01 incident) (#1613)

- **credential_reload rev gate.** The first relay credential handoff
  (v0.34.13) restarted one workspace's opencode 6 times in 11 seconds:
  the xdg config watcher was already fingerprint+cooldown gated, but
  the CONTROL path (one socket Restart per sidecar credential event)
  had no gate at all. The storm killed an in-flight turn and wedged
  opencode (every turn aborting at birth) until a manual pod bounce.
  `managedProcAdapter.Restart` now peeks the spawn-env mux read-only
  (new `credentialPuller` seam) and suppresses a credential restart iff
  clean pull ∧ not degraded ∧ served rev == the anchored rev the child
  spawned with — failing OPEN on any doubt. Four review rounds hardened
  the deliverable: the suppression is recorded on the SCRAPED sidecar
  registry from the socket outcome (`workspace_restarts_suppressed_
  total`), the sidecar no longer request-time over-counts suppressed
  pushes, restart round trips arm a deadline covering the grace window
  (the 2s client default silently under-counted slow restarts), and
  outcome-unknown transport failures Warn loudly.
- **Cross-uid restart-reason marker.** The shared marker sits in a
  STICKY root-owned dir and 0640 grants the other writer uid group-READ
  only — cross-uid rotation could never work (live EACCES observed).
  Per-uid fallback file on write failure + newest-wins boot read +
  sibling sweep; attribution survives in sidecar mode.
- **F7 e2e row** in the US-70 kind harness: same-rev credential restart
  suppressed with the child pid untouched (socket-verifiable), and a
  sidecar-initiated restart counted on the scraped surface.
- **Label semantics change (socket topology):** credential restart
  events are now outcome-counted as `credential_reload` (previously
  request-time as `env_secrets`/`api_key`). No chart consumer filters
  by reason today.

## [0.34.13] - 2026-10-01

### Fixed — relay staging un-break: the SSL 301 that made the raw-key fallback permanent (#1611)

- **Relay-only key delivery can actually stage now.** Since the secure
  path went live, EVERY workspace's staging pass died at its first
  credential fetch: the controller calls the API's internal
  llm-providers endpoint over plain HTTP in-cluster, but the security
  middleware's `SSLRedirect` (enabled by `RequireHTTPS`, empty
  `SSLHost`) answered with a 301 to `https://<same-host>:8080` — TLS
  on the plaintext port. The controller's Go client followed the
  redirect and failed (`http: server gave HTTP response to HTTPS
  client`), marking every workspace `CredentialsStaged=False /
  StageFailed`. The epic-35 exemption matched only `/internal/`; the
  US-72.3 controller endpoints live under `/api/v1/internal/`. Fix:
  the pre-redirect skip now covers both prefixes.
- **Impact:** nothing ever staged (zero envelope/handoff Secrets, no
  staged annotations), so the pre-flip raw-key fallback bridge became
  a permanent resident for every workspace and the strict-mode
  wind-down clock (7 clean days) never started. Post-deploy, staging
  should succeed on the next reconcile, `CredentialsStaged` flips to
  `True`, and the fallback delivery counter finally begins its
  decline.
- **org-status had the identical latent bug** (same prefix class,
  router.go) — fixed by the same exemption rather than leaving the
  next outage one call away.
- **Tests pin the seam in both directions:** middleware regression
  (mutation-verified red/green), narrowness negatives (no-trailing-
  slash / dash-lookalike / substring traps must still 301), the
  legacy `/internal/` epic-35 exemption pinned against refactor
  deletion, and a router-level integration test through the real
  `NewRouter` wiring armed with `DefaultRouterConfig()`'s
  `RequireHTTPS: true` posture (fails 301-vs-403 with the exemption
  deleted, plus a non-internal-route still-301s parity pin).
- **Verified live** against workspace `d8bed486` (the 301 reproduced
  in-cluster pre-fix; root-cause chain documented in the PR worklog).

## [0.34.12] - 2026-10-01

### Fixed — the API's pre-handler body buffering (SR-6's root, PR #1608)

- **Two latent contract breaks on the uploads route, both as old as the
  feature**: the logging and error-handler middlewares each buffered the
  ENTIRE request body before the handler ran (voiding the streaming
  contract — client pacing never reached agentd's admission control, so
  the §6.6 concurrent-cap boundary was unreachable and #1567's
  deterministic row failed 10 consecutive nightlies), and early
  refusals closed the connection over unread body bytes (RST clobbering
  the API's own 507/429 response — masked until the buffering fix
  exposed it). Captures are now bounded at 4KiB with live stream-through
  (MultiReader), early responses drain exactly the declared remainder
  (memory-free, deadline-bounded with the fired-deadline-survives
  semantics), and oversized captures log declared size only. Also closes
  an unauthenticated memory-exhaustion amplifier (both middlewares ran
  pre-auth on every route with unbounded reads).

### Fixed — busy truth through the SSE pipe (#1602/#1603)

- The frontend's session busy indicator now carries the derived truth:
  running/queued tool work, in-flight parts, and queue depth flow
  through the API's session.status emissions (overlaid from the
  authority snapshot, fail-open documented), the snapshot clone no
  longer drops the Busy field, silent tool runs hold the idle-drop gate
  (stuck-busy closed), compaction counts as busy, and aborted/deleted
  sessions render properly (#786 folded in).

### Features — ACP Stage A (design 0063, PR #1599)

- The adapter's internal event vocabulary for tool executions now
  speaks ACP: native dialect → spec-shaped (status machine, kind table,
  diff content) → the five contract part types, on both the live SSE
  path and history. Unknown events map to Custom and are never
  silently dropped; the part contract is unchanged (byte-identical
  where it matters). No transport change (Stage B remains
  trigger-gated).

### Fixed — nightly test-cluster capacity (PR #1605)

- The 2-node kind retirement never doubled schedulable capacity (the
  control-plane stayed tainted); the nightly config now untaints it,
  pinned by an exact-shape parsed-YAML assertion. First material for
  the migration e2e's rows to ever run.

### Docs — designs 0062 (dev-preview headers) and 0063 (ACP spike) merged

## [0.34.11] - 2026-09-29

### Fixed — global-default secrets reach existing workspaces (#1597)

- **The "Include in all workspaces" flag now means what it says** (#1597):
  global-default user secrets previously bound only at workspace
  creation — a workspace created before the secret existed never
  received it (no binding row → no revision mint → no push; restart
  could not fix it, since bootstrap builds from the same empty
  bindings). The flag is now POLICY materialized into binding rows:
  the secretsreconcile loop converges every Active workspace's
  global-default bindings each pass (before manifest derivation, so a
  flag flip diverges, mints, and notifies in the SAME pass), and
  pod-bootstrap runs the same sync at boot for suspended workspaces.
- **Binding provenance (migration 000033)**: `user_secret_bindings`
  gains `bind_source` (`manual` | `global_default`). Policy convergence
  adds/removes only auto rows — a manual binding is the user's explicit
  claim and always wins; a flag flip retracts the auto rows without
  touching manual ones. Legacy rows backfill `manual` (conservative).
  New metric `llmsafespaces_secrets_reconcile_policy_bindings_total{op}`
  and per-workspace skip reason `policy_sync`.
- **One builder, one truth preserved**: the batch builder and manifest
  tier still read binding rows only — no builder-side union, so the
  reconcile loop's convergence certification stays sound.
- Also fixed in passing: a blocking staticcheck lint failure in
  `pkg/repolint`, and `TestUploadApply_ConcurrentWallTime` false-failing
  on loaded hosts (absolute wall threshold replaced with a same-run
  measured serialized baseline — the load-robust discriminator).

## [0.34.10] - 2026-09-27

### Fixed — the busy-truth cluster, part 2: wedged-turn recovery (#1584, #1586)

- **The sidecar now learns the supervisor respawned opencode** (#1576
  layer 1, #1573 ask 3): the status poller derives generation boundaries
  from the supervisor's ChildPID + Restarts epoch (the composite key is
  namespace-immune — a container restart can reproduce a deterministic
  early PID) and fires the D2 reset and the authority's generation
  reseed at that edge. Previously split mode never noticed a respawn on
  either boot-restore or respawn; the production wiring is source-pinned.
- **Declared tool timeouts are enforced** (#1576 layer 2): a tool
  input's declared timeout registers a per-part deadline; the reconcile
  pass folds RUNNING parts past their own deadline terminal (ERROR +
  declared-timeout reason, first and above the store gate). Zero
  heuristics — no declaration, no action.
- **The wedged-busy view dies with the fold** (#1576 layer 2's busy
  effect): a folded terminal part no longer holds the session busy
  (active count is non-terminal TOOL parts only — the part stays
  renderable), the fold clears the streaming busy-latch and marks the
  session ERROR (arming #1578's terminal veto), and a fold-dead hold
  refuses stale store-BUSY re-derivation until a real harness event —
  including across stall-wake reseeds (the r3 fix: the hold is
  snapshotted and re-armed on BUSY seeds; B1-pinned end to end).
- **Restart deferral is on probation with numbers** (the owner's seam):
  the busy reads sit behind a swappable deferBusySource and the decision
  surfaces deferrals-fired, a defer-stall histogram on all three legs,
  and the orphaned-flag clear rate — the keep/replace/remove call is now
  a data question.
- **The runtime-env-reader ClusterRole** (#1586, bf54c386, #1551): fixes
  namespace-posture resolver starvation; the posture gate asserts the
  reconcile path.
- Docs-only #1585 (design 0062, dev-preview header configuration) is
  held out of this train pending owner sign-off.

## [0.34.9] - 2026-09-26

### Fixed — the busy-truth cluster, part 1 (PR #1578)

- **Verify failures can no longer kill live sessions** (#1573 part 1): a
  missing or malformed agentd binary pin is now an operator signal (exit 87 —
  loud via termination log, controller condition/event/metric, never the
  tamper verdict) instead of a fail-closed kill; the tamper verdict (81) and
  its message contract are unchanged. A stale baked agentd binary now refuses
  to run under the overlay contract (exit 88, mount-prefix fallback that a
  sanitized environment cannot disable) — the 2026-09-25 incident class
  (workspace b150f355: a pre-hardening baked binary re-exec'd with wiped env,
  its verify failure group-SIGKILLing two live turns) is structurally dead.
  The controller detects all four exit classes; the crashloop skip covers
  them.
- **busy means autonomous progress pending** (#1574, the owner's rule): busy
  is now derived once, from data — streaming, running tool executions,
  queued ones, and in-flight turn machinery all count; waiting on a
  permission/question prompt does not (its own surface). The single
  definition feeds the projection, snapshots, and statusz's authority
  overlay in both directions (false-IDLE and tracker divergence); an errored
  session is never busy (terminal veto, lift-pinned — a new turn re-marks).
  ABI contract updated: consumers read busy, never recompute it.

## [0.34.8] - 2026-09-25

### Fixed — the relay arming root cause and incident closure (#1546/#1548, design 0061)

- **Controllers could never arm relay-only delivery** (#1548's literal root
  cause, found by the M3 posture gate on its first real execution): the flag
  registration returned its struct by value while the flag package bound into
  the local — `flag.Parse` wrote an orphaned copy and `relay-only-key-delivery`
  stayed false under every value, on every release since #1448 (v0.34.4
  through v0.34.7 all audited affected; no released controller ever armed).
  Fixed at both flag seams (relay and free-models refresher — the same
  byte-identical bug), pinned three generations deep (seam, gate-structural,
  caller-wrapper), and closed tree-wide by class sweep (the remaining
  registrars are pointer-safe by construction). First live arming verified on
  kind by the posture gate; v0.34.8 is the first release whose controller can
  arm at all. (PR #1566)

### Features — design 0061: relay delivery hardening (PRs #1553/#1559/#1556/#1557/#1552/#1566)

- **M1 crash-loud arming**: a controller deployed relay-only that fails to
  arm exits 85 instead of running un-armed (#1553).
- **M2 migration fallback + both counters**: absent or expired staged
  handoffs deliver the pre-flip raw-key batch (migration mode, the chart
  default) with `relay_fallback_deliveries_total` firing at the exact moment
  of harm; `relay_degraded_batches_total` survives both modes; strict mode
  preserves the fail-closed class (#1559).
- **M3 posture gate**: a CI job cold-installing the chart's own shipped
  defaults on kind and asserting the four §5 checks; on its first real
  executions it caught its own invalid jsonpath, the arming root cause, and
  the RBAC bindings apply-loss, then ran green twelve times (#1556, #1566).
- **M4 CredentialsStaged**: the relay batch outcome surfaces as the existing
  condition on the Workspace CRD (#1557).
- **Posture RBAC fixes** (#1552); the api-leader-election and
  api-platform-info bindings moved to a dedicated template after a
  whitespace-eaten doc separator silently dropped one from every apply, with
  a doc-boundary class pin (#1566); namespace-scope cache-scoping derivation
  fixes the drill-shape crashloop (#1558).
- **Migration-recovery e2e** (design 0061 §10/§11), closing #1546's evidence
  (#1566).

### Fixed — MCP transport boundaries (PRs #1572/#1564)

- **send_message argument recovery**: model-emitted duplicated fragments in
  tool arguments (the #1530 class — proven by escaped-quote forensics, race
  falsified by 11,501 harness-exact wire bodies, now CI-wired as a standing
  probe) are recovered and delivered with a warning field; duplicate-key JSON
  bodies (intent unrecoverable) refuse loud with -32602, duplicate-path
  REPORTS capped (first 16 + count, paths truncated, join bounded —
  attacker-shaped amplification closed by construction and pinned); the raw
  read bounded by the socialized 1MiB cap (#1572, closes #1530). Misplaced
  params keys and trailing data reject loud with exact diagnostics, the
  spec's additive `_meta` key allowlisted (#1564, closes #1561).

### Fixed — upload admission and the nightly instrument (PRs #1563/#1567/#1545/#1540/#1542/#1544)

- **Admission precedence**: the count-cap check now precedes the budget
  clause — the 5th-concurrent 429 boundary (§6.6) was unreachable at shipped
  defaults (#1563); the SR-6B row forces the admission overlap
  deterministically (#1567). Upload serialization removed with its latency
  catch reclassified as the known-issue skip (#1545, #1540); the nightly's
  epic-72 evidence lane un-hostaged (#1542); the arbitration's final five
  rows closed — 29/29 contract-correct (#1544).

### Security — legacy-key scrub and rogue-agent sweep (US-72.6, PR #1537)

### Fixed — the agentd flake family (PRs #1568/#1547)

- **#1532 instances 1+3 de-timed**: the staging sweeper and watchdog clock
  sites route through an injectable seam (production byte-identical,
  behaviorally pinned); tests assert ordering and window boundaries
  deterministically instead of racing the runner; the supervisor family
  triaged I/O-inherent (instance 2 open) (#1568). The watchdog flake's
  signature decoded and the supervisor env read guarded (#1547).

### E2E — sidecar upload rows un-skip (design 0060 PR4, #1524)

## [0.34.7] - 2026-09-22

### Features — saved prompts and slash commands (#1496/#1499, PRs #1502/#1503/#1509)

- **User prompt manager, full stack**: a settings surface where users CRUD
  saved prompts (name + content, owner-scoped storage, typed clients in all
  four SDKs), recalled in the chat composer via `#` — a popup with live
  filtering, keyboard navigation, and inline expansion. The `#`-collision
  matrix is pinned deliberately (markdown headings suppressed, `C#`/`a#b`
  word-boundaries respected, recursion-guarded expansion).

### Features — file uploads in sidecar mode (design 0060, PRs #1515/#1516/#1518/#1523)

- **Uploads now work in the hardened sidecar configuration** via staged
  delivery: the sidecar stages bytes on a budgeted memory area (admission by
  reserved bytes — credential delivery is protected by construction), the
  supervisor writes the PVC as uid 1000, with write-time disk re-checks,
  atomic renames, orphan hygiene, and full observability. Disk/memory safety
  per the owner's binding directive; live-validated end-to-end by the nightly
  (persistence across suspend/resume, cross-tenant isolation, mid-kill
  recovery — all green in sidecar mode, first time in history). Operator
  knobs in Helm with typo-rejection at boot.

### Security — credential-plane hardening (#820, PRs #1526/#1534/#1529)

- **Raw provider keys off workspace pods by default**: relay-only key
  delivery flips on by default — keys are sealed into per-workspace envelope
  Secrets in a dedicated namespace with a per-request BYO resolver; the
  uid-1000 agent space never receives them. Exercised-rollback drill,
  live-fleet canary waves, and the runbook ship with it.
- **Permission tiers enforced**: the harness's directory-permission config is
  written to the live key (the months-inert shape is fixed, live-proven, and
  CI-tripwired), with the tier floor (pre-allow working paths, hard-deny
  credential surfaces, ask ambient) and five closed escape classes.
- **Self-send guard on inter-agent messages**: target==origin delivers with a
  loud warning — the orchestrator misfire class, closed at send time.

### Fixes — platform behavior

- **Suspend is bounded-graceful everywhere** (#1507/#1510): the busy-session
  drain gate is removed on suspend paths (a wedged agent can no longer block
  suspend indefinitely — the flap physics documented in-code); termination
  grace is explicit and tunable with a loud floor.
- **Refresh Compute is the force path** (#1505/#1506): the user-consented
  refresh stamps a generation-keyed force marker; the controller deletes the
  pod on first reconcile. Automated suspends keep the polite drain.
- **The parent-id contract, everywhere** (#1517/#1522/#1527): eight
  production surfaces returning opaque 500s on nonexistent parent references
  (workspaces, triggers, runs, MCP bindings, credentials, org members) now
  answer named 4xx — create and update paths, live-patrolled by new nightly
  rows. Two independent FK sweeps confirm the class is closed.
- **Session-index ground truth** (#1487): message counts rebuild from harness
  truth during reconcile (truncation-sentineled, never persisting a floor).
- **DAG retry-intermediate cleanup** (#1492), **agent-origin badge full-ID
  wrap** (#1489), **CI release-tag race structurally closed** (#1483).

### Fixes — test infrastructure (the nightly resurrection)

- The full-system nightly now completes end-to-end for the first time in the
  project's history: four never-executed harness corpses resurrected, sweeps
  that never swept now verify, the schedule is off-peak (2:17am Pacific),
  scale rows run at honest envelope, LLM-cred rows gate loudly, attachment
  rows run fully in both container modes, and the automation arbitration
  executes on a fully-swept platform. Test-side flake classes closed
  (Playwright detached-node dispatch, #1520).

### Fixed
- **#1519** — Triggers update-path workflow-existence contract: PATCH retargeting to a nonexistent or cross-owner workflowId now answers the named 400 "target workflow not found" (create parity), not the opaque 500 (store FK) or silent persist (cross-owner) of the pre-fix asymmetry. The check fires on the post-patch merged view inside the mapping-touching block — de-opt patches on stored ghosts 400 too; non-mapping patches stay editable.

## [0.34.6] - 2026-09-20

### Fixes — activating the v0.34.5 origin-provenance feature (#1488, #1489)

- **opencode overlay /plugins directory was untraversable (PR #1488)**:
  BuildKit applies `COPY --chmod` to implicitly created parent directories,
  so the 644 intended for the plugin file landed on /plugins itself — no
  non-root process could traverse it, the origin plugin silently no-loaded
  on every v0.34.5 pod, and send_message ran in self-declared fallback
  mode. Fixed to 755 with three pin layers: source allowlist (repolint),
  built-image tar-header checks in CI, and the incident documented as the
  motivating case. Pods activate injected-mode on the next recreate.
- **Origin badge truncates long session IDs (PR #1489)**: the provenance
  badge ellipsized the session ID; it now renders in full (break-all
  span, house convention) with a hover title for copying. Pinned by a
  real-browser layout row that fails on both truncation mutations.

### Fixes — release pipeline integrity (#1483)

- **CI manifest jobs no longer emit semver tags (PR #1483)**: the release
  commit's own main-CI manifest pushes raced the Release workflow onto
  the same appVersion tag (the v0.34.5 bump incident), making tag-based
  digest fetches unreliable. ci.yml now emits no version tags at all —
  release.yml is the sole semver writer — enforced by spelling-proof
  structural pins including raw-push tool markers.

### Fixes — session index ground truth (#1487)

- **message_count rebuilt from harness truth (PR #1487)**: the reconcile
  pass now folds in bounded count walks (page-capped, with a truncation
  sentinel so a floor is NEVER persisted as ground truth), writes
  absolutely on drift, and stays write-free when converged — closing the
  drifted-count class the index could never self-heal.

### Fixes — workflow-run retry hygiene (#1492)

- **DAG node-retry intermediates cleaned (PR #1492)**: the #1476
  superseded-attempt cleanup is adopted by the workflow-run executor —
  every non-final failed attempt's session is torn down over the honest
  204/502 route, with author-pinned sessions (spec sessionId) explicitly
  never cleaned.

### Fixes — nightly harness reliability (#1482, #1484, #1486)

- **ExecuteSmoke corpus completed (PRs #1482/#1484)**: all ten
  nightly-registered harness scripts now must execute under shim
  traversal to pinned depths — the never-executable class that hid three
  corpses and a dead api() is structurally closed.
- **Harness workspaces cleaned on every exit path (PR #1486)**: the
  attachments and test.sh harnesses leaked standing workspace pods into
  downstream suites (≈ exactly the CPU margin us-70 lacked — nightly
  35437562027's AC-1c exhaustion). Live-verified on the next run: census
  5 → 3, AC-1c passes.

### Changes — nightly schedule (#1485, #1491)

- **Nightly moved to 09:17 UTC (2:17am Pacific), off-peak odd-minute
  (PR #1491)**: GitHub's scheduler fired the old 06:00 UTC top-of-hour
  slot 4–6h late on an 11-run sample. A concurrency group (PR #1485)
  guards against any double-trigger. First run on the new slot delivered
  results in the owner's morning window as designed.

## [0.34.5] - 2026-09-19

### Features — agent-to-agent messaging provenance (#1465, PR #1469)

- **send_message origin metadata, hybrid and mode-labeled**: inter-agent
  messages now carry a versioned sentinel (`lsp:agent-message-v1`) with the
  originating session, auto-injected server-side. PRIMARY: a first-party
  opencode plugin (shipped in the pinned overlay artifact — design 0053 §3
  amended: digest-pinned platform plugins are part of the delivery
  contract) stamps the calling session via the documented
  tool.execute.before hook (mode "injected"); FALLBACK: a validated
  model-supplied from_session_id (mode "self-declared") when the plugin is
  absent or drifted — delivery⇔attribution holds in every mode and a
  degraded pod self-reports. Frontend renders agent-origin messages
  user-side with a provenance badge; a CI freeze-pin job guards the hook
  contract against opencode bumps. The opencode overlay image digest
  changes with this release.

### Fixes — routine fire lifecycle closure (#1454/#1470/#1473/#1476, PRs #1472/#1471/#1474/#1477)

- **One accounting primitive (#1454, PR #1472)**: the four copy-pasted
  increment/disable blocks and both target predicates collapsed into
  accountTriggerFailure + routineTargetWorkspace — pure refactor,
  behavior-pinned identical (characterization pins proven green on
  pre-refactor main).
- **Failed fires record their sessions (#1470, PR #1471)**: the agent-node
  envelope carries sessionId on success AND error; failed
  PreserveOnFailure/PreserveAlways fires origin-record and index their
  surviving sessions; ephemeral sessions are torn down on every failure
  leg; an opencode-DELETE 404 counts as gone (no phantom survivors).
- **Drain-door gaps closed (#1473, PR #1474)**: targetless drain fires now
  account and auto-disable (the #1440 cron guard's twin); transient
  trigger-fetch errors leave pending fires for re-drive instead of
  permanently failing them.
- **Retry intermediates cleaned (#1476, PR #1477)**: superseded retry
  attempts' sessions are deleted via the authorized route (never the
  final attempt's); the agent-node timeout leg surfaces 200+errorCode so
  timed-out fires' surviving sessions are recorded (script-node legs stay
  504 — the engine's pre-script branch has no ErrorCode handling).

### Fixes — script-node environment contract (#1455, PR #1475)

- **script_env_unavailable fails loud and classified**: agentd probes the
  scriptwrap prerequisites (writable temp dir, interpreter) before
  execution; sidecar-mode scratch containers now fail with a named,
  attributed error code (pinned outside the transient retry class — an
  environment does not heal in 2s) instead of an incidental script_failed.
  The http-node reader's secrets-env path now shares the US-4b coordinate.
  Runbook + epic-64 contract documented; Option-B (execute in the
  workspace container) remains an open design ruling.

### Fixes — e2e harness reliability (#1473/#1480/#1478, PRs #1474/#1480/#1478)

- **Three harnesses resurrected**: the api() status side-channel died in
  every caller's subshell — the 1410 harness had never executed, 1452 and
  1342 never called harness_start (killed at seed_workspace on every
  nightly). All three now run under a shared ExecuteSmoke shim layer
  (curl/kubectl fidelity, verdict gates, whsec_ leak bans), with WS_BASE
  canonicality pinned in the live unconditional form. Sister-script smoke
  coverage extension to the remaining eight nightly scripts is queued
  follow-up work.

## [0.34.4] - 2026-09-19

### Fixes — routine fire follow-up ledger (#1452–#1458, #1467)

- **Prompt growth under memoryMode=last_result (#1453, PR #1462)**: the
  captured result under captureMode=full embeds the round's full prompt,
  so injecting it verbatim into the next round's {{.prevResult}} compounded
  the prompt unboundedly (round N carried round N-1's prompt, which carried
  round N-2's result...). Both injection arms (single and multi-run) now
  reduce the stored envelope to {response, tokens} at read time; stored
  blobs that are not the expected envelope fail closed (placeholder left
  unreplaced, Info-logged) instead of passing unverifiable bytes into a
  prompt. Storage format and capture semantics unchanged.
- **Session-create failures rejoin the transient retry class
  (#1457/#1458, PR #1466)**: agentd's ephemeral session create collapsed
  every failure into session_not_found, hiding provider blips from the
  #1441 retry classifier one leg early. agentd now distinguishes
  session_create_failed (wrapping the transport error or opencode status);
  the classifier retries it alongside script_failed wrapping 5xx; the
  pre-script leg routes through executeWithRetry like the agent leg. A
  round-1-caught regression ships closed: a 200 create body with an empty
  ID is a failure, not a phantom-delivered fire.
- **Preserved routine sessions appear in the workspace session list
  (#1452, PR #1464)**: localization proved the pod-side list was never
  broken (session registered and listed on the pinned binary,
  restart-stable) — the gap was platform-side: GET /workspaces/:id/sessions
  serves only the PostgreSQL session_index, and no routine fire path ever
  wrote it, so visibility raced the 30s usage-stream idle gate. The engine
  now indexes preserved sessions at fire completion (same gate as the
  origin write), best-effort and non-fatal. Follow-up: #1470 (failed
  PreserveOnFailure fires and drifted deliveries still miss both tables).
- **Update path enforces the memory/capture cross-constraint (#1467, PR
  #1468)**: PATCH could flip a trigger to memoryMode=last_result without
  captureMode=full, which create rejects. The update handler now guards
  the post-patch merged view with the same error, byte-identical via a
  shared constant. Nightly harness bug fixed alongside: R8's R5_WS was
  undefined and set -u aborted every prior run at that line.
- **Nightly e2e: F8 executes for the first time; Epic 68 sidecar gate
  fixed (#1456, PR #1463)**: F8's Service discovery used a label the
  valkey Service does not carry — the check silently SKIPped on every run
  in history while the job actually failed at attachment row E2, where
  the sidecar gate probed only spec.containers and missed the native
  (init-container) sidecar, sending uploads into its read-only
  /workspace. F8 now looks up the Service by name with a three-way
  verdict (no-verdict no longer gates the ten downstream suites); the
  gate probes both container lists and loudly skips E2/E10/E11 in
  sidecar mode per design D1.
- **CI: cmd/relay-router race flakes (rides PR #1466)**: two US-72.2
  tests (watch-delete recovery, metrics scrape) raced async harness state
  and intermittently failed every PR's required full-suite race check;
  both now settle-bounded before asserting (test-only).

## [0.34.3] - 2026-09-18

### Fixes — routine fire resilience

- **Bounded retry on transient upstream 5xx (#1441 failure half, PR
  #1451)**: routine agent calls retry transient model-provider blips
  (agentd transport / opencode 500-502-503) up to three attempts with
  linear backoff — a single blip previously failed the fire and burned
  auto-disable budget. TIMEOUTS are deliberately excluded (fresh-session
  retries risk double execution; 10m-timeout retries would triple the
  scheduler worst case); exhausted retries fail the fire burning exactly
  ONE budget unit. Follow-up ledger: #1452-#1458 (session-listing
  surface, prompt growth, session-create retry leg, pre-script retry
  leg, lifecycle consolidation, interpreter environment).

## [0.34.2] - 2026-09-18

### Fixes — fire observability + security

- **Routine fire results are observable, and the fires endpoint is
  owner-scoped (#1441 observability / PR #1446)**: failure causes and
  captured outputs were written to a column no read path selected —
  routine failures were undiagnosable from outside. The fires list now
  carries `result` (distinct from `actionResult`), on both the user API
  and the agentd `trigger_fires` tool. The reviewer-caught pre-existing
  hole ships closed with it: `listFires` never used its owner params —
  any authenticated user could read any trigger's fires by UUID (and
  trigger UUIDs travel in shared webhook URLs); now guarded like every
  sibling (404 on mismatch). OpenAPI updated; nightly e2e rows assert
  captured-success content and failure-cause visibility end to end.

## [0.34.1] - 2026-09-18

### Fixes — scheduler

- **Targetless triggers fail loudly (#1440, PR #1443)**: deleting a
  workflow SET NULLs the referencing trigger's target (FK, migration
  000020) — the trigger then ticked silently forever: no fire rows, no
  failure count, no auto-disable, and the loud missing-workflow path
  was unreachable for real deletes. The nil-target guard now records a
  FAILED fire (`trigger_has_no_target`), increments
  `consecutiveFailures`, and honors `autoDisableAfter` — route-agnostic
  (covers the FK route and the #1442 create-anomaly consequence).
  Store integration test pins the FK semantics; nightly R4d covers the
  delete route end to end.

## [0.34.0] - 2026-09-18

### Features — automation (design 0059: trigger input mapping)

- **Triggered runs can finally satisfy schemas — #1425 + #1419 closed**
  (PRs #1439 + #1438): workflow-mode triggers gain `inputFrom`
  (envelope | body | mapped, default envelope — existing triggers keep
  byte-identical behavior) and an optional static `input` document.
  `inputFrom: "body"` (webhook sources) makes the posted payload the
  run input directly — schema-authored workflows stop reaching into
  `input.body.*`; `mapped` validates the static document against the
  workflow's inputSchema at trigger create; new un-opted envelope
  wiring against a schema requiring non-envelope properties is rejected
  with a remediation 400 (the V1-V7 create/update matrix; update runs
  only the rules whose inputs the patch touches).
- **Opt-in fire-time validation with redacted failure records**: fired
  runs carrying input mapping are validated before queueing; a mismatch
  records a `validation_error` fire with TYPED violations
  ({jsonPointer, keyword, message} — locations only, never instance
  values, recursively capped at 4 KiB with a truncation marker) and
  drives the failure counter/auto-disable; a non-compiling stored schema
  records `{"code":"invalid_input_schema"}` instead. Zero node
  executions burned on invalid input.
- **Webhook non-JSON fallback aligned** to the {raw, content_type}
  shape; migration 000031 (additive + reversible) carries the new
  triggers columns; agentd trigger_create/trigger_update/
  workflow_create/trigger_fires descriptions teach the contract
  (drift-pinned); nightly e2e rows R6a-c cover the wiring 400, the
  mapped-doc validation pair, and signed webhook deliveries — payload
  as top-level run input on the happy leg, redacted validation_error
  fire (no instance echo) on the unhappy leg.

## [0.33.2] - 2026-09-18

### Fixes — agent prompts

- **Dotted-path agent-prompt templating (#1417, PR #1430)**:
  `{{.body.topic}}` resolves through nested maps — parity with
  condition-node expression depth; webhook-driven prompts address the
  payload directly instead of inlining `{{.body}}`. Single-pass
  rendering: substituted values are never re-expanded (payload fields
  cannot smuggle refs into the prompt), unclosed refs never swallow
  valid ones (cross-newline or same-line), unresolvable refs stay
  literal. Back-compat preserved: any-charset top-level keys and flat
  dotted keys keep their pre-change rendering. Documented on the
  workflow_create tool surface; a registered nightly e2e (echo-upstream
  rows, happy + unhappy) pins it end to end.

## [0.33.1] - 2026-09-17

### Fixes — workflows

- **specYaml accepts real YAML** (#1418, PR #1428): extractSpecJSON now
  parses block- or flow-style YAML via yaml.v3 into the JSON shape the
  validator expects (valid JSON still passes through untouched); exactly
  one document is required — multi-doc and trailing YAML are rejected
  rather than silently truncated, and neither-JSON-nor-YAML input fails
  with a dialect-naming error instead of the old misleading JSON parse
  message. Create and update paths both covered.
- **inputSchema write gate requires object-rooted schemas** (#1433, PR
  #1435): a schema that compiles but roots at string/number/array made
  every subsequent run 400 ("got string, want object"); the write path
  now rejects non-object roots with a named error. Explicit JSON null
  normalizes to schema-less (create writes no schema; update keeps the
  stored one), and a null/absent stored schema behaves schema-less at
  run time.

### Fixes — agentd

- **Workflow agent-node harness POST is keyed** (#1327, PR #1436): the
  execution identity (workflowID/nodeID/runID) now threads API→agentd
  and keys the transcript POST (`msg_wf_<workflow>_<node>_<run>`) —
  retries/reruns of the same logical node execution upsert instead of
  appending duplicate transcript messages (the #1315 duplication class,
  closed for the outbox path). Routine triggers key on
  trigger/fire identity the same way.

### Fixes — CI

- **Nightly e2e unblocked** (#1437): the 8+-day nightly failure was a
  sparse `api.extraEnv[2]` --set against the chart's empty default
  rendering nulls at indexes 0-1 (template nil-pointer at chart
  install). The nightly now rides dense index [0]; the US-70 lockstep
  pin test asserts each workflow's dense layout separately.

### Documentation

- **Design 0059: trigger input mapping** (PR #1434): the merged design
  for #1425/#1419 — per-trigger `inputFrom` (envelope | body | mapped)
  plus a static `input` document, create/update wiring guards (V1-V7),
  opt-in fire-time validation with redacted location-only violation
  records (recursive 4 KiB cap), and byte-identical legacy defaults.
  Implementation follows.

## [0.33.0] - 2026-09-17

### Features — secrets

- **US-72.1: StagingProvider envelope (KMS/HPKE) + dynamic staged-key
  redaction rules** (PR #1407): `pkg/secrets.StagingProvider` implements
  the design-0058 §4.2 `stg:v1` envelope — wire format
  `stg:v1:<alg>:<keyID>:<base64(payload)>` with `alg` ∈ {aes-256-gcm,
  hpke} and the header (version + algorithm + plaintext keyID) bound as
  AEAD AAD, so an envelope cannot claim an algorithm or key it was not
  sealed under. KMS mode stores the 32-byte local KEK only KMS-wrapped
  in `llm-relay-kek` — never plaintext bytes in any artifact. The
  owner-directed redaction integration (§4.9 addendum) lands in
  `pkg/redact` as dynamic staged-key rules: redaction (payload hygiene)
  and staging (credential delivery) remain separate mechanisms meeting
  at exactly one place — the seal/resolve seam. 1.7k lines incl.
  dynamic-redaction and staging-provider suites plus benchmarks.

### Fixes — automation (live e2e findings #1410–#1415, PR #1420)

- **Cron triggers fire at the schedule they were given, not stale
  slots** (#1410): `UpdateTrigger` gained a `NextFireAt` column, and the
  update handler recomputes the slot the moment `sourceConfig` changes
  (previously a rescheduled trigger kept firing at the OLD schedule's
  time until that slot passed). A stale (past) slot is refreshed on
  disabled→enabled re-enable — transition-guarded so a no-op
  `enabled:true` never pushes an imminent-but-unclaimed fire.
- **Create validates schedules and never fires immediately** (#1411):
  cron exprs are parsed (five-field syntax) and tz validated (IANA) at
  create/update — 400 on garbage instead of the silent hourly retry
  loop — and `next_fire_at` initializes to the schedule's first real
  occurrence instead of `now` (every newly created enabled trigger
  previously fired ~7s after creation). Shared logic in the new
  `pkg/workflows/schedule.go`; the engine keeps legacy fallbacks for
  pre-validation rows.
- **DAG triggers work from the pod automation surface** (#1412):
  `workflowId` (camelCase — the snake_case alias is normalized,
  contradictory duplicates 400) no longer collides with the forced
  routine stamp; the pod handler gates the target on existence, owner,
  and targeting THIS pod's workspace (404/403). Firing at a deleted
  workflow now records a FAILED fire with a workflow-not-found payload
  and drives the failure counter / auto-disable — previously a silent
  forever-tick. All workspace/workflow keys fold case-insensitively
  (the decoder binds DTO fields case-insensitively; exact-spelling
  strips were bypassable via variants like `workspaceid`).
- **Workflow runs enforce inputSchema** (#1413): run input is validated
  against the workflow's `inputSchema` before queueing (400 with schema
  detail — previously garbage inputs burned node executions and failed
  deep in the DAG with confusing errors); schemas are compile-checked at
  workflow create/update. Trigger-fired runs deliberately bypass (the
  system envelope can never satisfy user schemas) — documented at the
  site, design options tracked in #1425.
- **Script failures name their cause** (#1414): script node language is
  validated at spec time (`python|node`); pre-execution failures keep
  their real error (`unsupported language: bash`) instead of the
  swallowed `exit -1: ` with empty stderr; real process exits keep the
  `exit N: stderr` shape.
- **Agentd MCP tool descriptions match the contracts** (#1415): the
  invented node vocabulary (transform/parallel/delay/mcp_call) is
  replaced by the validated four (script/agent/http/condition) with the
  script-node `handler(input) -> dict` contract, the `workflowId`
  spelling, targetWorkspaceId requirements, and no-immediate-fire
  semantics — content-pinned by drift tests so the class cannot
  silently return.

### Fixes — automation (pod seam, #1426 via PR #1431)

- **The pod-scoping invariant holds on the update path**: the rule
  above (a pod schedules work only in ITS workspace) was create-only —
  `trigger_update` replayed caller `workspaceId`/`workflowId` verbatim,
  allowing retargets to other workspaces or cross-workspace DAGs.
  Update patches now scope exactly like create (workspace retargets
  forced here; workflow retargets gated 404/403, folded
  case-insensitively — a `WorkflowId` variant previously escaped the
  gate entirely; clearing a DAG target lands the routine HERE; JSON
  null is an explicit 400).

### Fixes — platform

- **Epic-71 test flakes** (PR #1408): outbox-verify persistFirst data
  race fixed plus contended timing windows.
- **CI: gVisor bundle sidecar tree** (PR #1406): install step stages
  gvisor_sentry under STRICT policy.

### Testing — automation e2e

- Nightly kind-cluster rows R1–R5 (`local/issue-1410-1412-automation-
  e2e.sh`): create validation + first-slot, immediate reschedule,
  no-op-enable slot stability, missing-workflow failed fire with
  failure accounting, and run-input schema enforcement — API-only and
  LLM-free (ghost-workflow targets), with structural pins so rows
  cannot silently drop. Real-PG `UpdateTrigger` next_fire_at
  persistence row added to the integration suite.

## [0.32.1] - 2026-09-17

### Fixes — webhook triggers were unreachable at their advertised URL

- **Receiver resolved the wrong ID** (PR #1404): create/rotate advertise
  `/api/v1/hooks/<triggerID>` but the webhook row gets an independent
  `uuid.New()` id, and the receiver looked the path param up as
  `webhooks.id` — two UUIDs that can never match, so every webhook
  trigger ever created returned 404 at its advertised URL before HMAC
  verification. The receiver now resolves by trigger_id
  (`GetWebhookByTriggerID`), repairing all existing webhooks with zero
  migration; delivery dedup keys on the resolved row identity. Dead
  `Store.GetWebhook` removed; contract comments corrected. Guarded by a
  cross-handler e2e (`TestWebhookE2E_AdvertisedURLDelivers`): real
  create → advertised URL verbatim → rotate → signed POST → 202 + fire
  + queued run, plus bad-signature/unknown-id/duplicate unhappy legs.

### Features — agentd

- **`trigger_rotate_webhook_secret`** (PR #1405): the automation
  surface could create webhook triggers but never obtain a signing
  secret (create doesn't return one; rotate lived only on the user
  API). Delegates the existing rotate over pod identity; the response
  `{webhookSecret, webhookUrl}` surfaces verbatim to the owner's agent.
  Rotation is AUDITED — `trigger.rotate_webhook_secret` event with the
  resolved owner as actor (previously rotate issued credentials with no
  audit event while create was audited), the secret never enters the
  audit row. Also adds `TestMCPHandler_EveryAdvertisedToolDispatches`
  (every tools/list name must survive the dispatcher — kills the
  advertised-but-uncallable class caught in review).

## [0.32.0] - 2026-09-16

### Features — agentd automation tools (trigger_* / workflow_*)

- **Automation CRUD + debugging over pod identity** (PR #1402, 2 review
  rounds): eleven MCP tools on `/v1/mcp` — `trigger_list/create/update/
  delete/fires` and `workflow_list/create/update/delete/run/runs` —
  backed by `/internal/v1/automation` (13 routes) and a contained
  opencode seam. The surface authenticates the pod (TokenReview → SA
  principal → namespace+workspace match), resolves the owner
  server-side, and delegates to the EXISTING user handlers — auth +
  scoping only, zero duplicated domain logic. Trigger-create forces the
  routine target to this pod's workspace (a pod cannot schedule work
  into other workspaces). Bodies pass through verbatim (schema-decoupled:
  platform schema evolution needs zero agentd changes; agents learn
  shapes from live `*_list` output, and platform validation errors
  surface verbatim); patches carry caller fields only (id rides the
  URL, workspaceID the query); IDs are UUID-validated before any dial.
  `trigger_fires` (per-fire status, input envelope, error payloads) +
  `workflow_runs` (statuses, error codes) are the debugging reads;
  `workflow_run` is the manual fire button. The first review round
  caught three body-path defects (drained-body replay, query-clobber in
  the resolver guard, un-unwrapped tool wrapper keys) — all fixed with
  delegated-handler integration coverage against the REAL handlers,
  plus an exact-key workspaceID sniff (case-insensitive JSON folding
  could otherwise mix the DTO spelling into the identity check).

## [0.31.0] - 2026-09-16

### Features — cross-session management + user timezone

- **`send_message` + `abort_session`** (PR #1382, 6 review rounds): the
  cross-session management pair. `send_message` delivers a text message
  to an existing session fire-and-forget — the reply stays in the
  target; busy targets queue server-side and deliver at the turn
  boundary (L2-proven on the real binary); abort drops queued input
  (disclosed). `abort_session` stops a target's current turn via the
  consolidated, sessionID-validated `Client.Abort` (hardening the
  API-proxy interrupt path). Detached goroutines capture their logger
  at spawn — the cross-test global-log race class is closed.
- **Live browser timezone for `get_datetime`** (PR #1389, 10 review
  rounds): the tool reports user-local time with a `source` field
  (`argument` | `browser` | `pod`). The frontend reports the browser's
  IANA zone as a user setting; the API pushes it to workspace pods on
  every SSE connect and on setting update (`fanOutTimezonePush`);
  agentd serves `/v1/user-timezone` (§D1-gated, IANA-validated against
  embedded tzdata — FROM-scratch delivery carries no system zoneinfo,
  and a CI tripwire gates the embed's removal). The `timezone` key is
  absent from the JSON when unknown, never faked. SDK/MCP callers pass
  an explicit IANA argument. Kind-cluster e2e (Test 5b) gates the full
  pod-side channel nightly.

## [0.30.1] - 2026-09-15

### Fixes — agentd

- **`session_metadata` context usage mid-turn**: busy sessions reported no
  `context_tokens`/`context_limit`/`context_fill` — opencode zeroes the
  in-flight assistant message's token stamp until step completion, and
  both prompt-token scans (the wire seam's `SessionPromptTokens` and
  agentd's `fetchSessionPromptTokens`, which feeds fillGaps and statusz
  `ContextUsed`) stopped at the placeholder. Both scans now skip
  zero-total stamps and report the last COMPLETED step; post-#1342
  interrupt-first restarts had made the twin's blindspot permanent.
  Live-proven against a busy 0.30.0 session (0 vs the real 559,623).
- **`create_session` guidance contract**: the description now encodes the
  decision rule — use ONLY for independent fire-and-forget work the
  caller will not depend on, or tasks requiring human input / intended
  for human consumption; the result never returns to the caller; for
  outcome-needed autonomous work use the task tool (blocking, returns
  the result). Prompt InputSchema description carries the no-return
  line too, pinned alongside the 11 tool-description assertions.

## [0.30.0] - 2026-09-14

### Features — agent self-management (PR #1364)

- **Seven new agentd MCP tools** on `/v1/mcp`: `rename_session`,
  `rename_workspace`, `call_with_model` (per-call model override with
  optional image attachments, catalog-checked), `create_session`
  (top-level peer session, fire-and-forget delivery), `get_datetime`,
  `session_metadata` (message counts, context-window fill, token usage,
  age, busy flags; read-only aggregate with a pinned secrecy contract),
  and `compact` (summarize-based history compaction with
  run-at-turn-boundary scheduling for busy sessions).
- **Pod-identity workspace rename endpoint**
  (`POST /internal/v1/workspace-rename`): TokenReview-gated with
  namespace + workspaceID binding — a pod can only ever rename its own
  workspace; fail-closed wiring with enforced failure-path logging.
- **Contained opencode wire seam** (`pkg/agent/opencode/loopback.go`):
  all opencode HTTP wire knowledge now lives behind one seam; the
  agentd tools (and the pre-existing `session_list`/`session_read`)
  hold zero URLs/verbs (Rule 12). Full L0–L3 test plan shipped with it
  (`docs/testing/agentd-mcp-tools-test-plan.md`) plus a live-pod probe
  script (`scripts/mcp-tools-liveprobe.sh`).

### Fixes

- **Session rename never landed** (#1364): the adapter's rename used
  POST, which pinned opencode accepts with 200 and silently ignores —
  renames reached the platform index but never the agent. Now PATCH,
  live-verified, pinned at three test levels.
- **Integration-harness health probe unbounded**: stalled agent boots
  hung the probe forever; now 2s-bounded with loud failure.
- **Dev-preview ambient-env test flakes**: three tests broke when the
  runner carried `LLMSAFESPACE_API_PUBLIC_URL` (in-platform runs).
- **Pool F1 probe settle race** (#1356) and **fault budget sized to its
  converge window** (#1321).

### Epic 71 / epic-826 progress (already on main since 0.29.1)

- Input write path: input replies through Act, reject routing + deny
  feedback (#1302, #1360); unanswered-question inbox with
  whileAway re-presentation (#1313, #1355).
- Proxy adapter-only refactor, batches 2–final: session/input/
  permissions clusters, session-index/parents, transport deletion,
  required adapter + zero-site gate (#828).
- Fault-matrix legs 7/8 per-entry cardinality rows (#1358); epic71 soak
  dispatch row + leg-10 parse-site drift pins (#1312).

## [0.29.1] - 2026-09-12

### Fixes — post-0.29.0 correctness hotfixes

- **Delivered-signal exactly-once (#1339, #1348)**: two completers racing
  on the same message (multi-replica outbox) could each fire the
  delivered hook — duplicate "sent" browser events and duplicate usage
  metering for one message. The completion claim is now an atomic
  cross-list removal in one Lua step; only the winner fires, and every
  byte-identical copy of the entry is drained so no residual re-fires at
  boot. Pinned per-site by mutation-verified regression rows.
- **Frontend queue 503 handling (#1325, #1320)**: contended retry/dismiss
  (session busy delivering) no longer mints duplicate queue entries.
- **faultmatrix compile + dedup fixes (#1343, #1346)**: cross-stream
  StoreReader integration repairs after 2a landed.

### Epic 71 — harness completion

- **Fault-matrix assertion harness (#1337)**: the harness engine itself —
  violation counters, convergence samples, and legs 2/4/5 rows over the
  landed wave code.
- **Fault-matrix S5/L3 + leg-3 rows (#1341)**, **delivery-replay S2/S9,
  serve-storm cheapness, the soak driver with negative control
  (#1344)**: the assertion harness is complete; violation counters must
  read zero under injected faults at CI scale (the in-memory shape the
  at-scale soak reuses).
- **#828 batch 1 (#1347)**: adapter-only session cluster methods
  (epic-826 prerequisite work; no behavior change with the adapter
  configured — nil-adapter requests now return a typed 503 instead of
  the raw legacy-proxy fallback).

## [0.29.0] - 2026-09-11

### Epic 71 — input & delivery robustness, waves 0-2 (issues #1310/#1311/#1312/#1315/#1316)

- **Entry-level admission idempotency (#1323, #1315)**: the dedupe key
  travels through Deliver and is enforced at the harness write — the
  ~3-minute re-admission loop that duplicated a hung-turn message 16
  times is dead; retries and re-drives can no longer duplicate.
- **Ledger store-evidence convergence (#1317, #1311)**: the ledger joins
  the reseed — stranded admitted/ledgered rows resolve against store
  evidence (message present → promoted; turn ended → turn-ended), every
  state carries a deadline, and the boot sweep auto-heals wedged
  BUSY/queueDepth sessions on deploy.
- **Parked-error sweeper (#1318, #1322, #1316)**: outbox status:error
  entries re-verify against the ledger and recover automatically
  (admitted → completed; failed-with-budget → re-armed); entries never
  park error while the ledger holds them admitted. The #1308 manual
  Valkey recovery is automated. L9 ≤ 5min.
- **Resolve-by-absence + permission reply contract (#1324, #1310)**:
  AnswerInput treats a harness not-found as the resolution (S6) —
  stale prompts clear on click; AnswerInputAction gains the additive
  `reply` field (once/always/reject).
- **Pending-input leases (#1335, #1310)**: the authority's pending set
  and BUSY status re-derive from harness truth on snapshot serve and a
  15s reconcile cadence (30s convergence bound) — event loss is now
  bounded staleness, not permanent zombie prompts.
- **Loop-liveness gauges + canary probe service (#1319, #1328, #1312)**:
  every periodic loop exports last-run (dead loops detectable); the
  canary skeleton ships metrics-only with #1312 classification.
- **Fault-matrix knobs legs 7/8/10 (#1331, #1312)**: abitest gains
  duplicate/out-of-order Deliver, boundary-stall, and wire-drift
  corruption controls — the contract's failure modes are executable.

## [0.28.2] - 2026-09-10

### Fixed (outbox stranded as error pills — Connect unary ack misparse)
- The outbox deliverer's hand-rolled Connect client parsed unary
  responses as `{"message": {...}}` envelopes — a streaming-frame shape
  connect-go's unary codec never emits. Every real Deliver ack failed
  as "empty message envelope" while the agentd ledger held the
  successful admission: messages were delivered (transcript correct,
  no duplicates) but the outbox rows stranded as error pills in the
  frontend queue, and each retry skipped prior-attempt resolution and
  re-POSTed. Same bug in the typed-actions edge (proxy_actions
  abiAct). Unary wire shapes are now parsed (bare JSON body on 200;
  `{"code","message"}` on HTTP >= 400) and pinned against the real
  generated handler (abiconnect over abitest) so the stubs can never
  drift from the wire again.

## [0.28.1] - 2026-09-10

### Fixed (#1313 — outbox delivery poll window matched to V1 timing)
- The outbox's pollToCompletion window was 10 seconds (designed for
  V2 steer's fast admission). With V1 synchronous delivery, the LLM
  turn takes 30-120 seconds. The poll timed out, the outbox retried,
  each retry created a new admission, and messages duplicated every
  10 seconds. Window now 3.5 minutes (exceeds the admitter's 3-minute
  timeout).

## [0.28.0] - 2026-09-10

### Breaking: adapter split (#1314) + V1 delivery complete (#1313)

- **Separate V1 and V2 adapters**: the v2Store flag is gone. AdapterV1
  reads history from GET /session/:id/message (the store V1 delivery
  writes to). AdapterV2 reads from GET /api/session/:id/message.
  Each adapter is internally consistent — you cannot accidentally
  read V2 history from V1 delivery.
- **V1 delivery**: the admitter POSTs to /session/:id/message
  (synchronous, parts-based). MCP tools are included in the model's
  function definitions. The 3-minute timeout accommodates the
  synchronous LLM response.
- **MCP startup race fix** (#1312): createSessionOnWorkspace polls
  /mcp until all servers report connected before returning the
  session to the frontend (bounded 10s).
- **XDG config copy** (#1310): opencode writes config through the
  XDG path; the copy replaces the symlink (the read-only mount
  caused EACCES).

## [0.27.9] - 2026-09-10

### Fixed (#1313 completion — history reads from the V1 store)
- The adapter's history reads now use the V1 store
  (GET /session/:id/message) matching the V1 delivery path. The
  disappearing-history bug: WithV2Store(true) routed reads to the V2
  store which never sees V1-delivered messages — responses were
  visible during the turn (SSE) but vanished on reload. The 0851
  worklog documented this store split precisely; the adapter now
  reads from the same store it writes to.

## [0.27.8] - 2026-09-10

### Fixed (#1313 follow-up — V1 admission timeout caused duplicate messages)
- The V1 admission timeout was 10s (designed for V2 steer's fast
  admit-and-return). V1 is SYNCHRONOUS — the response IS the LLM turn
  (30-120s). The 10s window fired before the model finished, marked
  the admission failed, and the retry ladder re-sent the same message
  (the #1296 triplication class through a different mechanism).
  Timeout increased to 3 minutes; the admitter already runs in a
  goroutine so nothing blocks.

## [0.27.7] - 2026-09-10

### Fixed (#1313 - V2 steer delivery drops MCP tools; switch to V1)
- **The admitter uses the V1 message path** (POST /session/:sid/message,
  parts-based, synchronous) instead of V2 steer (POST /api/session/:sid/
  prompt). opencode 1.18.15's V2 session runner does NOT include MCP
  tools in the model's function definitions - every platform-delivered
  message silently lost workspace tools (session_list,
  dev_preview_url, etc.). Proven with controlled variables: same
  session, same wording - steer -> "no MCP tools", V1 -> all present.
- The ledger, admission retry, cross-attempt dedup, model pinning, and
  promotion correlation are unchanged - only the final POST to
  opencode changed.
- **The tests that would have caught it**: admitter wire-shape pins
  (V1 endpoint, parts body, V2-never-called), full-chain e2e with
  model pinning, and pool row AC-1e (MCP tools visible through the
  platform delivery path).
- Also in this release: the MCP startup race fix (#1312) -
  createSessionOnWorkspace waits for /mcp to report all-connected
  before returning the session to the frontend (bounded 10s).

## [0.27.6] - 2026-09-09

### Fixed (#1310 — opencode writes config; the 0.27.5 symlink pointed at a read-only mount)
- **XDG config COPY not symlink**: opencode WRITES to
  ~/.config/opencode/opencode.json (model switches, permission changes);
  the 0.27.5 symlink directed those writes into the read-only
  /agentd-config mount → PlatformError EACCES, wedging every turn that
  touched config (production: workspace d0116a27). The copy is
  uid-1000-owned, 0640, atomic (temp+rename); legacy symlinks are
  detected and replaced at boot.
- **Watcher re-copies before restart**: mid-life credential changes
  previously restarted opencode against the stale XDG copy; the copy is
  now refreshed before the restart fires.
- Same-class audit: auth store (already fixed, verified writable in
  production), opencode.jsonc/package.json/node_modules (PVC,
  uid-1000, no issue), opencode.db (PVC, no issue),
  workspace-config.json/secrets.json (agentd-internal, opencode never
  touches them). The managedCopyMarker concept was dropped — the
  platform config is authoritative at boot; user configs belong in the
  other XDG candidates opencode layers on top.

## [0.27.5] - 2026-09-09

### Fixed (the model registry never ingested provider config — #1300)
- **The XDG registry layer**: opencode's V2 model registry
  (model.available(), the list SessionRunnerModel.resolve searches)
  only ingests provider blocks from the XDG config layer; our
  OPENCODE_CONFIG file fed the config service (/config/providers —
  healthy-looking throughout) but never the catalog, so every
  platform-delivered provider was "Model unavailable" on every fresh
  pod. The supervisor now installs the XDG symlink before the first
  spawn (env-precedence resolution matching the child's
  OPENCODE_CONFIG; PVC holds only the link — US-35.7 intact) in BOTH
  supervisor topologies.
- **Bootstrap bounded retry**: first-boot fetch failures retry 3x
  (2s/4s) before the empty degrade — absorbs transient CNI warming;
  401 never retried; last-good batches skip the retry.
- **Post-spawn config watcher**: late-arriving credential batches
  restart opencode once per change (session-aware in single-container,
  grace in sidecar) to rebuild the frozen registry.
- **Quarantine + ownership**: malformed user configs are renamed
  aside at boot (a PVC-persisted artifact crashlooped opencode across
  pod recreations); foreign-owned auth stores normalize to the
  consuming uid (PUT /auth EPERM class).
- **Render/resolver residuals**: zen-kind credentials render no
  config block (auth-store delivery path); qualified defaults verify
  against allowlisted providers' model maps (stale re-pin guard).
- **Pool coverage (the gap that let this ship)**: AC-1b (registry
  admission via /api/model, not the lying view), AC-1c (mid-life bind
  heal), AC-1d (credential-backed TURN against a mock upstream), F6
  (faulted boot → heal → registry convergence); the mock rides the
  relay-router egress exemption (podSelector — ipBlock grants cannot
  carve Service VIPs, post-DNAT matching).
- helm: networkPolicy.allowRelayRouterEgress + extraEgressCIDRs
  (operator-granted internal egress knobs, both documented with the
  post-DNAT constraint).

## [0.27.4] - 2026-09-06

### Fixed (the fleet-wide "Model unavailable" — provider credentials never usable on fresh pods)
- **The auth store was unwritable by opencode** — the boot-time relay
  writer created auth.json 0640 uid-2000 (the #1119 read fix); opencode
  (uid 1000) must WRITE it (its auth subsystem persists at boot and on
  PUT /auth) → PermissionDenied → provider registry empty → every model
  unavailable on every pod whose store the sidecar created. The store
  lands 0660 umask-immune now.
- **Provider credentials never reached the store at boot** — the config
  file alone does not register providers with the session runner; the
  bootstrap materialize now merges staged providers into auth.json
  (payload parity with the live PUT /auth path incl. metadata.baseURL;
  symlink-aware atomic write; reserved-slug skip; non-fatal doctrine).
- Pinned at every level: mode (umask-immune), merge + preservation +
  parity, the wiring (real runMaterializeCommand), the first-boot
  dangling symlink (no plaintext on the PVC), stale-temp non-reuse,
  symlink-plant resistance, and the subprocess e2e (content + mode +
  the production symlink shape).

## [0.27.3] - 2026-09-06

### Fixed
- **Sessions stuck BUSY after the turn** — a 240s post-turn capture
  proves the pinned opencode NEVER emits session.idle on the live wire;
  the only terminal marker is a non-`tool-calls` finish on the last
  `step.ended` (live evidence includes `finish:"unknown"` from 741
  context-overflow rows, and abort shapes carrying no finish key).
  Terminal steps now map to SESSION_STATUS_IDLE (final cost preserved);
  the whitelist is inverted — only `tool-calls` is mid-turn.
- **Wrong model on admission** — the V2 prompt endpoint strips
  per-prompt model overrides (live proof: a steer body carrying
  glm-5.3 ran muse-spark). The admitter now sets the SESSION model
  first (providerID wire key per the ≥1.18.15 golden), fail-closed; the
  provider crosses the ledger as provider/id and PERSISTS in the WAL —
  crash/suspend replay re-runs the right model, not the default.
  SwitchModel's pre-existing wrong wire key fixed in passing.

## [0.27.2] - 2026-09-05

### Fixed (the live-render wire mismatch)
- **Live part events carried empty IDs** — the translator decoded
  `partID`/`messageID`, field names on NO frame the pinned opencode
  emits (the wire sends `textID`/`assistantMessageID`); every live part
  event arrived unkeyable and unattributed, and text deltas were dropped
  outright by the frontend. Reasoning and tool events had no handling at
  all. Translated against live production captures: text (textID),
  reasoning (reasoningID), tool lifecycle (callID; called/input memoize
  name+input, success/failure emit the COMPLETE part with decoded
  content[]/structured — memo misses drop rather than wipe; step
  boundaries purge the memo).
- **Ingest SIGSEGV on wire drift** — a frame claiming projectability
  whose properties fail to decode returned (nil, true, err) and crashed
  applyLocked(nil) on the production hot path; err now governs over ok.
- Counter races: parserFailures/droppedEvents synchronized; the panic
  counter is atomic (locking it deadlocked the reseed flush).
- Both production captures ship as replay/e2e fixtures (redacted,
  registered in REFRESH.md) with the invariants pinned at every level:
  unit replay, snapshot integration, delivered-surface e2e (happy +
  three unhappy paths), mutation-verified.

## [0.27.1] - 2026-09-05

### Fixed (the #1288 prompt-triplication incident)
- **Cross-attempt admission dedup** — the outbox retry ladder (10s
  doubling) re-POSTed an entry at attempt+1 and the ledger's dedup was
  attempt-scoped, so every rung manufactured a fresh opencode message
  (live incident: five identical turns from one user send). Dedup now
  keys on the outbox entry ID: a prior ADMITTED/PROMOTED/TURN_ENDED/
  STALLED attempt makes the retry carry that messageID without
  re-POSTing. A FAILED prior still re-POSTs (pinned).
- **Admission uses the TUI's delivery semantics** — `delivery:"steer"`
  (admit-and-run-now, synchronous messageID, events on the session
  stream) instead of `delivery:"queue"`, which the pinned opencode
  1.18.10 never drains (#755; the API's adapter path abandoned queue
  long ago). The restart-destroyed-message window is recovered via
  stall escalation (pinned: fsync'd STALLED state, wake-once,
  stalled-entries gauge).

## [0.27.0] - 2026-09-05

The V2-delivery validation release: the US-70 delivery pool ran fully green
for the first time (delivery suite + revisions + Epic 69 evidence + faults
F1–F5, under `OPENCODE_V2_DELIVERY=1` + `AGENTD_STATE_AUTHORITY=1`, gVisor,
40-workspace scale). Production flip follows this release.

### Fixed (security)
- **Bootstrap-token expiry enforced in the handler** — the cluster's
  TokenReview does NOT enforce TokenRequest `exp` (kind v1.35: a mint 40
  minutes past expiry authenticated). After successful TokenReview the
  handler reads the token's own `exp` claim and 401s past `exp` + 60 s
  clock-skew leeway; absent `exp` fails open (legacy tokens). (#1282)

### Fixed (secret delivery)
- **File-class live delivery** — file-class secrets (ssh-key,
  git-credential, secret-file) applied only at spawn; a bind on a RUNNING
  workspace staged forever (~/.ssh empty, filesRev hashed the empty set).
  New control-protocol `refresh_files` seam: the supervisor re-pulls and
  applies WITHOUT restarting the editor; the reload pipeline triggers it
  on file-class batches; a boot-race heal covers the supervisor's spawn
  pull beating the sidecar's first staging publish. Permissions contract
  verified: uid-1000-owned, 0600, config owner = consuming uid. (#1271)
- **Concurrent workspace reconciles** — the workspace controller ran at
  controller-runtime's default of 1: fleet resume was linear (~8 s per
  workspace; 40 workspaces ≈ 5+ minutes on an idle node). New
  `-max-concurrent-reconciles` flag (default 4, clamped 1..64), chart
  value `controller.maxConcurrentReconciles`. (#1257)

### Fixed (harness/CI — the pool's first green run)
- The delivery pool's real runner blocker: kind's default serviceSubnet
  collided with the host cluster's `kubernetes` service IP (10.96.0.1) —
  in-cluster clients got the HOME apiserver's CA ("x509: certificate
  signed by unknown authority") and kube-proxy nftables mode does not
  DNAT under dind nesting. Non-colliding subnet + iptables pin. (#1247)
- gVisor could never run on Talos workers (`user.max_user_namespaces=0`
  — every runsc sandbox needs a userns; clone past the cap is ENOSPC).
  Worker-scoped sysctl 63154. (talos-ops-prod#2389)
- AC-13 resume stopwatch: three separate set-e/pipefail bugs made the
  first-ever execution report zero samples; write-epoch comparison
  replaces full-rev comparison (revs are per-workspace-unique by
  design); activate bursts retry past the API's token-bucket 429s. (#1253–#1261)
- F3 semantics: a DEK restore is invisible to the revision system by
  design (not manifest data; W2 apply-guard blocks in-place re-materialize
  at the same seq) — suspend/resume is the deterministic recovery. (#1280)
- Fast-iteration cycle: dispatch `dwell_seconds=60` + `use_prebuilt_images`
  takes a debug loop from ~5 h to ~17 min. (#1253)

### Changed
- **The pool runs the V2 regime** — `OPENCODE_V2_DELIVERY=1` +
  `AGENTD_STATE_AUTHORITY=1` at install; the outbox/authority stack now
  wires in the pool (Park/Unpark/inflight live). (#1275)
- AC-13 scale target calibrated to the dind runner class: knee measured
  ~55 concurrent gVisor workspaces across two runs; `RESUME_SCALE=40`.
  The pass criterion is completeness + write-epoch identity, not the
  invented 45 s p95. (#1250–#1257)

### Known issues
- AC-4-lite (a bind racing a pod-delete mid-apply does not survive the
  recreate) — documented known-fail, US-70.3 durability question.

## [0.26.0] - 2026-08-31

### Changed (breaking — read before upgrading)

- **Platform overlay delivery is the only mode (design 0053, #1116)**:
  the runtime base is a pure dev-OS image — agentd (with redact as a
  subcommand, #1152), opencode, and the entrypoints are no longer baked.
  Both delivery pins are now **mandatory at Helm render, controller
  startup, and pod build** (`controller.agentdDelivery.image` +
  `controller.opencodeDelivery.image`, digest-pinned; the release job
  prints the ready-to-paste values block). A render without them fails
  closed — that is the design working, not a bug to work around.
- **The main container execs the pinned overlay agentd directly**
  (`/agentd/.../workspace-agentd --supervise`; sidecar mode:
  `supervise-opencode`), self-verifying its sha256 before any work
  (exit 81/82); the supervisor verify-before-spawns opencode from the
  `/opencode` volume (exit 83/84 → Workspace conditions + events).
- **Base image content-versioned off the release train**: CalVer
  `YYYY.MM.x`, first row `bookworm@2026.08.0`, published solely by the
  new `base-image.yml` workflow (checksum-verified direct binaries,
  cosign + SBOM). Existing clusters promote the default via admin
  upsert — the seed does not move operator defaults (#936).
- **opencode version pin moved to one place**: `runtimes/opencode/Dockerfile`
  (validated 1.18.15, do-not-bump window documented); the base no longer
  carries any opencode coordinate.

### Added

- **S5 overlay-validation kind suite** (`local/s5-overlay-validation.yml`,
  weekly Sun 06:00 + dispatch): missing-pin render legs, full
  launch→ready on the stripped base (PID1 = pinned agentd, opencode
  serving, redact wrapper on-pod), tamper-pin → verify-failure
  conditions, cold-node resume pull cost (measured 13–19s vs the
  pre-overlay ~22s), and the gVisor runsc leg — **design 0051's open
  item answered: image volumes work under gVisor** (pod Active, both
  volumes, opencode serving).
- **Epic 69 (design 0055) session-state machinery**: PVC-backed
  delivery ledger (#1168), outbox terminus switch with inline-first
  delivery and the M4 flag matrix wiring (#1172), typed actions op with
  capability negotiation and sole-writer serialization (#1190),
  API shadow consumer + divergence comparator (#1159), `/v1/events`
  stamped-snapshot surface (#1138). All inert until the V2/authority
  flags flip per the M4 ordering.
- **Epic 70 (design 0057) US-70.1**: spawn-time env pull at the
  supervisor with bounded wait, last-good delta cache, and
  `spawned_rev` terminal verification — closes the sidecar env-class
  secret loss (#1163/#1164); R2b file-class ownership flip (#1171).
- **US-70.0 delivery fault harness** (#1187): env-gated API fault
  injection, key-row corruption fixtures, SA-token time-travel, gVisor
  pool, chaos kills — default-off, zero behavior change.
- `pkg/session` flips to schema-generated types (ADR 0056 T3, #1189).

### Fixed

- Single-container pods materialize cross-uid readable credential stores
  (#1131, in 0.25.7 lineage) — carried forward with the spawn-time pull.
- The worklog renumber bot commented on issue numbers picked off merge
  titles and failed its job after the rename landed; now prefers the
  trailing `(#N)` convention, validates before commenting, and treats
  comment failure as non-fatal (#1191).

### Upgrade notes

1. Set BOTH delivery pins in the same values change as the upgrade
   (digests from this release's printed values block).
2. `agentdSidecar.enabled` remains **off** by default; the flip is a
   separate, preconditions-green action per `docs/runbooks/sidecar-flip.md`.
3. Workspace pods recreate on their next natural cycle; suspended
   workspaces upgrade free at resume.

## [0.25.7] - 2026-08-29

### Fixed

- **Single-container pods materialize cross-uid readable credential
  stores (#1119 follow-up, #1131)**: the uid-2000 init wrote rt/* files
  with owner-only modes — opencode (uid 1000) could never read its
  auth store, so every custom provider failed init and every
  their-model turn died at resolve (fleet-wide
  `ModelUnavailableError` on 2026-08-29 while the relay served the
  models and the generated config listed them; `PUT /auth` 500s with
  EACCES on the same file). The single-container bootstrap and
  materialize now arm `LLMSAFESPACES_CROSS_UID_FILES` exactly as the
  sidecar path always has (files 0640 via the shared gid 1000).
  Existing pods need recreation to land the fixed modes.

## [0.25.6] - 2026-08-29

### Fixed

- **Model fidelity under V2 delivery (#1119 follow-up, #1129)**: the
  V2 prompt endpoint strips per-prompt model overrides entirely
  (verified live: a prompt carrying a bogus model admitted cleanly and
  ran on the session default — the field is not in the endpoint's
  schema). Entries carrying a model selection now apply it to the
  SESSION via the /model route before admission — the same mechanism
  the SPA uses. `SetSessionModel` sits on `agent.Adapter` (wrappers
  inherit; the #1127 lesson). Regression-gated: model-set-before-admit
  ordering, model VALUE arrival, 5xx-without-admission, and the
  no-model no-op.

- **V2 turn lifecycle + session indexing — first R5 bridge slice
  (design 0054 Mechanism 3, #1129)**: V2 turns emit session.next.*
  only; the frontend's turn lifecycle (session.status busy/idle) and
  the sidebar index rode V1 events that never fire. Live symptoms:
  every turn timed out as "Response interrupted" (useChatStream waits
  for session.status=idle) and new sessions never reached the sidebar
  (agent listed 3, API served 2). busy now derives from
  session.next.prompted; idle from the terminal step (finish stop /
  end_turn; failed steps idle too; tool finishes ignored); sessions
  are indexed AT CREATION — the platform owns session CRUD.

## [0.25.5] - 2026-08-29

### Fixed

- **The delivery-verify oracle works through production adapter
  composition (#1119 follow-up, #1127)**: `systemnotices.Wrap` embeds
  the `agent.Adapter` interface, and the handlers' local
  `deliveryVerifier` type assertion failed silently against the wrapper
  — every verification returned inconclusive instantly (no network
  call, no log line). V1 masked it (blocking send completes without the
  oracle); under V2 every delivery parked as "delivery unverifiable:
  agent unreachable" against healthy agents — a total outage.
  `VerifyDelivery` is promoted to `agent.Adapter` (the wrapper was the
  "second consumer" the seam's own comment said would fund the
  promotion), the assertion-failure branch now logs at Error level with
  the adapter type, and a wiring-level regression test composes the
  production wrapper stack and asserts the seam — the 2026-08-29
  outage passed every unit test because test environments wire the raw
  adapter.

### Added

- **Outbox observability (design 0054 I6, #1127)**:
  `llmsafespaces_outbox_entries{status}` and
  `llmsafespaces_outbox_oldest_nonterminal_age_seconds` — the depth and
  stranding signals the 2026-08-28 incident grepped for and did not
  find. Refreshed on a bounded cadence from the delivery worker.

## [0.25.4] - 2026-08-29

### Fixed

- **V2 per-prompt model overrides honor the picked model (#1119
  follow-up, #1123)**: the wire-shape capability probe now targets the
  caller's real session (existence-first binaries answered "Invalid
  session ID" for synthetic IDs, leaving the shape indeterminate), and
  indeterminate probes default to the pinned runtime floor (1.18.15
  `{id, providerID}`) — the legacy `{modelID}` shape is used only on a
  positive legacy probe. The 2026-08-29 regression (all messages forced
  onto the workspace default model) is pinned as a contract test. The
  positive-only capability cache is adapter-scoped: one probe per
  workspace endpoint per process, and a transient probe failure can no
  longer freeze a floor-defaulted classification.

- **The delivery-verify oracle is wedge-immune and visible (#1119
  follow-up, #1123)**: verification runs on a dedicated no-keep-alives
  client with hard per-request bounds — one wedged agent connection
  costs a single bounded pass instead of the shared pool and the whole
  promotion-await budget (first-live-traffic incident: seven
  inconclusive passes against a healthy agent, zero log lines). Verify
  transport errors now WARN with workspace/session/entry context.

- **V2 delivery budget sized around the promotion-await window
  (#1123)**: `outbox.DeliveryTimeout` = window + 20 s margin, so the
  detached context cannot expire inside the await loop and masquerade as
  a transport ambiguity.

## [0.25.3] - 2026-08-29

### Fixed

- **V2 delivery can no longer silently drop messages — the outbox entry
  completes at PROMOTION, not admission (#1119, #1121, 2026-08-28
  incident)**: with `OPENCODE_V2_DELIVERY=1`, the deliverer previously
  fired `queue.update/sent` the moment the V2 prompt endpoint admitted
  the row. But the user text persists at promotion, and a defect-class
  death (model-resolve failure, park race — observed live when a LiteLLM
  config roll churned the model catalog) consumes or strands the
  admitted row with no signal: no assistant, no retry, no trace in the
  serving read mode. The deliverer now polls the promotion oracle
  (`VerifyDelivery` anchored at admission time) for up to 30s
  (production observations: persist lands <1s, including dying
  promotions); an unobserved window expires ambiguous into the existing
  #987 verifying machinery — late promotion completes,
  absent-after-window re-admits (bounded nudge). Regression-gated by
  the inverted pickup-completion test plus never-promotes and
  slow-promotion contract tests against a fake that models
  persist-at-promotion.

- **opencode wire-shape drift is detected, not assumed (#1119, #1120)**:
  the adapter now probes the live binary's capabilities (V2 prompt
  route presence; `Model.Ref` key shape — upstream changed
  `{modelID,providerID}` → `{id,providerID}` by 1.18.15, silently
  breaking per-prompt model overrides) and serializes accordingly, with
  goldens captured from live 1.18.15 traffic. New bump-gate
  `local/opencode-binary-contract.sh` (B1–B5) validates candidate
  binaries behaviorally before a runtime bump; validated end-to-end
  against pristine upstream 1.18.15.

## [0.25.2] - 2026-08-28

### Fixed

- **Bootstrap DEK failures are loud; legacy `user_keys` rows heal
  opportunistically (#1114, 0052 Phase 1 precursor, 2026-08-28
  incident)**: `InjectSecretsForPodBootstrap` audits every
  non-"owner-has-no-secrets" degrade as `pod_bootstrap_dek_failed`
  with the workspace and underlying error — the incident's silent
  sessionless degrade (six-hour forensic diagnosis) now leaves a
  breadcrumb at the failure site. `GetDEKServerSide` self-heals
  unwrappable rows: when the master provider cannot decrypt the stored
  wrap (June-era un-prefixed blob), the DEK is recovered from the
  session source (warm login cache / `jwt_sessions`), the row is
  re-wrapped at the active version with verify-after-write (the new
  wrap must round-trip before the row is replaced), and the secrets
  deliver. Incident-class rows converge on their next resume while a
  30-day login cache still exists; the login-independent reconciler
  follows in design 0052 Phase 1. Regression-gated: the heal is
  asserted one-shot (second boot unwraps directly) and the no-session
  degrade is asserted non-silent.

## [0.25.1] - 2026-08-27

### Fixed

- **User-DEK secrets deliver at pod bootstrap with no active session —
  pod exists ⟹ secrets (#1084 follow-up to #1087, #1104)**:
  `InjectSecretsForPodBootstrap` no longer gates user-owned bindings on
  `jwt_sessions` state. The password-tier removal made every user DEK
  server-recoverable at rest (`user_keys` wrapped under the master
  RootKeyProvider), so the session gate was an ACL defending a
  constraint that no longer existed. New `KeyService.GetDEKServerSide`
  unwraps the `user_keys` record directly and publishes a synthetic
  single-use Redis handle (fresh UUID jti, 5-minute TTL, deliberately
  no `jwt_sessions` row so it can never be rehydrated); bootstrap
  delegates to `InjectSecrets` under that handle. Root cause closed:
  on suspend/resume with no active `jwt_sessions` row, pod-bootstrap
  degraded to server-KEK-only (user env-secrets audited-and-skipped
  while org credentials still delivered), materialize wrote the reload
  cache with the degraded batch (design 0045 Change 3), `hasUserCreds`
  reported `UserCredsPresent=true`, and the `secretautopush` recovery
  was permanently skipped (`skipped_ucp_true`) — total loss of user
  credentials on every resume that outlived the session TTL.
  Degrade semantics preserved for infra failures only (provider
  unwired, store/cache unreachable, owner has no `user_keys` record);
  `InjectSessionlessSecrets` (API-key SDK surface) is untouched.
  Secrets remain user-DEK encrypted at rest — delivery-only change on
  the pod-bootstrap path (TokenReview trust model unchanged).
  Regression-gated at every level: unit session-independence rows (no
  store / empty table / rotated-out signing keys), e2e
  no-active-session and password-reset boots asserting user providers
  materialize, and jwt wiring retained as an interference guard.

## [0.25.0] - 2026-08-27

### Added

- **Image factory: automatic release-train tracking (#1094)**: the
  catalog default base now follows the deployed platform release
  without operator action. When the API's build version outstrips the
  default base row, a boot + hourly reconciler digest-resolves
  `base:<version>` from the registry — doubling as the existence gate,
  so unpublished tags never enter the catalog — then upserts the row
  and moves the default in one transaction (#950 semantics). Existing
  builds/configs are immutable; pinned configs keep their pin and
  migrate via the base-update pill as before. Root cause: the
  2026-08-27 investigation found the default at bookworm 0.21.2, five
  releases stale, because nothing reconciled the catalog — the same
  class as the 2026-08-25 incident (worklog 0821).
- **Image factory: hash display + hash-based re-selection (#1094)**:
  every image config now shows its schematic hash, and
  `GET /image-factory/resolve/:hash` recovers the selection a hash
  names plus every base version built under it (newest first; failed
  builds excluded). Any authenticated user may resolve any hash — a
  hash is a content address over public catalog extension IDs, and
  builds already coalesce across scopes. The create form's "Build from
  hash" input prefills the selection (retired extensions dropped with
  a notice — parity with the #928 refresh prefill) and pre-targets the
  newest built version.
- **OpenAPI (#1090)**: org-core CRUD documented; the last org
  spec-only allowlist block dropped — the SDK contract now covers the
  full org surface.

### Fixed

- **Git auth survives suspend/resume; decoupled from `gh`/`GH_TOKEN`
  (#1087, #1096)**: the runtime image now points git's
  `credential.helper` at the materialized
  `/home/sandbox/.git-credentials` via two layers — `/etc/gitconfig`
  and `GIT_CONFIG_*` image ENV (the env layer survives a PVC-persisted
  `gh auth setup-git` helper reset; validated against git 2.39).
  Cold-boot materialization from a bound `git-credential` secret is
  regression-gated by `cmd/workspace-agentd/git_creds_boot_test.go`
  (suspend/resume cycle) and the image-build smoke test.
- **Bare `workspace-agentd` invocation no longer nil-panics**: the
  admin-token assignment on the (nil, by design) server-only process
  was the one unguarded deref (`main.go`); now guarded. Also repaired
  the stale `local/test-entrypoint.sh` harness (sed-patched
  bash-materializer era) to drive the real subcommands — 7/7 green.

### Changed

- **UI version semantics (#1094)**: image-factory versions are
  platform releases, not Debian suite versions — cards, the base
  picker, and update pills now read `bookworm — platform 0.25.0`
  instead of the ambiguous `bookworm 0.25.0`.
- **CI (#1095)**: Epic 68 attachment rows E2/E10/E11 execute in a
  weekly single-container kind workflow.

## [0.24.0] - 2026-08-27

### Added

- **Epic 67 chat file attachments — full pipeline**: US-67.2 upload route
  with streaming multipart + gates (#1083), US-67.4
  `workspace_file_upload` MCP tool + `session_message` files param
  (#1084), US-67.5 composer drawer, upload chips, manifest strip
  (#1085).
- **SDKs (#1072)**: MCP-server CRUD in Go/TS/Python/Java; VS Code
  extension removed.

### Fixed

- **mise toolchains resolve on PATH in non-interactive shells (#1086)**:
  three stacked causes — build-time reshim ran before the ENV block and
  wrote to /root (dead weight); MISE_DATA_DIR shims under /workspace are
  PVC-shadowed at runtime; sidecar mode (production) bypasses the baked
  entrypoint, so no boot step ever rebuilt shims. Fixed at every layer:
  build reshim into the system dir (baked, never shadowed),
  `$MISE_DATA_DIR/shims` first on PATH, entrypoint reshim (legacy
  mode), `ensureMiseShims` in the supervisor (sidecar mode, before
  first child spawn). Regression-tested per layer (verified fail/pass
  against pre-fix code) + K14 kind check: real pod, fresh PVC,
  `sh -c 'command -v go python3 node'`. Smoke test now asserts PATH
  resolution, not just `mise which` discoverability — the gap that let
  this ship green.
- **Creating-phase wedge (#1081, #935)**: init-crashloop recovery +
  restart rebuild.
- **MCP live part.delta capture (#1053, #1082)**: content from
  session.event envelopes.

### Changed

- **Docs (#1089)**: README-LLM/OpenAPI reference synced to
  post-epic-1032 reality.

## [0.23.0] - 2026-08-27

### Added

- **Epic 68 — port-in-subdomain preview origins (#1069, design
  epic-68-port-in-subdomain-preview-origins)**: dev-preview hosts move to
  `https://<port>-<uuid>-preview.<baseDomain>/<app-path>`, fixing the
  root-absolute-URL breakage class (apps emitting `303 Location: /login`
  lost the port prefix and died with an indistinguishable 502 — the
  tinyrsvp incident). The port now lives in the host, so root-absolute
  URLs, SPA routers, and runtime `fetch('/api/...')` resolve correctly
  with zero app changes (same shape as GitHub Codespaces/Gitpod). Host
  disambiguation via two fixed-length regexes proven mathematically
  disjoint (port quantifier {1,5} < UUID first-segment length 8 — F1
  digit-leading UUIDs cannot be misparsed), backed by a 10k-iteration
  property test and an exhaustive boundary sweep. Legacy
  `<uuid>-preview.<baseDomain>/<port>/...` URLs keep working; bootstrap
  redirects to the new shape. Token binding rejects host-port ≠
  token-port; T3 indistinguishable-from-dead verified byte-for-byte
  (blocked vs dead port); landing page serves deep links on both host
  shapes. Also fixes `LLMSAFESPACE_API_URL` missing from the main
  container (relative bootstrap links).
- **Default platform prompt for fresh installs (#1073)**: the
  platform-tier system prompt is no longer empty until an admin
  configures it — `types.DefaultPlatformPrompt` ships in code
  (posture of `DefaultJWTIssuer`), used only when the
  `sys_prompt_platform` row is absent; a saved row, including an
  explicitly cleared one, always wins. Content merges the operator's
  production prompt (tier framing, egress model, credential handling,
  behavioral invariants, tone) with mise environment guidance — the
  load-bearing rule that `command not found` almost never means the
  toolchain is absent (check `mise which`/`mise ls`/installs dirs
  first). Fixes a factual error along the way: `/tmp` and
  `/home/sandbox` DO persist (PVC subPaths), contrary to the Dockerfile
  comment. `GET /admin/prompt` surfaces the effective default so admins
  can edit from it.
- **MCP tool guidance contract (#1076, #1079)**: agentd's
  `tools/list` descriptions — the only documentation the model ever
  sees — now carry when-to-use/when-NOT-to-use guidance per tool.
  `dev_preview_url`: offer the preview link when a server is running, a
  UI is finished, or the user is doing frontend/UI work (offer to spin
  up the dev server and share the link unprompted); never mint links at
  dead ports or re-share a deterministic URL. `session_list`/
  `session_read`: concrete underuse triggers (unfamiliar workspace,
  user references earlier work, about-to-rebuild checks,
  suspend/resume continuity) and negative scope.
  `TestMCPHandler_ToolDescriptionGuidance` pins the guidance as
  contract.
- **US-68.1 (#1074)**: agentd file-ingest endpoint `PUT /v1/files`.
- **US-68.3 (#1075)**: attachment manifest v1 contract + `files[]`
  send-path integration.
- **SDKs (#1068)**: cursor-paginated session history in Go/TS/Python/Java.
- **OpenAPI (#1064)**: org/admin/image-factory surface documented; every
  handler wired into the contract fixture.

### Fixed

- **Chat stream interruption false alarms (#1051)**: session status is
  rechecked before declaring an interrupted stream.
- **Outbox freeze (#1078, #1019 C+D)**: deploy-drain grace +
  frozen-queue surfacing.
- **Chat disk pressure (#1071)**: nudges moved to the Adapter seam +
  typed disk-full 507.
- **Billing migrations (#1070)**: renumbered past
  000026_session_alerts; chart synced; quota integration tests added.

### Changed

- **SDK contract gates (#1067)**: Hurl contract tests now blocking;
  expectedPaths dropped; PACKAGES claims corrected.

## [0.22.0] - 2026-08-27

### Added

- **D6 — unattended escalation for hung sessions (#998, design 0050
  §D6)**: sessions that are busy beyond 15 minutes with no progress
  (hung-and-alive — suppressed-forever by design D1) are now surfaced
  instead of sitting silent. agentd statusz reports
  `oldest_busy_seconds` + per-session `busy_ages`; the API escalation
  sweep (60s reconciler tick, 30min per-workspace cooldown,
  panic-isolated, notify-only — nothing is ever restarted or killed)
  publishes `workspace.alert`/`session_hung` SSE events, persists each
  escalation to the new `session_alerts` table (migration 000026,
  24h retention), and exposes `GET /workspaces/{id}/alerts`. The
  frontend shows an amber banner (auto-clears on idle) and a sidebar
  badge (replaces the busy dot), recovers hung state from persisted
  alerts on (re)connect, and the workflows page flags runs whose
  workspace has a persisted hung alert.

### Fixed

- **Billing metering integrity (#1065)**: lossless metering, held
  export cursor, persistent token dedup.
- **MCP wire drift (#1052)**: four P0 client↔router wire bugs repaired
  plus an integration gate.

### Changed

- **OpenAPI (#1055)**: documented Epic 64 workflows/triggers, passkeys,
  and history pagination (`#1039 #1040 #1041`).

## [0.21.4] - 2026-08-26

### Fixed

- **image-factory catalog reads 500 on a NULL digest row (#1030,
  live incident 19:45 UTC)**: a direct-SQL catalog row with digest
  NULL made every /image-factory/catalog (and base-update enrichment)
  fail — ListBases scanned digest into a plain string. The table has
  no NOT NULL constraints; reader availability must not depend on
  writer discipline. Scan-side NULL tolerance (sql.NullString, NULL
  reads as the zero value) in ListBases/GetBase, pinned by sqlmock
  regression tests using the incident's exact row shape.

## [0.21.3] - 2026-08-26

### Fixed

- **init-fs left rt/ itself outside the 0770 managed set (#1028, kind
  run 3)**: the sidecar's bootstrap writes rt/secrets.json directly in
  rt/, and a 0750 parent (MkdirAll default under umask, +setgid from
  the pod fsGroup) made the empty-batch write fail SILENTLY — the
  restart guard never held (K11) and rt/ stat'ed 2750 (K3), while the
  graceful degrade chain kept the pod booting (why K13 — the
  degraded-base incident regression — still passed). rt/ now joins the
  exact-0770 set; writeEmptySecrets reports failures (never-block-boot
  preserved; pinned by the loudness regression test); the K10 script
  check switches from pod-name diff (names are deterministic) to
  creationTimestamp.

## [0.21.2] - 2026-08-26

### Fixed

- **init-fs created rt dirs 0700 instead of the US-4b 0770 cross-uid
  contract (#1025, found by kind run 2)**: the uid-2000 sidecar's boot
  phase failed materialize with EACCES — the shared-gid-1000 bridge
  needs the group-write bit. Exact-mode semantics (chmod after
  MkdirAll): a bare MkdirAll(0770) under umask 022 yields 0750, which
  also EACCES. Pinned two ways: the always-on group-write-bit contract
  test (red against 0700 AND the 0750 umask variant) and a root-gated
  real cross-uid reproduction (init-fs as uid 1000/gid 1000, the
  materialize write probed as uid 2000/gid 1000 via test re-exec).

## [0.21.1] - 2026-08-26

### Fixed

- **Platform init containers did not mount the agentd image volume
  (#1023, found by the first K1–K13 kind run)**: platform-init,
  platform-bootstrap, and platform-materialize exec the overlay binary
  path but the `agentd` image volume was never mounted on them
  (wireAgentdOverlay wires main+sidecar only) — every NEW workspace pod
  on an overlay-mode cluster crash-looped at Init with exit 128
  (`stat /agentd/usr/local/bin/workspace-agentd: no such file or
  directory`). No L0–L2 layer validates mount-vs-command consistency;
  only a real kubelet could see it. Pinned by
  TestPlatformInit_AgentdVolumeMountedEverywhere (both modes, init +
  main containers).

## [0.21.0] - 2026-08-26

### Added

- **Sidecar migration steps 1–2: platform boot leaves the runtime image
  (#1021, design 0051 D7)**: closes the 2026-08-25 incident class
  structurally. `workspace-agentd init-fs` (uid 1000, digest-pinned
  artifact) absorbs the bash init containers — PVC subPath roots, the
  US-35.7 symlink farm hardened against pre-planted symlinks (lstat
  semantics: the link inode is replaced, never followed), G21 password
  0600, admin-token 0400, free-models copy. In sidecar mode
  bootstrap+materialize run inside the sidecar's boot phase (the #857
  stamp-before-read guarantee rides the startup probe) with a restart
  guard (no API refetch on sidecar restart; materialize is idempotent)
  and fail-fast propagation. In legacy overlay mode they run as
  `platform-bootstrap`/`platform-materialize` init containers. No shell
  executes from the runtime image in overlay mode.
- **Supervisor Command bypass + self-verify (#1021)**: sidecar-mode main
  containers run the overlay supervisor directly (baked entrypoint
  bypassed); the #863 verify moved into the supervisor — self-hash of
  /proc/self/exe against the pod-spec pins, exit 81 +
  `expected=/got=` message contract preserved for
  `detectAgentdVerificationFailure`. Entrypoint env relocated to the
  pod spec (OPENCODE_CONFIG, XDG_DATA_HOME, event system,
  OPENCODE_SERVER_PASSWORD via secretKeyRef — fail-closed).
- **BootReady condition + platform-boot-failure visibility (#1021)**: a
  crash-looping platform container surfaces as `BootReady=False`
  (`ReasonPlatformBootFailed`) with a one-shot event and the
  `llmsafespaces_workspace_platform_boot_failures_total` metric — the
  eternal-reasonless-Creating incident presentation now self-reports.
- **Image factory base compatibility floor (#1021)**:
  `imagefactory.MinBaseVersion = 0.15.7` (the #871 release) enforced at
  render time — the factory refuses to stage onto bases whose baked
  agentd predates the secrets contract. Ships in the API release train;
  bumps are reviewed release actions.
- **L3 kind suite K1–K13 (#1021)**: the step-1/2 world is pinned at L3 —
  platform-init ordering, subcommand form + symlink farm, resumed
  legacy PVC (force-upgrade path), sidecar restart guard, supervisor
  Command bypass, and K13: a DEGRADED runtime base (baked agentd +
  entrypoints deleted) boots Ready — the incident-class regression.
  e2e-nightly now runs sidecar-mode full stack. Flip runbook at
  docs/runbooks/sidecar-flip.md (the `agentdSidecar.enabled` default
  stays false until K1–K13 passes once on released artifacts).
- **agentd US-4a spawn_env consumer end-to-end (#1015, design 0051)**:
  parent+delta env merge for the supervised opencode child.
- **agentd/controller US-4b mount relocations by consumer (#1020,
  design 0051)**: agentd-config (RO in the workspace container) and
  agentd-secrets (sidecar-only) emptyDir volumes; rt/* re-materialized
  0640/0770 (cross-uid via shared gid 1000); admin-prompt and the
  reload cache leave uid-1000 space entirely.

### Fixed

- **`materialize` subcommand ignored `LLMSAFESPACES_CROSS_UID_FILES`
  (#1021)**: only the reload handler passed `CrossUID` to the
  Materializer, so sidecar-mode boot-phase materialization wrote 0600
  uid-2000 files that the uid-1000 supervisor/entrypoint silently
  failed to read (EACCES degraded `buildEnvFrom` — env-secrets would
  vanish with no signal). Found by writing the integration test before
  the fix.

## [0.20.1] - 2026-08-21

### Fixed

- **Outbox: Run joins its workers — the release-pipeline hang root cause
  (#1016)**: Run returned on ctx.Done without joining the deliverOne
  goroutines it spawned. The abandoned-goroutine gap caused the 10-minute
  race-detector package hangs that killed the v0.19.1 and v0.20.0 release
  runs, the #1005 watchdog stalls, and a goroutine-stack data race against
  subsequent tests. Post-delivery Redis bookkeeping (staging cleanup,
  state restore, lock release) now runs on a detached, bounded context
  minted AFTER the deliverer returns, with a fixed 5s bookkeepingTimeout
  (never coupled to DeliveryTimeout). Verified 25× -race with zero
  failures and zero races (baseline flaked 2–4/25).

## [0.20.0] - 2026-08-21

### Added

- **agentd US-3 credential split (#1011, design 0051)**: `agentdPassword`
  secret key + per-endpoint credential table (§D1) — user-mux and
  admin-mux no longer share a credential.
- **Release-pipeline integrity (#1008)**: agentd is built, signed,
  Trivy-scanned, SBOM'd, and release-noted inside the release workflow —
  release success now means ALL artifacts published (the v0.19.1 gap).
  Enforced by a new repolint invariant (every `*_IMAGE` in the sign/
  scan/SBOM loops and release table; every merge job gates the release).

### Fixed

- **Dev-preview chat button did not render in production (#1013)**:
  workspace pods without `LLMSAFESPACE_API_URL` emitted RELATIVE
  bootstrap links, which the button parser (absolute-only) rejected.
  agentd now derives `https://api.<baseDomain>` in origin mode; the
  frontend resolves relative links against the app's API base (never
  the frontend origin). E2E covers split- and same-origin deployments
  via mocked `/env.json` — the blind spot that shipped this bug.
- **agentd path-mode tests were environment-dependent**: they inherited
  the pod's real `PREVIEW_ORIGIN_BASE_DOMAIN` and flipped to origin
  mode inside refreshed pods; all affected tests pin their env now.
- **US-2 L3 kind CI (#1007, #1009)**: crictl sandbox resolution,
  1s-granular timestamp ties.

## [0.19.2] - 2026-08-21

### Fixed

- **Outbox test teardown hang (#1005)**: TestRun_ConcurrentSessionsNoHeadOfLine
  hung the race-detector package (10m timeout) on every full-suite CI run,
  blocking the v0.19.1 tag build entirely. The test's slow-fn waited on the
  deliverer context — which deliverDetached deliberately detaches from
  cancellation (D3 disconnect-immunity) — so test teardown could never wake
  it; the unbounded cleanup join then stalled indefinitely under scheduler
  contention. The fn now selects on a test-owned channel, and the teardown
  join runs under a 5s watchdog that fails fast with a diagnostic instead
  of killing the package.

## [0.19.1] - 2026-08-21

### Fixed

- **Landing page CSP blocked its own bootstrap form (#1000)**: the
  per-workspace preview landing page carried `form-action 'self'`, but
  its port form submits cross-origin to the dev-preview-bootstrap
  endpoint on the API origin — browsers blocked the landing's only CTA
  (observed in production minutes after the 0.19.0 rollout). The
  landing now carries a landing-specific policy with
  `form-action 'self' <api-origin>`; proxied app pages keep the strict
  `form-action 'self'` (token-boundary test-pinned — the extension
  cannot leak to app responses).

## [0.19.0] - 2026-08-21

### Added

- **Epic-66 Phase 2 — relaxed CSP on per-workspace preview origins
  (#992, redesign §5.4)**: preview-origin responses now carry the
  dev-friendly policy (script/style `unsafe-inline`, `unsafe-eval`,
  `img-src data: blob:`) while `connect-src` stays strictly `'self'`
  (no bare `ws:`/`wss:` — test-enforced). Unblocks Next.js App Router
  hydration, Vue full-build, and inline scripts/styles. Safe because
  preview origins hold no platform credentials (T1 closed by Phase 1's
  architecture) and no network exfil channel opens. The API origin's
  strict policy is untouched (path-based route pinned by test).
  `frame-ancestors` configurable via `previewOrigin.frameAncestors` /
  `LLMSAFESPACES_PREVIEW_ORIGIN_FRAMEANCESTORS` (empty = none) —
  enables dashboard embedding per deployment.
- **Epic-66 UX round 2 (#986)**: `dev_preview_url` renders an
  always-visible **Open dev preview :&lt;port&gt;** button in chat history
  (collision-proof `LSP_DEV_PREVIEW_V1` sentinel; legacy output renders
  unchanged); port optional everywhere (default 5173); landing page port
  always editable + "How dev preview works" explainer;
  `X-Forwarded-Host`/`X-Forwarded-Proto` forwarded to dev servers so
  absolute-URL generators emit the real origin.

### Fixed

- **D3 follow-ups (#907, #964)**: durable-prompt server-side retry
  wiring + pre-redelivery dedupe.
- **US-2 CI (#989, #990)**: kind integration hardening — binary sha
  pins, failure-preserving teardown, digest-splice fix, controller-lean
  install.
- **Outbox re-delivery on turn-timeout (#987)**: a prompt whose agent turn
  outlived the outbox `DeliveryTimeout` (10m) was classified "not
  delivered" and retried as a fresh POST — re-running the user's turn
  (observed 3×) and then parking the entry as a terminal error that never
  left the queue UI. Unknown-outcome sends (timeout mid-turn, transport
  cut) now enter a `verifying` state and are resolved against the agent
  transcript (opencode persists the user message before the turn runs):
  delivered → complete; proven absent → retry; unverifiable → bounded
  recheck. Ambiguous attempts no longer count toward the retry bound;
  API-crash recovery requeues staged entries as verifying (never
  blind-re-sent). Every confirmed delivery now emits the
  `queue.update/sent` SSE (the frontend pill clears deterministically),
  and queue pills surface `verifying`/`delivering` states. Also fixed a
  stress-found production race: concurrent accepts could overshoot the
  per-session queue cap across API replicas (atomic check-and-push).

## [0.18.0] - 2026-08-20

### Added

- **Dev-preview landing page (#983, epic-66 Phase 1 UX)**: unauthenticated
  browser navigations to a preview origin now get a human front door — a
  one-click bootstrap link for deep links whose preview session lapsed,
  and a no-JS port form on the bare host root — instead of a bare 401/502.
  fetch/XHR/curl keep the 401 (Sec-Fetch-Mode cannot be forged by script);
  token redemption always wins over the landing. The landing is static
  HTML+CSS with no scripts, no credentials, and a test asserting no
  password input can ever appear: the preview origin serves
  prompt-injectable content, so it must never train users to enter
  credentials there. Identity stays exclusively on the API origin.
- **agentd sidecar split (#980, design 0051 US-2)**: `--sidecar` mode,
  chart-gated native sidecar, socket grace/metrics.
- **OpenAPI spec + SDKs on the session contract (#969, #975)**.

### Fixed

- **US-2 pod-spec sidecar invariants restored (#982)**: tests lost in the
  #980 squash, re-established.

## [0.17.0] - 2026-08-20

### Added

- **Per-workspace dev-preview origins (#974, #976, #977; epic-66 Phase 1)**:
  dev preview moves off the API origin onto per-workspace hosts
  (`<uuid>-preview.<baseDomain>`), closing the redesign threat-model's T1
  (prompt-injected preview JS making credentialed same-origin API calls)
  by architecture. Owner-authenticated bootstrap on the API origin
  (existing auth + ownership chain) 302s to the preview host with a
  one-time HMAC token (jti consumed via cache SetNX), redeemed for a
  `__Host-pv` preview-session cookie — the platform session cookie never
  reaches preview hosts, and preview hosts never serve `/api/*` (T5).
  Blocklisted/privileged/invalid ports are indistinguishable from dead
  ports (T3); the `dev_preview_url` tool refuses them before minting any
  URL and emits the bootstrap URL when the deployment sets
  `PREVIEW_ORIGIN_BASE_DOMAIN` (controller stamps it into workspace pods
  from the same `previewOrigin` chart values the API consumes). Off by
  default with a fail-closed boot guard (enabled without
  baseDomain+tokenSecret refuses to start). 20 new tests across handler,
  config, router, agentd-tool, and pod-builder layers.

## [0.16.1] - 2026-08-20

### Fixed

- **Session load took 10–15s on large transcripts (#971, #973)**: the
  first history page now uses opencode's native `?limit=N` (measured
  live: 26ms vs 1.8s for the full-transcript fetch+decode the handler
  previously performed per page — ~95% of the hot path was decoding
  messages it then discarded). Back-pagination keeps the full-fetch
  path (opencode 1.18.10's cursor params are unusable — verified live:
  `before=` 400s every shape, `cursor=` is ignored). The
  `X-Next-Cursor` contract is unchanged.
- **D3 follow-ups (#960→#964)**: queue retry wired server-side
  (POST /queue/:id/retry re-arms the same entry) and a pre-redelivery
  transcript check that collapses the dominant duplicate-turn source.

## [0.16.0] - 2026-08-20

### Added

- **D3 durable prompts (#907, #943)**: Valkey-backed outbox with
  accept-then-202 semantics and detached delivery — prompt submissions
  survive API restarts and deliver when the consumer is ready (api +
  frontend).
- **repolint ForbiddenPathsCheck (#854, #959)**: live guard against
  resurrection of deleted filesystem trees (the c9c68684-class stale
  merge), wired into pre-commit + CI.

### Fixed

- **Dev preview served stale HTML through the CDN/browser chain (#957,
  epic-66 P0-1)**: the API edge now forces `Cache-Control: no-store` on
  `text/html` responses — the chain had been observed serving stale
  previews across dev-server changes, sending developers debugging code
  that never reached the browser. Non-HTML responses keep app-set caching
  so hashed bundles still cache.
- **Dev preview stripped WebSocket upgrades — HMR broken end-to-end
  (#958, epic-66 P0-2)**: the G34 header wipe in the dev-preview proxy
  discarded `Connection`/`Upgrade` (set by ReverseProxy before Rewrite
  runs) and the allowlist omitted the `Sec-WebSocket-*` handshake family,
  degrading WS handshakes to plain GETs at dev servers. Upgrade headers
  are now re-established after the wipe; 101 responses skip the
  response-size body wrapper (upgrade streams are bidirectional and
  unbounded). Full WS echo round-trips pinned by tests on both the API
  edge and the agentd hop.
- **agentd fail-closed boot (#934, D5.2/D5.3)**: an admin token is now
  required at boot (no silent tokenless mode) and an empty workspace
  password is rejected; the boot-resolved token threads through to the
  muxes.
- **Distinct admin-mux token (#933)**: agentd's admin mux authenticates
  with its own file-only-delivered token (env scrubbed of the old
  secret), with a mixed-fleet bearer fallback for upgrade ordering.

### Changed

- **Legacy per-language runtime images deleted (#854, #959)**:
  `runtimes/{go,nodejs,python}` were dead (built by no pipeline since
  Epic 7; accidentally resurrected by a stale-branch merge). Tenant
  toolchains are managed by mise (the tenant runtime manager) in the
  base image. Docs now describe the one-image reality.

## [0.15.12] - 2026-08-19

### Fixed

- **Dev preview could not be enabled on any stock deployment (#946, #948)**:
  the three Epic 66 instance settings
  (`devPreview.enabled` / `maxResponseBytes` / `maxConnsPerWorkspace`) were
  registered in `KnownKeys` only — unreachable through every consumer: no
  admin-UI section (the settings page is schema-driven), a boot-time
  `GetBool` index-miss whose error was swallowed (hard-disable regardless
  of DB state), and admin PUT rejected as unknown key. The keys are now
  registered in `InstanceSettings()` (SchemaVersion 11→12, canary twins in
  lockstep), the app.go read is extracted into
  `devPreviewConfigFromSettings` with warn + typed-default fallback, and
  the wiring is pinned by schema/service/app-wiring/HTTP-level tests
  (red→green verified).
- **lib/pq dropped (#945)**: GO-2026-6166..6173 have no fixed upstream
  release; the API no longer depends on lib/pq (pgarray migration).
- **Relay `/provider` hang (#927, #911)**: the response-header phase is
  now bounded on both relay-router and relay-proxy — an unresponsive
  upstream provider no longer hangs the endpoint indefinitely.
- **agentd live-validation findings (#926)**: overlay verification is
  pod-gated and startup logs are visible.
- **image-factory 409 name collisions + one-call default move (#950,
  #936)**: creating a config with a colliding name returns a typed 409
  instead of an unhandled error, default moves in one call, and seeding
  preserves the runtime default. Migration 000025 (single default).
- **CI flake (#920)**: agentd watchdog/healthz tests no longer time out
  under `-race -coverpkg`.

### Added

- **Epic 65 wire seam + ContextUsage contract (#938)**: adapter-mediated
  usage persistence — the platform's context/usage reading now flows
  through the `pkg/agent` adapter seam instead of opencode-shaped parsing
  in platform code.
- **Usage + metering decode migrated onto the wire seam (#949, #939)**:
  the SSE tracker's metering decode consumes the adapter contract;
  `MeteringFromEvent` log-before-err obligation documented on the
  interface.
- **Image-factory base-update (#928 v1+v2: #930, #931; ruling #929)**:
  read-side staleness signal (base-update pill in the launch picker) and
  the refresh flow; the base image is the version axis.
- **G7 stress harness (#924, #907)**: watchdog/restart/turn assertions
  under CPU storm.

## [0.15.11] - 2026-08-17

### Fixed

- **All-sessions 502 on chat (hotfix, #917)**: the per-prompt model
  selector was sent to opencode's POST /session/:id/message as the
  "providerID/modelID" STRING, but opencode 1.18.10's schema requires the
  OBJECT `{"modelID","providerID"}` — every prompt 400'd and surfaced as
  a 502 (reported as a CORS failure in the browser, since the error page
  carries no CORS headers). Regression from #909, where mocked tests
  asserted the string form with no real-schema validation. The adapter
  (V1 Send and V2 prompt) now sends the object; the test fake validates
  the body against opencode's schema so a string-form revert fails the
  suite structurally, and a full-pipeline e2e pins the object through
  handler → adapter → backend.

## [0.15.10] - 2026-08-17

### Fixed

- **MCP surface** (#880/#905): `credential_list` decoded the API's
  `{"secrets":[...]}` wrapper incorrectly — every MCP call failed; the
  `workspace_create` tool schema advertised `name` as optional while the
  API 422s on empty names.
- **SSE observability** (#901/#906): per-workspace tracker state
  (`connected` — initialized at arm time, deleted on stop —
  `reconnects_total`, `last_event_age_seconds`, `pod_ip_unavailable_total`),
  drop-accurate delivered-events counter, admin-auth-gated pprof with a
  self-contained mux, open/close lifecycle logs on both SSE endpoints
  (heartbeat frames excluded from event counts), `relay_free_models` in
  statusz with all terminal paths driving it, seven alert rules —
  including the incident-shaped `SSETrackerWatchesZero` and the
  replica-aware (`min by`) `SSETrackerWatchFailing` — every expression
  executed by promtool rule tests in CI. Dead SSE-subscription backoff
  reset fixed (healthy long-lived connections kept the maxed 30s retry).
- **SDK canary**: tools contract now a stable named subset + ≥24 floor
  with a cross-module parity guard, canary-module unit tests in CI, and
  `CANARY_NO_CONTROLLER=1` environment truth replacing phase inference.

### Added

- Observability for the event-delivery pipeline (#901, #902 follow-ups):
  per-workspace `llmsafespaces_sse_tracker_connected` /
  `..._reconnects_total` / `..._last_event_age_seconds` /
  `..._pod_ip_unavailable_total` gauges and counters;
  `llmsafespaces_sse_broker_delivered_events_total` (drop-accurate);
  admin-auth-gated `/api/v1/admin/debug/pprof/*` (self-contained mux);
  open/close lifecycle logs on both SSE endpoints;
  `relay_free_models` in agentd statusz (free-tier routing degradation
  visible); seven alert rules incl. SSETrackerWatchesZero (critical) and
  SSETrackerWatchFailing (armed-but-not-connected paging).
- Fixed dead SSE-subscription backoff reset: `connectAndRead` always
  returns non-nil, so healthy long-lived connections that ended kept the
  maxed 30s retry backoff forever.
- Fixed MCP `credential_list` for all clients (decoded the API's
  `{"secrets":[...]}` wrapper incorrectly → every call failed) and made
  the `workspace_create` tool schema require `name` (#880).
- SDK canary: tools contract now a stable named subset + ≥24 floor with
  cross-module parity guard (pkg/repolint) and canary-module unit tests
  in CI; `CANARY_NO_CONTROLLER=1` env truth replaces phase inference.

## [0.15.9] - 2026-08-16

### Fixed

- **SSE tracker watches never re-armed (#902)** — the halting-sessions
  bug: sends succeeded and turns ran to completion, but client streams
  received zero agent events whenever no API replica held an event
  watch for the workspace. Watches were armed only on phase
  *transitions* into Active; prior-phase state is Redis-backed and
  survives API restarts, so post-restart seeds skipped arming and
  watches that died later (workspace pod churn) had no re-arm path.
  Now: every Active event arms the tracker (idempotent; transitions
  still force a fresh connection), and a 60s reconciler re-arms watches
  for all Active workspaces on every replica — permanently converting
  event-blindness into at most one interval.
- Observability that was missing during the incident: tracker arm/stop
  at Info, tracker disconnects at Warn (was Debug — invisible),
  `llmsafespaces_sse_tracker_watched_workspaces` gauge per API replica
  (0 = that replica's user streams are event-blind). Full gap backlog:
  #901.

### Changed

- `buildAgentd` e2e test timeout 120s → 240s (cold-cache go build under
  `-race` on loaded CI runners exceeded it; spurious suite failures).

## [0.15.8] - 2026-08-16

### Fixed

- **Starvation-proof session truthfulness (#892, design 0050)** — the
  2026-08-15/16 repeated-session-halt incident chain, end to end:
  - **Watchdog demotion (D1)**: the health-watchdog's kill set is
    dead-listener-only — TCP dial refused AND supervised pid alive AND
    past the 180s boot grace. Starved (CPU advancing), flat (blocked on
    upstream I/O — alive), respawn-window (crash recovery owns it), and
    unknown evidence (probe degraded) all suppress forever, counted in
    `workspace_watchdog_suppressions_total{reason}`. No stand-down;
    unknown no longer fails open to a kill. The max-defer force path
    fires only on corroborated dead-listener evidence.
  - **Orphaned busy-flag reset (D2)**: sessions whose busy state was
    produced by a dead opencode generation — the phantom-busy
    "busy with no progress for 20-30 min" class — clear at every
    generation change; context meters survive. API-side reconcile fixed
    for >55-session workspaces (statusz decode cap 16 KB → 1 MB).
  - **Probe truthfulness (D4)**: `/v1/readyz` answers from agentd
    liveness + a kernel-level TCP check on opencode's port — never
    opencode responsiveness, and lock-free (atomic provider snapshot; a
    concurrent statusz fetch can no longer block it past probe
    timeouts). Probe budgets: readiness 5s/5s/×12, startup 5s/3s/×36
    (180s — covers starved boot-to-listen), liveness 10s/10s/×8 on the
    lock-free healthz.
  - **Elapsed-time badges (D5)**: running tools show a live, coarse
    elapsed time (history and live SSE paths, both opencode wire
    shapes) — live-silent ("42s") vs dead state ("3h") at a glance.
  - **Tool-parallelism caps (D7)**: `GOMAXPROCS` on workspace pods,
    derived from the effective CPU burst limit — build-tool children no
    longer oversubscribe the 2-CPU quota (the incident's CFS-stall
    source). No quota change.
  - Supervisor `stop()` race (hang on pod termination when a child
    crashed during backoff) fixed; restart-reason marker write failures
    now counted in `workspace_restart_marker_write_failures_total`
    (incident: 9 attempted writes, 0 landed, stdout-only).

### Changed

- healthz reports `commit_sha`/`build_time` (build identity; "unknown"
  when unstamped).

## [0.15.7] - 2026-08-15

### Changed

- **`AgentConfigInput` fully describes the agent config sources**. The
  admin system prompt and allowed external directories are now
  first-class `Apply` inputs (`AdminPrompt`, `AllowedDirs`) with the
  same pointer semantics as providers/model/relay/MCP — previously
  they were writer-construction options invisible to the seam contract,
  requiring a writer rebuild to revise. Construction still seeds them
  from the bootstrap side-car files; Apply updates thereafter; replace
  and clear are authoritative over prior renders.

### Fixed

- **Binding any MCP server crash-looped workspace boot.** The injection
  pipeline writes mcp-server metadata per MATERIALIZE-CONTRACT.md with
  native JSON types (`"args": [...]`, `"timeoutMs": 5000`), but the
  materializer's `Secret.Metadata` required `map[string]string` — the
  whole secrets.json parse aborted on the first bound server, taking
  every other secret down with it (Init:Error loop). `Secret` now
  decodes both shapes (complex values carried JSON-encoded as strings),
  and malformed metadata is reported per-entry instead of failing the
  file. Found on v0.15.6 the moment the platform opengist server got an
  auto-apply rule — the first MCP binding this cluster ever had.

## [0.15.6] - 2026-08-15

### Changed

- **Consistent build version injection across every component**. All Go
  binaries (api, controller, workspace-agentd, relay-router, relay-proxy)
  now read their build identity from `pkg/version` — the single source of
  truth — stamped via `-ldflags` in every image build (VERSION/COMMIT_SHA/
  BUILD_TIME). Previously the controller and workspace-agentd used their own
  `"dev"` fallbacks, the API Makefile targeted non-existent linker symbols,
  and release/CI never passed `VERSION`, so production images reported
  `"dev"` regardless of tag. Un-stamped local builds now report
  `"unknown"`. The base runtime healthz, the controller startup log, the
  API `/livez`/`/v1/admin/platform-info`, and the relay healthz
  endpoints all surface the injected semver for tagged releases.

### Fixed

- **Workspace pods could boot with no platform MCP server, system
  prompt, or `/tmp` external-dir approval**. Those blocks are rendered
  only by the agent-config writer's marshal path, and every write
  trigger at boot was conditional (pre-boot relay needs a free-models
  catalog; the relay injector needs a successful fetch; credential
  reload needs user action). When all skipped, opencode read the
  materialize base config `{$schema, provider, model}` and ran without
  the built-in `llmsafespaces` MCP server until the first credential
  reload. agentd now performs one unconditional empty-input `Apply`
  before starting opencode (`ensureBootAgentConfig`), stamping the MCP
  entry, admin prompt, and allowed dirs on every boot. Found while
  verifying the 2026-08-15 stale-image incident on v0.15.5.
- **Writer rebuilds no longer drop user-staged MCP servers** (found in
  review of the above): the config writer captured only
  provider/model/agent/mode from the existing config, so any rebuild
  from a writer with no staged MCP source (boot normalize, pre-boot
  relay, relay injector) silently deleted workspace-bound MCP servers
  written by materialize (Epic 53) until the next credential reload.
  The on-disk `mcp` section is now captured and re-emitted unless a
  staged source supersedes it.
- **Floating-tag default workspace image (`workspace.defaultImage`)**. The
  schema seeded `ghcr.io/lenaxia/llmsafespaces/base:latest` as the platform
  default. Floating tags resolve per-puller — registry mirrors (e.g. spegel)
  serve stale per-node digests while upstream has already moved the tag —
  so new workspaces silently launched week-old images (incident 2026-08-14:
  a v0.13.0 agentd missing the MCP 4098→4097 port fix was served for
  `latest` while upstream had published v0.15.5). Fixes, in depth:
  - Schema default is now empty — new workspaces fall through to the
    `base` RuntimeEnvironment, whose image tag is pinned by the Helm chart
    (`runtimeEnvironments.base.image.tag`, defaulting to the chart
    AppVersion).
  - `workspace.defaultImage` writes are validated: image refs must be
    pinned to an explicit non-mutable tag (`latest`, `main`, `master`,
    `dev`, `edge`, `nightly`, `stable`, `prod`, `current`, `release`
    rejected) or a digest; untagged refs are
    rejected (implicit `:latest`). RuntimeEnvironment names still pass.
  - The workspace-service read path skips stored floating-tag values with
    a warning, so values written before validation existed cannot launch.
  - Migration `000024` (renumbered from 000023 after #734 took that slot) removes the seeded `base:latest` row from existing
    deployments (admin-customized values are preserved).

## [0.15.5] - 2026-08-14

### Fixed

- **Swallowed adapter errors on prompt/delete paths (#817, #851)**.
  `SendMessage`, `SendPromptAsync`, and `DeleteSession` returned bare 502
  responses without logging the underlying adapter error — production
  failures (including the 125-second hangs under investigation in #817)
  were undiagnosable. All three paths now log `adapter failed` with the
  underlying error, matching every other adapter call site. Each branch
  has a regression test.

- **Five nil-request panic sites surfaced by Go 1.26 (#853)**. Go 1.26's
  stricter `net/url` parsing exposed a latent panic class: call sites that
  discarded the `http.NewRequestWithContext` error and called `Do(nil)`.
  Most severe: the workflows engine's PreserveOnFailure cleanup embedded a
  session ID from workspace-agent **output** into a URL — a control
  character would crash the API process (the routine runs in the scheduler,
  outside gin's recovery middleware; tenant-triggerable). Also fixed in
  bulk agent-reload, MCP session list/read, and agentd session cleanup.
  Dedicated regression tests pin every new error branch.

### Changed

- **Go toolchain 1.25.12 → 1.26.6 (#853)**. Fixes 7 Go standard-library
  CVEs (GO-2026-6218, -6091, -6090, -6089, -6088, -5972, -5026) that began
  failing govulncheck on every PR after the OSV database refresh. Verified
  red→green locally and on CI (race detector and short suites green on
  1.26.6). Pins updated across go.mod, 14 workflow pins, 6 Dockerfile base
  images; the runtime base's mise `go@latest` is now pinned. The
  CVE-2026-46600 `.trivyignore` entry is removed — obsolete under 1.26.6.

### Known issues

- #817 root cause (the 125-second prompt hangs) remains open — this
  release ships the logging needed to diagnose it on the next occurrence.
- #854: the tenant Go runtime image still pins Go 1.20.5 (follow-up).

## [0.15.4] - 2026-08-14

### Fixed

- **Snapshot bloat root cause — `git init` in workspace boot (#810, #807)**.
  Every workspace pod ran `git init` + `git config` during initialization,
  creating a `.git` directory on the PVC that grew unbounded. Combined with
  `filediff.Producer` (dead code that was never wired), this caused PVC
  snapshot bloat and Longhorn volume-attach timeouts. The `git init` is
  removed entirely; `buildWorkspaceDirsInit` now only runs `mkdir -p`.

- **Health watchdog for opencode hangs (#807)**. A previously-healthy
  opencode process that later hangs (deadlock, CPU starvation) was
  undetectable — the pod stayed 1/1 Running forever with no restart. The
  new health watchdog fires after 3 consecutive `/global/health` failures
  (post-boot), triggers a restart via the managed-process supervisor, and
  is rate-limited to 3 restarts per 10 minutes. Session-aware deferral
  avoids killing in-flight LLM turns (checks busy state before restarting,
  with a 5-minute max-defer cap). Restart reasons are recorded as markers,
  Prometheus metrics, and structured logs.

- **Structured 503 error responses (#810)**. Both `proxy.go` and
  `proxy_handlers.go` now return `code`/`reason`/`message` fields on 503
  responses. The frontend's `ChatHistoryErrorBanner` surfaces a yellow
  "recovering" state when `reason` is present. All three SDKs (Go, Python,
  TypeScript) raise `ServiceUnavailableError` with structured fields.

- **SSE `agent_died` message consumed (#810)**. The `message` field in
  `agent_died` SSE events was written to the wire but never read by the
  frontend. `ChatPage.tsx` now reads `event.data.message` into
  `agentDiedMessage` state and renders it in the banner.

### Changed

- CI workflows now configure git identity for the review bot (fixes
  "Author identity unknown" errors in pr-review, ai-comment, issue-opened,
  and renovate-analysis workflows).
- `runtimes/base/Dockerfile` pins Python to 3.12.13 (floating `python@3.12`
  resolved to 3.12.14 which has no precompiled binary in mise, breaking
  the from-source build).
- Makefile `release-tag` accepts semver pre-release suffixes (e.g.
  `0.15.4-rc.1`).

## [0.15.4-rc.1] - 2026-08-13

### Fixed

- **SSE tracker billing silent-zero on incomplete events (#751 F1c)**.
  `handleSessionUpdated` silently dropped events with empty model ID,
  zero output tokens, or empty session ID. All failure paths now emit
  warn logs so operators can detect billing drift.

- **SSE tracker cost-as-object silently zero (#751 F1a)**. When cost
  arrived as a JSON object instead of a plain number, the value was
  never extracted — billing stayed at zero. Now extracts the `cost`
  field from object shapes with a plain-number fallback.

- **SSE tracker reconnect race (#751 F3)**. `StopWatching` didn't wait
  for the subscribe goroutine to exit, allowing stale events to
  resurrect cleared billing state. Added `sync.WaitGroup` per workspace;
  `StopWatching` and `Stop` now block until goroutines drain.

- **`sessionStartTime` keying mismatch (#751 F2)**. The start-time map
  was keyed by bare session ID while cleanup used `workspaceID:` prefix
  matching — entries leaked forever. Re-keyed to composite
  `workspaceID:sessionID`.

- **Frontend cold-start busy detection (#752 F4)**. `seedBusy` checked
  `status === "active"` but the backend enum has no `"active"` value
  (`idle/busy/unknown/error/compacting/archived`). Now accepts `"busy"`.

- **Unknown SSE event types silently dropped (#752 F6)**. Both SSE
  handlers (ChatPage + SessionActivityProvider) now log unknown event
  types via `console.debug`, making version drift visible.

- **Wrong workspace evicted on max-active limit (#770)**.
  `enforceMaxActiveWorkspaces` sorted by DB `UpdatedAt` (bumped by any
  row mutation) instead of `LastActivityAt` (CRD annotation written by
  ActivityTracker on real user interaction). An actively-used workspace
  with old `UpdatedAt` could be auto-suspended while a stale one stayed
  running. Now sorts by `LastActivityAt` with `UpdatedAt` fallback for
  pre-US-23.3 workspaces.

- **window.confirm fail-open data loss (#775)**. Two call sites wrapped
  `window.confirm()` in try/catch where the catch block proceeded with
  deletion. In sandboxed iframes, `window.confirm` throws → session or
  workspace deleted without confirmation.

- **All confirm dialogs migrated to accessible ConfirmDialog (#814)**.
  All 14 `window.confirm` call sites replaced with the Radix Dialog-
  based `ConfirmDialog` component via the new `useConfirmDialog` hook.
  Dialogs now render in the DOM, working in sandboxed iframe contexts
  where `window.confirm` is blocked entirely.

- **CI review bot git identity (#813)**. The pr-review workflow failed
  with "Author identity unknown" because `persist-credentials: false`
  left no git identity. Added `git config` step to all 4 opencode
  workflows.

### Changed

- `fetchUserWorkspacePhases` refactored to `fetchUserWorkspaceStates`
  returning both phase and `LastActivityAt` from a single K8s API call
  (zero additional API calls).

## [0.15.3] - 2026-08-13

### Fixed

- **Sessions stuck "busy" after message — TRUE root cause (#805, #806)**.
  The SSE event scanners in both agentd and the API had a 64KB per-line
  buffer. opencode 1.18.10 emits `message.part.updated` events that
  exceed 300KB (patch parts listing thousands of changed files; observed
  370KB in production). When the scanner hit such a line, it failed
  silently and dropped the SSE connection. The tracker then missed the
  subsequent `session.status:idle` event and the session stayed "busy"
  forever. Both scanners raised to 16MB. Regression tests verified to
  fail on revert.

## [0.15.2] - 2026-08-12

### Fixed

- **Frontend crash on empty response bodies (#782)** — the frontend
  `client.ts` unconditionally called `res.json()` for any non-204 success.
  A 202/200 with empty body (e.g., the old prompt endpoint behavior)
  threw "Unexpected end of JSON input". Now reads `text()` first and
  returns `undefined` for empty bodies, matching the TypeScript SDK's
  defensive pattern.

- **Stale sessions accumulate in agentd tracker (#792 Pattern 2 S8)** —
  the `sessionStatusTracker` never pruned sessions from its in-memory
  maps during the 30s `fillGaps` cycle. Deleted sessions stayed forever,
  causing phantom busy counts and incorrect restart gating. The cycle
  now calls `prune(activeIDs)` at the top of each run.

## [0.15.1] - 2026-08-12

### Fixed

- **AbortSession silently broken on opencode 1.18.10 (SEV1)** — the V2
  interrupt endpoint (`POST /api/session/:id/interrupt`) was removed in
  opencode 1.18.10 (the entire `v2/` route group was deleted). On 1.18.10
  the V2 path returns 204 from a catch-all stub but does nothing —
  clicking "Stop" had no effect. Switched to V1 `POST /session/:id/abort`
  which actually stops the in-flight turn.

- **V1 Send response truncated at 4 MB** — same class of silent-
  truncation as #737. A single assistant turn with verbose tool output
  (>4 MB) would truncate, the JSON parse would fail, and the user's
  message would appear to vanish even though the LLM completed.
  Raised to 64 MB.

- **statusz 16 KB decode cap too small** — a workspace with 55+ sessions
  produces a statusz body >16 KB. The decode silently failed and the
  stuck-busy self-heal stopped working for heavy users. Raised to 1 MB.

### Root cause

The entire `v2/` route group directory was deleted from opencode 1.18.10
(`packages/opencode/src/server/routes/instance/httpapi/groups/v2/`).
Both V2 endpoints (`/api/session/:id/prompt` and `/api/session/:id/interrupt`)
are gone — they return 200/204 from a catch-all stub but execute nothing.
Confirmed via source diff of anomalyco/opencode v1.15.12 vs v1.18.10.

## [0.15.0] - 2026-08-12

### Summary

First production-ready release since v0.13.1. Bundles all v0.14.x hotfixes
(Epic 65 adapter migration fallout) plus the stuck-busy ground-truth fix.
Production was intentionally held at v0.13.1 through v0.14.x; this release
is the culmination of the full investigation, root-cause analysis, and fix
verification marathon documented in worklog 0744.

The minor version bump (0.14 → 0.15) is justified by the breaking
`SendPromptAsync` response change (202 → 200 with full assistant message
body) and the switch from V2 queue to V1 synchronous send.

### Fixed

- **Sessions stuck "busy" forever (#792, #795)** — the in-memory
  `activeSess` map retained stale "active" entries when the SSE stream
  dropped or the API pod restarted mid-turn, making sessions appear
  stuck busy indefinitely. `GetAuthoritativeActiveSessions` now queries
  the workspace pod's `/v1/statusz` for ground-truth busy/idle status
  and self-heals stale entries on every session-list request.

- **Dev-preview URL used relative origin (#793, #797)** — dev-preview
  links now use the absolute API origin instead of a relative path that
  broke when the frontend was served from a different host.

### Changed

- **`SendPromptAsync` now returns 200 with the assistant message body**
  (#755) — previously returned 202 (accepted) via V2 queue, which is
  admitted but never drained on opencode 1.18.10. Now uses synchronous
  V1 `POST /session/:id/message` and returns the completed assistant
  message as JSON.

### Testing

- **E2e regression coverage closed for three hotfix gaps (#800)** —
  worklog 0744's audit found that three of the four Epic 65 symptom
  fixes were marked "Code path verified" (non-evidence per Rule 0).
  Added three integration tests that exercise the full gin → handler →
  adapter → HTTP path and are verified to FAIL when the corresponding
  fix is reverted:
  - `TestE2E_Adapter_GetHistory_LargeBodyOver16MiB_No502` (#737)
  - `TestE2E_Adapter_SendPromptAsync_UsesV1SendNotV2Queue` (#755)
  - `TestE2E_Adapter_GetHistory_EmptySession_ReturnsArrayNotNull`

### Included from v0.14.x (previously unreleased to production)

All fixes from v0.14.3 through v0.14.6 are included. See the individual
changelog entries below for details. Key user-facing fixes:

- Messages disappear on send (#755) — V1 synchronous send
- GetHistory 502 on large sessions (#737) — streaming decoder
- Null history crash on new sessions — nil→[] guard
- Sessions stuck busy after LLM finishes — SSE watch on read paths
- opencode 1.18.10 wire-shape drift (#740, #743, #744, #748) — providerID,
  Agent, Status, flat tool parts, summary object, cost tokens
- SSE tracker billing drift + memory leak (#751)
- Secrets rotation restart mid-turn (#753)
- Reasoning content silently dropped (#750)

## [0.14.6] - 2026-08-12

### Fixed

- **GetHistory returns `null` for empty sessions — frontend crash** — when
  opencode returns no messages (new session), the adapter returned a nil
  slice which Go serializes as `null`. The frontend called `.filter()` on
  the response, crashing with "Cannot read properties of null". Fixed to
  return `[]`.

## [0.14.5] - 2026-08-12

### Fixed

- **Sessions stuck "busy" after LLM finishes (#755)** — adapter read
  paths (GetHistory, GetSession, ListSessions) never called
  `adapterEnsureSSEWatch`, so opening a busy session without sending a
  message never started the SSE tracker. The `session.status=idle`
  event was never received and the session appeared stuck busy forever.
  Fixed by adding SSE watch to all three read-path handlers.

## [0.14.4] - 2026-08-12

### Fixed

- **Messages disappear on send — Sev1 (#755, #757)** — `SendPromptAsync` always routed through V2 queue (`delivery:"queue"`) which is admitted but never drained on opencode 1.18.10. Every message from every workspace vanished. Switched to synchronous `adapter.Send` (V1 `POST /session/:id/message`) which works on all versions.

- **GetHistory 502 on large sessions — streaming decoder (#737, #738)** — the 16 MiB `readBody` cap truncated history bodies >16 MiB (94 MB sessions), causing JSON parse failure → 502. Replaced with streaming `json.Decoder` that has no body-size cap and returns partial results on truncation.

- **Reasoning content silently dropped (#750, #757)** — `translate.go` read `p.Reasoning` but opencode 1.18.10 puts reasoning text in `p.Text`. Added fallback.

- **Adapter 10s HTTP timeout broke sync Send (#746, #757)** — the hard `http.Client.Timeout: 10s` covered the entire exchange including body read. Removed; context deadline is the correct boundary.

- **CapDiff falsely advertised (#745, #757)** — `Capabilities()` unconditionally returned `CapDiff` even though filediff is never wired. Made conditional on `a.differ != nil`.

- **SSE tracker billing drift + memory leak (#751, #757)** — `handleSessionUpdated` parsed cost as `float64` (breaks on object shape), provider key only accepted `providerID` not legacy `provider`, and per-session billing maps never cleaned up on `StopWatching`. Fixed all three + data race on `sessionStartTime` mutex.

- **Secrets rotation restart mid-turn (#753, #757)** — agentd session tracker only stored `busy`/`idle`, treating `retry`/`error`/`compacting` as neither (stale idle). Restart decision then fired mid-turn. Fixed to treat all non-idle statuses as busy.

- **agentd parser drift (#747, #757)** — `fetchSessionPromptTokens` had silent zero-return paths with no logging and unbounded body read. Added debug logs + 16 MB `io.LimitReader` cap.

- **MCP server OOM risk (#749, #757)** — `mcpSessionList`/`mcpSessionRead` used unbounded `io.ReadAll` with no status check. Added 4 MB/16 MB caps + status-code validation. Fixed MCP functions to use `getAgentAddr()` for testability.

- **session_index channel drop (#754, #757)** — `RecordMessage` silently dropped events when channel full. Added nil-guarded warn log.

- **opencode 1.18.10 wire-shape drift (#743, #748)** — `providerID` dropped (custom UnmarshalJSON), `Agent` field missing, `Status` latent 502 (polymorphic decoder).

- **V2 bridge: spurious wake + TTL leak + nil-guard (#744, #748)** — busy-session guard in `wakeStrandedV2Sessions`, TTL pruning in `v2PendingSessions`, symmetric nil-guard.

- **ParseSessionWire/ParseSessionListWire 502 on 1.18.10 (#740, #741)** — `Summary` string→object, `Cost` object→bare number, `Time` field name drift. All fixed with `json.RawMessage` + custom unmarshalers.

- **Context usage backfill from CRD (#739, #741)** — `context_used` NULL after API pod restarts. Backfill from CRD status + observability.

- **Adapter cross-cutting regressions (#740, #741)** — all adapter handler paths now enforce workspace readiness, connection limits, session limits, metering, quota, activity tracking.

## [0.14.3] - 2026-08-11

### Fixed

- **Per-session context usage broken after API pod restarts (#739, #741)** —
  `session_index.context_used` stayed NULL for sessions whose last LLM step
  completed during an API pod restart. The SSE tracker reconnects but does
  not replay missed events. `ListWorkspaceSessions` now backfills NULL
  values from the workspace CRD status (which carries fresh per-session
  values from agentd) and persists them to the DB (self-healing). Also adds
  warn logs to `persistContextFromEvent`'s previously-silent no-op paths.

- **Epic 65 adapter cross-cutting regressions (#740, #741)** — every
  `if h.adapter != nil` handler branch bypassed `proxyToWorkspaceWithErrBody`,
  skipping workspace-readiness checks, connection limits, session limits,
  metering, quota enforcement, and activity tracking. Extracted 5
  cross-cutting helpers (`resolveWorkspaceForAdapter`,
  `checkAdapterSessionLimit`, `checkAdapterQuota`, `postAdapterSuccess`,
  `adapterEnsureSSEWatch`) and wired them into SendMessage, SendPromptAsync,
  GetHistory, GetSession, ListSessions, and CreateSession.

- **ParseSessionWire/ParseSessionListWire deterministic 502 on opencode
  1.18.10 (Finding O, #741)** — `ocSession.Summary` was typed as `string`
  but 1.18.10 returns an object; `ocSession.Cost` was `*ocCost` but 1.18.10
  returns a bare number; `ocTime` used `startedAt`/`completedAt` but 1.18.10
  uses `created`/`updated` (epoch ms). All three fixed with `json.RawMessage`
  and custom unmarshalers. Added `ocTokens` struct to extract session-level
  token data.

- **Read-path adapter error paths swallowed errors with no logging (Finding
  N, #741)** — CreateSession, ListSessions, GetSession now log via
  `h.logger.Error` before returning 502, matching the existing GetHistory
  pattern.

- **opencode 1.18.10 wire-shape drift: providerID, Agent, Status (#743,
  #748)** — (1) `ocModelRef.Provider` used JSON key `"provider"` but 1.18.10
  sends `"providerID"` — provider silently dropped for every session.
  Fixed with custom `UnmarshalJSON` accepting both keys. (2) `ocSession`
  had no `Agent` field — `session.AgentID` always empty. Added field +
  mapping. (3) `ocSession.Status` was a required value type — latent 502
  if opencode sends status as a bare string. Changed to `json.RawMessage`
  with polymorphic decoder.

- **V2 session-queue bridge: spurious wake + TTL leak + nil-guard (#744,
  #748)** — (1) `wakeStrandedV2Sessions` had no busy-session guard,
  creating concurrent turns on SSE reconnect. Now skips non-idle sessions.
  (2) In-memory `v2PendingSessions` had no TTL — lost events left entries
  forever. Added `lastAdded` timestamp + prune-on-read matching Redis's
  10-min TTL. (3) Legacy `enqueueV2` path lacked nil-guard on `v2Pending`.

- **Epoch-millis timestamp parsing in translator (#735)** — message
  timestamps from opencode were parsed incorrectly, causing incorrect
  message ordering in the frontend.

- **Dec 31 timestamps — `CreatedAt` JSON tag missing omitempty (#736)** —
  `session.Message.CreatedAt` serialized as `"0001-01-01T00:00:00Z"` when
  unset, causing frontend to show epoch-zero timestamps.

- **Nightly e2e: RBAC scope + Workspace CRD schema + session routes (#742)** —
  fixed e2e test infrastructure for controller RBAC, CRD schema validation,
  and session route mapping.

## [0.14.2] - 2026-08-11

### Changed

- **Session queue is now inboard/durable (US-63.7)** — the external Redis-
  backed message queue (`api/internal/services/msgqueue/`) and its incident-
  scarred workarounds (message-id hack, 409-requeue, destructive abort,
  stranded-queue sweep) have been deleted. The proxy now uses opencode's V2
  session API exclusively (`delivery:"queue"` prompt + non-destructive
  `interrupt`). The `LLMSAFESPACE_V2_SESSION_QUEUE` feature flag is removed;
  V2 is the only path.
  - **Abort is non-destructive:** queued messages survive an abort and drain
    on the next turn.
  - **Dismiss/revoke is removed:** the V2 API has no revoke on admitted-but-
    unpromoted rows. `DELETE /sessions/:sid/queue/:messageId` now removes
    from the shadow marker only (best-effort).
  - **`ForceAbortSession`** (Epic 44 admin escape hatch) is retained.

## [0.14.1] - 2026-08-11

### Fixed

- **Session history 502 on opencode 1.18.10 — flat-string tool shape (#730,
  #731)** — the 0.14.0 GetHistory typed parser declared `ocPart.tool` as a
  JSON object (`*ocTool`) but opencode 1.18.10 emits `"tool"` as a bare
  string (the tool name) with `callID`/`state`/`input`/`output` hoisted to
  the part level. Every `GET /api/v1/workspaces/{wid}/sessions/{sid}/message`
  returned **502 Bad Gateway** for any session containing a tool call —
  effectively every session in every workspace within minutes of creation
  (Sev1 outage, ~24 min, mitigated by rolling the API back to 0.13.1).

  **Root cause:** silent opencode wire-shape assumption drift (README §7).
  The parser was "validated from opencode 1.15.12 binary"; production pods
  run **1.18.10**. The Epic 65 translator tests built `ocPart` values with
  the legacy nested-object shape, so every test passed while production
  502'd. Same failure class as #707 and #486.

  **Fix 1 (parser):** a custom `UnmarshalJSON` on `ocPart` normalizes both
  wire shapes — flat-string (1.18.10+: `tool:"bash"` + part-level
  `callID`/`state.time.{start,end}`) and legacy nested (≤1.15.x) — into the
  canonical `ocTool`. The `session.ToolPart` contract is unchanged.

  **Fix 2 (resilience, README §12 containment):** `ParseHistoryWire` now
  decodes in two stages — `[]json.RawMessage` first (only fails on non-array
  bodies), then per-message. A future opencode schema change in one part
  degrades that single message to a `session.MessageSystem` notice instead
  of 502-ing the whole history. Operators get a signal via the adapter WARN
  log (`downgraded` count + session/workspace context).

  **Regression tests (TDD, golden fixtures as schema pins):**
  `testdata/history_1_18_10_flat_tool.json` (verbatim captured payload) and
  `testdata/history_1_15_12_nested_tool.json` guard both shapes. Six tests
  cover the verbatim shape, legacy non-regression, mixed shapes in one
  history, one-malformed-part no longer killing the page, totally-garbage
  still erroring, and all 9 tool names observed in production. Plus adapter
  integration and two handler e2e tests.

- **WorkspaceImagesTab re-fetch loop (#731)** — `load` had `baseName` in
  its `useCallback` deps but `load` itself sets `baseName` on first run,
  creating a re-fetch loop (`load → setBaseName → load recreated → useEffect
  → load → setLoading(true)`). The component flickered back to the spinner
  after initial load, racing the scope-routing test. Fixed with a `useRef`
  guard so `load` has a stable identity and `useEffect` fires once.

- **Playwright e2e mock shape drift (#731)** — after Epic 65 the GetHistory
  endpoint returns the contract shape (`type`/`createdAt`/`parts`), not
  opencode's raw shape (`info.role`/`time.created`). The e2e mock helpers
  in `composer.spec.ts` and `session-activity.spec.ts` were never updated,
  so `transformHistory` filtered out every message and 7 Playwright tests
  failed on main. Updated mocks to emit contract shape.

- **TypeScript SDK — `baseUrl` visibility (TS2341, #731)** —
  `WorkspacesAPI.devPreviewUrl()` accesses `this.client.baseUrl`, but
  `baseUrl` was declared `private`. The SDK Contract Tests CI job was
  masked by the failing frontend-test dependency; fixing the frontend
  unblocked it and exposed the bug. Changed to `readonly` (package-visible).

- **SDK canary `expectedSchemaVersion` drift (8/4 → 10, #731)** —
  `pkg/settings/schema.go` bumped `SchemaVersion` to 10 but the Go/TS/Python
  SDK canary tests expected 8/4 respectively. The schema-version assertion is
  a SCHEMA DRIFT DETECTOR by design — it caught the gap. Aligned all three
  SDK canaries with the current version.

- **SDK canary login rate-limit exhaustion (#731)** — the `/auth/login`
  endpoint has a per-route rate limit of 10/min (burst 10). Each SDK canary
  suite calls `jwtLogin` per scenario, exhausting the bucket and causing
  spurious 429 failures in cred-crud and other login-bearing scenarios. The
  Go section already had a `sleep 65` before S-RATE-LIMIT; added matching
  waits before the Python and TypeScript sections and a mid-Go wait before
  S-CRED-CRUD.

## [0.14.0] - 2026-08-11

### Added

- **Workspace dev preview — authenticated HTTP/WS tunnel to in-workspace dev
  servers (#725)** — users can now view web applications (Vite, Next.js,
  webpack-dev-server, Playwright, ad-hoc HTTP servers) running inside their
  workspace pod from a browser, with full hot-module-replacement (HMR)
  support. The tunnel is authenticated to the workspace owner only (via the
  existing `lsp_session` cookie or Bearer header) and reuses the existing
  API→pod:4097 NetworkPolicy allowance — zero new ingress rules, no
  per-workspace Service or Ingress objects, no publicly shareable URL.

  **Architecture:** browser → API (`AuthMiddleware` +
  `WorkspaceAccessMiddleware`) → `DevPreviewHandler`
  (`httputil.ReverseProxy`, G34 header allowlist, response size cap,
  connection cap) → agentd:4097 (`devPreviewHandler`, port denylist, Host
  rewrite for CVE-2025-30208) → `localhost:<port>`.

  **Opt-in:** toggle via `PUT /workspaces/:id/dev-preview` or the workspace
  settings drawer. Operator kill-switch via `devPreview.enabled` instance
  setting. Configurable response size cap (default 50 MiB) and per-workspace
  connection cap (default 50).

  **MCP tool:** the in-workspace agent can call `dev_preview_url` to
  construct a preview link for the user, with enable instructions included.
  Fixed a pre-existing bug where the MCP server was injected on the wrong
  port (4098 admin → 4097 user mux), which had prevented all in-workspace
  MCP tools from being reachable.

  **SDKs:** `setDevPreview` + `devPreviewUrl` helpers in all four SDKs
  (TypeScript, Python, Go, Java). OpenAPI spec updated with both routes.

  **Security:** G34 header allowlist applied (caller Cookie/Origin/Referer
  stripped before forwarding to pod). Agentd strips Basic auth before
  forwarding to the dev server. Port denylist (4096/4097/4098 + privileged
  ports <1024) enforced at both API and agentd layers.

### Fixed

- **MCP injection port mismatch (pre-existing)** —
  `injectAgentdMCPServer` injected the MCP server URL as
  `http://127.0.0.1:4098/v1/mcp` (admin port), but `mcpHandler` is
  registered on the user mux (4097). Fixed to `AgentdPort` (4097). This
  had prevented `session_list`, `session_read`, and the new
  `dev_preview_url` tools from being reachable by the in-workspace
  opencode agent.

## [0.13.1] - 2026-08-10

### Fixed

- **Session origin recording for routine triggers (#706)** — the
  `session_origins` table, `RecordSessionOrigin` store method, and sidebar
  badge UI were built during Epic 64 but never wired into the engine.
  `executeRoutine` now calls `RecordSessionOrigin` after success, so
  routine-created sessions show the purple "routine" badge in the sidebar
  and link back to their trigger. Session deletion for `PreserveOnFailure`
  now only suppresses origin recording when the DELETE actually succeeds.
- **Create forms moved to detail pane (#705)** — trigger and workflow
  create forms now open in the detail pane instead of a modal, matching
  the edit flow.

## [0.13.0] - 2026-08-10

### Added

- **Session contract + V2 session queue (#695)** — platform-owned session
  contract (US-65.2/65.6) and V2 session queue (US-63.1→63.4). Decouples
  the platform from opencode via a typed session model behind a single
  adapter seam.
- **SSE bridge + stranded-input recovery (#698)** — US-63.5 SSE bridge
  for reliable streaming and US-63.9 stranded-input recovery for sessions
  interrupted during queue drain.
- **Fresh-load queue visibility (#701)** — Redis-backed shadow marker for
  V2 queue pills on fresh page load.

### Fixed

- **Trigger concurrent-fire race (#700)** — `ListDueCronTriggers` used a
  plain SELECT with no row locking; every API replica's scheduler tick
  fired the same due trigger concurrently, inflating the failure counter
  past `auto_disable_after`. Replaced with `ClaimDueCronTriggers` using
  `FOR UPDATE SKIP LOCKED` + atomic timestamp advance.
- **Trigger counter not reset on re-enable (#700)** — `UpdateTrigger`
  now resets `consecutive_failures` to 0 on the `enabled=false→true`
  transition via a CASE clause.
- **Workspace re-suspend during activation (#696)** — `K8sWorkspaceActivator`
  now refreshes `last-activity-at` when patching `spec.suspend=false`.
- **Stuck-Creating escape hatch + FailedMount auto-recovery (#702)** —
  `spec.suspend=true` now honored in Pending + Creating phases.

## [0.12.1] - 2026-08-09

### Fixed

- **Mobile-responsive drill-down navigation** — workflows and triggers pages
  now show one pane at a time on mobile (list → tap → detail with back button).
  Desktop layout unchanged.
- **Trigger editor routine parity** — full editing support for routine-mode
  triggers (prompt, memory, capture, preserve session).
- **Cron fire/run IDs must be UUIDs** — cron trigger fires were generating
  non-UUID string IDs, causing silent DB insert failures. All IDs now use
  `uuid.New().String()`.

## [0.12.0] - 2026-08-09

### Fixed — Completing items 1-7

- **PreserveOnFailure type comment corrected** — now accurately describes
  the engine calling `DELETE /v1/workflow/session/delete` on success.
- **Session origin API endpoint** — `GET /workspaces/:id/session-origins`
  returns origin mappings for sidebar enrichment. `SessionListItem.Origin`
  field added to the session list response type.
- **agentd MCP server tests** — 10 tests covering initialize, tools/list,
  tools/call (unknown tool, missing args), injection (empty + existing MCP).
- **Active runs indicator on chat page** — shows a pulsing badge with count
  of in-flight workflow runs when the workspace has active runs.
- **Wake-up latency in run detail** — shows the time between run creation
  and run start (wake-up cost) as a yellow indicator.
- **Integration tests run in normal CI** — removed `//go:build integration`
  tag from `store_integration_test.go`. Tests compile in every CI run
  (catching SQL drift) and skip gracefully when PG is unavailable.

## [0.11.0] - 2026-08-09

### Added — Remaining Epic 64 items

- **agentd built-in MCP server** — serves `session_list` and `session_read`
  tools via MCP JSON-RPC protocol at `/v1/mcp`. Injected as a "remote" MCP
  entry in agent-config.json so the in-workspace opencode agent discovers
  it natively. Tools call opencode's local API — no platform credentials
  needed. Enables the agent to read previous session history for
  cross-run continuity (Flow 3 roll-forward).

- **`PreserveOnFailure` session deletion** — the engine now calls
  `DELETE /v1/workflow/session/delete` on agentd when a routine succeeds
  with `preserveSession: "on_failure"`. The session is preserved on
  failure (for investigation) and deleted on success (clean workspace).
  New agentd endpoint: `DELETE /v1/workflow/session/delete?sessionId=X`.

- **Full cron expression support** — replaced the hand-rolled 4-pattern
  parser with `github.com/robfig/cron/v3`. All standard cron expressions
  now work: `30 9,14 * * *`, `0 0 * * 0`, `*/15 8-18 * * 1-5`, etc.

- **Session origin tracking** — migration 000022 adds `session_origins`
  table linking opencode sessions to their creating trigger/workflow.
  Store methods: `RecordSessionOrigin`, `ListSessionOrigins`. (API
  enrichment + frontend icon rendering: next step.)

- **Active runs by workspace** — `GET /api/v1/workspaces/:id/runs/active`
  returns non-terminal runs for a workspace. For the run-active-on-
  workspace indicator.

- **`ListPendingRoutineFires`** integration test — verifies the SQL-level
  webhook-routine flow (fire created → pending → processed → delivered).

- **`GetLastRoutineResult`** integration tests — verifies the memory query
  returns results from 'delivered' fires (not 'fired').

- **Design docs** — `ENGINE-MIGRATION-DESIGN.md` (API→controller migration
  plan, 3-5 day estimate) and `SESSION-OWNERSHIP-DESIGN.md` (unified
  session ownership, 2-3 week estimate).

### Fixed

- **`computeNextFire_Hourly` test corrected** — the old parser was wrong
  (`0 * * * *` at 12:30 returned 13:30 instead of 13:00). robfix/cron
  gives the correct result.

## [0.10.0] - 2026-08-09

### Changed — Breaking: Triggers as Routines

This release redesigns the trigger model. Triggers are no longer limited
to firing DAG workflows or scripts — they directly embody agent routines
(scheduled agent turns with memory, capture, and session lifecycle).

**Breaking changes:**
- `target_type` and `target_config` columns dropped from `triggers` table
  (migration 000020). Replaced with explicit routine fields: `workspace_id`,
  `prompt`, `agent`, `script_path/args/env`, `memory_mode`, `capture_mode`,
  `preserve_session`, `workflow_id`.
- Trigger API: `POST /me/triggers` no longer accepts `targetType` or
  `targetConfig`. Use `workflowId` for DAG mode, or `workspaceId + prompt`
  for routine mode.
- `run_script` target type eliminated. Use a routine trigger with
  `scriptPath` + `prompt` instead.
- Synthetic workflow rows for `run_script` triggers eliminated.
  `BuildRunScriptSpec` and `GetOrCreateScriptWorkflow` deleted.
- SDK trigger create/update signatures changed (all 4 SDKs).
- MCP `trigger_create` tool signature changed.

### Added — Routine execution

- **Routine executor** in the scheduler: activates workspace, renders prompt
  with `{{.prevResult}}` memory, executes optional script, sends prompt to
  opencode agent, captures result per capture policy, deletes or preserves
  session per preserve policy.
- **Memory modes**: `none` (every run independent) or `last_result` (injects
  previous successful result into prompt template). Enables roll-forward
  state for hourly/daily routines without requiring session continuity.
- **Capture modes**: `errors_only` (default — only stores on failure) or
  `full` (always stores the agent response).
- **Session preservation**: `never` (delete after capture), `always` (keep
  session in sidebar), `on_failure` (keep only on error).
- **Migration 000020**: trigger routine fields + backfill from old
  target_type/target_config.
- **Migration 000021**: `trigger_fires.result` column for memory +
  observability. `session_origins` table for session origin tracking.
- **Scheduler dependencies**: scheduler now holds `WorkspaceActivator` and
  `AgentdExecutor` for routine execution.
- **Frontend**: trigger create form rebuilt with routine/workflow mode
  toggle, workspace picker, prompt editor, memory/capture/preserve controls.
- **Design doc**: `design/stories/epic-64-triggers-workflows/ROUTINES-REDESIGN.md`.

### Fixed

- **`run_script` now actually executes.** Previously it just logged
  "delivered" and did nothing. Replaced entirely by the routine executor.
- **Ephemeral session deletion** in agentd `execAgentNode`: sessions marked
  ephemeral are now deleted after response capture (`DELETE /session/:id`).
- **Full transcript capture** in agentd `execAgentNode`: all parts (text +
  tool-use) are captured, not just concatenated text.

## [0.9.1] - 2026-08-08

### Added — Epic 64: Triggers & Workflows UX v2

- **Visual DAG editor** — `@xyflow/react` canvas with typed node palette
  (script/agent/http/condition), drag-to-connect edges, condition-branch
  handles, minimap. Per-node typed edit panels replace raw JSON editing.
  Visual/JSON toggle.

- **Run detail page** — per-node timeline with status icons, attempt counts,
  branch labels, durations. Expandable input/output/error per node. Cancel
  button. Auto-polls while running. Route: `/workflows/:wfId/runs/:runId`.

- **`onMissingWorkspace` policy** — migration `000019`. When a workflow's
  target workspace is gone: `abort` (default, fails run fast) or `create`
  (auto-provisions a new workspace for the owner, pins it as target, waits
  for Active). Includes `WorkspaceCreator` engine interface + app.go wiring.

- **Trigger UX overhaul** — cron frequency builder (every N min/hours, daily,
  weekdays, timezone) with raw-expression toggle. Webhook create flow with
  one-time secret reveal, URL copy, signing example, IP allowlist, idempotency
  mode. Circuit-breaker progress bar + auto-disabled badge + re-enable.

- **Trigger editor** — editable schedule (inline edit with friendly/raw
  toggle), swap target workflow, edit input template. Full update support.

- **`run_script` target** — trigger create form supports both `run_workflow`
  and `run_script` targets (workspace + path + args + env).

- **Workspace picker** — workflow editor has a workspace dropdown and an
  "If missing: abort/create" toggle. Run dialog includes workspace picker
  when no default target set.

- **inputSchema + defaults editors** — Advanced section in workflow editor
  for JSON Schema input validation and node defaults block.

- **Delivery log** — `GET /me/triggers/:id/fires` endpoint. Live-polling
  delivery log panel with expandable rows (envelope + action result).

- **Webhook secret rotation** — `POST /me/triggers/:id/rotate-secret`
  endpoint. One-time reveal of new secret with copy button.

### Fixed

- **Webhook rate limiting wired** — the handler comment claimed rate limiting
  but the code didn't do it. Now calls `RateLimiterService.Allow` per-webhook
  with 429+Retry-After.

- **Hash idempotency implemented** — `computeHashDedupKey(body, ts)` derives
  dedup key from `sha256(body + 5min-window)`. Was a stub returning empty.

## [0.9.0] - 2026-08-08

### Added — Epic 64: Triggers & Workflows

- **Workflow engine** (#655–#686). Deterministic DAG-structured pipelines
  running inside workspace pods, fired by cron/webhook triggers. The engine
  runs in the API server as background goroutines (reconciler + scheduler),
  not the controller — preserving the architectural boundary (controller →
  K8s only, API → PostgreSQL + K8s + HTTP).

- **4 node types**: `script` (Python/Node inline handlers via mise),
  `agent` (named opencode agent with structured output), `http` (outbound
  HTTP through workspace egress), `condition` (expr-lang branch routing).

- **Trigger sources**: `cron` (5-field expression with timezone support)
  and `webhook` (HMAC-SHA256 verified, IP allowlist, idempotency dedup).

- **Single-in-flight enforcement**: partial unique index on
  `workflow_runs(workflow_id) WHERE status IN ('queued','running')` —
  atomically prevents concurrent runs without leader election.

- **Circuit breaker**: triggers auto-disable after N consecutive failures
  (default 10). Missed cron fires are logged as 'skipped', not silent.

- **DAG validator**: 9-pass validation at create/update — cycles, dangling
  edges, unreachable nodes, condition branch coverage, expr-lang type-check.

- **Migration 000016**: 7 tables (workflows, triggers, webhooks,
  webhook_deliveries, workflow_runs, workflow_node_runs, trigger_fires).

- **11 MCP tools**: workflow/trigger CRUD for external agents (Claude
  Desktop, Cursor). Registered on the platform MCP server.

- **4 SDKs**: Go, TypeScript, Python, Java — all have workflow + trigger
  methods with wire-format tests.

- **Frontend**: sidebar entries (Workflow/Zap icons) + two-column layout
  pages for workflow and trigger management. YAML editor with server-side
  validation.

- **7 Prometheus metrics**: run duration, node duration, concurrent runs,
  trigger fires, webhook deliveries, scheduler tick.

### Fixed

- **agentd: loadAllowedDirs append bug (#687).** `loadAllowedDirs` appended
  to `allowedDirs` instead of replacing, causing 2 tests to fail in any real
  workspace. Fixed: reset before each load.

- **agentd port mismatch (#686).** Controller's `HTTPAgentdExecutor` used
  port 4098 (admin) instead of 4097 (user). Fixed by moving the engine to
  the API server.

## [0.8.13] - 2026-08-07

### Fixed

- **Image factory: consistent scope sections + member configs read-only
  (#680).** Org and platform admin tabs now show consistent section
  headings ("Org Images" / "Platform Images" / "Member Images"). Member
  configs are always read-only from org/platform tabs. Cross-scope
  configs shown in separate sections with no edit buttons.
- **Image factory: audit fixes (#683).** Delete confirmation now shows
  the config name instead of the hash. Base selector correctly tracks
  and sends the version to the create API. Backend doc comment corrected
  for `canMutateScope` cross-org limitation.

## [0.8.12] - 2026-08-07

### Added

- **Image factory: build row billing attribution (#679).** Added `scope`
  and `org_id` columns to `image_factory_builds` (migration 000018).
  Org-scoped builds are attributed to the org; platform-scoped builds to
  the platform owner. Backward compatible (nullable + backfill from
  config scope/org_id). Two partial indexes for billing queries.

## [0.8.11] - 2026-08-07

### Added

- **Image factory: org/platform-scoped config creation (#664, #667).** Org
  admins and platform admins can now pre-build images at their scope.
  New routes: `POST /orgs/:id/image-factory/configs` (OrgAdminGuard),
  `POST /admin/image-factory/configs` (AdminGuard). Cross-scope coalescing
  (same selection = one shared build). Extended delete/rename ownership
  (org admin status check, platform admin bypass). Org admin Images tab +
  platform admin Image Factory tab in the frontend.
- **Image factory: `allowed_image_configs` restriction policy (#668).** Org
  admins can restrict which org/platform images members can launch. Empty
  = unrestricted (default). Member configs always exempt. Enforcement at
  API (`resolveImageFactoryConfig`) as a backstop. Migration 000017.
- **Epic 64: Triggers & Workflows (#655, #656, #657, #663, #665, #666).**
  Data model, storage layer, DAG spec validator, workflow CRUD handlers,
  trigger CRUD + webhook secret crypto, and design definition.

### Fixed

- **Mobile settings tab horizontal scroll (#658).** Tab items lacked
  `shrink-0` and `overflow-x-auto` was on the wrong element, making
  "Workspace Images" unreachable without rotating to landscape.

## [0.8.10] - 2026-08-05

### Added

- **Org admin Settings tab — surface 6 backend-only policies (#653).** New
  `/orgs/:id/settings` tab with three cards: Workspace Limits
  (`max_workspaces_per_member`, `max_active_workspaces_per_member`), Model &
  Provider Restrictions (`allowed_models`, `allowed_providers`), and MCP &
  Image Defaults (`max_mcp_servers_per_workspace`, `default_runtime`).
  Previously these 6 policies were backend-only since Epic 43 with no UI.
- **E2e coverage for org-admin portal (#654).** 9 Playwright tests covering
  deep-linking, sidebar navigation, role gating, save flow, and unhappy
  paths (org load 404, save 403). Closes the codebase-wide e2e gap for the
  org-admin portal.

## [0.8.9] - 2026-08-05

### Fixed

- **Image-list dropdown: viewport-aware positioning (#652).** The
  `NewWorkspaceSplitButton` popup was hardcoded to `absolute right-0
  top-full` and overflowed the viewport edge when the button sat near the
  right or bottom of the screen. Now portals to `document.body` via
  `createPortal` and positions via `computeMenuPosition` (reused from
  `KebabMenu`): flips above when no room below, clamps horizontally, caps
  height with scroll. Repositions on scroll/resize.

## [0.8.8] - 2026-08-05

### Fixed

- **MCP org-tab: routing, list-envelope crash, member-policy toggle (#650).**
  Three org-admin MCP bugs: (1) org-level add was rejected with "org admin
  has disabled member MCP servers" because the router mounted the org tab
  without passing `orgId`, causing every org-scope request to fall through
  to the user endpoint; (2) `n.map is not a function` crash because the API
  client typed the `{servers:[...]}` envelope as a bare array; (3) no UI for
  org admins to manage member MCP servers (`allow_user_mcp_servers` policy
  was backend-wired but had no frontend toggle). Also fixes a latent
  `api.put`/`api.post` falsy-body drop (silently dropped `false`/`0`).
- **MCP deferred-wiring regression gaps (#651).** Closed two gaps from the
  v0.7.1 production 500 fix: admin audit events were silently dropped
  (nil-wired `SetAudit`) with no regression test; `resolveWorkspaceQuota`
  lacked the same nil-orgChecker guard `UserCreate` got. Both now have
  red→green regression tests.

### Added

- **Image Factory: delete + rename config API and UI (#649).** Member-scope
  `DELETE /configs/:hash` (rejects building status) and
  `PATCH /configs/:hash` (rename with collision detection). Shared
  `resolveConfigByHash` (member → org → platform scope loop).

## [0.8.7] - 2026-08-04

### Fixed

- **PWA: autoUpdate (#648).** Switched service worker from `prompt` to
  `autoUpdate` to prevent stale-chunk 404 errors after deploys (the admin
  portal `PlatformAdminLayout` 404).
- **Dark mode dropdowns (#648).** Native `<option>` elements now inherit
  dark-mode background/foreground via global CSS.
- **Image factory UI pills (#648).** Split-button popup shows Ready/Building
  status pills. Workspace Images tab shows scope pills (Platform/Org/Personal)
  and an expandable drawer with extension chips. Status pills are dark-mode-safe.

### Added

- **Catalog: R + Julia (#648).** Added `r-base`, `r-devtools` (CRAN build
  dependencies), and Julia LTS to the image factory catalog.
- **Preferences: preferredRuntime dropdown (#648).** The Default Image user
  setting now renders as a dropdown of Ready configs instead of freeform text.

## [0.8.6] - 2026-08-04

### Added

- **Workspace: split-button launch + default-image hierarchy (#642).** The
  new-workspace button is now a segmented control: `[+]` launches the default
  image in one click, `[▼]` opens a popup menu of Ready image-factory configs.
  Default image is resolved via a 4-tier hierarchy: user preference
  (`preferredRuntime`) → org policy (`default_runtime`) → platform setting
  (`workspace.defaultImage`) → `"base"`. Org admins set the default via the
  existing policy API (`PUT /orgs/:id/policies/default_runtime`).

## [0.8.5] - 2026-08-03

### Added

- **Image Factory: workspace launch integration (#641).** Workspace creation
  now supports selecting an image-factory config. The new-workspace dialog
  shows a config picker with status pills (Ready=selectable, Building/Rejected=
  disabled). The API resolves the selected config to its built image ref and
  sets it as the workspace runtime — leveraging the controller's existing
  image-ref passthrough, so no controller or CRD change is needed.

## [0.8.4] - 2026-08-03

### Fixed

- **Image Factory: accept 204 from GitHub workflow_dispatch (#640).** GitHub's
  `workflow_dispatch` endpoint returns 204 No Content on success, not 201 Created.
  The dispatcher only accepted 201, so every successful dispatch was treated as a
  failure → 503 → DB rollback. This was the second of two compounding bugs behind
  the 2026-08-03 outage (the first was the missing `Actions: Write` permission,
  fixed by granting it; #639's logging made this one visible). Accept both 201
  and 204 as success.

## [0.8.3] - 2026-08-03

### Fixed

- **Image Factory: dispatch error logging (#639).** `POST /image-factory/configs`
  returned a generic `503 "failed to dispatch build"` with the underlying error
  discarded, making the 2026-08-03 outage (GitHub App missing `Actions: Write`
  permission → `403 "Resource not accessible by integration"`) look like a
  wiring/version problem for hours. The dispatch-failure path now logs the
  dispatcher's wrapped error via the handler logger before returning 503.
  Adds `SetLogger`/`HasLogger` on `ImageFactoryHandler` (mirrors the
  `pod_bootstrap.go` precedent for swallowed-error observability gaps) plus a
  handler-level regression test and an app-level wiring guard.

## [0.8.2] - 2026-08-03

### Fixed

- **Image Factory: null knownFailures crash (#638).** Go serializes nil
  slices as JSON null; the catalog endpoint returned "knownFailures":null
  which crashed the frontend (catalog.knownFailures.some on null). Fixed:
  API returns [] not null; frontend has a defensive guard.

### Added

- **Image Factory: redesigned catalog — 30 extensions in 3 groups (#638).**
  Language Packs (Python/Node/Go/Rust/Java/.NET/Ruby/PHP via mise), System
  Packages (Playwright, Chromium, TeX Live, Tesseract, Graphviz, etc via apt),
  Files (MOTD). Frontend groups extensions by type.

## [0.8.1] - 2026-08-03

### Changed

- **Image Factory: GitHub App authentication (#637).** Switches from PAT
  to GitHub App (`llmsafespaces-builder`, App ID 4470040). The dispatcher
  mints a JWT from the App's private key, exchanges it for an installation
  token (cached 50min, mutex-protected), and uses it for workflow_dispatch.
  The workflow uses `actions/create-github-app-token` to push packages to
  `ghcr.io/lenaxia/llmsafespaces-images/ws`. Config: `appId` + `privateKey`
  replace `apiToken`. Helm chart: `appCredentials.secretName` replaces
  `secretKeyRef`. 7 dispatcher tests (happy + error paths + caching).

## [0.8.0] - 2026-08-02

### Added

- **Image Factory — custom workspace images (#616, #619, #624, #628, #629,
  #631, #634).** Talos-style image factory enabling users to self-serve
  workspace images with system-level dependencies that mise/pip/npm/go
  cannot supply. Users select extensions from an operator-curated catalog;
  the API renders a deterministic Dockerfile and dispatches a GitHub Actions
  build; the workflow builds multi-arch, pushes to ghcr.io, and calls back
  with the result.

  **Design:** `design/0046` (28 decisions) + `design/0047` (contracts).

  **Architecture:**
  - **Immutable extensions** (design #7): catalog entries are publish-new +
    retire, never edited in place. This makes content-addressing and the
    failure blocklist simple by construction.
  - **Eager build on save** (design #12): configs are only launchable once
    Ready → the workspace controller stays genuinely untouched.
  - **Dispatch-before-commit** (design #17): GH Actions dispatch happens
    before the config row commits; on dispatch failure the row is never
    created.
  - **Build coalescing** (design #16): duplicate in-flight/succeeded builds
    are linked, not re-dispatched.
  - **Per-build callback token** (design #18): `subtle.ConstantTimeCompare`
    on the callback endpoint — the only path an external runner can mutate
    build state.
  - **Atomic transitions** (design): `TransitionBuildSucceeded`/`Failed`
    do all writes in a single DB transaction.

  **Components:**
  - Migration `000013`: 6 tables (platform_config, bases, extensions,
    known_failures, configs, builds).
  - `api/internal/imagefactory/`: pure logic — `HashSelection` (content-
    addressed `s-<sha256[:16]>`), `ResolveSelection`, `RenderDockerfile`
    (deterministic), `ValidateResolved`, `LoadSeed`/`SeedCatalog` (embedded
    YAML with 9 initial extensions).
  - `api/internal/handlers/imagefactory*.go`: consumer endpoints (catalog,
    configs, callback), admin endpoints (bases/extensions/known-failures
    CRUD), `ghActionsDispatcher` (production GitHub Actions API client),
    `llmExplainer` (OpenAI-compatible failure explanation with degradation).
  - `.github/workflows/image-build.yml`: the build workflow — receives a
    pre-rendered Dockerfile, runs `docker buildx build`, pushes to ghcr.io,
    POSTs the result via authenticated callback.
  - Frontend: `WorkspaceImagesTab.tsx` settings page with status pills
    (building/ready/rejected), extension checkboxes, base selector,
    create-and-build form. API client `imageFactory.ts`.
  - Helm chart: `imageFactory.*` values (imageRepo, callbackURL,
    ghDispatcher with secretKeyRef for PAT, llmExplainer, architectures).
  - ConfigMap + deployment templates for secret-based credential delivery.

  **Tests:** 60+ Go tests (unit, sqlmock store, postgres integration,
  handler-level e2e round-trips, dispatcher, seed, callback security,
  AdminGuard), 9 vitest frontend tests. All `-race` clean.

  **6 design docs** (`design/0046` + `design/0047`) capture the full
  decision history including 3 stress-test passes.

### Added

- **Image Factory S6 — LLM failure explainer (#624).** Wires the platform's
  in-cluster LLM (LiteLLM/vLLM) into the image-build failure-explainer
  interface defined in S5, producing human-readable explanations of build
  failures with graceful degradation when the LLM is unavailable.
- **Image Factory S7 — Admin portal endpoints (#628).** Platform-owner admin
  CRUD endpoints for the image factory catalog: bases, extensions,
  known-failures, and platform config (architectures).
- **Image Factory S8 — GH Actions image-build workflow (#629).** The
  `.github/workflows/image-build.yml` triggered by `workflow_dispatch` from
  the API's `POST /configs` handler; the API pre-renders the Dockerfile and
  the workflow builds and pushes the custom workspace image.

### Changed

- **Repolint gate: gin.SetMode under t.Parallel (#630).** Adds a repo lint
  check that fails if a `*_test.go` file calls `gin.SetMode` from a function
  reachable by a `t.Parallel()` test body. Prevents recurrence of the
  data race that blocked the v0.7.1 release gate. Detection is per-function
  transitive reachability; serial-only calls remain allowed.

## [0.7.1] - 2026-08-02

### Fixed

- **Nil orgChecker panic on POST /me/mcp-servers (#622).** The user MCP
  handler was constructed with `pgOrgStore` before `pgOrgStore` was
  initialized (init ordering in `app.go`). `UserCreate` called
  `h.orgChecker.GetUserOrgID()` on a nil interface → nil pointer dereference
  → generic 500 on every user-scope MCP server creation. Fixed: pass nil at
  construction, wire via `SetOrgChecker` after `pgOrgStore` is created.
  Added nil-guard (503 instead of panic). Same nil-wiring bug fixed on
  `adminMcpHandler.SetAudit` (was silently dropping all platform-admin MCP
  audit events).

## [0.7.0] - 2026-08-02

### Added

- **MCP Server Integration — Epic 53 (#613, #615, #617).** Platform admins,
  org admins, and individual users can now register external MCP servers
  whose tools workspace agents gain at startup. Three scopes (platform/org/user),
  three transports (http, sse, stdio), and full governance:

  - Three-tier encrypted storage mirroring the Epic 30 credential model:
    admin/org via master KEK, user via session DEK (zero-knowledge).
  - Injection pipeline: `mcp-server` entries in `secrets.json` → materializer
    stages → `AgentConfigWriter` renders opencode `mcp` config section.
  - Live reload via `agentpush.Service` after bind/unbind/token rotation.
  - Org policy `allow_user_mcp_servers` (default locked) gates member access.
  - Plan-tier quota `MaxPersonalMcpServers` (free=5, team+=unlimited).
  - Org policy `max_mcp_servers_per_workspace` (default 5) enforced at bind.
  - Instance setting `mcp.allowOrgAdminServers` (default true, fail-closed)
    kill-switch for all org-admin MCP mutations.
  - Secret references via `{env:VAR}` — resolves from existing `env-secret`
    entries at opencode runtime.
  - SSRF validation: IP range blocking (RFC1918, loopback, link-local, CGNAT,
    Unspecified) + DNS resolution.
  - Env var name validation reuses `validation.ValidateEnvVarName` (blocks
    `LD_PRELOAD` etc). Header CRLF injection prevention.
  - OpenAPI spec updated with full MCP CRUD + bindings + auto-apply.
  - Frontend: shared `McpServersTab.tsx` for all three admin surfaces with
    secret-reference picker.
  - E2E verified against real opencode 1.15.12 binary — all 3 transports
    connect successfully.

  Migration `000012`: `mcp_servers`, `mcp_server_bindings`, `mcp_server_auto_apply`.
  Schema version bumped to 8. No CRD — MCP servers are API-owned relational data.

  Design: `design/stories/epic-53-mcp-server-integration/`. PRs: #613, #615, #617.

- **README-LLM.md MCP Server Integration section (#615).** Documents the
  as-built system per DoD item 15.

### Fixed

- **Passkey login redirect (#613).** `redirectAfterAuth` only redirected when
  `return_to` was set. Passkey login without `return_to` stayed on `/login`.
  Now falls back to `/chat`.

- **Passkey e2e cookie auth (#613).** The Playwright mock for `/auth/me`
  accepted only Bearer headers; now also accepts `lsp_session` cookies,
  matching production HttpOnly cookie behavior.

## [0.6.0] - 2026-07-30

### Added

- **Instance-default `/tmp/*` auto-approval for agent permissions (#602).** New
  instance setting `workspace.allowedExternalDirectories` (default `["/tmp/*"]`)
  injects `mode.permissions.external_directory` allow-rules into every
  workspace's `agent-config.json` at boot. Agents no longer prompt for `/tmp/*`
  on every session. `/sandbox-cfg/*` is deliberately excluded (plaintext
  credential path). Configurable via admin UI; schema v7.

- **Go SDK `Secrets.CreateWithMetadata` method (#603).** Non-breaking addition
  alongside existing `Create` (which delegates with nil metadata). Required for
  `env-secret` type secrets that need `metadata.var_name`.

### Fixed

- **Remember-me sessions last 30 days, not 24 hours (#602).** The Helm chart's
  ConfigMap never rendered `auth.rememberMeDuration`, so the Go auth service
  silently fell back to `tokenDuration` (24h) for every Helm-deployed install.
  Users checking "remember me" were bounced to `/login` daily.

- **API-layer workspace storage validation (#603).** The API had no storage size
  format/magnitude validation (only the controller webhook did, which can be
  disabled). Added format regex (`^[1-9][0-9]*(Gi|Mi)$`) + 1024Gi max cap.

- **GetWorkspace returns 404 after deletion (#603).** Previously returned the
  DB row with `Phase=""` when the CRD was garbage-collected — clients couldn't
  distinguish "deleted" from "creating". Now returns 404 NotFound.

- **SDK `BindingItem.ID` JSON tag corrected (#603).** Changed from `json:"id"`
  to `json:"secretId"` to match the API's actual response shape.

- **10+ SDK canary assertion fixes (#603).** Fixed cascading pre-existing
  failures across S-WS-CRUD, S-WS-STATUS, S-SECRET-CRUD, S-SECRET-REVEAL,
  S-SECRET-AUDIT, S-SECRET-BINDINGS, S-ENV-VARS, S-OWNERSHIP, S-USER-SETTINGS,
  S-RATE-LIMIT scenarios. Root causes: env-secret metadata requirement, DEK
  requires JWT auth (not API key), binding JSON tag mismatch, schema version
  bump, rate-limit burst threshold.

## [0.5.5] - 2026-07-25

### Fixed

- **API-key-only admin workspaces now reach Active (#593, #595).** Three
  root-cause fixes: (1) `agentHealth.providersConfigured` was always `0` for
  healthy workspaces because the controller's healthy condition message
  omitted the `configured=N` token the API regex parses; (2) `POST
  /provider-credentials` and `GET /provider-credentials/:id/models` returned
  an opaque `503 "encryption unavailable"` for API-key callers — now returns
  `403` with actionable guidance pointing at `decryptAccess=true` or password
  auth; (3) admin credentials never reached admin-owned workspaces without a
  manual second API call — `SeedWorkspaceCredentials` now cascades all admin
  credentials when the workspace owner has `users.role='admin'`.

- **Deleted API keys are now immediately rejected (#597).** The 15-minute
  validation cache was not evicted on delete, so a deleted key kept
  authenticating until the TTL expired. `DeleteAPIKey` now evicts the
  `apikey:<hash>` cache entry after the DB delete.

- **`motd` field always present in `/auth/config` response (#596).**
  `AuthConfig.MOTD` had `json:"motd,omitempty"` which dropped the key when
  the value was empty (the default). All three SDK canaries assert field
  presence.

- **returnTo redirect race under react-router v7 (#600).** v7's
  `startTransition` default caused `navigate(returnTo)` to lose a race with
  `GuestOnly`'s `<Navigate to="/chat">`. Login and Register pages now use
  `window.location.href` for the returnTo redirect.

### Changed

- **Frontend major-version bumps: vite 5→8, vitest 2→3, react-router-dom
  6→7 (#600).** Clears all moderate+ npm CVEs (esbuild dev-server CORS,
  vite server.fs.deny, react-router open-redirect ×3). Zero config changes;
  the app already used `createBrowserRouter` (v6.4+ data-router API). The
  returnTo fix above was the only code change required.

- **SDK canary CI runs a real kind cluster (#599).** The `sdk-canary` job
  previously used a stub kubeconfig pointing at a non-existent server, which
  broke workspace-creating scenarios (`S-WS-CRUD`, `S-WS-STATUS`,
  `S-WS-QUOTA`, `S-OWNERSHIP`). Now creates a kind cluster + installs CRDs
  so workspace CRUD operations succeed.

- **SDK versions synced to 0.5.4 (#598).** Python `pyproject.toml`,
  TypeScript `package.json`/`package-lock.json`, Java `pom.xml`, and
  `openapi.yaml` were stale at `0.4.5` on main (the release pipeline
  overwrites at publish time and never bumped them back).

- **postcss bumped to 8.5.18+ via npm overrides (#597).** Clears
  GHSA-r28c-9q8g-f849 (path traversal, HIGH).

## [0.5.4] - 2026-07-23

### Fixed

- **SDK publish pipeline: PyPI packages-dir and TypeScript build path.**
  The PyPI publish job looked for `dist/` at the workspace root instead of
  `sdks/python/dist/`. The TypeScript npm job failed because v0.5.2 was
  manually published without build output. Both fixed.

## [0.5.2] - 2026-07-23

### Added

- **SDK refresh, parity, and publishing (Epic 62, #584 #586 #588).** The four
  hand-written SDKs (Go, Python, TypeScript, Java) are now at typed-surface
  parity with the current API server. The OpenAPI spec went from 45 → 84
  paths. Python and TypeScript SDKs publish to PyPI and npm respectively on
  platform release tags. SDK versions track the platform version. The Java
  SDK was rewritten from a generic HTTP wrapper into a typed facade with
  unchecked exception hierarchy. A live contract test validates the
  `/message` blocking + JSON assumption against a real workspace.

## [0.5.1] - 2026-07-23

### Fixed

- **Swipe-to-open-sidebar no longer triggers browser back-nav (#590).**
  In mobile mode, ~50% of swipe-to-open-sidebar gestures triggered the
  browser's back navigation instead. Root cause: `touchstart` was
  registered `{ passive: true }`, making `preventDefault()` impossible at
  the moment of contact — the browser/OS committed to the back-nav
  gesture during touchstart / first-touchmove before the JS `touchmove`
  handler could claim it. Fix: make `touchstart` non-passive and call
  `preventDefault()` for edge touches (`clientX < EDGE_ZONE`) at contact,
  claiming the gesture before the OS can engage back-nav. Applies to both
  `AppShell` (chat) and `PortalLayout` (admin/orgs portals).

## [0.5.0] - 2026-07-23

### Added

- **Platform versions display in the admin portal (#587).** A new
  "Versions" tab (`/admin/versions`) shows the running version of every
  platform component (API, Controller, Frontend, Relay Router, Base
  Runtime) in a table. Versions are read from the deployed Deployment
  image tags via a new `GET /api/v1/admin/platform-info` endpoint
  (admin-gated) — the most truthful "what is running" signal — rather
  than each component self-reporting. The handler discovers components
  by the `app.kubernetes.io/name=llmsafespaces` label (release-name
  independent) and degrades gracefully (200 with partial data) if the
  K8s API or settings are temporarily unavailable.

- **SDK refresh — OpenAPI parity + Python/Java/TypeScript SDKs (#584,
  #586, #588).** Closes the drift between the four hand-written SDKs and
  the current API server:
  - **OpenAPI spec refresh (US-62.1):** 45 → 84 paths covering the full
    in-scope router surface; version 1.0.0 → 1.1.0.
  - **Python SDK parity (US-62.2):** rewritten for parity with the
    refreshed spec; contract tests (US-62.9).
  - **Java SDK typed-facade rewrite (US-62.5, #586):** builder-constructed
    facade with 9 service groups + typed model classes, replacing the
    generic HTTP wrapper.
  - **Python/TypeScript SDKs published to PyPI + npm (US-62.8, #588):**
    SDK versions now track the platform version; trusted-publishing
    release pipelines (OIDC, no stored tokens) run on `v*.*.*` tags.

## [0.4.5] - 2026-07-22

### Fixed

- **Kebab menus now viewport-aware (#583).** The kebab (three-dot)
  menu always opened directly below its trigger; the bottom-most
  workspace/session in the left nav overflowed past the viewport
  bottom — partially unreadable/unusable. Since `KebabMenu` is the
  shared component backing all three usages (sidebar workspace, sidebar
  session, chat header) and is the only custom portal in the codebase,
  the fix applies everywhere. Positioning now: flips above when there
  isn't room below; clamps horizontally to the viewport edges; caps
  height and scrolls (`overflow-y-auto`) when the menu is taller than
  the viewport; repositions on scroll/resize. The geometry is extracted
  into a pure `computeMenuPosition()` (unit-tested) and measured via
  `scrollHeight` (not `offsetHeight`) so the `maxHeight` cap stays
  stable across re-measures — measuring the capped height would drop
  the cap and re-expand the menu.

## [0.4.4] - 2026-07-22

### Fixed

- **Chat links open in a new tab (#581).** Markdown links rendered by
  `ReactMarkdown` in chat messages (both assistant text and
  thinking/reasoning parts) opened in the same tab, navigating the
  user away from the chat. Added a shared `MarkdownLink` component
  that renders every link with `target="_blank"` and
  `rel="noopener noreferrer"` (the `rel` is required for security —
  `target="_blank"` without it lets the opened page access
  `window.opener`). Both `ReactMarkdown` instances in `MessagePart.tsx`
  now wire the override; `rehype-sanitize` strips `target`/`rel` from
  the hast tree by default, but the component override adds them at
  render time so they always reach the DOM.

## [0.4.3] - 2026-07-22

### Fixed

- **Frontend image — patch system-library CVEs (openssl, libpng,
  libxml2, musl, nghttp2, zlib).** The frontend image builds on
  `nginxinc/nginx-unprivileged:1.27-alpine`, whose packaged system
  libraries lagged Alpine's security advisories. The release Trivy
  gate found HIGH/CRITICAL CVEs against `libssl3` (CVE-2026-31789
  CRITICAL, CVE-2025-15467 HIGH), `libpng`, `libxml2`, `musl`,
  `nghttp2-libs`, and `zlib`. Added `apk upgrade --no-cache` to the
  nginx stage of `frontend/Dockerfile`, which pulls the fixed
  versions from Alpine's repository (e.g. openssl 3.3.7-r0, libpng
  1.6.55-r0). This is a real package upgrade, not a `.trivyignore`
  accept — the vulnerable bytes no longer ship. The other
  control-plane images (api/controller/relay-router/relay-proxy) are
  `gcr.io/distroless/static` and were already clean; the base runtime
  image is warn-only by design (Debian bookworm CVE backlog).

- **Release workflow — two gate bugs that blocked the v0.4.2 release
  (#577, #578).** (1) The `wait-for-ci` job's `ALL_DONE` jq computation
  counted its own in-progress check run (self-referential — the same
  class of bug #574 had set out to fix), making the success branch
  unreachable and forcing a 40-minute timeout. (2) After (1) was fixed,
  nightly `gremlins` (mutation tests) check runs on the tag SHA — which
  are expected mutation-testing signal, not a release gate — were being
  treated as release blockers. Both fixes add the appropriate name
  exclusion to the gate's jq filter.

## [0.4.2] - 2026-07-22

### Fixed

- **Streaming scroll-follow — user no longer yanked to the bottom (#575).**
  During streaming, scrolling up to read earlier content while waiting
  for a response immediately pulled the viewport back to the bottom,
  trapping the user at the tail. Root cause: in
  `frontend/src/components/chat/MessageList.tsx`, the `scroll` handler
  deferred the `stickToBottom.current` intent update into a
  `requestAnimationFrame`. The streaming `MutationObserver` auto-scrolls
  to the bottom on every token while `stickToBottom` is `true`; because
  the intent update was deferred one frame, a token whose observer rAF
  was registered before the user's scroll rAF ran first and re-scrolled
  to the bottom, so the deferred handler read an at-bottom position and
  kept `stickToBottom=true`. With tokens arriving every few ms this
  happened on essentially every scroll attempt. Fix: update
  `stickToBottom.current` synchronously in the scroll handler; the very
  next token's observer now sees the user's intent and leaves the
  viewport alone. Follow mode and the "Resume tailing" button are
  unchanged.

- **Release workflow — self-referential CI-gate failure (#574).** Both
  the v0.4.0 and v0.4.1 releases were blocked by a self-referential
  failure in `lewagon/wait-on-check-action`: when the `Wait for CI`
  job failed (transient API error) and was re-run, the prior failed
  check run persisted on the ref, so every retry immediately saw the
  old failure and re-failed — making the release unrecoverable without
  manual `gh release create`. Replaced the action with a custom polling
  script in `release.yml` that filters out Release workflow job names
  explicitly (in-progress or failed), so it never observes its own
  check. This is what makes the v0.4.2 release cuttable cleanly.

## [0.4.1] - 2026-07-21

### Added

- **zstd in runtime base image (#569).** Added the `zstd` package to
  the workspace runtime image. Required by modern package managers
  (apt .deb contents, conda .conda format, npm v10+ tarballs), git
  pack compression (2.22+), and container image layers. Pre-fix, agents
  hitting any of these got a confusing "command not found".

- **Synthetic monitor for CORS expose-headers (#571).** New script at
  `hack/monitor-cors-expose-headers.sh` that catches the class of bug
  where an ingress-controller middleware overrides the app's
  `Access-Control-Expose-Headers` — silently stripping `X-Next-Cursor`
  from the browser's view. The app's own tests cannot catch this
  because they only cover the app; the override happens at the edge.
  Schedule via UptimeKuma, cron, or GitHub Actions.

### Fixed

- **EnqueueMessage body cap — DoS hardening (#568).** The canonical
  `EnqueueMessage` handler at `proxy_handlers.go` read the request body
  via unbounded `c.ShouldBindJSON`. A client could POST a multi-gigabyte
  body that the API would buffer in full before the 100KB text limit
  rejected it. Applied `http.MaxBytesReader` cap (101KB) matching the
  pattern already used in `redirectPromptToQueue` and `proxy.go:275`.

- **Stable sort tiebreaker in message history (#570).** The
  `selectChronological` sort used `id.localeCompare` as a tiebreaker
  when two messages shared the same `createdAt` millisecond — the
  documented root cause of issue #387. Replaced with stable-sort by
  original array index (backend delivery order). Immune to future
  opencode ID format changes; the lex tiebreaker assumed IDs are
  sortable by creation time (they're not).

- **Test isolation from host's reload-secrets cache (#572).**
  `TestE2E_PasswordReset_FullPurgeThenBoot_NoProviders` was failing on
  hosts where `/sandbox-runtime/last-reload-secrets.json` exists
  (including the opencode sandbox). The materialize subcommand reads
  this file by default; the test forgot to override
  `LLMSAFESPACES_RELOAD_CACHE_PATH` to a tempdir. Fixed in all three
  materialize call sites in `pod_bootstrap_e2e_test.go`.

- **Trivy scan split — control-plane blocks, base warns (#567).** The
  v0.4.0 release was blocked because Trivy found HIGH-severity CVEs in
  the base runtime image (Debian bookworm packages, mostly
  `fix_deferred` upstream). Split the scan loop: control-plane images
  (api/controller/frontend/relay-router/relay-proxy) still gate the
  release; the base runtime image surfaces findings as a warning
  annotation without blocking. The base inherits Debian's CVE backlog;
  failing on every bookworm CVE would prevent any release from shipping.

## [0.4.0] - 2026-07-19

### Added

- **Redis TLS support (#465, #557).** The API's Redis client config now
  exposes `tls` and `insecureSkipVerify` fields, wired through the chart
  values and configmap. Required for AWS ElastiCache
  (`TransitEncryptionEnabled`), GCP Memorystore with TLS, and any
  self-hosted Redis with TLS. Production should leave
  `insecureSkipVerify: false` and use a CA-signed cert; the field is a
  dev/test escape hatch for self-signed certs. Chart test renders the
  configmap fields; cache test asserts TLS connect + cert-verification
  paths.

- **Image digest pinning (#476, #556).** Every image section in the chart
  (`api`, `controller`, `frontend`, `runtimeEnvironments.base`,
  `relay-router`) now accepts an optional `digest` field. When set, it
  overrides `tag` and the image reference becomes `repository@digest`.
  Operators hit by #454 (tag GC'd from GHCR) wanted immutable pins; both
  `sha-<commit>` and `sha256:<digest>` forms are now first-class. Three
  new chart tests pin the helper behavior.

- **GCP KMS provider for master KEK (US-57.3, #528).** The KEK provider
  abstraction now has a GCP KMS implementation alongside AWS KMS. The
  master KEK can be hosted in either cloud; switching is a configmap
  change. Closes the multi-cloud KEK story for operators who already
  run GCP.

- **`migrate-kek` CLI for cross-provider KEK migration (US-57.2, #532).**
  New binary at `cmd/migrate-kek` that re-wraps the existing master KEK
  under a new provider (e.g. AWS KMS → GCP KMS, or local → cloud) with
  zero downtime. The KEK is decrypted in-memory under the old provider,
  re-encrypted under the new, and the result is atomically written.
  Audit-logged via the standard `secret_audit_log`.

- **Invitation accept page + settings deep-linking + `return_to`
  redirect (#533).** Org invite emails now land on a dedicated accept
  page that handles logged-in vs. logged-out flows. Settings tabs are
  deep-linkable (`/settings/billing`, `/settings/security`, etc.) with
  a `return_to` query param preserved across the auth gate. New e2e
  specs cover invitation accept, return_to redirect, and settings
  messaging.

- **gVisor overhead benchmark harness (Epic 51, #549).** Operator-run
  script at `helm/scripts/gvisor-benchmark.sh` that measures gVisor
  overhead on a representative LLM-coding workload. Documented
  methodology in `docs/operator/gvisor-benchmark.md` — accept/reject
  decision per the <30% overhead target.

- **Supply-chain hardening: cosign signing + Trivy scanning + Renovate
  digest pinning (#534).** All release images are signed with cosign
  keyless OIDC; `cosign verify` is documented in the install runbook.
  Trivy scans run in CI on every PR and on the release tag. Renovate
  is configured to open digest-pinning PRs for base images.

- **Traefik CORS guidance in operator docs (#560).** The chart
  previously documented only the nginx-ingress security-headers path.
  Added a "CORS at the edge" subsection to `docs/operator/networking.md`
  with a complete Traefik Middleware example, the 5-header
  expose-list pinned to `security.go:64`, the drift-hazard warning, and
  a verification snippet. Closes the documentation gap that caused a
  production CORS bug (queued-message button never rendered).

### Fixed

- **FIFO race in message queue — drained messages rendered out-of-order
  after reload (#563, #564).** When a session transitioned busy→idle,
  a direct POST /prompt could race ahead of the still-draining queue
  goroutine. opencode assigned the direct send an earlier
  `info.time.created` than the queued message, so on reload
  `selectChronological` placed the queued message AFTER the direct
  send. Two-layer fix: (1) frontend `handleSend` now also checks
  `queue.queuedMessages.length > 0` before routing to direct send,
  closing the common case; (2) backend `SendPromptAsync` checks
  `queueSvc.Len()` and redirects to `Enqueue` when non-empty, closing
  the residual race window where the client's view is stale (the
  `refreshQueue` poll hasn't landed yet). Regression tests at both
  layers with FIFO-order assertions.

- **Free-models refresher ClusterRole configmaps grant (#469, #558).**
  The controller's ClusterRole was missing `get/list/watch` on
  `configmaps`, so `freeModelsRefresher` could not read the cached
  free-models list. The chart now renders the grant conditionally on
  `controller.inferenceRelay.enabled`.

- **Relay `scrapeRouterMetrics` silent error swallowing (#475, #555).**
  The metrics scraper logged at Debug level on HTTP errors, masking
  real failures (router not yet running, wrong port, wrong path).
  Elevated to Warn with the status code and URL in the log fields.

- **arm64 image variants contained x86-64 binaries (#462, #554).** The
  multi-arch build matrix produced arm64 image manifests whose contents
  were x86-64 binaries — `docker pull ...arm64` followed by
  `uname -m` returned `x86_64`. Fixed the Dockerfiles and the build
  matrix's platform handling.

- **copy-html initContainer PSA restricted compatibility (#468, #551).**
  The frontend pod's initContainer ran `cp -a` into a read-only path
  under PSA `restricted`. The chart now mounts an `emptyDir` at
  `/usr/share/nginx/html` and the initContainer copies into it.

- **KEK post-migration audit gate + nil-fallback guard (US-57.2, #548).**
  After a `migrate-kek` run, the API now refuses to start if the new
  KEK provider returns nil — previously it would silently fall back to
  the un-encrypted path. Audit log entries are also gated: a missing
  audit logger no longer silently drops entries.

- **Materialize test isolation from workspace reload cache (#559).**
  The materialize test suite shared state with the workspace reload
  cache, producing flaky failures when run in parallel. Tests now
  isolate their cache fixture.

- **Production CORS expose-headers override by Traefik Middleware
  (talos-ops-prod #2053).** The API correctly emitted
  `Access-Control-Expose-Headers` with 5 entries
  (`X-Request-ID, X-RateLimit-Limit, X-RateLimit-Remaining,
  X-RateLimit-Reset, X-Next-Cursor`), but a Traefik Middleware at the
  cluster edge overwrote the value with only `X-Request-Id`. The
  browser hid `X-Next-Cursor` from JS, breaking the "Load earlier
  messages" button. Fix: updated the cluster Middleware to mirror the
  app's list. Documentation: see "CORS at the edge" above.

### Removed

- **Cloudflare Worker inference relay (Epic 60, #553).** Zen
  (`opencode.ai/zen/v1`) now blocks all Cloudflare Worker egress IPs,
  making the Worker relay path unreachable. Removed: the
  `workers/inference-relay/` directory, the chart's
  `inferenceRelayURL` / `inferenceRelaySecret` / top-level
  `cloudflare:` block, the `--inference-relay-secret` controller flag,
  the `INFERENCE_RELAY_SECRET` env var, and the `relay-secret-sync`
  Helm Hook Job. Operators should switch to direct-to-Zen (the new
  default) or the self-hosted InferenceRelay fleet (Epic 42).
  **Upgrade note:** existing chart values with
  `inferenceRelayURL: https://relay.safespaces.dev` will break — clear
  it to empty.

### Security

- **G13 — Account lockout now keys on email + IP (Medium).** The
  lockout counter was keyed on email only
  (`lockout:<email>`). An attacker who knew a victim's email could
  submit bad passwords from any IP and lock the victim's account — a
  DoS amplification vector. The lockout key now includes the client IP
  (`lockout:<email>:<ip>`), so an attacker from a different IP cannot
  trigger the victim's lockout. A new `WithClientIP(ctx, ip)` context
  helper propagates the IP from the gin router through `Login`. Callers
  that don't set it fall back to email-only keying (backward compat).
  Regression: `TestLogin_G13_AttackerFromDifferentIPCannotLockVictim`,
  `TestLogin_G13_SameIPLockoutStillWorks`,
  `TestLogin_G13_NoIPContextFallsBackToEmailOnly`.

- **G38 — ChangePassword now revokes all sessions (High).** The handler
  at `POST /api/v1/account/change-password` previously re-wrapped the
  DEK with the new password and updated the bcrypt hash but left every
  outstanding JWT — including the caller's — valid until natural expiry.
  A token stolen before the change kept reading secrets (the cached DEK
  in Redis was already evicted by `KeyService.ChangePassword`, but the
  JWT signature remained valid and a re-login re-cached the DEK under
  the new password, which the stolen token could then use). The handler
  now calls `auth.Service.RevokeAllUserSessions` after both the DEK
  re-wrap and the bcrypt update commit, writing the per-jti and per-hash
  revocation markers and clearing durable `jwt_sessions` rows — the same
  primitive `password-reset/confirm` already used (OWASP ASVS V2.5.2).
  Best-effort: a revocation failure is logged and the password change
  still reports success (the cryptographic change is irreversible).

- **G37 — Workspace env-var name blocklist (High).** The handler at
  `PUT /api/v1/workspaces/:id/env` accepted any POSIX-shaped env-var
  name, including `LD_PRELOAD`, `PATH`, `PYTHONPATH`, `BASH_ENV`,
  `DYLD_INSERT_LIBRARIES`, etc. Setting one of these via the env-secret
  mechanism would let a workspace owner compromise every process
  spawned in the pod (agentd, opencode, mise-installed interpreters) —
  a container-escape-equivalent in practice because the pod's single
  UID shares the same trust boundary. The new
  `pkg/validation.ValidateEnvVarName` enforces three rules at the API
  layer: POSIX shape (`[A-Za-z_][A-Za-z0-9_]*`), length ≤ 256, and not
  on a curated blocklist of ~30 dangerous names sourced from ld.so(8),
  bash(1), Python, Node, Ruby, Perl, Java, and glibc docs. The same
  validator is now used by agentd's materialize-time check as defense-
  in-depth. Locale vars (`LANG`, `LC_ALL`, `TZ`) are intentionally NOT
  blocked — they don't execute code and users legitimately set them.

- **G35 — `/account/recover` per-route rate limit (High).** The
  recovery endpoint at `POST /api/v1/account/recover` was mounted on
  the root router behind only the global 100/min/IP rate limiter. While
  the recovery key is 128-bit random (brute-force is mathematically
  infeasible), the endpoint does Argon2id work to re-derive the DEK
  under the new password, making it a CPU-exhaustion DoS target. The
  new `PerRouteRateLimitMiddleware` (separate from the global
  `RateLimitMiddleware`) applies a stricter per-route limit (default
  20 tokens/burst 5, from the previously-dead-code `authRatePerMinute`
  /`authRateBurst` constants) using per-route bucket isolation
  (`<path>:<identity>` key) so a user hitting `/recover` cannot deplete
  the budget for other routes. The middleware is generic — future
  endpoints (e.g. G41 `/secrets/:id/reveal`) can be added to the same
  routes map.

- **G25 — Secret `value` field no longer logged (High).** The request
  logging middleware (`api/internal/middleware/logging.go`) masked
  sensitive JSON fields by name (`password`, `token`, `secret`, `key`,
  `apiKey`, `credit_card`) but NOT `value` — the field name used by
  the secrets API to carry the plaintext credential on
  `POST /api/v1/secrets` and `PUT /api/v1/secrets/:id`. A request to
  create a secret logged the plaintext API key in the application log,
  visible to anyone with log access. Two-layer fix: (1) added `value`
  to the default `SensitiveFields` list (defense in depth — catches
  any logged JSON with a `value` field, even on paths not in the skip
  list); (2) added `SkipPathPrefixes` to `LoggingConfig` and configured
  the default with the credential-bearing paths (`/api/v1/secrets`,
  `/api/v1/account`, `/api/v1/auth`, `/api/v1/admin/provider-credentials`)
  so bodies on those paths are never logged at all. Either layer alone
  prevents the leak.

- **G36 — Workspace secrets cleaned on deletion (High).** The
  workspace controller's `handleTerminating`
  (`controller/internal/workspace/phase_terminating.go`) deleted the
  pod, PVC, and `workspace-pw-*` Secret but NOT `workspace-creds-*`,
  which persisted indefinitely after workspace deletion. The existing
  `cleanupFailedWorkspaceSecrets` primitive (already used for the
  Failed-phase path in `recovery.go`) knows how to delete both
  `workspace-creds-*` and `workspace-pw-*`; this PR wires it into the
  graceful-termination path too. Best-effort (failures logged, not
  propagated — the workspace is already being torn down and the
  finalizer must still release). `handleDeletion` (the CRD-deletion
  entry point) inherits the fix automatically because it calls
  `handleTerminating`.

- **G28 — Workspace bind handler reclassified as Accepted (was High/Open).**
  The threat-model row originally flagged "PUT /workspaces/:id/bindings
  returns 204 but K8s Secret is never created." Epic 35 (secretless
  injection) removed the durable K8s Secret path entirely; the
  architecture now persists bindings to PostgreSQL and the init
  container fetches them via `/internal/v1/pod-bootstrap` at boot. The
  live HTTP push to running pods is best-effort; `ErrNoRunningPod` is
  an accepted, documented transient state. The "no-op for first-time
  delivery" is the intended behavior in the new architecture. Added
  `TestSecretService_G28_BindingsSurviveNoPodState` to lock the
  persistence invariant — bindings survive the no-pod window and are
  visible to the bootstrap read path (`GetBindings`) when the pod
  eventually boots.

### Threat model reconciliation

A fresh audit of the 50 gaps in
`design/stories/epic-17-security-review/THREAT-MODEL.md` against the
current code found 6 rows whose status no longer matched reality. This
entry reconciles the threat model without changing any production code.

- **G29 — Path-traversal `mount_path` accepted by API → 🟢 Fixed (was Medium/Open).**
  The threat-model row claimed `POST /api/v1/secrets` accepts
  `mount_path = "../../etc/passwd"`. `validateMountPath` was added at
  `pkg/secrets/secret_service.go:582` (Bug 13 in worklog 0085); it is
  called from line 563 BEFORE secret creation. Stale row corrected.
- **G45 — Legacy `source /sandbox-cfg/env` in entrypoint → 🟢 Fixed (was Low/Open).**
  US-35.7 moved the env-secret source path to `/sandbox-runtime/secrets-env`.
  The legacy `/sandbox-cfg/env` source no longer exists in
  `entrypoint-opencode.sh`. Stale row corrected.
- **G50 — Decrypt operations not audited → 🟢 Fixed (was Medium/Open).**
  The threat-model row claimed `NewAuditedProvider` had zero call sites.
  US-50.12 wired it at three production sites in `api/internal/app/app.go`:
  `app.go:408` (providerCredsProv), `app.go:409` (orgCredsProv),
  `app.go:624` (apiKeyProv). Every Decrypt on those providers now logs
  to `secret_audit_log`. Stale row corrected.
- **G4 — No mTLS between API and sandbox pods → 🟡 Accepted (was Medium/Open).**
  Real gap, but the fix requires either a service mesh (Linkerd/Istio)
  or per-workspace certificate provisioning — both substantial
  infrastructure additions outside the scope of threat-model-gap fixes.
  Compensating controls: NetworkPolicy default-deny, RFC1918/CGNAT
  egress filter, explicit header allowlist (G34 fix), per-request
  basic-auth rotation. Operator runbook: deploy Linkerd or Istio in
  `inject` mode on the LLMSafeSpaces namespace to close this gap
  without code changes.
- **G30 — Egress NetPol allows external DNS resolvers → 🟡 Accepted (was Medium/Open).**
  Real gap; standard Kubernetes `NetworkPolicy` cannot restrict DNS by
  destination domain. Closing requires Cilium FQDN policies, Calico
  `GlobalNetworkPolicy`, or a custom filtering resolver — operator
  infrastructure decisions. Compensating controls: workspace pods use
  cluster DNS by default; egress allowlist blocks RFC1918/CGNAT; DNS
  exfil bandwidth is naturally limited.
- **G40 — Agentd user port (4097) has no application-layer auth → 🟡 Accepted (was Medium/Open).**
  Real defense-in-depth gap; the trust boundary is the NetworkPolicy
  (`workspace-network-policy.yaml`) — only API server pods can reach
  workspace pods on port 4097. Adding `requireBearerToken` at the
  application layer is defense-in-depth that the existing controls
  make redundant for the documented deployment topologies.

Threat model counts: 26 Fixed / 16 Open / 8 Accepted →
**38 Fixed / 0 Open / 12 Accepted** (50 total). All gaps resolved.

All 50 gaps are resolved: 38 Fixed, 12 Accepted, 0 Open.

- **G6/G41 — `/secrets/:id/reveal` per-route rate limit (Medium).**
  The reveal endpoint at `POST /api/v1/secrets/:id/reveal` accepts
  the user's password as input to re-authenticate before decrypting.
  Without a per-endpoint cap, a single IP could attempt 100 password
  guesses per minute against the global limiter. The route is now in
  `DefaultRouterConfig.PerRouteRateLimitConfig.Routes` at 5/min + burst
  5 — matches the legitimate-user pattern (re-reveal several secrets in
  quick succession) while making brute-force impractical. Closes both
  G6 and G41 (duplicate rows for the same gap). Regression:
  `TestRouter_G41_RevealSecretRateLimited`.

- **G21 — `/sandbox-cfg/password` mode 0600 (Medium).** The init
  container's `cp /mnt/secrets/password/password /sandbox-cfg/password`
  preserved the K8s Secret's `defaultMode: 420` (0644), leaving the
  opencode basic-auth password world-readable in the pod filesystem.
  Replaced with `install -m 0600` so the mode is set atomically with
  the copy. Regression: the existing `TestE2E_Reconcile_*` test now
  asserts the `install -m 0600` line in the rendered credScript.

- **G42 — SSE connection tracking prunes stale entries (Medium).** The
  `sseConnCounts` global map grew monotonically — every distinct
  client IP that ever attempted a connection left a permanent entry.
  `sseConnAllowed` now opportunistically prunes expired entries on
  every call (O(N) sweep, N bounded by per-IP rate limit). Regression:
  `TestSSEConnAllowed_G42_PrunesStaleEntries`.

- **G44 — Pod-level RunAsNonRoot (Low).** `buildPodSecurityContext`
  set RunAsUser/RunAsGroup/FSGroup/SeccompProfile but not
  RunAsNonRoot. Every container today sets it explicitly at the
  container level, but a future sidecar without its own
  SecurityContext would inherit the pod default (nil) and could run
  as root. Added `RunAsNonRoot: &true` at the pod level — the kubelet
  enforces it by refusing to start any container resolving to UID 0.
  Regression: `TestG44_PodSecurityContextHasRunAsNonRoot`.

- **G46 — Silent password file read failure now fatal (Low).**
  `readAgentPassword` previously logged a Warn on file-read error and
  returned an empty string. The workspace would start silently non-
  functional — opencode without auth, every proxy request fails basic-
  auth. Now logs at Error and calls `os.Exit(1)` so the pod enters
  CrashLoopBackOff, surfacing the failure as a pod-level signal.
  Regression: `TestReadAgentPassword_HappyPath` (error path uses
  os.Exit, documented as not unit-testable without subprocess exec).

- **G47 — Inference relay secret no longer exposed as CLI arg (Low).**
  The Helm chart's plaintext fallback
  `--inference-relay-secret={{ .Values.inferenceRelaySecret }}`
  rendered the secret into the controller's container args, visible
  in `kubectl get pod -o yaml` and audit logs. Removed the fallback;
  operators who set `inferenceRelaySecret` without configuring
  `externalSecret.create` or `externalSecret.existingSecret` now get
  a `helm template`-time error with an actionable remediation message.
  Regression: `TestControllerArgs_G47_NoPlaintextRelaySecretFallback`
  (fail-fast) and `TestControllerArgs_G47_EnvVarPathStillWorks`
  (legitimate path).

## [0.3.0] - 2026-07-11

Network hardening sweep + KMS-backed master KEK foundation + Go security bump.

### Security

- **G34 — proxy header allowlist (Critical).** The workspace reverse proxy
  previously forwarded every client request header (Cookie, Origin, Referer,
  X-Forwarded-*, arbitrary custom headers) verbatim into the tenant pod. Now
  uses an explicit allowlist (`Content-Type`, `Accept`, `X-Request-ID`) and
  strips RFC 7230 hop-by-hop headers in both directions. `Accept-Encoding` is
  deliberately not forwarded — Go's http.Transport handles gzip transparently.
  ([#513](https://github.com/lenaxia/LLMSafeSpaces/pull/513))

- **G39 — terminal WebSocket Origin check (High).** The terminal WebSocket
  upgrader accepted any Origin (`CheckOrigin: return true`), enabling
  cross-site WebSocket hijacking from a malicious page in a browser holding
  the user's session ticket. Now defaults to same-origin only, with an
  operator-controlled allowlist (`terminal.allowedOrigins` Helm value).
  Removed the dead `WebSocketSecurityMiddleware` and
  `RouterConfig.AllowedWebSocketOrigins` plumbing — the gorilla `Upgrader`
  is now the single enforcement point.
  ([#515](https://github.com/lenaxia/LLMSafeSpaces/pull/515))

- **CORS hardening.** `security.allowedOrigins=["*"]` combined with
  `security.allowCredentials=true` is now rejected at config load with an
  actionable error. The combo is forbidden by the CORS spec (Fetch §3.2.1)
  because it would let any website read authenticated responses from this API
  in a victim's browser. Browsers reject the combo client-side; we now also
  fail closed at boot.
  ([#516](https://github.com/lenaxia/LLMSafeSpaces/pull/516))

- **NetworkPolicy CGNAT drift (chart/controller parity).** The chart-side
  default `blockedEgressCIDRs` was missing `100.64.0.0/10` (CGNAT),
  `127.0.0.0/8` (loopback), and `224.0.0.0/4` (multicast) that the
  controller-side list already had. The CGNAT gap specifically affected
  managed Kubernetes offerings (AKS default VNet, some EKS configs, k3s
  default flannel) where `100.64/10` is the pod CIDR — workspace pods on
  such clusters could reach internal pods/services in the CGNAT range,
  defeating cross-tenant isolation.
  ([#517](https://github.com/lenaxia/LLMSafeSpaces/pull/517))

- **runtimeClass webhook admin-gate (S51.2 closure).** The Workspace CRD's
  `spec.runtimeClass` field (Epic 51 S51.1, used for per-workspace gVisor
  opt-out) was documented as "admin-gated, not tenant-selectable" but the
  webhook enforcement was deferred to S51.2. Without it, any user with
  workspace create/update RBAC could set `spec.runtimeClass="runc"` to
  escape gVisor via direct kubectl. The workspace validating webhook now
  rejects `spec.runtimeClass` unless the object carries the annotation
  `llmsafespaces.dev/allow-runtime-class-override: "true"`, applied via
  cluster-admin RBAC.
  ([#518](https://github.com/lenaxia/LLMSafeSpaces/pull/518))

- **JWT iss/aud claims.** JWTs now carry explicit `iss` and `aud` claims,
  minted from `auth.jwtIssuer` / `auth.jwtAudience` (default
  `"llmsafespaces"`), and validated on every parse. Pre-fix tokens carried
  only `sub/jti/exp/iat`, so any service sharing the same HMAC secret could
  mint accepted tokens. Pre-fix tokens are rejected after this change; tokens
  are short-lived (24h default) so rotation is fast.
  ([#519](https://github.com/lenaxia/LLMSafeSpaces/pull/519))

- **Epic 57 US-57.1 — CompositeProvider + prefix-aware local providers
  (foundation for KMS-backed master KEK).** The first of three PRs closing
  the largest unbuilt gap in the threat model: API-pod RCE → permanent KEK
  exfiltration for offline DB decrypt. This PR lands the dispatch mechanism
  (`CompositeProvider` with prefix-sniffing ciphertext routing) and the
  prefix-aware local providers (`lkms:v1:` prefix on new writes, legacy
  un-prefixed blobs still decrypt via fallback). Subsequent PRs add AWS KMS
  and GCP KMS providers and the `migrate-kek` CLI.
  ([#510](https://github.com/lenaxia/LLMSafeSpaces/pull/510) design,
  [#511](https://github.com/lenaxia/LLMSafeSpaces/pull/511) composite,
  [#512](https://github.com/lenaxia/LLMSafeSpaces/pull/512) AWS KMS provider + Go 1.25.12 bump)

### Infrastructure / Tooling

- **Go 1.25.12.** Bumped from 1.25.11 to fix `GO-2026-5856`
  (Encrypted Client Hello privacy leak in `crypto/tls`).
  ([#512](https://github.com/lenaxia/LLMSafeSpaces/pull/512))

- **Supply chain hardening.** Release images are now cosign-signed
  (keyless OIDC, Rekor transparency log). Trivy image scanning runs
  on every built OCI image. Renovate `docker:pinDigests` opens PRs to
  pin Dockerfile FROM lines to immutable digests.
  ([#534](https://github.com/lenaxia/LLMSafeSpaces/pull/534))

- **Documentation site.** New MkDocs Material site at
  https://lenaxia.github.io/LLMSafeSpaces/ — 32 pages across 7
  sections (Getting Started, Operator Guide, Architecture, API
  Reference, Contributing, Reference). Docs-maintenance runbook
  documents content inventory, drift triggers, and procedures.
  ([#527](https://github.com/lenaxia/LLMSafeSpaces/pull/527),
  [#529](https://github.com/lenaxia/LLMSafeSpaces/pull/529),
  [#530](https://github.com/lenaxia/LLMSafeSpaces/pull/530))

- **Chart path cleanup.** `charts/llmsafespaces/` → top-level `/helm`.
  Zero impact on consumers (chart registry URL unchanged).
  ([#526](https://github.com/lenaxia/LLMSafeSpaces/pull/526))

- **New chart values:** `terminal.allowedOrigins`, `auth.jwtIssuer`,
  `auth.jwtAudience`. See the security entries above for usage.

- **Helm chart NetworkPolicy default** now includes the full
  controller-side private-or-internal CIDR set (CGNAT/loopback/multicast),
  not just RFC1918 + link-local.

## [0.2.2] - 2026-07-07

## [0.2.1] - 2026-07-06

## [0.2.0] - 2026-07-06

## [0.1.0] - 2026-07-04

[0.39.0]: https://github.com/lenaxia/LLMSafeSpaces/compare/v0.38.1...v0.39.0
[0.14.2]: https://github.com/lenaxia/LLMSafeSpaces/compare/v0.14.1...v0.14.2
[0.14.1]: https://github.com/lenaxia/LLMSafeSpaces/compare/v0.14.0...v0.14.1
[0.5.1]: https://github.com/lenaxia/LLMSafeSpaces/compare/v0.5.0...v0.5.1
[0.5.0]: https://github.com/lenaxia/LLMSafeSpaces/compare/v0.4.5...v0.5.0
[0.4.5]: https://github.com/lenaxia/LLMSafeSpaces/compare/v0.4.4...v0.4.5
[0.4.4]: https://github.com/lenaxia/LLMSafeSpaces/compare/v0.4.3...v0.4.4
[0.4.3]: https://github.com/lenaxia/LLMSafeSpaces/compare/v0.4.2...v0.4.3
[0.4.2]: https://github.com/lenaxia/LLMSafeSpaces/compare/v0.4.1...v0.4.2
[0.4.1]: https://github.com/lenaxia/LLMSafeSpaces/compare/v0.4.0...v0.4.1
[0.4.0]: https://github.com/lenaxia/LLMSafeSpaces/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/lenaxia/LLMSafeSpaces/compare/v0.2.2...v0.3.0
[0.2.2]: https://github.com/lenaxia/LLMSafeSpaces/compare/v0.2.1...v0.2.2
[0.2.1]: https://github.com/lenaxia/LLMSafeSpaces/compare/v0.2.0...v0.2.1
[0.2.0]: https://github.com/lenaxia/LLMSafeSpaces/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/lenaxia/LLMSafeSpaces/releases/tag/v0.1.0
## Placeholder

