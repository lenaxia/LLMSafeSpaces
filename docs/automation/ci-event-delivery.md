# CI event delivery (#1844): the synchronize-drop fault class

The repo's PR automation (CI, Secrets Integration, Security scan, PR
Review) triggers on `pull_request` events. Since 2026-10-22 (#1844),
GitHub has INTERMITTENTLY failed to deliver `pull_request.synchronize`
to Actions: a pushed PR head gets zero workflow runs — not queued, not
pending, ABSENT — while other event types on the same repo keep
firing. This page is the evidence register for the class, the config
audit that rules our side out, and the mitigation inventory.

## The 2026-10-29 window (19:28–21:25Z) — evidence register

All times UTC, 2026-10-29. "Runs" = `pull_request`-event workflow
runs on the exact head SHA (`GET /repos/…/actions/runs?head_sha=`).

### Dropped heads (zero pull_request runs; recovery in parens)

| push | PR | head | outcome |
|---|---|---|---|
| 19:28:25 | #1983 | `616f8b9c` | zero runs (manual dispatch 19:35:17Z) |
| 19:56:47 | #1983 | `882398c3` | zero runs (manual dispatch 19:57:19Z) |
| 20:21:23 | #1984 | `6437426f` | zero runs (manual dispatch 20:30:23Z) |
| 20:30:07 | #1983 | `3353d878` | zero runs, dark 42 min (manual dispatch 21:12:44Z) |
| 21:24:12 | #1984 | `c455f193` | zero runs of ANY event — never recovered |

Reviewer corroboration: #1983 round-2 (20:06:50Z bot comment) —
"No CI exists on this head (`gh pr checks` empty)"; #1984 round-2
(21:44:48Z) — "the only branch CI run (SUCCESS) is pinned to the
*prior* head — not evidence for this head".

### Healthy controls (same repo, interleaved)

| time | PR / branch | head | outcome |
|---|---|---|---|
| 18:01:23 | #1983 r1 | `227bb647` | 5 pull_request runs |
| 21:23:46 | #1985 (opened) | `db1de1aa` | 4 runs in 3s of the `opened` event |
| 21:46:29 | #1984 merge-of-main | `ca839042` | 4 runs immediately |
| 21:47:35 | #1983 merge-of-main | `21c1acb7` | 6 runs immediately |
| 23:30:27 | #1986 r1 | `87df47c7` | 4 runs in 23s |
| 00:25:36 | #1986 r2 | `a00e1247` | 4 runs in 8s |

### Other event types delivered DURING the drop window

- `issue_comment` → AI Commands ran at 19:31:09, 20:30:13, 20:30:20,
  21:24:18.
- `schedule` → Run Watchdog 19:39:30, Renovate PR Analysis 20:47:19.
- `workflow_dispatch` → the four manual CI recoveries above.
- `pull_request.opened` → #1985 at 21:23:49, inside the window.

### Reading

The window's shape is event-type-scoped, not repo-wide, not
branch-scoped: `synchronize` deliveries between ~19:28Z and ~21:25Z
were lost upstream of Actions (five of five pushes), while
`issue_comment`, `schedule`, `workflow_dispatch`, and at least one
`opened` delivered normally in the same minutes. Everything before
18:0x and after 21:46 fired. The fifth head (`c455f193`) additionally
shows the loss can persist past the operator's patience: it was never
recovered and its review proceeded on locally-executed suites.

GitHub's status page records no Actions incident for the window (only
a Copilot Cloud `minor`, 10:19Z) — consistent with the earlier #1844
instances: this degradation class is unlisted, partial, and
event-type-specific.

## Why it is not our workflow config

Audited 2026-10-30 across the four PR-triggered workflows
(`ci.yml`, `secrets-integration.yml`, `security-scan.yml`,
`pr-review.yml`):

- **paths filters: none exist** on any `pull_request` trigger (and the
  dropped PRs were code PRs touching `pkg/`, `helm/`, `docs/`).
- **branch filters**: `branches: [main]` on the PR BASE everywhere;
  every dropped PR targeted main.
- **concurrency cannot erase runs.** `cancel-in-progress` groups
  produce runs that are CANCELLED-but-visible in history. Live-proven
  2026-10-30 06:47Z: two probe pushes 14s apart → the superseded head
  `5c6bf40f` shows four runs `conclusion=cancelled`; the dropped heads
  showed ZERO runs. Absence ≠ concurrency.
- **no trigger indirection**: pr-review.yml triggers on
  `pull_request` directly (no `workflow_run`/`issue_comment` chain to
  break). The `issue_comment` → ai-comment.yml path is the `/review`
  command lane, which demonstrably worked through the window.
- **the same workflow files ran fine on `workflow_dispatch` during the
  window** — same refs, same `on:` blocks, different event type.

The decisive shape: on each dropped head, all FOUR workflows — four
independent `on:` blocks — produced zero runs. No per-workflow config
fault produces that; only a lost event does.

## The 2026-10-30 probe (post-window health + tracing method)

Scratch branch `diag/1844-sync-probe-20261030`, PR #1991 (closed after
probing; empty commits only):

- PR opened 06:45:20Z → 4 workflow runs created 06:45:25Z (5s).
- Empty-commit synchronize pushed 06:47:07Z → 4 runs at 06:47:12Z (5s).
- Second push 06:47:21Z → 4 runs at 06:47:27Z (6s); the prior head's
  runs visible as `cancelled` (the concurrency control above).

Tracing one-liner for future instances:

```sh
gh api "repos/lenaxia/LLMSafeSpaces/actions/runs?head_sha=<full-sha>" \
  --jq '.workflow_runs[] | [.created_at, .event, .name, .conclusion] | @tsv'
```

Zero rows with `event=pull_request` on a head older than ~10 min =
this class; go to the playbook below.

## What is GitHub's (the residual)

- The webhook-delivery / Actions-ingestion audit trail for
  19:25–21:45Z 2026-10-29 is owner-side (org settings → webhooks);
  it is not readable by the repo PAT (403/404). **Owner action**: ask
  GitHub support to reconcile `pull_request.synchronize` deliveries
  for this repo in that window against run creations; cite the table
  above and #1844's earlier instances.
- **Scheduled events are ALSO being dropped at a high rate**: Run
  Watchdog's `*/10` cron yields 5–15 runs/day against 144 nominal
  ticks (measured 2026-10-23→30). This starves the #1844 mitigation
  itself — detection latency is bounded by sweep cadence, and cadence
  is GitHub's, not ours. Worth the same support ticket.

## Mitigation inventory

| lever | where | notes |
|---|---|---|
| run-watchdog sweep | `run-watchdog.yml` | every 10 min nominal + on every PR comment (`issue_comment` — delivered through the drop window); faults at ≥10 min dark, posts evidence comment, re-dispatches CI once per head SHA |
| dispatch recovery | `pr-review.yml` job | manual `workflow_dispatch` lever; posts the #1844 fault comment, re-dispatches CI |
| `ci: retrigger` | git | tree-identical empty commit once events flow again |
| playbook | below | for humans staring at a check-less PR |

### Playbook: a pushed head shows zero checks

1. Confirm with the tracing call above (not the PR UI, which collapses
   absent and pending).
2. If `total_check_runs == 0` and the head is >10 min old:
   `gh workflow run pr-review.yml --ref <branch>` (dispatch recovery:
   comment + CI re-dispatch), or the minimal form
   `gh workflow run ci.yml --ref <branch>` plus an admitted `/review`
   comment.
3. Post the instance (PR, head, window) to #1844 — instance count is
   the evidence GitHub support will ask for.
