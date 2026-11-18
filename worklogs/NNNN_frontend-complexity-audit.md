# Worklog: frontend complexity audit — read-only, reports/audits/frontend-complexity.md

**Task:** tsk-pallid-lavender-supports (board story) — read-only complexity
assessment of `frontend/src`; deliverable is the findings document; NO code
changes.

**Worktree:** `/tmp/opencode/frontend-audit-wt`, branch `audit/frontend-complexity`
off `origin/main` @ `bd56d8464`.

## What was audited

- **ChatPage.tsx** (1,467 ln) — full read; 10 inline state machines inventoried
  (optimistic reconcile, stream buffer + echo gate, stuck-session auto-abort,
  context/compaction, prompt fold-sync, queue-vs-direct send, duplicated reset
  effects, banner zoo, auto-rename, auto-create/deletion navigation). Spec-file
  count used as the complexity metric: 21 spec files / 6,962 test lines
  (base spec included — the count rule is applied uniformly across surfaces),
  plus
  the `hookcount` meta-spec that statically scans source for hook-order safety.
- **Provider tree** — SessionActivityProvider (2,346 ln; ~1,050-ln SSE handler
  closure; busy-lease/flight/tombstone machines; queryCache reach-in; fresh
  context identity per render → all-consumer re-renders), useUserEventStream
  (invalidation hub + watchdogs), AppShell/App wiring.
- **Board surfaces** — BoardPage.tsx (2,629 ln; 49 useState; rail/kebab/drawer/
  focus/drag/SSE/filters/scope/CRUD machines; ~950 unextracted lines incl.
  StageRail/StageCell; git archaeology of the 5-day fix cluster) +
  components/board ownership split + boardRules leakage + re-render chain.
- **Settings/org/platform tiers** — pairwise diffs + history of the credential
  trio (779/533/541 ln; ~50-60% duplicated; 4 shipped divergences incl.
  unconfirmed org-tier delete), OrgSettingsTab name collision, agent-config
  pair, audit tabs, paged-list scaffold; McpServersTab/WorkspaceImagesTab
  identified as the in-repo scope-prop exemplars.
- **The v0.15 refresh bug** (`dc5632044`) analyzed as a shape lesson: frontend
  exonerated, but the staleness/liveness concern is smeared across three
  transport watchdogs with no domain-freshness owner (recorded as finding
  FC-9, deliberately excluded from refactor candidates — needs product input).

## Deliverable

`reports/audits/frontend-complexity.md` — findings matrix FC-1..FC-9
(origin-traced, costed, simplification + functional-equivalence arguments),
per-area deep dives, Top 5 candidates ranked by value/risk.

## Verification

- `make fmt-check` green (gofmt-only target; docs-only change).
- No production code touched; the only tree changes are this worklog, the
  report, and the CHANGELOG entry.
