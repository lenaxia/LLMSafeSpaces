# Worklog — #1959b: bounce re-dispatch delivery seam — V1, not the dead V2 queue

Lane: `lane-1959b` (orchestrator novel-nutmeg-verifies). Issue: #1959 residual on
v0.44.2 — root cause comment 6420872512, ruling 6405969037, live-verification
residual 6420212397 (all three read in full before starting; not re-litigated).

## 1. Validate-first (born RED, at the WIRING level)

The gap the orchestrator named: the `TestBounce1959_*` pins faked the adapter, so
they could not see the HTTP route the production seam actually rode. I rebuilt
the evidence at the wiring level before touching the delivery:

- **Stub opencode mirroring the 1.18.x floor**: V1 `POST /session/:id/message`
  answers; the V2 prompt route `POST /api/session/:id/prompt` 404s (the route
  group is deleted in 1.18.10 — #755). Production `opencode.Adapter` pointed at
  it via static pod-IP/password resolvers + `WithAdapterPort`.
- **Constructor refactor enabler** (commit ea3601dd2): `newBoardMessenger` /
  `newBoardRedispatcher` take an injected `boardScopeResolver` + the adapter
  instead of the pool — the production closures are now drivable end-to-end
  without a database. app.go call sites updated; delivery logic UNCHANGED
  (still SendAsync) in that commit.
- **Born RED** (captured verbatim, the live 0.44.2 fallback signature):

  ```
  Error: "agent http status: agent V2: session not found: session ses_original: HTTP 404"
  Messages: the re-dispatch must deliver over the LIVE V1 route — an error here
            is the dead V2 queue (#755) firing the fresh-worker fallback
  --- FAIL: TestBoardWorkerComms_Redispatcher_RidesV1NotTheDeadV2Queue
  --- FAIL: TestBoardWorkerComms_Redispatcher_DeadSession_FastFailKeepsFallback
  --- FAIL: TestBoardWorkerComms_Redispatcher_SlowTurnIsInFlight_NotCallerBlocked
  --- FAIL: TestBoardWorkerComms_Messenger_HaltAndResume_RideV1Detached
  ```

  All four red for the SAME root cause the live probe hit: a live V1 endpoint
  sits right next to the dead V2 route the seam insisted on. (The scope-error
  pin passed pre-fix too — the scope leg was never broken.)

## 2. Fix (commit e2046badc)

`api/internal/app/board_worker_comms.go`:

- **`deliverBoardTextV1`** — the shared delivery core: `adapter.Send` (V1
  `POST /session/:id/message`, `adapter.go:341`) fired in a detached goroutine
  with `context.WithoutCancel(ctx)` + a 30min budget. This is the
  `llmsafespaces_send_message` discipline (cmd/workspace-agentd/mcp_tools.go: the
  V1 POST queues server-side on a busy target and lands at the turn boundary —
  live-proven run-at-boundary semantics; WithoutCancel so the delivered turn
  survives the caller's request). The alternative — a synchronous Send in the
  caller's ctx — was rejected on the codebase's own context discipline
  (pkg/board engine.go:491/:600 deliver via detached Background goroutines by
  design): the verdict request would block on all of Round 2, and its
  cancellation would kill the re-dispatched turn mid-flight.
- **Redispatcher**: detached V1 Send + a **bounded fast-fail wait**
  (`boardV1FastFail` = 10s; a dead/reaped session 4xx's in well under a
  second). Error within the window → returned synchronously → pkg/board's
  fresh-worker fallback fires (decision tree untouched, per instructions).
  Window expiry or caller-ctx end → nil (in-flight; lease lapse heals a silent
  death — never a fallback on a timing artifact).
- **Messenger** (same-disease audit item): pure fire-and-forget (fastFail=0) —
  the pause/resume request never blocks on any worker's turn; delivery failures
  surface as the detached goroutine's warn line (every pkg/board caller already
  discards the messenger error).
- **`boardV1Sender`** — the constructors' dependency narrowed to an interface
  exposing ONLY `Send`. `agent.Adapter` satisfies it structurally
  (`var _ boardV1Sender = agent.Adapter(nil)`); the dead `SendAsync` path is
  now a COMPILE ERROR away, the wiring pin the fakes could not give us.
- **`SendAsync` deprecation** (pkg/agent/opencode/adapter.go + the
  pkg/agent/adapter.go interface doc): NOT deleted — remaining production
  callers enumerated in place: (1) `proxy_lifecycle.go:353`, the
  env-gated `OPENCODE_V2_DELIVERY` outbox regime (off by default; requires
  opencode ≥ 1.18.15 where the queue drains), (2) the `systemnotices`
  pass-through wrapper (reachable only through that regime).

Deliberately untouched: pkg/board's decision tree (redispatchBounce fallback
logic, nudge semantics, bounceTx), the Aborter seam (already V1), #1958 RunID
discipline, the #1119/#987 verify machinery.

