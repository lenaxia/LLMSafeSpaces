# Worklog: tool-children parallelism caps (#892 D7)

**Date:** 2026-08-16
**Session:** Implement design 0050 D7 on `fix/892-d7-tool-parallelism-caps`, stacked on #896. Tracking #892.
**Status:** Complete

---

## Objective

The starvation that drove the 2026-08-15/16 incident was self-inflicted oversubscription: the pods' own build-tool children (`go`, `esbuild`, `tsc`) spun machine-sized thread pools inside a 2-CPU cgroup quota — 4,460–10,576 throttled CFS periods, hundreds of seconds of stall — which starved opencode's event loop past every timeout in the system. Within the no-quota-raise constraint, cap the pools to the quota so tool parallelism competes inside the budget instead of thrashing against it.

---

## Work Completed

- `controller/internal/workspace/pod_builder.go`:
  - `toolParallelismEnv(reqs)` — derives integer cores from the effective CPU limit (ceil of milli); emits `GOMAXPROCS` and `ESBUILD_WORKER_THREADS`.
  - `requirements` computed once before the container literal; `Resources` and the env caps now derive from the same quantities.
- Tests: explicit 2-CPU request → 4× burst → caps at 8; default 500m → 2; fractional limit ceilings (2500m → 3).

## Key Decisions

- **Cap at the burst limit** (4× request, or the explicit `cpuLimit`), not the request — matches what the cgroup actually enforces at peak.
- **Two levers only** (`GOMAXPROCS`, `ESBUILD_WORKER_THREADS`): the tools observed in the incident's zombie set (`[go] [esbuild] [tsc]`; esbuild covers the tsc-transform path in most build chains). tsc-native and npm parallelism have no effective env knob; per-invocation flags still override env when the agent explicitly passes them.
- **Env inheritance rather than wrapper scripts:** every tool child of opencode inherits the pod env; no runtime image change, no entrypoint surgery, works for arbitrary future tools that respect the same vars.

## Assumptions (validated)

1. `GOMAXPROCS` caps `go build -p` (defaults to GOMAXPROCS) and test-binary parallelism — Go-documented.
2. `ESBUILD_WORKER_THREADS` caps esbuild's worker pool — esbuild-documented env var.
3. Env vars reach tool children: opencode spawns bash-tool commands with the pod env (observed: zombies' env matched the container's).

## Blockers

None. Effect on real builds is G6's measurement pass (pre/post throttle counters on a build-capable workspace).

## Tests Run

- `go test ./controller/internal/workspace/ -count=1` — green
- `go build ./controller/... && go vet ./controller/... && gofmt -l` — clean

## Files Modified

- controller/internal/workspace/pod_builder.go
- controller/internal/workspace/pod_builder_test.go

---

## Round 2 corrections (review on #897)

- **Assumption 2 disproven (Rule 7 finding):** round 1's worklog listed
  `ESBUILD_WORKER_THREADS` as a "validated, esbuild-documented env var".
  Review verified against esbuild's shipped source (0.16.10 and 0.28.x
  `lib/main.js`): the only use is `if (process.env.ESBUILD_WORKER_THREADS
  !== "0")` — a disable-flag for the sync-API worker thread, of which the
  JS wrapper spawns exactly one. No numeric semantics, no host-core-count
  pool. The var is REMOVED; GOMAXPROCS alone remains (esbuild is a Go
  binary — its pool follows GOMAXPROCS transitively; the incident's
  `[esbuild]` zombies were the Go process).
- Tests reworked: default-path test de-vacuousized (name lookup with
  presence assertions); ceil-boundary and zero/missing-limit guard paths;
  persisted-pod e2e per repo convention (`reconcileToCreatingPod`, custom
  + default shapes) so a wiring drop between buildPod and persistence is
  caught.
- Design 0050 D7 amended: placebo var dropped, `npm config jobs` dropped,
  G6-order inversion recorded with rationale.
