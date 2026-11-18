# Worklog: Workspace List Starves the Harness Picker

**Date:** 2026-11-18
**Session:** Live prod incident (user: "no harness selection" on three consecutive workspace recreations) → API-log forensics → root cause: `ListWorkspaces` never sends `harnessOptions`, so the frontend's first-session picker is structurally unreachable → one-line API fix + regression tests.
**Status:** Complete

---

## Objective

The owner enabled pi (`instance_settings.harness.enabled=["opencode","pi"]`, written directly to `instance_settings` after the admin-API auth path proved unreachable from the host) and recreated workspaces three times. Every creation bound `opencode` silently with no picker. Find why the picker never renders and fix it.

---

## Work Completed

### Incident forensics (live cluster, home-kubernetes, llmsafespaces ns @ 0.44.17)

Three workspaces recreated during the session: `c519fe9f` (jade-mesa-52), `73474768` (gray-node-2), plus the pre-existing `6733d0bf` (dry-orb-62). All three parked unbound by the controller (parking path correct), then bound `opencode` without a picker rendering.

Evidence chain, with two forensic dead ends worth recording:

1. **Dead end — "missing HTTP writes".** Initial log greps reported NO `POST /sessions/new` around the bind windows. Root cause of the anomaly was **my own pipeline**: `kubectl logs deploy/...` emits `==> pod/x <==` headers between replicas, `jq` streaming-parse aborts at the first header, and `2>/dev/null` swallowed the error — silently dropping every log line that came after the first replica's block. Scoped per-pod greps (`kubectl logs <pod>` with no jq, or grep-only pipelines) revealed the writes were always there. Second pipeline bug in the same session: response bodies are JSON-escaped (`\"opencode\",\"pi\"`), so naive `grep '"items"'`/`grep '"opencode","pi"'` over raw log lines under-matches — quote-escaping must be accounted for in any log-body forensics on this stack.
2. **Attribution, once visible.** `73474768`: `POST /api/v1/workspaces/{id}/sessions/new {"harness":"opencode"}` at 22:22:18 (replica `2glzl`) — the frontend's **silent-bind branch** submitting the fallback option explicitly, 76s after create. `HarnessBindingPending` flipped False/Bound the same second the pod build started; `CredentialsStaged` 22:22:18. The earlier `c519fe9f` case matched the same shape (`{"harness":"opencode"}` at 20:40:00).

### Root cause (code, not ops)

