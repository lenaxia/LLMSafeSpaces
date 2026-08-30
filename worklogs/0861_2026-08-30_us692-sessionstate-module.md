# Worklog: US-69.2 — sessionstate module: ingestion, seq authority, reseed, 4097 auth, platform/ subPath

**Date:** 2026-08-30
**Session:** Epic 69 (#1134) US-69.2 (#1136): the module-sealed session-state authority in agentd — SSE ingestion with recover walls, monotonic seq with a durable PVC cursor, the ordered reseed procedure, the authenticated ABI surface on the user mux, and the platform/ PVC subPath from pod-builder down.
**Status:** Complete

---

## Objective

Implement design 0055's placement section: agentd becomes the pod-local session-state authority — machinery only (translation is US-69.3, endpoints US-69.4, ledger US-69.7). Invariants in scope: I1 (single seq authority, atomic stamp), I2 (subscribe-before-snapshot), I3 (reseed on boot + generation change), I8 (every route authenticated, per-session rate limits), I9-prep (durable cursor), M3.1 (no synchronous opencode call on hot paths).

---

## Work Completed

### sessionstate module (`cmd/workspace-agentd/sessionstate/`, new)
- `authority.go` — the Authority: one lock owns seq + projection + fanout registration (atomic stamp by construction). `Ingest` is the recover wall (parser panics contained + counted); during reseed, ingest buffers (I3 ordering). `Stream` implements the mandatory connection ordering: register per-connection buffer → capture snapshot@S under the lock → flush seq>S → live; slow consumers dropped (M3.4) and resync via fresh snapshot. `Reseed` runs the ordered procedure: store read OUTSIDE the lock (M3.1), quiesce → buffer → rebuild from store → emit `projection.reseeded` (consuming a seq) → flush buffered → live.
- `cursor.go` — durable seq high-water mark at `platform/seq-cursor`: fsync-before-publish (persist under the lock BEFORE fanout — a published seq is never reused after kill -9), temp+rename+dir-fsync, plain decimal format, corrupt file = hard error at open (never guess).
- `service.go` — the ABI handler: generated connect service behind Basic auth (constant-time, D6.1 password pair, empty entries skipped); Events streams live frames; the other four ops return `NotSupported` per capability until their stories (surface exists + authenticated + rate-limited — what I8's every-route audit needs). Per-session token-bucket limiter → `CodeResourceExhausted` (HTTP 429 on connect), map bounded by active sessions.
- Dialect-free by construction: `EventParser` + `StoreReader` seams injected; module seal enforced by `seal_test.go` (no cmd/ or supervision imports; ABI schema + pkg/agentd constants only).

### agentd wiring (`sessionstate_wiring.go`, main, sidecar, server, tracker)
- opencode implementations of the seams at the wiring layer (package main, consistent with existing dialect handling): session.status flat+nested → contract event; `ListSessions` → store statuses.
- `sessionStatusTracker.onRawEvent` forwards raw payloads before dialect parsing.
- `startManagedProcess(supervise, tracker, authority)`: child start = generation change → tracker reset + async `Reseed(GENERATION_CHANGE)` (A9: the parent's hook is the authoritative signal).
- Boot reseed retries with backoff until opencode is reachable (opencode boots after agentd).
- Sidecar mode: same authority construction (sidecar is not opencode's parent — boot reseed only; the socket-crossed generation signal is noted for US-69.3/69.4).
- `serverDeps.stateAuthority`; user mux mounts the ABI handler subtree.
- Degraded-cursor policy (S1): missing/unwritable platform dir → loud WARN + non-durable temp-dir cursor — the surface must be additive and harmless (M4); at S2 the authority flag makes durability a boot requirement.

### platform/ PVC subPath (controller + init-fs)
- `init-fs --platform-subpath=create|skip|only` (default create): single-container platform-init (uid 1000) creates it; sidecar mode skips in platform-init and a new `platform-dirs` init (uid 2000) creates it — ownership follows the writer because cross-uid chown is impossible for non-root. Legacy `workspace-dirs` script creates it too (idempotent: existing PVCs gain it on next boot).
- Mount topology: single-container main container mounts `/platform` (documented uid-1000 weakening); sidecar mode — sidecar RW mount only, NEVER in the workspace container (mount-topology integrity per design 0055 M2).

### Tests (TDD — authority/service/seal tests written first)
`TestIngestFuzzRecover` (10k adversarial payloads incl. parser panics), `TestSeqMonotonicAcrossKill9` (hard-kill reopen: no published seq reused), `TestStampAtomicityRace` (-race), `TestReseedUnderActiveStreaming` (buffering, one reseed notice, no seq gap, fresh snapshot shows store truth), `TestReseedConvergesToStoreTruth` (restart with opencode alive), `TestReseedStoreFailureKeepsServing`, `TestStreamSnapshotFirstFrame`, `TestNoEventLossMidConnect` (exact contiguity from stamp+1 to final seq), `TestCursorFileFsyncPolicy`, `TestAuthEveryRoute401` (all five ops × none/wrong + valid-password-never-401), `TestRateLimitPerSession` (429 on burst, no cross-session leak), `TestWedgedOpencodeHotPaths` (blackholed store: stream + state within budget; failed reseed doesn't wedge), `TestEventsStreamReseedNoticeOnWire`, `TestModuleSealDependencies`, `TestPlatformSubPath_{SingleContainerLegacy,SingleContainerOverlay,SidecarTopology}`, `TestInitFS_PlatformSubPath_{Create,Skip_Sidecar,Only,ExistingPVC_Idempotent}`.

---

## Key Decisions

1. **fsync-before-publish cursor policy** (I1): seq persisted under the lock BEFORE fanout; a cursor write failure drops the event (integrity over availability). Group-commit documented as the measured-p99 optimization, mirroring I9's wording.
2. **Design correction recorded**: design 0055 M2 says platform/ "mode 0640" — directories need the x bit to traverse; implemented dir 0750, payload files 0600 (stricter than 0640; nothing else reads the cursor). Noted here per the design-amendment rule.
3. **Ownership follows the writer**: uid-2000 `platform-dirs` init in sidecar mode (non-root cannot chown); single-container stays uid-1000-owned with the design's documented weakening.
4. **Events live now, other four ops NotSupported**: the fanout machinery is this story's deliverable; endpoint semantics are explicitly US-69.4/69.7/69.9. Rate limits + auth + route presence are the I8 surface.
5. **`Stream` uses `context.WithoutCancel`**: the connection must outlive the caller's request-scope cancellation handling nuance — frames pump until the returned cancel func or stream close; documented at the call site.

## Assumptions (validated)

| # | Assumption | Validation |
|---|---|---|
| A1 | connect client stream API is `Receive()/Msg()/Err()` (v1.20) | pkg-cache source inspection; tests |
| A2 | The user mux can host the connect procedure subtree alongside REST routes | probe test; `TestAuthEveryRoute401` |
| A3 | `ListSessions` (opencode `/session`) reflects store truth for statuses | session_tracker fillGaps prior art (same source) |
| A4 | init containers run per boot → idempotent creation covers existing PVCs | `TestInitFS_PlatformSubPath_ExistingPVC_Idempotent` |
| A5 | fsync-per-event is acceptable for S1 event rates (cursor ~8 bytes) | 51s full -race suite incl. 10k-fuzz + 203-event flood; p99 budget revisit at US-69.6 measurement story |

## Blockers

None. Cross-uid behavior (uid-1000 EACCES on uid-2000 platform/) needs a real K8s fsContext to prove — covered by the kind e2e when the S1 scenario suite lands (US-69.5/#1139 note); unit level pins the pod-spec topology.

## Tests Run

- `go test -race -count=1 ./cmd/workspace-agentd/...` — PASS (agentd 179s, sessionstate 58s)
- `go test ./controller/internal/workspace/` — PASS (incl. updated footprint/overlay contract tests)
- `golangci-lint run --new-from-merge-base ./cmd/... ./controller/...` — 0 issues

## Next Steps

1. US-69.3 (#1137): contract projection & translation — widen `opencodeSessionEventParser` into full Epic 65 translation (parts/messages/inputs) behind the adapter seam; projection grows in-flight parts + pending inputs.
2. US-69.4 (#1138): implement GetSnapshot/Deliver(stub→ledger 69.7)/Act semantics + real capability report (provenance wiring from 0053 pins).
3. Wire `Metrics()` into ops_metrics gauges (US-69.12 formalizes alerts).

## Files Modified

- `cmd/workspace-agentd/sessionstate/{authority,cursor,service}.go` (new) + `{authority,service,seal}_test.go` (new)
- `cmd/workspace-agentd/sessionstate_wiring.go` (new)
- `cmd/workspace-agentd/{main,server,sidecar_mode,session_tracker}.go` (wiring)
- `cmd/workspace-agentd/init_fs.go` (+`init_fs_platform_test.go` new)
- `controller/internal/workspace/{pod_builder,platform_init,agentd_sidecar}.go` + `platform_subpath_test.go` (new); updated `platform_init_test.go`, `security_test.go`
