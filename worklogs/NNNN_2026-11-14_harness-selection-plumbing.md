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

## Review round 2 (CHANGES_REQUESTED response) — this session

The first AI review round returned CHANGES_REQUESTED with five findings; the CI race leg also surfaced two timing failures, golangci-lint thirteen findings, and the latent-boot bug below.

**Finding 1 (correctness)** — `PodTransport` shared a 120s `http.Client.Timeout` with the SSE `Events` subscription: every stream died mid-turn at exactly 120s. Fixed with the client split (`client` 120s for RPC, `streamClient` no client-wide Timeout — the caller's context governs the subscription), pinned by `TestPodTransport_ClientTimeoutSplit`.

**Finding 2 (correctness)** — `singleProcessPolicy.Attach` held `p.mu` across the blocking `switch_session` Call; the pump's `sessionOf` closure needs the same mutex on every event. Restructured: state under `p.mu`, the blocking Call outside it, serialized by a dedicated `switchMu` the pump never takes. Events in flight during a switch attribute to the old session until `p.current` updates (GenerationStart marks the boundary) — noted in-code.

**Finding 3 (security) — root-caused deeper than the finding** — the review said the pi branch's `OPENCODE_SERVER_PASSWORD` env "has no pi-path consumer". The truth was worse: the bridge password came from `readAgentPassword()` — the `/sandbox-cfg/password` FILE the main container never mounts in sidecar pods → every real pi pod would boot-fatal at the supervisor (the subprocess tests passed only by accidentally reading the dev runner's own password file). Fixed the delivery properly: the supervisor reads the env (`readBridgePasswordEnv`, fatal on empty — G46 parity), and the child spawn in `pi_rpc.go` STRIPS the password from the `os.Environ()` passthrough (no child leak — the original finding's concern). The subprocess test now sets the env explicitly.

**Finding 4 (test-integrity)** — zero PodTransport tests. Added `pkg/agent/pi/podtransport_test.go`: resolver error surfaces (incl. `ErrNoRunningPod`), basic-auth on every call, JSON roundtrip, error-field surface + 404→`errPiSessionGone` (the mapping was missing in `Call` — added for the SessionClient contract), SSE events with injected sessionId + keepalive skipping, control-client no-stream, and the timeout-split pin.

**Finding 5 (test-integrity)** — no assembled `spec.harness=pi` leg; duplicated env literals. Fixed: `AGENTD_HARNESS`/`LLMSAFESPACES_PI_AGENT_DIR` single-sourced as `pkg/agentd.EnvAgentdHarness`/`EnvPiAgentDir` (pinning test in pkg/agentd; all five spell-time sites now reference the constants), and `TestHarnessDelivery_Assembled_PiWorkspaceReconcile` drives the FULL Reconcile from PhaseCreating and asserts the created pod (supervise-pi args, exactly-one harness overlay, PiAgentDir on main, `AGENTD_HARNESS=pi` on the native-sidecar init container).

**Lint round**: bodyclose (goroutine hand-off nolint with justification), contextcheck (`context.WithoutCancel(r.Context())` for the shutdown ladder), gosec G702/G705 (verified-overlay argv; non-HTML content types), noctx (`net.ListenConfig{}.Listen`), misspell (analog/marshaled), dead `harnessStatusPoller.run/pollOnce` removed (the loop lives in sidecar_pi.go — it also feeds the healthz cache).

**CI race round**: the two supervise-pi subprocess tests' 15s boot-attach windows exceeded on loaded 2-core runners — widened to 60s (exit waits too); the repeat-signal bound re-derived as <29s (the semantic is "did not wait out the 30s grace", not "fast on an idle box").

**govulncheck**: GO-2026-6859 (net/http, fixed in go1.26.11) fails the Security scan on origin/main's own head (ccccb580) — pre-existing toolchain CVE, not this diff; flagged to the orchestrator in the PR.

**Verify**: `go build ./...`, `go vet`, `make fmt-check` clean; race suites green: cmd/workspace-agentd (410s), controller/internal/workspace, controller/internal/webhooks, api/internal/app, pkg/agent/..., pkg/agentd, pkg/apis.
