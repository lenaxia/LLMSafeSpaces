#820 CLOSURE DRAFT — fill-in-and-post when the drill+sweep nightly runs green
================================================================================

STATUS: scratch/prep — DO NOT POST until the evidence slots are filled with a
real green run. Source checklist: the orchestrator's reopen comment
(issuecomment-5787129226). Standing blocker: PR #1555 (the #1550 successor,
branch fix/rbac-namespace-scope-cache-scoping-v2 @ cb353c54 — the namespace-
scope cache-scoping derivation the drill-shape step needs) awaits event-pipe
recovery + bot review + merge BEFORE the nightly can pass the drill-shape step.
CI is proven green on the record (dispatched run 35936198384, 01:17:50Z).

--------------------------------------------------------------------------------
## (a) The completion-criteria checklist (copy from the reopen comment; evidence slots inline)

- [x] US-72.6 code merged: #1537 (scrub + sweep script + condition/event mirror)
      — the sweep step in nightly skip-louds until it lands
      EVIDENCE: #1537 merged (owner r7, bot APPROVED) — DONE 2026-09-23.

- [ ] First green nightly run of the relay-only flip + rollback drill
      (hardened lane merged in #1542; two prior attempts never reached the drill)
      EVIDENCE: RUN_URL=<fill>, drill step green, R1-R4 PASS lines extracted below.

- [ ] `CredentialsStaged=True` observed on a canary workspace (§6.2 gate 3)
      from the drill log
      EVIDENCE: the drill's own row output — `R2 — token-only canary` block's
      `CredentialsStaged=True` PASS line from RUN_URL=<fill>.

- [ ] First green nightly run of the rogue-agent sweep — zero provider-key
      bytes in any uid-1000-readable path (the epic's exit criterion,
      design 0058 §8)
      EVIDENCE: RUN_URL=<fill>, sweep step green, R1 + R2 PASS rows + the final
      `all rows passed — the rogue agent finds nothing (K1, #820's exit
      criterion)` line extracted below.

- [ ] Owner decisions recorded: K1 non-frontable resolution; envtest
      3-unwired-suites + per-function guard granularity (#820 comment
      5769972896; offered to the epic lane, needs the owner's go)
      EVIDENCE: <owner decision links — fill when recorded>.

--------------------------------------------------------------------------------
## (b) Extraction commands (run against the green nightly run; fill RUN_ID)

# Locate the nightly job and pull its log:
RUN_ID=<fill>
JOB_ID=$(gh api repos/lenaxia/LLMSafeSpaces/actions/runs/$RUN_ID/jobs --jq '.jobs[0].id')
gh api repos/lenaxia/LLMSafeSpaces/actions/jobs/$JOB_ID/logs > /tmp/nightly-$RUN_ID.log

# Drill R1-R4 PASS lines:
grep -E "^(.* )?(R[1-4] (PASS|—)|.* PASS: (llm-relay router|token-only|config apiKey|config baseURL|flip|all rows))" /tmp/nightly-$RUN_ID.log
#   expected rows: R1 router-ready PASS; R2 CredentialsStaged=True +
#   "canary bytes in ZERO uid-1000-readable paths" + apiKey-is-token +
#   baseURL-is-router; R3 positive-control PASS (canary RETURNS); R4
#   CredentialsStaged=True again + token-only sweep zero + "all rows passed".

# CredentialsStaged=True observation (§6.2 gate 3 — the drill's own assert):
grep -E "CredentialsStaged=True" /tmp/nightly-$RUN_ID.log

# Sweep PASS rows + final line:
grep -E "(R1 PASS: K1 holds|R2 PASS|all rows passed — the rogue agent finds nothing)" /tmp/nightly-$RUN_ID.log

# Step-level green (the workflow summary):
gh pr checks --watch 2>/dev/null || gh api repos/lenaxia/LLMSafeSpaces/actions/runs/$RUN_ID --jq '.conclusion'
# Run URL pattern: https://github.com/lenaxia/LLMSafeSpaces/actions/runs/<RUN_ID>

--------------------------------------------------------------------------------
## (c) The merge-list table (for the closure comment's "landed" section)

| Story / work | Vehicle(s) |
|---|---|
| US-72.0 relay injector re-arm (#910 precondition) | #1401 |
| US-72.1 StagingProvider (KMS-envelope prod / HPKE dev) | #1407 |
| US-72.2 llm-relay ns + 2-replica BYO router | #1432 |
| US-72.3 controller staging + conditions + flag | #1448 (+ #1531 envtest-matrix repair) |
| US-72.4 agentd token-only emission + liveness | #1529 |
| US-72.5 default flip + runbook + drill | #1534 (owner) + #1536 (reconciliation: consumer pins, nightly drill wiring) |
| US-72.6 scrub + sweep script + mirror | #1537 (owner) |
| US-72.6 sweep wiring + pins + evidence lane | #1538 |
| Evidence-lane hardening (SR-6 skip class) | #1542 (#1541) |
| Namespace-scope cache-scoping derivation (drill-shape unblock) | #1555 (successor of #1550; CI-green dispatch on record) — MERGE PENDING |
| Watchdog-flake close-out (CI hygiene during the epic) | #1547 (#1543) |
| API keys/creds hygiene (this session's unrelated fixes) | — (none needed) |

--------------------------------------------------------------------------------
## (d) Open owner decisions (the checklist's fifth row)

1. **K1 non-frontable carve-out resolution** — bedrock/vertex/azure_openai/
   opencode stay raw under flag-on (owner-accepted carve-out, #1536 runbook
   "Known behaviors"); the sweep asserts frontable-only zero. Decision needed:
   accept permanently (amend K1's wording) or schedule fronting.
2. **envtest 3-unwired-suites + per-function guard** (#820 comment 5769972896)
   — TestEnvtestPlatformInit_*, TestEnvtestRecoveryExhausted_*,
   TestEnvtestAgentdSidecar_*/TestEnvtestUS4B_* never executed by CI; the
   repolint wiring guard is package-granular. Standing offer from this lane:
   can be taken on request; otherwise awaits the owner's go.
3. **KMS wiring deferral record** — design 0058 trade-off (iii): the
   PubUnreadable/KMS-mode distinction deferred (US-72.5 recorded it); confirm
   the deferral stands as the record for epic close.

--------------------------------------------------------------------------------
## Filling instructions (when green)

1. Confirm #1555 merged (the drill-shape step cannot pass without it).
2. Wait for the first nightly on the merged main (the monitor
   (us72-evidence-monitor-r3/r?) reports; or watch the schedule 09:17 UTC).
3. Fill RUN_ID, extract (b), tick rows 2-4 with the extracted lines, update
   row 5 with decision links.
4. Post as the closure comment on #820 and close (the owner's call to press
   the button per the standing arrangement — draft only from this lane).
