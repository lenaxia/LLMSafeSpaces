# Worklog: #1777 — adopt the /workspace/.local/opencode/* children deny in the permission floor

**Date:** 2026-10-20
**Session:** Owner-funded adoption of the shape proven live by the #1771 residual-3(d) experiment: the slash-star children deny over the opencode data dir, landing as a floor key with model/render/carve/live pins.
**Status:** Complete

---

## Objective

Add "/workspace/.local/opencode/*": "deny" to platformPermissionTiers, closing the workspace auth.json bash-read trade and protecting opencode.db-class paths from agent tools.

---

## Work Completed

- Red-first: 3 matrix subtests (auth.json, auth.json.bak — the #1771 grammar-edge row flips to a denial pin — and opencode.db), the carve-ordering row, and the render assertion all RED pre-key; GREEN post-key.
- The floor key with its byte-order rationale ('*' 0x2A < '.' 0x2E — the deny is the last matching rule under findLast for every path under the dir).
- Live leg TestPermissionTier_BashDeniesOpencodeDataDir — the 3(d) experiment promoted to a permanent pin: bash cat of the auth path (piped to wc — a regression returns a count, never content) → denied=true with `"/workspace/.local/opencode/*"` quoted in the engine dump, on the real pinned 1.18.15 with the root allow rendered. Not pod-qualified: the gate fires before file access.
- Residual 3(b)/(d) updated coherently: (b) now states a file's protection is the directory children deny (adopted here) and scopes the unprotected-sibling claim to dirs without one; (d) records the adoption and cites the permanent pin.

## Key Decisions

- **The trade (owner-accepted by funding #1777)**: agent bash/file tools lose access to /workspace/.local/opencode/* — the auth store symlink and opencode.db-class session state; the opencode server's native IO is unaffected (external_directory gates tool invocations only, not the harness's own file access).
- No exact-key twin for the workspace auth path: the children deny covers the file itself (it is a child of the dir) and is the shape that actually fires — an exact twin would be the inert class residual 3 documents.

## Blockers

None. (Known environmental: the PR-review gate is temporarily refusing bot-authored PRs — the review check will sit red; per the pause-aware protocol this is expected and non-actionable; packet delivered review-ready.)

## Tests Run

- Red: matrix (3 subtests) + carve + render FAIL pre-key (demonstrated).
- Green: `go test -count=1 ./pkg/agent/opencode/` ok (full unit corpus incl. deny-shape premise, floor dominance, idempotency, boot-normalize).
- Live: `go test -count=1 -tags=integration -run TestPermissionTier_BashDeniesOpencodeDataDir -v` → PASS, denied=true ruleQuoted=true on the pinned 1.18.15.

## Next Steps

- CI green on all non-review checks; rebase current; review-ready packet to the orchestrator; review loop + merge when the bot-PR gate lands.

## Files Modified

- pkg/agent/opencode/permissiontiers.go — the floor key + residual 3(b)/(d) updates
- pkg/agent/opencode/permissiontiers_test.go — matrix rows (3), carve row
- pkg/agent/opencode/permissiontiers_configwriter_test.go — render assertion
- pkg/agent/opencode/permissiontiers_integration_test.go — the permanent 3(d) live leg
- worklogs/NNNN_2026-10-20_opencode-data-dir-children-deny.md — this worklog
