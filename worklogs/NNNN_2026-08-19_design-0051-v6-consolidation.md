# Worklog: design 0051 v6 — adopt the unmerged 0050 sidecar architecture

**Date:** 2026-08-19
**Session:** Fifth-round review of #932 rejected v5's Phase-2 mechanism; consolidation rewrite.
**Status:** Complete

---

## Objective

Close five rounds of blocking findings on design 0051 without weakening the pod's hardening.

## Work Completed

- **Admitted the root cause**: v1–v5's in-container uid split (D1) is kernel-impossible under the pod's own security context (`drop ALL` + `AllowPrivilegeEscalation=false` + `RunAsNonRoot` → no setuid; `pod_builder.go:176-178`), and its V8 rationale ("parent→child signals unaffected by uids") is a false rule — `kill(2)` across uids needs matching ids or CAP_KILL. Both were reviewer findings F1/F2 for five rounds; v6 concedes them in §4 rather than patching around them.
- **Discovered and adopted the prior art**: the bot's `design/0050_agentd-uid-separation` draft (issue #887, run #31919066574) had already done this analysis correctly — including finding the `agent-config.json` embedded MCP Basic header v5 never named — but its branch push failed, its number was reused by #892's starvation design, and 0051 was written blind to it. v6 adopts 0050's architecture wholesale (native sidecar uid 2000/gid 1000, `supervise-opencode` as PID 1 of the workspace container with a 127.0.0.1:4099 control socket, `agentdPassword` credential split so agentd's secret never exists in uid-1000 space, integrity mounts for `agent-config.json`) and records the supersession honestly.
- Fixed all carried nits: §2 line cites corrected; §10 gVisor-V3 claim removed (envp inheritance isn't a ptrace phenomenon); v5 §6 sidecar rejection withdrawn (premised on the impossible D1; #863 D1 governs delivery provenance, which the same digest-pinned artifact preserves); Q numbering de-gapped; the MCP Basic header and reload-secrets integrity attack are now named in §2; V-matrix rewritten around the sidecar topology (no cross-uid signaling exists anywhere, so V8-class findings cannot recur); Phase-1-shipped record retained.

## Key Decisions

1. **Concede, don't patch** — the reviewer's kernel claims were verified against the code before rewriting (caps drop, syscall.Kill call sites).
2. **Two containers, zero capability grants** — any CAP_SETUID/CAP_KILL buy-back is rejected in-design.
3. **Honest residuals over theater** — the password env copy and the MCP Basic header in `agent-config.json` are recorded as upstream-gated necessities, not "protected" by mode tricks that opencode's own read path would break.

## Tests Run

Design doc only; structural self-checks (§ numbering, no stale cross-references, Supersedes record accurate vs. git/PR history).

## Files Modified

- `design/0051_2026-08-18_agentd-uid-separation.md` (v6 rewrite)
- `worklogs/NNNN_2026-08-19_design-0051-v6-consolidation.md` (this file)
