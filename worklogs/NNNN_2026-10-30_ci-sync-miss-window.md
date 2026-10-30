# CI synchronize-miss window 2026-10-29: evidence register, config clearance, watchdog upgrade

2026-10-30 · branch `ci-sync-miss-diagnosis` · tsk-shaded-sturgeon-ambles

## §1 The problem

Four timestamped incidents in the task record (PRs #1983/#1984/#1986,
19:05–23:1xZ) of pushes producing ZERO workflow runs — absent, not
pending — against interleaved pushes that fired in seconds. The task:
audit our `on:` config for swallow conditions, pull GitHub's delivery
evidence, map every incident to a cause, fix ours, document theirs,
and land-or-defer a dead-man's check.

## §2 The evidence (API-level, per head SHA)

Method: `actions/runs?head_sha=<oid>` grouped by event — the run
registry IS the delivery record at the Actions boundary.

Dropped (zero `pull_request`-event runs on the head): #1983
`616f8b9c` 19:28:25 (dispatch 19:35:17), `882398c3` 19:56:47
(dispatch 19:57:19 — NOT in the task record, a fifth instance),
`3353d878` 20:30:07 (dark 42 min, dispatch 21:12:44); #1984
`6437426f` 20:21:23 (dispatch 20:30:23), `c455f193` 21:24:12 (zero
runs of ANY event — never recovered; the reviewer accepted
locally-executed suites at 21:44:48).

Healthy controls: #1983 `227bb647` 18:01 (5 runs); #1985 `db1de1aa`
opened 21:23:46 → 4 runs in 3s INSIDE the window; merge-of-main
synchronizes 21:46/21:47 fired immediately; #1986 all four heads
23:30→02:23 fired in 8–23s.

In-window deliveries of OTHER event types: issue_comment (AI Commands
19:31:09, 20:30:13, 20:30:20, 21:24:18), schedule (watchdog 19:39:30,
renovate 20:47:19), workflow_dispatch (the recoveries), and
pull_request.opened (#1985).

Mapping the task's four: (1) #1983 round-2 = `616f8b9c` — CONFIRMED
drop (task's 19:05:48Z was the local commit; the push landed 19:28:25
per committer date and the 20:06:50 reviewer comment). (2) #1983
final = `3353d878` CONFIRMED, 42-min dark. (3) #1984 creation =
`6437426f` CONFIRMED. (4) #1986 round-1 "no CI on head" = RECORD
ERROR — #1986 was created 23:30:46 and `87df47c7` fired all four
workflows in 23s; the quoted "No CI exists on this head" is #1983
round-2's reviewer text (20:06:50). Documented, not papered over.

Verdict: GitHub-side, event-type-scoped synchronize loss,
~19:28–21:25Z, ending by 21:46. Status page: no Actions incident
listed for the window.

## §3 Config clearance (the audit)

- paths: NO `pull_request` paths filter exists in ANY of the four
  PR-triggered workflows.
- branches: base [main] everywhere; every dropped PR targeted main.
- concurrency: cancel-in-progress produces CANCELLED-VISIBLE runs,
  never absence — live-proven by the probe (superseded head `5c6bf40f`
  carries four `conclusion=cancelled` runs).
- pr-review.yml: no workflow_run/issue_comment indirection in its
  trigger; the /review lane (ai-comment.yml) worked through the window.
- Decisive shape: all FOUR independent `on:` blocks produced zero runs
  on each dark head while the same files ran on workflow_dispatch in
  the same minutes. Config cannot do that; a lost event does.

## §4 The probe (PR #1991, closed; branch deleted)

opened 06:45:20 → 4 runs 06:45:25 (5s). synchronize 06:47:07 → 4 runs
06:47:12 (5s). Second push 06:47:21 → 4 runs 06:47:27; prior head's
runs visible-cancelled. Suppression is not reproducible on demand
(GitHub-side); the probe validates current health, the tracing method,
and the concurrency control.

## §5 What landed

- `docs/automation/ci-event-delivery.md` — the evidence register,
  clearance, probe, residual (owner-side webhook log + support-ticket
  asks incl. measured schedule scarcity: 5–15 of 144 daily cron ticks
  delivered 2026-10-23→30), mitigation inventory, and the operational
  playbook.
- `run-watchdog.yml` — the dead-man's check UPGRADED, not deferred:
  threshold 15→10 min (healthy latency measured 5–50s; #1983's final
  head sat dark 42 min) and an `issue_comment: [created]` trigger (the
  delivery lane that demonstrably flowed while synchronize was lost);
  recursion-safe via the GITHUB_TOKEN exemption.
- `tests/ghaworkflows/run_watchdog_test.go` —
  TestRunWatchdogTriggerSurfacePinned (trigger surface + threshold +
  cron pins).
- CHANGELOG entry; this worklog; a #1844 instance-accumulation comment
  (the documented protocol).

## §6 Verification

`make fmt-check`; `go test ./pkg/runwatchdog/... ./tests/ghaworkflows/...`
(all green, incl. the new pin and the wiring-contract round-trip).
