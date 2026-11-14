# Worklog: harness-selection plumbing (second harness S5)

**Date:** 2026-11-14
**Session:** Story tsk-hazel-welder-orders — make the harness a first-class, choosable dimension of the Workspace per pi-scoping report §6.
**Status:** Complete

---

## Objective

The five scoped plumbing items of report §6: (1) CRD `spec.harness` (default `opencode`, immutable after create), (2) controller per-harness delivery map + `buildPod` branch (overlay wiring, supervisor subcommand), (3) agentd harness driver behind an internal interface, (4) registry-built per-workspace Adapter in the API server, (5) frontend capability gating (already driven by `Adapter.Capabilities()` — no change). Constraint: zero behavior change for default workspaces — golden pod-spec diff empty when `harness=opencode`.

## Work Completed

### CRD (item 1)
- `pkg/apis/llmsafespaces/v1/workspace_types.go`: `HarnessType` (`opencode` | `pi`), `spec.harness` with `+kubebuilder:validation:Enum=opencode;pi` + `+kubebuilder:default=opencode`; `helm/crds/workspace.yaml` regenerated.
- Webhook immutability (`controller/internal/webhooks/workspace_webhook.go` §7a): UPDATE decodes the old object, normalizes empty→opencode on BOTH sides (no false denial for `""`→`"opencode"`), denies any switch with the recreate remedy in the message. CREATE passes (the enum constrains).

### Controller (item 2)
- `harness_delivery.go`: `harnessDeliveries()` per-harness map (the `OpencodeDeliveryConfig` generalization at the point of consumption), `workspaceHarness` (the ONE defaulting site for controller consumers), `wireHarnessOverlay` (exactly ONE harness overlay per pod), `harnessSupervisorSubcommand` (`supervise-opencode` | `supervise-pi`), `validateWorkspaceHarness` (loud refusals: pi-without-sidecar — supervise-pi is a PID-1 supervisor; no-delivery harness; unpinned overlay image).
- `agentd_sidecar.go`: `main.Args` follows spec.harness; the harness-neutral env (sidecar mode, restart marker) rides both branches; the opencode branch stays byte-identical to pre-S5; the pi branch adds the workspace password + `PI_CODING_AGENT_DIR` and the shared RW pi-agent-dir EmptyDir.

### agentd (item 3)
- `harness_driver.go`: the busy/progress seam (statusz derivation behind the driver interface — HTTP+SSE for opencode, control-socket/JSONL for pi); `pi_supervisor.go`'s `piSupervisorProc` adapts the supervisor onto the design-0051 control-socket surface; sidecar `AGENTD_HARNESS` branch (pi statusz/readyz + S3 configwriter) — carried from the S1–S4 story branches merged under this one.

### API server (item 4)
- `pkg/agent/agent.go`: `AdapterFactory`/`AdapterFactoryDeps`/`RegisterAdapter`/`NewAdapterForType` (the adapter-factory registry — separate map from the AgentRuntime registry: the two seams run in different processes with different lifetimes), `UnregisterAdapter` (test hygiene).
- `pkg/agent/harnessrouter.go`: the routing Adapter — per-workspace dispatch via `HarnessLookup` (backed by `resolverHost.GetWorkspaceHarness`), TTL-bounded harness cache, eager default-harness construction (failure fatal — mirrors the pre-S5 hard-wired boot), lazy per-harness construction with loud fallback, stateless translators on the default adapter. `systemnotices.Wrap` wraps the router unchanged (#944 one-point injection preserved).
- `api/internal/app/app.go`: `agentoc.RegisterAdapter()` + `agentpi.RegisterAdapter()` at boot; `NewHarnessRouter` replaces the hard-constructed opencode adapter.
- `pkg/agent/{opencode,pi}/register.go`: `RegisterAdapter()` factories (opencode parity: `NewAdapter → NewAdapterV1`; pi: `NewAdapter(NewPodTransport(...))`).

### Frontend (item 5)
- No change: gating is capability-driven by construction (`Adapter.Capabilities()`); pi's set advertises no ask/permission affordances (§1.5 ruling) — landed with S4.

### This session's repairs (attempt 2)
The prior attempt stranded a merge mid-flight and never compiled: `app.go` declared `router` twice (the HarnessRouter at the top and `server.NewRouter`'s gin engine at the bottom — `no new variables on left side of :=`). Fixed by renaming the S5 variable `harnessRouter`; main's gin `router` untouched. Completed the merge against current origin/main (single conflict: `pkg/agent/pi/register.go` add/add — kept our `RegisterAdapter` addition; `NewAdapter`/`NewPodTransport` signatures verified identical across both sides).
- Updated four S1-era pi overlay tests to S5 semantics (pi harness workspace + sidecar-enabled reconciler — pi requires the sidecar topology); rewrote `TestPiOverlay_AllThreeOverlaysWired` (the S1 tri-overlay shape died with the harness dimension) into `TestPiOverlay_PerHarnessSelection_Coexistence`: one fully-pinned controller, opencode ws wires agentd+opencode and never pi; pi ws wires agentd+pi and never opencode.
- Renamed the S2 test helper `piSupervisorProc` → `piSubprocessRun` (collided with S5's production control-socket adapter type of the same name).

### Tests added
- `controller/internal/workspace/harness_delivery_test.go`: golden zero-diff (`harness=""` vs `harness="opencode"` marshalled pod specs byte-identical, both topologies, non-triviality pinned), subcommand dispatch (pure fn + built pod args), `validateWorkspaceHarness` refusals.
- `controller/internal/webhooks/workspace_webhook_test.go`: `TestS5Workspace_HarnessImmutableOnUpdate` (9-case matrix incl. empty↔opencode equivalence), `TestS5Workspace_HarnessSettableOnCreate`.
- `pkg/agent/harnessrouter_test.go`: per-workspace dispatch, lookup-failure → default, lazy-construction-failure → default, default-construction-failure fatal, TTL lookup caching, stateless-translator default delegation (behavioral marker), registry round-trip + unknown-type + nil-logger defaulting.

## Verification

- `go build ./...` clean; `make fmt-check` clean.
- Suites green at the pushed head: `pkg/agent`, `pkg/agent/opencode`, `pkg/agent/pi`, `pkg/apis/...`, `cmd/workspace-agentd` (292s — subprocess builds), `controller/internal/workspace`, `controller/internal/webhooks`, `api/internal/app`.

## Files touched this session (atop the merged S1–S4 branch state)

- `api/internal/app/app.go` — merge resolution + harnessRouter rename
- `pkg/agent/agent.go` — UnregisterAdapter
- `pkg/agent/harnessrouter_test.go` — new
- `controller/internal/workspace/harness_delivery_test.go` — new
- `controller/internal/workspace/pi_overlay_test.go` — S5 semantics update
- `controller/internal/webhooks/workspace_webhook_test.go` — immutability tests
- `cmd/workspace-agentd/pi_supervisor_test.go` — helper rename
