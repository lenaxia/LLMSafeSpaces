# SESSION-OWNERSHIP-DESIGN.md — Unify Session Ownership in the Platform (Item 10)

**Status:** Design (pre-spike — see "Hard Unknown")
**Created:** 2026-08-09
**Depends On:** Epic 64 (routines + `session_origins`), Epic 03 (proxy/sessions), `ROUTINES-REDESIGN.md` (the bridge layer this design replaces)
**Supersedes:** The `session_origins` bridge table as the long-term session-ownership answer (it becomes an interim step)

---

## Problem Statement

### Current state: sessions physically live in opencode

Every workspace runs `opencode serve` (README-LLM.md:38). opencode persists its sessions — the conversation graph, messages, tool calls, token counts — in a **SQLite database on the workspace PVC** (`opencode.db`). The platform does not own this data. To answer any session question ("list this user's sessions," "what was the origin of session X," "how many tokens did this session burn," "is this session still active"), the platform must **proxy the query through opencode** on the target workspace pod — a live HTTP round-trip per operation, only possible when the workspace is `Active`, and opaque to the platform's own filtering, labeling, and metering layers.

Three PG-side data flows have grown up *around* this opencode-owned store, none of them consistent with it:

| System | What it holds | Who writes it | Source of truth? |
|---|---|---|---|
| **`opencode.db`** (SQLite, on PVC) | Session rows, messages, tool calls, full transcript, parent/session hierarchy | opencode, inside the pod | **De-facto yes** — the only complete record |
| **`session_index`** (PG) | workspace_id, session_id, title, last_message_at, parent_id, context_used | `sessionindex.Service` mirrors via proxy events (`sessionindex/service.go`) | **No** — a partial mirror, drops events under load (`service.go:63-69` drops oldest when channel full) |
| **`trigger_fires.result`** + **`session_origins`** (PG) | Routine run outputs, session origin tags (manual/routine/workflow/api), `pkg/types/workflows.go:59-64` | Routine executor + proxy intercepts | **No** — only covers routine-sourced sessions |

### The split-brain problem

Session data flows through **three systems that nothing reconciles**:

1. The user creates a session interactively → it exists in `opencode.db`. It may or may not appear in `session_index` (depends on whether the proxy's `onRawEvent` fired and whether the drain channel had room).
2. A routine creates an ephemeral session → it briefly exists in `opencode.db`, is tagged in `session_origins` with `origin=routine`, its text output lands in `trigger_fires.result`, then (per `preserve_session: never`) the session is **deleted from opencode.db**. The PG origin row now points at a session that no longer exists anywhere.
3. A workflow's `agent` node with `session: new` creates a session → it lives in `opencode.db`, is tagged `origin=workflow` in `session_origins`, but its messages never reach `session_index` (the routine path doesn't go through the interactive proxy event path).

Concretely, the platform today **cannot** reliably:
- List all sessions for a user across their workspaces without round-tripping every workspace pod (and only the `Active` ones).
- Filter sessions by origin, status, or source in SQL.
- Report per-session token usage from its own DB (the `session_index.context_used` is a single rolling counter, not per-message).
- Label, archive, or manage sessions the user can't see because the workspace is suspended.
- Serve an observability/billing view of session activity without joining three inconsistent sources.

This is the split-brain: opencode owns the truth, the platform owns shrapnel, and the shrapnel is the only thing the platform can query at scale.

---

## Target State

**The platform owns session metadata in Postgres. opencode uses its SQLite DB as a working cache.** Session list, origin, status, title, and per-message observability all query PG. The PVC-backed `opencode.db` becomes an implementation detail of how opencode produces turns — not the system of record the platform reads from.

```
BEFORE                                       AFTER
─────                                        ─────
Browser ─► API ─► proxy ─► opencode          Browser ─► API ─► PG (sessions, session_messages)
                     │                                    │ (list, filter, label, observe — all in SQL)
                     ▼                                    │
              opencode.db (SQLite)                  proxy intercepts create/message
              = the system of record                ▼
                                          opencode.db (SQLite) = working cache
                                          (opencode reads/writes it; platform may eventually flush it)
```

The platform's session list, origin indicators, observability dashboards, and billing all read PG. The proxy layer intercepts every session create/message call and mirrors into PG *before/alongside* forwarding to opencode. opencode keeps working against its own DB for turn production; the platform stops depending on it for anything the platform needs to query.

