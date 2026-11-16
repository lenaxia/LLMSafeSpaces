# Worklog: Bind-once harness binding (owner ruling 2026-11-16) — workspaces do NOT switch harnesses

**Date:** 2026-11-16
**Session:** story tsk-visioned-hurdle-leaves (epic "User-facing harness switching", re-scoped by owner ruling 2026-11-16)
**Status:** Complete — implementation + pins + docs addenda; PR iterating with AI reviewer

---

## Objective

Implement the re-scoped harness design: `harness` becomes a nullable, bind-once workspace
field (null -> value exactly once; value -> anything is a loud 409 at the API tier and an
admission denial at the CRD tier), with the enabled set as platform config
(`harnessOptions`), first-session atomic binding (the picker's submit), the legacy
backfill to explicit opencode, and the owner-ruling addenda appended to the prelim's
three spec docs. Pin the ABSENCE of any switch/recreate endpoint. The 2026-11-15 prelim
(switch-with-recreate, PR 2068's designs) is superseded — this branch carries those docs
(stacked on `docs/harness-switch-prelim-designs`) plus the addenda, so the record ends at
the real decision.

## Design decisions (and why)

| # | Decision | Rationale |
|---|---|---|
| D1 | Discriminator marker `llmsafespaces.dev/harness-unbound` on the CRD | Removing the schema default makes "" ambiguous: born-unbound (the picker path) vs legacy pre-ruling object (old default = opencode). The marker is the single discriminator BOTH tiers key on; removed at bind. Without it, a controller restart could mass-stamp (and thus mis-bind) every born-unbound workspace — the marker makes the backfill repeatable and safe forever. |
| D2 | Webhook 7a admits ""->opencode for anyone; ""->pi only on the marker | The backfill stamp (controller or API) must pass admission; a legacy object must never be bindable to pi (it has only ever run opencode — binding it would strand state, the exact hazard the ruling closes). |
| D3 | API treats legacy ""-no-marker as BOUND-opencode (`effectiveBoundHarness`) | Uniform truth: legacy workspaces answer idempotent-200 on opencode binds, 409 on pi, and sessions run opencode — identical to today's behavior; the explicit spec stamp belongs to the controller sweep alone (single writer). |
| D4 | Unbound workspaces build NO pod (Creating + `HarnessBindingPending` condition; PVC still created) | The pod's topology is harness-class — nothing can run until the bind. Creating-the-PVC keeps storage independent of the choice; the bind then releases the phase machine (no drift arm exists; the only spec change happens before any pod exists). |
| D5 | First-session binding = bind BEFORE pod-wait/session-create | "Bind atomically with the session": a session can never exist on an unbound workspace. A post-bind session-create failure keeps the binding — the user's picker choice persists for the retry (convergent, by design). |
| D6 | Omitted harness on unbound at first session binds OPENCODE | Non-interactive callers (board workers, spawned sessions, the current frontend) never present a choice; binding the platform default keeps every existing client working unchanged through the default change. Documented in openapi + DTO. |
| D7 | Unbind attempts (value -> "") are 409, not 422 | "value -> anything else is a loud 409" — "" on a bound workspace is the unbind attempt; the 409 carries the immutable message. "" on an UNBOUND workspace is the 422 required-field error. |
| D8 | Enabled set = `harness.enabled` instance setting (schema v16) + opencode union at the consumer | v1 platform config per the ruling; the union makes the platform's default harness unwithdrawable even against a hostile/typo'd admin write; settings failure degrades to opencode-only (reads never 500). |
| D9 | `harnessOptions` on Get/List; bound workspaces skip enabled-set validation | Clients render pickers honestly; enablement changes never affect bound workspaces (pinned by the enablement-immunity test). |
| D10 | Session registration carries the BOUND harness; pi sessions create via the Agent Adapter | The hardcoded "opencode" register and the raw opencode-only create path would both mis-serve a pi-bound workspace. The adapter (harness router) already dispatches per workspace; `SetAgentAdapter` wires it (the `SetSessionIndex` pattern). |
| D11 | No Postgres migration | The binding lives on the CRD; the DB workspaces table never carried harness. The "migration" is the controller's marker-aware spec backfill (D1/D2). |

## What was NOT built (the re-scope's point)

No switch endpoint, no recreate path, no webhook 7a' switch marker, no harness-drift arm,
no board sweep/depose handoff, no claim-quiesce gate, no switch window, no status.harness,
no session-index harness column. Pinned absent at router + openapi by
`TestHarnessNoSwitchOrRecreateEndpointExists`.

## Verification

- Webhook: 11-case bind-once matrix (switch denials both directions, unbind denials,
  unchanged values, backfill stamp, legacy pi denial, born-unbound pi admission).
- Controller: backfill stamps opencode + requeues; bound-pi never overwritten; unbound
  parks twice idempotently with NO pod and the marker intact; the bind releases the gate;
  discriminator + condition-clear units.
- API service: enabled-set (default/pi/union/degrade); effective-binding resolution; the
  bind matrix (bind-once + marker removal, idempotent no-write, 409 matrix incl. unbind,
  422 disabled/unknown/empty, legacy both arms, enablement-change immunity);
  first-session resolution (omit->opencode bind, picker pi bind, mismatch 409, legacy);
  pi-bound session-create through the adapter with bound-harness registration.
- Absence pin (router + openapi). openapi-validate green. Go SDK builds; Python compiles.
- Docs addenda appended to all three prelim specs (verified).

## Coordination

Stacked on `origin/docs/harness-switch-prelim-designs` (PR 2068 carries the prelim docs;
this branch includes them + the addenda so the merged record ends at the ruling — 2068
merges after this). The UI story (tsk-vibrant-geyser-masks) builds the picker on the
shipped surfaces (harnessOptions, sessions/new body, PUT /harness).
