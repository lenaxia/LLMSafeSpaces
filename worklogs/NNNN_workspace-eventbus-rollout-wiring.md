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

## Review rounds (AI reviewer, pr-review.yml)

### Round 1 — CHANGES_REQUESTED (head c887d9fc2)

The verdict: the envtest suite uncompilable on the exact tree, and —
the fundamental one — the arming wiring a functional NO-OP in both pod
topologies. Both correct; the handoff's "all suites green" was true
only for the surfaces its session ran, and its env injection armed a
process that never read it.

- **Finding 1 (blocking, envtest uncompilable)** — `SetupControllers`
  gained 3 params but the `//go:build envtest` caller was not updated;
  the envtest TAG compiles the file out of plain `go test ./...`, which
  is exactly how both the handoff's and this session's verification
  missed it. FIXED: the dormant caller updated (+ a comment on why the
  tag hides it), and — the armed-posture pin the reviewer asked for —
  `TestEnvtestSetupWiring_EventbusArmsPodEnv` drives the FULL
  production plumbing (flags → SetupControllers → reconciler →
  buildPod) against a real apiserver and asserts the pod's MAIN
  container carries both env vars in their exact shapes. It lives in
  its OWN package (`controller/internal/controller/eventbuswiring/`)
  for a mechanical reason: controller-runtime v0.20's controller-name
  registry is process-global and never unregisters (pkg/controller/
  name.go checkName) — a second SetupControllers in the same test
  binary collides ("controller with name workspace already exists").
  New CI step in envtest.yml runs it (the #820 drift class — a test
  file no workflow executes — is why the step is explicit).
- **Finding 2 (blocking, the wiring armed nothing)** — two legs:
  (a) single-container mode (the helm DEFAULT): agentd runs in the
  MAIN container, and pod_builder.go had zero eventbus wiring;
  (b) sidecar mode: the env landed in a container whose process
  (`agentd --sidecar`) never calls eventbusFromEnv — the consumers
  lived only in the single-container boot path. FIXED both: a shared
  `eventbusEnv()` reconciler helper appended in pod_builder.go (main
  container) AND agentd_sidecar.go (the #1332 both-modes convention
  the reviewer cited), and `wireSidecarNotifierBus`
  (cmd/workspace-agentd/sidecar_eventbus.go) runs the SAME
  notifier + bus-consumer wiring in the sidecar process — which also
  closes the latent 0071 gap (the sidecar serves the MCP subscription
  surface but never ran its delivery engine). Pinned: the main
  -container buildPod pins (composed / absent-when-unconfigured) and
  the sidecar wiring pins (armed-from-env / nil-bus-when-unarmed /
  notifier-always-wired).
- **Security (the credentialess invariant was prose-only)** — an
  operator passing `redis://:password@host` got that string into every
  pod spec AND the controller's authless-arm log line, raw. FIXED by
  ENFORCING rather than documenting: the controller boot guard refuses
  any "@"-bearing address (userinfo requires "@" in every spelling; no
  legitimate Redis coordinate — host, host:port, redis:// URL, IPv6
  literal — contains one), the helm chart carries the same check as a
  render failure, and the agentd side already redacted (round 1 of
  PR #1979). Pinned in the parseEventbusWiring table (credentialed
  URL / bare user:pass@host / both-with-secret / IPv6-legal arms) and
  the chart suite (three credentialed spellings fail render with the
  guard's message).
- **Robustness** — the reconciler comment claimed the secret key is
  "validated at startup"; boot validation checks NAME:KEY SYNTAX only
  (no cluster lookup — a typo'd name surfaces per-pod as
  CreateContainerConfigError, visible not silent). Comment corrected
  to the honest posture. And the both-set config smell (credentialed
  URL + password env) now WARNS instead of winning silently — pinned
  (`TestEventbusPasswordBothSetWarnsAndURLWins`, zap observer: the
  warn fires and the URL's credentials still wire).
- **Minor** — the dead `if addr == ""` branch in parseEventbusWiring
  (both arms returned the same) restructured away.
- **Alignment** — design/0072's "Affects" line said "No proto/ABI,
  controller, or frontend changes"; amended with the rollout note
  (status line + parenthetical) so the doc matches what shipped.

Not introduced but documented (found while verifying): an UNFILTERED
`-race -tags envtest ./controller/internal/controller/` run fails on
origin/main too — a pre-existing race between controller goroutines
sharing a test logger buffer in a shape CI never executes (every
envtest step is `-run`-filtered; both filtered runs green here with
-race).

## Verification (2026-10-30, this worktree; CI on the PR head)

- `go build ./...` → exit 0.
- `go test -count=1 ./controller/...` → all ok (workspace 68s incl.
  the sidecar + main-container env pins; the main-package
  flag/boot pins green incl. the credentialess-refusal arms).
- `go test -count=1 -race ./cmd/workspace-agentd/` → ok 423.6s (the
  full agentd suite with the sidecar wiring + both-set warn pins; the
  handoff's 377s baseline was without -race).
- `go test -count=1 ./helm/...` → ok 37.4s (helm v3.16.4 on PATH; the
  eventbus render pins incl. the credentialed-addr refusals PASS, no
  skips).
- envtest (CI shapes, KUBEBUILDER_ASSETS 1.35.0, -race):
  `./controller/internal/controller/ -run TestEnvtestSetupWiring` →
  ok 7.4s; `./controller/internal/controller/eventbuswiring/` → ok
  8.4s (the armed-posture pin).
- `golangci-lint run ./controller/... ./cmd/workspace-agentd/...
  ./helm/...` → 0 issues; `make fmt-check` → clean.