---

## Schema

Two new tables, owned by the platform. Migration `000022_sessions_unified` (+ helm mirror via `make chart-sync-migrations`). These supersede `session_index` and `session_origins` for platform-owned session metadata (both are folded in — see "Relationship to existing tables").

### `sessions`

```
id            text PRIMARY KEY              -- opencode session id (e.g. ses_...); platform does NOT mint its own
workspace_id  uuid NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE
user_id       text NOT NULL                 -- the user who owns the session (for cross-workspace listing)
source        text NOT NULL                 -- interactive | routine | workflow | api | mcp
                                            --   (matches SessionOrigin* in pkg/types/workflows.go:59-64,
                                            --    with 'interactive' = the renamed 'manual')
status        text NOT NULL DEFAULT 'active' -- active | archived | deleted
title         text                           -- first-user-message-derived or agent-set; updatable
origin_ref    uuid                           -- nullable FK to triggers(id) for routine/workflow sources
                                            --   (replaces session_origins.trigger_id)
created_at    timestamptz NOT NULL DEFAULT now()
updated_at    timestamptz NOT NULL DEFAULT now()
-- index on (user_id, created_at DESC)  -- cross-workspace session list
-- index on (workspace_id, created_at DESC)  -- per-workspace sidebar
-- index on (source, created_at DESC)  -- origin-filtered views
```

### `session_messages`

```
id           uuid PRIMARY KEY DEFAULT gen_random_uuid()
session_id   text NOT NULL REFERENCES sessions(id) ON DELETE CASCADE
seq          int NOT NULL                   -- monotonic per session; orders the transcript
role         text NOT NULL                  -- user | assistant | system | tool
parts        jsonb NOT NULL                 -- opencode message parts (text, tool-call, tool-result, step-*)
                                            --   verbatim shape from opencode /session/:id/message response
tokens       jsonb                          -- {input, output, reasoning, cache:{read,write}} per A1 contract
created_at   timestamptz NOT NULL DEFAULT now()
UNIQUE (session_id, seq)
-- index on (session_id, seq)  -- transcript reconstruction
```

### Relationship to existing tables

