# Worklog: user timezone — live browser zone for get_datetime (agentd + API + frontend)

**Date:** 2026-09-16
**Session:** opencode (main dev box). Owner request: get_datetime reported
pod time (UTC), not the user's; the browser knows the exact zone and
GeoIP was explicitly rejected (VPN inaccuracy, country granularity,
privacy, infra cost — the browser is authoritative and free).

## Design

`get_datetime` resolves the user's zone in priority order and reports
which source won:

1. **`argument`** — an explicit IANA `timezone` arg. The caller-owned
   path: SDK/MCP integrations (no browser ever connects — they leave it
   to the caller, per the owner's framing) and agents acting on
   zone-from-context.
2. **`browser`** — the platform's live push: frontend reports
   `Intl.DateTimeFormat().resolvedOptions().timeZone` as the user
   `timezone` setting; the API pushes it to the pod's agentd over the
   control plane. The push fires on (a) every SSE workspace connect
   (the reliable re-delivery path — fires for every browser session,
   including zone changes mid-life via the 60s re-report) and (b) the
   setting-PUT hook (catches mid-session changes for already-active
   pods). Failures are latency-only by design.
3. **`pod`** — TZ env or UTC; the honest fallback, `timezone` name
   ABSENT (omitempty on the typed result struct — verified by
   `assert.NotContains` pins; an earlier draft emitted `""`, fixed in
   review round 2).

The agentd delivery image is FROM scratch — `time/tzdata` is embedded
so arbitrary IANA zones resolve without a filesystem copy.

## Why not TZ-at-spawn

A snapshot of the browser's zone at pod build — stale on travel until
the next resume, and wrong-headed (copies browser state into pod state
when the live value is one push away). Considered and rejected in
design; the live push is the primary, TZ-at-spawn would only ever be a
secondary for pod-wide surfaces (log timestamps) and is NOT built.

## Components

- **agentd**: `cmd/workspace-agentd/user_timezone.go` — atomic zone +
  `POST /v1/user-timezone` (§D1 carve-out credential pair, IANA
  validation via embedded tzdata, 256-byte body cap); route wired in
  `server.go`; `get_datetime` gains the optional `timezone` arg +
  resolution order + `source` field; `time/tzdata` import in main.
- **API**: `agentpush.Service.PushUserTimezone` (mirrors the Notify
  transport: pod-IP resolve, workspace-password Basic, latency-only
  failure class); `StreamEvents` fires the async best-effort push on
  SSE connect (`pushUserTimezoneOnConnect`); settings handler runs the
  post-PUT hook for the `timezone` key; app.go wires both (the hook
  enumerates the user's Active/Creating/Resuming workspaces and pushes
  to each). Settings schema: user `timezone` key (IANA pattern, empty
  default valid), SchemaVersion 14 → 15.
- **frontend**: `useTimezoneReporter` hook — reports on login, re-reports
  on zone change (60s poll: an Intl format + string compare), retries
  on failure; mounted via `<TimezoneReporter />` in App.

## Assumptions validated

1. `Intl...resolvedOptions().timeZone` returns IANA names — standard
   Ecma-402 behavior; the agentd side validates with LoadLocation so
   garbage is refused at both ends (schema pattern + handler).
2. The §D1 carve-out pair covers the API-driven push — same credential
   contract as every user-mux route the API drives (resync precedent).
3. SSE connect is the right push trigger — every browser chat session
   opens one; SDK/MCP callers do not (by design — they get
   argument/pod).

## Review round 4 remediations

- The round-3 `PW2` alias removal left three usages unbound — 5 of 6
  timezone probes were silently dead (`set -u` made each `$PW2` curl
  exit without executing; the parent `|| note 1` then failed them).
  Repaired to `$PW` throughout; reviewer stub-repro confirmed 6/6.
- Three get_datetime probes grepped the RAW /v1/mcp body for
  `"source":"browser"` — but the tool result is a JSON string nested
  in content[0].text (escaped quotes on the wire): all three were
  permanent false negatives. Replaced with a double-layer parse
  (envelope → content[0].text → fields).
- Test-plan "10/10" rows corrected: 10 pass on pre-#1389 agentd; the 6
  timezone probes auto-skip until this PR's agentd deploys.
- Frontend: zone-change re-report pinned (injectable `browserTimezone`
  seam, scoped fake interval timers — full-fake broke React's mount
  scheduler, order documented as load-bearing); once-per-session
  dedup-half pin added.
- `(nil,nil)` lister pin added (real discriminator: dropping the guard
  panics); `got.userID` in the StreamEvents pin.

## Review round 3 remediations

- Liveprobe header corrected: the script is NOT read-only (the timezone
  leg persistently overwrites the pod's last-known zone; an
  already-connected browser will not re-push until its next connect —
  the "harmless" rationale was wrong for active sessions).
- `fanOutTimezonePush` nil-result guard (lister returning (nil, nil)
  no longer panics; logged as a list failure).
- Weak duplicate `assert.Empty` pod-fallback pin replaced with the
  binding `assert.NotContains`.
- `userTimezone()` loaded once (`switch tz := userTimezone();`).
- **CI-gated tzdata tripwire**: `TestTimeTzdataImported` runs
  `go list -deps` in the agentd package and fails if `time/tzdata`
  leaves the graph — the removal was invisible to every automated gate
  (runners carry system tzdata; the live-pod leg is manual). This is
  the static gate for the named invisible failure; a full kind e2e
  (browser→setting→push→pod on CI hardware) remains a documented
  follow-up.

## Review round 2 remediations

- tzdata import actually placed in `main.go` (round 1's was
  accidentally absent — the reviewer's scratch-context repro caught the
  feature resting on undocumented base-image inheritance).
- Route-level pin (`/v1/user-timezone` through `buildUserMux`) and
  StreamEvents call-site pin (`TestStreamEvents_FiresTimezonePushOnOpen`,
  the ArmsUsageGateOnOpen precedent).
- `fanOutTimezonePush` extracted to a testable function — phase filter,
  per-pod-failure continuation, list-failure and nil-collaborator pins.
- Typed result struct with omitempty timezone; `assert.NotContains`
  pins the absent-key contract. Handler param names aligned with deps.
- L3 liveprobe: 6 timezone probes (auto-skip on pre-#1389 agentd).
- Frontend retry-on-failure test (verified discriminator).

## Tests

- agentd: resolution-order matrix (pod/browser/argument/unknown-zone),
  handler auth+validation matrix (401, bad zone, opencode cred,
  control-plane cred), dispatcher arg test, guidance pins updated
  (source naming, IANA example, do-not-assume).
- API: `PushUserTimezone` transport pins (route/auth/body, empty-zone
  no-op, pod-error surface); SSE-connect push (fires, empty-zone
  no-push, nil-seams no-panic); settings hook (fires for timezone key
  only, PUT still 200).
- frontend: hook test (reports once per session) + `tsc --noEmit` clean.
- Full suites: agentd 260s ✅, handlers 72.4s ✅, agentpush ✅,
  settings ✅.
