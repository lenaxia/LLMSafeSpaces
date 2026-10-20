# Worklog: #1759 — PVC root allow rules in the permission-tier floor + platform prompt mount-topology block

**Date:** 2026-10-19
**Session:** Implementation lane for issue #1759 (worktree /workspace/wt-1759, branch fix/1759-pvc-default-allow-rules): default read-write external_directory allows for the three PVC mount points (/workspace, /tmp, /home/sandbox), deny preservation under the new allows, two stop-and-report engine investigations, and the platform-prompt persistence block (charter amendment).
**Status:** Complete — lane superseded: the owner adjudicated the duplicate external PR #1762 (merged 2026-10-19, d8496b8d) as the #1759 vehicle and closed PR #1764 as the duplicate. This branch carries the lane's unique deltas (harness env fix, live legs, engine-grammar findings, prompt block) pending orchestrator/owner disposition.

---

## Objective

Make agentd's rendered opencode config grant allow (not ask) on all three PVC mount points by default, preserve every existing deny, validate the issue's precedence-ordering claim mechanically before relying on it, and document the three-mount-point persistence topology in the platform-tier system prompt.

---

## Work Completed

### Current-state tier map (pre-change, all three roots in the platform-floor layer)

- /workspace: NO tier key (the 2026-09-19 ruling's project-root-exemption reading) → ambient ask, bare + children.
- /tmp: "/tmp/*" allow for children; bare "/tmp" matched no rule → ambient ask.
- /home/sandbox: "ask" on both "/home/sandbox" and "/home/sandbox/*" (the home carve), with .cache/.config/.local pre-allows and credential denies beneath.
- The /tmp entries in allowed-dirs test fixtures are operator-source TEST DATA, not defaults; the operator source is empty by default.

### The floor change (pkg/agent/opencode/permissiontiers.go)

- "/home/sandbox" + "/home/sandbox/*": ask → allow (#1759 supersedes the home-carve ask).
- Added "/tmp" and "/workspace" + "/workspace/*" allow keys (bare keys close the mount-point-itself gap; bare "/home/sandbox" already existed).
- Added the exact-key deny twin "/workspace/.local/opencode/auth.json" (XDG_DATA_HOME authJSONPath per README; mirrors the pre-existing /home/sandbox twin; sorts after "/workspace/*" — '.' 0x2E > '*' 0x2A).
- Rewrote the file's tier-map comment and residual 3 (see findings).

### Engine findings (two stop-and-reports, both adjudicated by the orchestrator)

1. **Precedence ordering claim — VALIDATED LIVE** (the issue's load-bearing "observed" word): new live probe TestPermissionTier_LiveDenySpecificBeatsAllowGlob boots the pinned 1.18.15 with a broad allow sorted BEFORE a specific deny; the deny fires; the allow-only control permits. findLast/last-match-wins in rendered-key order is the engine's observed behavior. The deny-preservation leg (cat /home/sandbox/.ssh/id_ed25519 with the new root allows rendered) stays denied with the deny quoted in the rule dump.
2. **Live deny grammar (18-probe evidence set, residual 3)**: only "/*"-suffixed patterns ever fire a deny. Exact keys: allow fires, deny never fires (loaded, never matched). Terminal-star "X*": allow fires, deny never fires. The engine APPENDS its own self-allows (tool-output, /tmp/opencode/*, its XDG-resolved auth store) AFTER config rules — appended rules beat config under findLast. Consequence: bare names (file or dir) cannot be denied on this engine; the exact deny keys (pre-existing set + the new twin) are intent/model rules, transparent at evaluation. Ruling B (add trailing-star denies to make bare names bite live) was implemented, proven engine-infeasible, and REVERTED per ruling B'': trailing-star keys load but never match for deny — dead config (Rule 5).
3. **Security trade surfaced to the owner** (PR "surfaced findings"): the "/workspace/*" allow makes /workspace/.local/opencode/auth.json bash-typed-readable by agent tools (pre-change: ambient ask = blocked-by-hang; post-change: executes — via the config allow regardless of the engine self-allow). No rule shape can prevent it on 1.18.15. Within the accepted threat model (design 0060 Part D inversion: arbitrary user code reads uid-1000-readable files ungated; the pre-existing "/home/sandbox/.config/*" allow exposes gh tool-auth the same way; US-35.7 tmpfs+wipe and #821 egress bounds are the real controls). Upstream asks: honor exact/terminal-star denies; an exclude shape that beats allow-globs; stop self-allowing the auth store. A grammar-conformant partial mitigation (children deny on /workspace/.local/opencode/*) exists for a future engine but cannot protect auth.json while the appended self-allow stands — deliberately not added.

### Platform prompt block (charter amendment; pkg/types/agent_prompt.go)

- Replaced the DefaultPlatformPrompt "Persistence:" block with the approved 6-bullet text: three roots = subPaths of ONE persistent volume (shared capacity, all three survive suspend/resume and pod death — including /tmp), tmpfs contrast with the never-delete-the-symlinks contract, package homes, and the inspect-all-three cleanup directive with live-state guardrails (/workspace/.local, /tmp/opencode, /home/sandbox/.config) and the mtime + /proc/*/maps check.
- **DB-row-override semantics (adjudication requirement)**: DefaultPlatformPrompt is the FRESH-INSTALL fallback only. A deployment with a saved sys_prompt_platform row keeps its override — including an explicitly empty one — so the new text reaches default-tier workspaces only. That is correct behavior (admin's deliberate choice always wins) but must be stated: existing instances that customized the platform prompt will NOT see the new persistence block until they reset it.
- Content pins: pkg/types/agent_prompt_test.go (facts-not-phrasing pins + the persistence-contrast guard + the ≤10k round-trip cap).

### Harness fixes (Rule 5 — pre-existing defects found by the new legs)

- integration_harness.go env construction appended overrides AFTER os.Environ(), contradicting its own comment (duplicated env vars resolve FIRST-match in libc): on a workspace pod the inherited XDG_DATA_HOME/HOME silently won, and opencode adopted the POD's live auth store. Now the harness STRIPS inherited OPENCODE_CONFIG/XDG_DATA_HOME/XDG_CONFIG_HOME/HOME/OPENCODE_SERVER_PASSWORD before appending the test's values.
- Credential-shaped probes now log VERDICTS ONLY (denied=… ruleQuoted=…), never raw tool output: a regression that lets such a command execute must not pour credential bytes into the test log.

### Leak-hygiene confirmation (adjudication requirement)

The first live run (pre harness fix) captured live key material from the pod's auth store into one log file (/tmp/wt-1759/green-live.log) via the then-raw t.Logf. The file was DELETED ENTIRELY (not scrubbed in place). Confirmed by marker grep (sk-/lrt_/known key fragments) across every remaining file under /tmp/wt-1759/ and the worktree: no other log, fixture, or worklog draft ever contained raw output from a credential-shaped probe. The remaining diagnostic logs contain rule dumps, byte counts, and filename listings only. No credential material is committed to the branch.

---

## Key Decisions

- **Floor placement (permissiontiers.go) over the operator allowedDirs source**: mechanically forced, not preferred — allowReopensTierDeny DROPS any operator allow that can match a tier-denied path (an operator "/home/sandbox/*" has literal prefix extended by the "/home/sandbox/.ssh/" deny root → dropped at ingress). Only tier keys bypass the filter. Floor placement also makes the PVC allows un-clearable (AllowedDirs replace/clear never touches tier keys), per design 0060 Part D ("platform-baked, not user-editable"). Orchestrator-verified.
- **Workspace exact twin kept (ruling B'')**: inert-on-live intent key, same class as the pre-existing home twin; held by the model (tierMatch + matrix + render pins); correct the moment an engine honors exact denies.
- **Trailing-star keys reverted after live disproof**: implemented per ruling B, proven never-matching for deny (they DO load — the live rule dump lists them), reverted per B'' as dead config.
- **Model (tierMatch) left as-is**: the ported matcher's wildcardless-exact-match premise matches neither deny direction live; the divergence is documented in residual 3 rather than silently rewriting the ruling's model in this lane. Model-vs-engine reconciliation is a follow-up lane decision.

---

## Blockers

None. (Two stop-and-reports during the session, both adjudicated: floor placement + auth twin approved; ruling B reverted to B'' after the engine-grammar disproof.)

---

## Tests Run

- Red (pre-fix, demonstrated): `go test -run 'TestPermissionTier_PrecedenceModel|TestPermissionTier_AlphabeticalCarveOrdering|TestConfigWriter_RendersPVCRootAllowFloor|TestConfigWriter_FloorAllowSurvivesOperatorSourceDrop' ./pkg/agent/opencode/` → 6 red subtests + 2 red render tests + carve red; 28 subtests green (deny rows). Live: both new allow legs RED via ask-hang (SessionSend context deadline — the literal permission wall); .ssh deny leg green-stays-green.
- Green (post-fix): `go test -count=1 ./pkg/agent/opencode/ ./pkg/types/` → ok. `go test -count=1 -tags=integration -run TestPermissionTier -v ./pkg/agent/opencode/` → 12/12 PASS (7 live legs on the pinned 1.18.15 binary at /opencode/usr/local/bin/opencode: /tmp allow read, canonical-target read deny, /etc bash deny, ordering probe + control, /home/sandbox allow read, /workspace allow read, .ssh deny with root allow; 5 model/structure tests).
- `go test -count=1 ./api/internal/services/prompt/ ./api/internal/handlers/ -run Prompt` → ok (identity + length pins).
- `make build` → ok. `make lint` → 0 issues.
- cmd/workspace-agentd: dev-preview subset passes in 0.18s isolated; one full-package run starved under sibling-lane memory pressure (timeout at the shared-pod ceiling, not a code failure — re-run at gate).
- Ruling-B intermediate state (implemented then reverted): trailing-star legs failed live exactly as the grammar predicts — evidence recorded in the stop-and-report transcripts, not in the tree.

---

## Next Steps

- PR review loop to APPROVED (Fixes #1759); adjudication packet to the orchestrator at APPROVED.
- Full `make test` sweep at the gate window (memory-staggered; sibling lanes active).
- Follow-up candidates (orchestrator escalating to owner): upstream engine asks (exact/terminal-star denies; exclude shape; auth-store self-allow); model-vs-engine reconciliation for wildcardless patterns; the children-deny partial mitigation for a future engine.

---

## Files Modified

- pkg/agent/opencode/permissiontiers.go — the three-root allows, the workspace exact twin, tier-map comment + residual 3 grammar note
- pkg/agent/opencode/permissiontiers_test.go — matrix rows (red-first), carve-ordering rows, deny-shape premise note
- pkg/agent/opencode/permissiontiers_configwriter_test.go — TestConfigWriter_RendersPVCRootAllowFloor, TestConfigWriter_FloorAllowSurvivesOperatorSourceDrop
- pkg/agent/opencode/permissiontiers_integration_test.go — tierBootExtDir helper, ordering probe + control, /home/sandbox + /workspace allow legs, .ssh deny-preservation leg (verdict-only logging), tierBashLeg
- pkg/agent/opencode/integration_harness.go — env construction fix (strip inherited override keys)
- pkg/types/agent_prompt.go — DefaultPlatformPrompt persistence block (approved text)
- pkg/types/agent_prompt_test.go — new content + contrast pins
- worklogs/NNNN_2026-10-19_pvc-root-allow-permission-tiers.md — this worklog
