# Worklog: the five missing builder digests — the残 gap in main's 1.26.10 remediation

**Date:** 2026-10-21
**Session: Final re-scope of #1819. The original security carry was progressively absorbed by main itself (owner's #1828: workflows + go.mod + vite 8.1.6; then 43a7ba29's toolchain bump) — this branch's surviving unique value is the FIVE Dockerfile builder digests, which every main-side carry missed, leaving main's Build matrix broken (go.mod requires 1.26.10; the pinned images run 1.26.9 with GOTOOLCHAIN=local → 'go mod download' refuses).
**Status:** Complete

---

## Objective

Green main's Build jobs: re-pin the five golang builder images to the 1.26.10-era digests.

---

## Work Completed

- Five Dockerfiles (api, controller, relay-proxy, relay-router: `golang:1.26@sha256:5c3f3f28…`; workspace-agentd: `golang:1.26-bookworm@sha256:ee3901c6…`) — byte-identical lines to #1807's r5/r6-verified commits (0cb33e05 + 99703900), re-based onto current main so each file's diff is exactly one digest line.
- Dropped from the earlier carry as superseded by main: the 20 workflow pins + go.mod (#1828 / 43a7ba29), the vite lockfile (main's 8.1.6 is CVE-fixed), and the raced 1161 heal (the renumber bot's b544f6dc).

## Key Decisions

- Minimal-surviving-diff discipline: as main absorbed each piece, this branch shrank to only what main still lacks; the PR title/body track every re-scope honestly (heal raced → carry → five digests).

## Blockers

None.

## Tests Run

- Digest diff-vs-main verified one-line-per-file; CI on this branch exercises the actual Build jobs against the new digests.

## Next Steps

- Review → merge → #1807 rebases onto a green main (its own diff: invariant + docs + its 1164 worklog).

## Files Modified

- api/Dockerfile, controller/Dockerfile, cmd/relay-proxy/Dockerfile, cmd/relay-router/Dockerfile, cmd/workspace-agentd/Dockerfile
- worklogs/NNNN_2026-10-21_builder-digests-1.26.10.md — this worklog
