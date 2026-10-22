# Worklog: the five missing builder digests — the remaining gap in main's 1.26.10 remediation

**Date:** 2026-10-21
**Session:** Final re-scope of #1819. Main absorbed the original security carry piecewise — 43a7ba29 carried go.mod 1.26.10 plus 19 go-version pin lines across 12 workflow files, and 57495a4c (#1828) carried security-scan.yml's govulncheck go-version pin (the 20th) plus the frontend vite files (8.1.6, CVE-fixed) — but every carry missed the FIVE Dockerfile builder digests: the images run 1.26.9 with GOTOOLCHAIN=local against go.mod's 1.26.10, so 'go mod download' refuses and ten Build jobs (five services x two arches) stay red. This branch's surviving unique value is exactly those five digest lines.
**Status:** Complete

---

## Objective

Green main's Build jobs: re-pin the five golang builder images to the 1.26.10-era digests.

---

## Work Completed

- Five Dockerfiles (api, controller, relay-proxy, relay-router: `golang:1.26@sha256:5c3f3f28…`; workspace-agentd: `golang:1.26-bookworm@sha256:ee3901c6…`) — byte-identical lines to #1807's r5/r6-verified commits (0cb33e05 + 99703900), re-based onto current main so each file's diff is exactly one digest line.
- Dropped from the earlier carry as superseded by main: the workflow pins and go.mod (43a7ba29: 19 pin lines across 12 workflows + go.mod; #1828: security-scan.yml's 20th pin + the frontend vite files, 8.1.6 CVE-fixed), and the raced 1161 heal (the renumber bot's b544f6dc).

## Key Decisions

- Minimal-surviving-diff discipline: as main absorbed each piece, this branch shrank to only what main still lacks (heal raced by the bot → carry → five digests). Review history, honestly: r4 caught the stale second-version PR body (a silently-failed edit) and the #1828/43a7ba29 misattribution; r5 caught that the r4 fix's replace had silently no-opped (the no-op landed with b1fc44e7's commit; the actual Session-line fix is 72b738ee); r6 caught three residual defects (a stray CJK character, the Session marker, line 18's collective attribution — fdbe84d2); r7 caught this line's own overclaim (9dc0cd87); r8 caught its swapped SHA labels (this commit). Each round's fix rides that round's commit — including this one correcting the last.

## Blockers

None.

## Tests Run

- Digest diff-vs-main verified one-line-per-file; CI on this branch exercises the actual Build jobs against the new digests.

## Next Steps

- Review → merge → #1807 rebases onto a green main (its own diff: invariant + docs + its 1164 worklog).

## Files Modified

- api/Dockerfile, controller/Dockerfile, cmd/relay-proxy/Dockerfile, cmd/relay-router/Dockerfile, cmd/workspace-agentd/Dockerfile
- worklogs/1164_2026-10-21_builder-digests-1.26.10.md — this worklog