## 3. Pins

- `TestBoardWorkerComms_Redispatcher_RidesV1NotTheDeadV2Queue` — V1 hit exactly
  once with the re-dispatch text (token included), V2 hit ZERO times, scope
  resolved through the task ref. Born RED (the dead-route error).
- `TestBoardWorkerComms_Redispatcher_DeadSession_FastFailKeepsFallback` — V1
  404 (dead session) → synchronous error naming the V1 route, within the
  window: the engine fallback contract preserved.
- `TestBoardWorkerComms_Redispatcher_SlowTurnIsInFlight_NotCallerBlocked` — a
  600ms-pending POST vs a 100ms window: seam returns nil in <500ms; the caller
  cancels its ctx; the detached delivery still completes (WithoutCancel
  discipline pinned, not just implemented).
- `TestBoardWorkerComms_Messenger_HaltAndResume_RideV1Detached` — both
  directives land on V1 (never V2) with the correct PAUSE/RESUME texts; the
  messenger call itself returns immediately.
- `TestBoardWorkerComms_ScopeErrorPropagatesToFallback` — scope failure is a
  synchronous error with zero delivery attempts.

Mutation-verified, then restored (suite green after restore):
- **Old wiring restored** (constructors re-widened to agent.Adapter +
  SendAsync): all four delivery pins RED — the discriminator this PR adds.
- **Detachment dropped** (`return <-done` — synchronous delivery):
  SlowTurnIsInFlight RED ("the caller must return at the fast-fail window, not
  wait out the turn") — the context-discipline is pinned, not incidental.
- **Compile-time pin demonstrated live**: the revert mutation could not even
  reference SendAsync through the narrowed interface without re-widening the
  constructor signature — the regression class is now a build failure.

## 4. Suites

- `go build ./...` — clean. `go vet ./api/... ./pkg/agent/... ./pkg/board/` — clean.
- `golangci-lint run api/internal/app/... pkg/agent/...` — 0 issues (misspell ×2
  and a staticcheck QF1008 in the new test fixed en route).
- `go test -short ./...` — green.
- `go test -race ./api/internal/app/... ./pkg/agent/... ./pkg/board/` — green.
- pkg/board integration suite not re-run here (no code path in pkg/board
  changed; the seams' pkg/board-side contract is exercised by the existing
  `TestBounce1959_*` suite unchanged, and the api-side wiring is covered by the
  new pins).

## 5. Notes

- Memory stagger honored: waited out >6.0e9 readings before each heavy command
  (readings proved to be reclaimable page cache; host `available` 98G; `ps`
  confirmed no co-running heavy process before proceeding serialized).
- A pre-existing foreign stash (`stash@{0}: On main-tmp: pre-1962-merge…`) was
  observed and NOT touched.
- Fast-fail window 10s rationale: dead-session 4xx arrives in <<1s even with
  k8s resolve; generous headroom keeps the fallback arm correct under slow
  resolves while bounding the reviewer's verdict request. Package vars so tests
  bound them.
- Live re-verification (the owner-side probe showing the re-dispatch turn
  arriving in the ORIGINAL session's transcript) is post-merge validation —
  this lane ships the seam fix + wiring pins.
