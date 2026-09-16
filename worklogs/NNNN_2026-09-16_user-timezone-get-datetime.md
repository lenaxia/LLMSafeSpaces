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
   ABSENT (not faked) when unknown.

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
