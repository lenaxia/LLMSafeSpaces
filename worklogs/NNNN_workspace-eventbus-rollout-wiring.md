# Worklog: workspace event bus rollout wiring (design 0072 — controller flags + sidecar env + helm values)

Task `tsk-weathered-urn-ambles` (epic: In-session reference patterns for
platform surfaces). Owner direction 2026-10-30: "Do it" — arm the
design-0072 workspace event bus for production: every agentd sidecar
receives the bus address, the notifier's delivery leg rides the
per-workspace Redis Stream (`workspace:{id}:events`), v1 point-to-point
stays the fallback.

## The handoff (honest resume note)

The implementation arrived ~95% complete as commit `bc4640f01` on this
branch (a `wip:` handoff from the charter session — all suites green
there: controller 68s, agentd 377s, lint 0). This worklog's session
VERIFIED the handoff diff against the design, then COMPLETED it — no
restart, no rewrite:

- **Verified preserved (byte-identical, see the history note below):**
  the agentd password-env composition, the controller flags +
  boot-validation + reconciler fields + sidecar env, the helm values +
  args wiring + render-fail guard, and all four handoff pins.
- **Added (the gaps the review would have found):** the
  `registerEventbusFlagsInto` seam (the relayFlags #1548 pattern — the
  handoff registered on the global FlagSet directly, untestable), the
  flag-surface pins (parse-into-returned-struct + defaults-dormant),
  the `parseEventbusWiring` boot-validation table (all seven arms:
  dormant default, authless-legal, NAME:KEY split, and the four
  refusals), and the helm chart render pins (criterion 3 was template
  code with NO Go pin — the repo pins render contracts in
  `helm/*_chart_test.go`).

## Delivered

### agentd (`cmd/workspace-agentd/workspace_eventbus.go`)

- `AGENTD_EVENTBUS_PASSWORD` composes onto host:port redis options —
  applied ONLY when the parsed options carry no credentials of their
  own (a `redis://user:pass@…` URL keeps what it embedded; both-set is
  a config smell the URL wins, silently and deterministically). This
  is the secretKeyRef spelling: the controller injects the password
  from the platform credentials secret so the address itself stays a
  plaintext host:port in the pod spec.
- Pinned: `TestEventbusPasswordEnvComposesOntoHostPort` — miniredis
  with `RequireAuth("sekrit")`: the right password wires the bus, a
  wrong password disables to nil (the composition is real auth, not a
  passthrough string — a half-authed client must never ride).

### controller (`controller/main.go`, `internal/workspace/`)

- `--workspace-eventbus-addr` + `--workspace-eventbus-password-secret`
  (NAME:KEY) flags via the `eventbusFlags` wrapper (the relayFlags
  pattern: keeps main() under funlen, one name for the flag surface);
  `registerEventbusFlagsInto` is the Into seam the #1548 orphaning
  regression pin parses against.
- Boot validation (`parseEventbusWiring`/`mustEventbusWiring`): a
  malformed NAME:KEY or a secret-without-addr REFUSES startup; an
  addr-without-secret is legal (an authless Redis) and logged. The
  posture: a bus address whose secret is missing would arm every pod
  with a doomed connection — refuse the misconfiguration up front
  rather than degrade N pods loudly.
- Reconciler fields `EventbusAddr`/`EventbusPasswordSecretName`/
  `EventbusPasswordSecretKey`; `buildAgentdSidecarContainer` composes
  `AGENTD_EVENTBUS_ADDR` (plain value — pod specs are readable,
  credentials NEVER ride it) + `AGENTD_EVENTBUS_PASSWORD`
  (secretKeyRef). Unconfigured = no eventbus env at all = the dormant
  pre-0072 posture, byte-identically.
- Pinned: the 3 sidecar env pins (composed / addr-only / absent-when-
  unconfigured) + the flag-surface and boot-validation pins above.

### helm (`helm/values.yaml`, `templates/controller-deployment.yaml`)

- `controller.workspaceEventbusAddr` +
  `controller.workspaceEventbusPasswordSecret` values (documented in
  values.yaml: never embed a password in the addr; the secret is
  NAME:KEY of an existing credentials secret), args wiring, and the
  render-fail guard: a password secret without an address refuses to
  render (install-time twin of the boot validation — belt and braces).
- Pinned: `helm/workspace_eventbus_chart_test.go` — 4 render pins over
  real `helm template` output (both-flags-verbatim, addr-only, absent-
  by-default, secret-without-addr fails with the guard's message).

## Intended production values (the talos-ops-prod bump is a SEPARATE follow-up — NOT this PR)

- `controller.workspaceEventbusAddr:
  llmsafespaces-valkey.llmsafespaces.svc.cluster.local:6379`
- `controller.workspaceEventbusPasswordSecret:
  llmsafespaces-credentials:redis-password`

(The Valkey requires auth; the API already uses this secret's
redis-password key.) This PR ships code + chart only; the values bump
rides the talos repo so rollout is a values change, reviewable alone.

## History note

The pushed handoff head was a `wip:`-messaged commit; this repo
merge-commits carry individual commits into main, so before opening the
PR the branch was re-committed as conventional commits with the
implementation TREE BYTE-IDENTICAL to the handoff (verified
`git diff bc4640f01 <impl-commit>` = empty) — preservation of the green
suites is provable, only the history shape changed.

## Verification (2026-10-30, this worktree; CI on the PR head)

- `go build ./...` → exit 0.
- `go test -count=1 ./controller/...` → all ok (workspace 68s incl.
  the 3 env pins; the main-package flag/boot pins green).
- `go test -count=1 -run 'TestEventbus|TestRunEventbusConsumers'
  ./cmd/workspace-agentd/` → ok (the full 377s agentd suite was green
  at the handoff head; the package is untouched since — the password
  pin re-run here confirms).
- `go test -count=1 ./helm/...` → ok 43.6s (helm v3.16.4 on PATH; the
  4 new eventbus render pins PASS, no skips).
- `golangci-lint run ./controller/... ./helm/...` → 0 issues;
  `make fmt-check` → clean.