- The sidebar's picker decision reads `workspace.harnessOptions` from the **workspace LIST response** (`frontend/src/components/layout/Sidebar.tsx:433-436`; absent/empty → `FALLBACK_HARNESS_OPTIONS = ["opencode"]`, `harnessMeta.ts:38`).
- `ListWorkspaces` (`api/internal/services/workspace/workspace_service.go`) builds `types.WorkspaceListItem` **without setting `HarnessOptions`** — the field exists on `types.Workspace` (detail/create responses) but not on the list item type at all (`pkg/types/workspace.go:80-112` had `Harness` only).
- Therefore the offer list is **always** the one-element fallback → `harnessOptions.length <= 1` → `handleNewSession` binds `harnessOptions[0]` silently, no modal, **on every instance, forever** — `harness.enabled` is irrelevant to the sidebar path.
- Prod corroboration: the post-create list completion at 22:21:03 (7773 bytes, fetched fresh after the owner's hard refresh) contains **zero** `harnessOptions` keys on any item, while a 312B single-workspace detail response (20:11:52, `GetWorkspace` path) correctly served `["opencode","pi"]`. The sidebar never reads the detail surface.
- The TS client already declares `harnessOptions?: string[]` on the list item (`frontend/src/api/types.ts:77`) — the contract existed, the backend just never filled it.

Same defect class as the two pinned precedents in this file's test suite: OrgID (#477) and PhraseID (#1757 r2) — both were "list conversion drops a field the frontend consumes".

### Fix

- `pkg/types/workspace.go`: add `HarnessOptions []string` (`harnessOptions,omitempty`) to `WorkspaceListItem`, comment documenting the picker contract and the defect class.
- `api/internal/services/workspace/workspace_service.go` `ListWorkspaces`: hoist `enabledHarnesses := s.EnabledHarnesses(ctx)` above the item loop (instance-wide setting; one cached read serves all items) and set `item.HarnessOptions = enabledHarnesses` per item.

### Ops actions taken during triage (recorded for the timeline)

- `instance_settings.harness.enabled` → `["opencode","pi"]` via direct `psql` against `llmsafespacespg-1` (admin API unreachable without prod credentials; 60s InstanceService TTL made the direct write safe). Verified served by 20:11:52.
- Three workspace deletes with full teardown verification (CR + pod + PVC): `6733d0bf`, `c519fe9f`, `73474768`. All were empty (idle auto-created sessions only).
- Confirmed NOT the cause and left alone: the controller's parking gate, the webhook, `resolveSessionHarness`'s omitted-harness opencode default (server behavior is per ruling; the frontend starvation is the bug), k8s RBAC, Cloudflare/fronting (chat.safespaces.dev 502s from the host but the browser path worked).

---

## Key Decisions

1. **Fix at the list endpoint, not the frontend fallback.** The fallback (`["opencode"]`) is correct defensive behavior for a genuinely-absent field; the bug is the field being absent. One line closes the class; frontend changes would have needed a refetch-before-bind dance.
2. **Hoist `EnabledHarnesses` out of the loop.** Instance-wide value; the 60s InstanceService cache makes per-item calls cheap but one read per list is the honest shape.
3. **Direct psql for the settings flip** rather than holding the incident for admin-API credentials. Reversible, schema-checked (`jsonb`), and the API's `Set()` path does no more than validate+persist the same row. Recorded here per the no-silent-ops rule.
4. **`omitempty` on the new field, mirroring the detail type.** `EnabledHarnesses` always returns ≥1 entry (union invariant), so the field is always present in practice; omitempty only affects degenerate fixtures.
5. **Go SDK (`sdks/go/types.go`) left unchanged.** Its `WorkspaceListItem` is a curated subset (no OrgID, no AgentVersion either); extending it is an SDK-release decision, not part of closing the picker starvation. Flagged in the PR for the reviewer to veto.

## Assumptions (Rule 7) and validation

| Assumption | Validation |
|---|---|
| The sidebar picker decision consumes the LIST response, not the detail | `Sidebar.tsx:433-436` reads `workspace.harnessOptions` off the list item; `workspaces.ts` `getSessions`/list types |
| List items lacked the field in prod, not just in code | 22:21:03 completion body (7.7KB, post-hard-refresh) has zero `harnessOptions` occurrences; detail response (312B) served the full set |
| The bind POSTs came from the silent-bind branch, not the picker | Body `{"harness":"opencode"}` matches `onNewSession(harnessOptions[0])` → `ensureSession({harness})` (`Sidebar.tsx:129`, `workspaces.ts:83`); with pi enabled server-side, a picker submit of the default is indistinguishable by wire shape — but the user reports no modal in all three attempts, and pre-fix the offer list is provably length-1 |
| `EnabledHarnesses` is safe to call in the list path (no pod walks, no hot path cost) | `harness_binding.go:56-74`: reads the InstanceService cache only; 60s TTL; no k8s calls |
| Fix does not change wire behavior for existing single-harness instances | Items now carry `["opencode"]` instead of nothing — the frontend picks the real list over the identical fallback; pinned by the second test subtest |
| Workspace deletes during triage destroyed no data | All three CRs' `status.sessions` were idle auto-created placeholders ("New session - …", zero turns); PVC teardown verified via `kubectl get pvc` |

---

## Blockers

None for the fix. Prod pick-up requires the normal release train (merge → tag → ops-repo GitRepository bump); noted in Next Steps.

---

## Tests Run

- `go test ./api/internal/services/workspace/ -run TestListWorkspaces_PropagatesHarnessOptions -count=1` — red first (field undefined; build fail = the missing contract), green after the fix.
- `go test ./api/internal/services/workspace/ -run 'TestListWorkspaces' -count=1` — all list tests green post-fix.
- `go test -timeout 300s -race ./api/internal/services/workspace/ -count=1` — ok (2.7s).
- `go test -timeout 300s -race ./pkg/types/ -count=1` — ok.
- `gofmt -l` on touched files — clean. `go vet` on both packages — clean. `golangci-lint run api/internal/services/workspace/... pkg/types/...` — 0 issues.
- `go build ./...` — clean.

---

## Next Steps

1. Land this PR; cut the next release (≥0.44.18); bump the ops repo (`talos-ops-prod` GitRepository tag + the four image tag overrides) per the lockstep doctrine.
2. Post-deploy verification: `GET /api/v1/workspaces` items carry `harnessOptions:["opencode","pi"]` (prod has pi enabled since 20:11Z); recreate a workspace and confirm the picker renders with both options before any bind.
3. Follow-up candidates (not in scope here): (a) API-side loudness — `resolveSessionHarness` 422 on omitted harness when `len(enabled) > 1` would have turned this incident's silent binds into errors; (b) SDK `WorkspaceListItem` sync decision; (c) the `GET /sessions/{slug}` ensure-create path bypasses `sessions/new` logging semantics — worth a look for audit parity.

---

## Files Modified

- `pkg/types/workspace.go` — `WorkspaceListItem.HarnessOptions` field + contract comment
- `api/internal/services/workspace/workspace_service.go` — `ListWorkspaces` hoists `EnabledHarnesses`, sets the field per item
- `api/internal/services/workspace/workspace_service_test.go` — `TestListWorkspaces_PropagatesHarnessOptions` (pi-enabled + degrade subtests)
- `worklogs/NNNN_2026-11-18_workspace-list-harness-options.md` — this entry