| Existing table | Fate under this design |
|---|---|
| `session_index` | **Folded in.** Its columns (title, last_message_at, parent_id, context_used) become derivable from `sessions` + `session_messages`. `parent_id` (opencode's session hierarchy) is the one field not yet modelled — add `parent_id text` to `sessions` if sub-session hierarchy must survive (the proxy already mirrors it via `sessionindex.UpsertParent`). The `sessionindex.Service` drain is retired once the proxy mirrors into `sessions`/`session_messages` directly. |
| `session_origins` | **Folded in.** Its `(origin, trigger_id, fire_id)` columns become `sessions.source` + `sessions.origin_ref`. The `ROUTINES-REDESIGN.md` bridge layer (`session_origins`) was explicitly interim ("When unified sessions arrive ... this table becomes the source of truth and opencode.db becomes a cache" — ROUTINES-REDESIGN.md:138). This design is that arrival. |
| `trigger_fires.result` | **Unchanged** — still stores routine run outputs (the routine's captured result, distinct from the session transcript). A routine session's messages *also* live in `session_messages` when `capture: full` or `preserve_session != never`. |

### Why the platform does NOT mint session IDs

`opencode.db` is the working cache and opencode is the writer of session rows during a turn. If the platform minted its own IDs, every proxy intercept would need an ID-translation map. Instead, the proxy lets opencode assign the `ses_...` id (opencode's `POST /session` returns it — validated in `NODE-EXECUTE-CONTRACT.md` A1) and the platform mirrors that exact id into `sessions.id`. opencode and the platform agree on the key by construction.

---

## Migration Path

### Phase 0 — Spike the hard unknown (BLOCKING)

Before any production code, validate the assumption that gates the whole design (see "Hard Unknown"). If opencode cannot operate with an external session store, the "platform owns metadata, opencode is a cache" model is unreachable and this design degrades to "expand the bridge layer" — a different, smaller effort. **No production code merges before this spike produces evidence.**

### Phase 1 — Proxy-side mirror (write-both)

The proxy layer (`api/internal/handlers/proxy.go`, `proxy_handlers.go`) already intercepts every session create/message call to forward to opencode. It gains a **mirror-before-forward** step:

| Proxy intercept | Mirror action |
|---|---|
| `POST /session` (create) | `INSERT INTO sessions (id, workspace_id, user_id, source, status)` with the id opencode returns; `source=interactive` (or `routine`/`workflow`/`api` from the calling path) |
| `POST /session/:id/message` (user or assistant turn) | After opencode's synchronous response (A1 contract — the full assistant reply is in the HTTP body), `INSERT INTO session_messages (session_id, seq, role, parts, tokens)` for both the user part and the assistant parts |
| `DELETE /session/:id` | `UPDATE sessions SET status='deleted'` (soft delete — keep the row for audit/billing) |
| `POST /session/:id/abort` | No mirror (no new message) |

Mirror writes are **non-blocking to the user's turn**: the proxy returns opencode's response as soon as opencode does; the mirror write happens on a bounded queue (same `sessionindex.Service` drain pattern at `service.go:42-56`, but writing to `session_messages` instead of `session_index`). A dropped mirror event (channel full) is logged and recovered by a reconciliation sweep (Phase 3). The user's interactive latency is never gated on PG.

### Phase 2 — Platform reads flip to PG

Once the mirror has backfilled (Phase 3 ensures consistency), platform read paths stop round-tripping opencode:

| Read path | Before | After |
|---|---|---|
| Session sidebar list | Proxy → opencode `GET /session` per workspace | `SELECT FROM sessions WHERE workspace_id=$1 ORDER BY created_at DESC` |
| Cross-workspace "my sessions" | Not possible (would need every workspace Active) | `SELECT FROM sessions WHERE user_id=$1 ORDER BY created_at DESC` |
| Session transcript | Proxy → opencode `GET /session/:id/message` | `SELECT FROM session_messages WHERE session_id=$1 ORDER BY seq` |
| Origin indicator | Join `session_origins` (incomplete) | `sessions.source` (complete) |
| Per-session token totals | Not available in PG | `SELECT tokens FROM session_messages WHERE session_id=$1` aggregate |

Suspended workspaces now have a fully queryable session history — the data is in PG, not on a pod that isn't running.

### Phase 3 — Reconciliation sweep (consistency backstop)

A controller-side `manager.Runnable` (same pattern as `freemodels/refresher.go` — `NeedLeaderElection`, periodic tick) reconciles PG against opencode for Active workspaces:

- For each `Active` workspace, list opencode sessions; diff against `sessions` rows; backfill missing.
- For each session in PG marked `active`, verify it still exists in opencode; if not (user deleted via opencode UI directly), mark `deleted`.

This sweep is the safety net for dropped mirror events (Phase 1's bounded queue) and for sessions created through paths the proxy doesn't intercept (if any exist). It runs at low frequency (e.g. 5min); it is not the primary write path.

### Phase 4 — opencode.db becomes flushable

Once PG is confirmed as the read source across all paths and the reconciliation sweep reports zero drift for a sustained period, `opencode.db` is demoted to a working cache in the operational sense: the platform can flush/truncate it (on workspace recycle, on size pressure) without losing platform-queryable history. opencode continues to use it for turn production; the platform no longer depends on its contents for anything the platform needs to query.

Whether opencode *itself* can tolerate its DB being truncated (vs. needing session rows present to continue a multi-turn conversation) is part of the Phase 0 spike. If it cannot, "flushable" degrades to "the platform doesn't read it" — still a win, just not a storage-pressure win.

---

## The Hard Unknown: opencode's Session Store

This is the single assumption that determines whether the design is a 2–3 week effort or a 4+ week effort.

**Can opencode operate with sessions sourced from outside its own SQLite DB — or does it require its DB to be the system of record it reads back from between turns?**

Two outcomes, materially different in effort:

### Outcome A — opencode treats its DB as a self-sufficient cache (the optimistic case)

opencode writes sessions to its DB on create, reads them back on the next turn of the same session, but does not require the DB to be the *only* copy. In this case:
- The proxy mirror (Phase 1) is purely additive — opencode keeps working unchanged.
- "Platform owns metadata" is satisfied by the mirror alone; opencode.db is a redundant cache the platform simply doesn't read.
- The 2–3 week estimate holds.

**Evidence to look for in the spike:** does opencode re-read `opencode.db` session rows on every `/session/:id/message`? Does deleting `opencode.db` between turns break a continuing session? Does opencode have any config flag for an external/sessionless mode?

### Outcome B — opencode requires its DB as the system of record (the pessimistic case)

opencode reads session history from its own DB to construct the prompt context for each turn. The DB is not a cache; it is load-bearing for turn production. In this case:
- The platform cannot unilaterally "own" session metadata without either (a) an opencode patch/fork to externalize the session store, or (b) a bidirectional sync that keeps opencode.db populated from PG on workspace resume.
- The proxy mirror still works for platform *reads* (PG is the query source), but opencode.db must be preserved and kept consistent — "flushable" (Phase 4) is off the table.
- The 4+ week estimate applies, and may grow if an opencode patch is required (coordination with upstream, or maintaining a fork).

**Evidence to look for in the spike:** trace opencode's `/session/:id/message` handler — where does it load prior turns? Is there a session-provider interface, or is it hardcoded to SQLite? Is there an opencode config for a different session backend?

### Why this gates the design

Under Outcome A, the migration is a platform-side additive change with no opencode dependency — low risk, bounded effort. Under Outcome B, the design either accepts a permanent bidirectional-sync complexity or requires changing opencode itself — a materially different project that touches the "external dependency behind a single seam" principle (README-LLM.md:254-272). The spike must produce evidence (a traced code path, a config flag found or not found, a live test of DB-truncation-between-turns) before Phase 1 begins.

---

## Scope

### In scope

- `sessions` + `session_messages` schema (migration `000022`).
- Proxy-side mirror layer (write-both on create/message/delete).
- Platform read-path flip (sidebar, transcript, origin, cross-workspace list, token totals) from opencode-proxy to PG.
- Reconciliation sweep (`manager.Runnable`, leader-elected) as consistency backstop.
- Fold `session_index` + `session_origins` into the unified tables (backfill + retire the old writers).

### Out of scope (deferred)

- **Making opencode.db truly flushable** — depends on Outcome A/B from the spike; pursued only if Outcome A confirms it's safe.
- **opencode patch/fork for external session store** — only if Outcome B forces it and the cost is justified; otherwise live with bidirectional cache.
- **Full-text search over session transcripts** — `session_messages.parts` is JSONB; a GIN index + `@>` queries cover exact/filter; a real search tier (pgvector, full-text index over text parts) is a follow-on.
- **Cross-workspace session federation / sharing** — `user_id` enables listing, but sharing a session across users is a separate permission model.
- **Migrating opencode's session *hierarchy* (parent/sub-sessions) fully into PG** — `parent_id` is preserved on `sessions` if the spike shows it's needed, but the hierarchy model is opencode's; the platform mirrors it, doesn't redefine it.

---

## Risks

### R1 — The hard unknown (Outcome B) doubles the effort

Documented above. The Phase 0 spike is the mitigation — it converts the estimate from "2–3 or 4+" into a definite number before code is written.

### R2 — Mirror write failure = silent data divergence

If the proxy's mirror queue drops events under load (the `sessionindex.Service` pattern already drops oldest at `service.go:63-69`), PG drifts from opencode.db. Users see a session list missing sessions that exist, or a transcript with gaps.

**Mitigation:** the reconciliation sweep (Phase 3) is the explicit backstop — drift is detected and repaired within a sweep interval, not silently permanent. The mirror queue's drop policy is logged with a counter (`session_mirror_dropped_total` Prometheus metric) so divergence is observable, not invisible.

### R3 — Token/part schema drift when opencode upgrades

`session_messages.parts` stores opencode's message-part JSON verbatim. An opencode version bump that changes the part schema (new part type, renamed field) makes historical rows inconsistent with new ones.

**Mitigation:** `parts` is `jsonb`, not a typed column — schema drift does not break reads, it only means consumers must handle multiple shapes (which they already do, since the proxy passes parts through today). A `parts_schema_version` column is optional hardening; defer unless drift becomes a real consumer problem.

### R4 — Large transcripts bloat PG

A long-running session with hundreds of turns and large tool outputs could push `session_messages` rows into megabytes each. PG handles this, but unbounded growth is a storage cost the platform didn't have when sessions lived on per-workspace PVCs (distributed, per-tenant storage).

**Mitigation:** `parts` is capped at write time (the existing `maxNodeOutputBytes` discipline applies — oversize parts are truncated or the message is flagged). A TTL/retention janitor (the `jwt_session_janitor` pattern) is a follow-on, not a blocker. PVC storage was never free — this centralizes a cost that was already being paid, distributed.

### R5 — Tenant isolation: cross-user session visibility

With sessions in a shared PG table, a query bug could expose one user's sessions to another. Per-PVC storage made this impossible by construction (the session lived on the user's own volume).

**Mitigation:** every read path is `WHERE user_id = $authenticated_user` (or workspace-scoped with org membership checks). Row-level security policies on `sessions`/`session_messages` are belt-and-suspenders if the deployment wants DB-enforced isolation. This is the same isolation discipline the rest of the PG schema already requires; sessions are not a new exposure class, just a new table in an existing trust model.

---

## Adversarial Review

### Weaknesses

1. **"This re-introduces a SPOF: if PG is down, sessions are unreadable."** True — but PG is already a hard dependency of the platform (auth, workspaces, credentials, billing all read it). Sessions moving to PG does not add a new SPOF; it moves data into an existing one the platform already cannot function without. The interactive turn itself still works during a PG outage (opencode.db is on the PVC; the proxy forwards; only the *mirror* write fails and is recovered by the sweep). The session *list* failing during a PG outage is the same failure mode as every other list in the product.

2. **"Mirror-before-forward adds latency to every turn."** No — mirror is *after* opencode responds (the assistant parts come from opencode's response body) and is non-blocking (bounded queue, drain in background). The user's turn latency is unchanged. The only synchronous cost is the `POST /session` create mirror, which is a single row insert.

3. **"Why not just make opencode use PG directly (skip the mirror)?"** That is Outcome B's hard version — it requires opencode to support an external session store, which the spike must confirm. If opencode already supports it, yes, the mirror is redundant and the design simplifies to "point opencode at PG." The mirror exists precisely *because* we don't yet know if opencode can be pointed at PG. The mirror is the Outcome-A-compatible path that works regardless.

### False alarms

- **"This breaks the PVC-backed persistence model."** No. The PVC still backs `/workspace` (files, git, the working tree). Only the *session transcript* moves to PG. Files stay per-workspace.

- **"`session_index` already solves this."** No — `session_index` is a partial mirror that explicitly drops events under load (`service.go:63-69`) and carries only metadata (title, last-message timestamp, parent, a rolling context-used counter). It cannot reconstruct a transcript, cannot report per-message tokens, and does not cover routine/workflow-sourced sessions. It was a sidebar-display optimization, not a session-ownership model. This design subsumes it.

- **"We should wait for opencode to add a session API."** We already have the session API (`/session`, `/session/:id/message`, `DELETE /session/:id` — all validated in `NODE-EXECUTE-CONTRACT.md` A1). The question is not API availability; it is whether opencode can *source* sessions externally. That is the spike.

---

## Effort Estimate

| Phase | Effort — Outcome A | Effort — Outcome B |
|---|---|---|
| 0 — Spike (hard unknown) | 2–3 days | 2–3 days |
| 1 — Schema + proxy mirror | 4–5 days | 4–5 days |
| 2 — Read-path flip (sidebar, transcript, origin, cross-workspace, tokens) | 4–5 days | 4–5 days |
| 3 — Reconciliation sweep | 2 days | 2 days |
| 4 — Fold `session_index` + `session_origins`, retire old writers | 2 days | 2 days |
| **Outcome-B-only extra:** bidirectional cache sync OR opencode external-store integration | — | 5–10+ days |
| **Total** | **~2–3 weeks** | **~4+ weeks** |

The spike is the single largest variance driver. It should start before any other phase and its result (Outcome A vs B) is recorded as evidence per README-LLM.md Rule 7 (validated assumptions) before Phase 1 merges — the same discipline applied to Epic 64's A1–A9 assumptions.

---

## Next Steps

1. **Run the Phase 0 spike** — trace opencode's session read/write path; determine Outcome A vs B; record evidence. This is the gating item.
2. Draft migration `000022_sessions_unified` (schema only) — can proceed in parallel with the spike since it's pure DDL.
3. On spike resolution: if Outcome A, proceed through Phases 1–4 as estimated. If Outcome B, re-scope Phase 4 and decide between bidirectional-sync vs opencode-patch before continuing.
4. Coordinate with the `ROUTINES-REDESIGN.md` bridge: the `session_origins` table's interim role ends here. Its writers (routine executor, proxy intercepts) redirect to `sessions.source` + `sessions.origin_ref` in Phase 4.
