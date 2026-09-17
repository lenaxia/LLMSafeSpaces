# Worklog: repolint rules — agent ID-prefix lexicon + spec coupling markers (#1305)

**Date:** 2026-09-15
**Session:** Implement #1305 (epic-71 / 4b-c3): the two repolint guardrails modeled on `event_literal`, plus design/0049 + README-LLM enforcement documentation.
**Status:** Complete

---

## Objective

Ship the two #1305 repolint rules with zero known-leaks entries at birth:

1. `agent_id_prefix_literal` (`pkg/repolint/agent_id_prefix.go`) — flags `que_`/`per_`/`ses_`/`msg_` string literals in matching/minting contexts outside `pkg/agent/opencode/` (incl. `testdata/`), `cmd/workspace-agentd/`, `pkg/repolint/`, and (per-file, see Key Decisions) `pkg/mcp/server.go`.
2. `spec_coupling_marker` (`pkg/repolint/spec_coupling_marker.go`) — lints `sdks/openapi.yaml`: fails on any `x-opencode-proxy` key and on descriptions matching `tracks upstream` / `from opencode` / `opencode session object`; the top-level `info.description` is the single anchored allowlist.

Both registered in `cmd/repolint/main.go` (CI wiring: `.github/workflows/ci.yml` `make repolint` → `./bin/repolint` runs every registered check — no per-rule flags exist; registration IS the wiring, proven by the binary integration runs below).

---

## Work Completed

### TDD red-first
- Wrote `pkg/repolint/agent_id_prefix_test.go` (12 tests) and `pkg/repolint/spec_coupling_marker_test.go` (13 tests incl. subtests) BEFORE the rules; verified RED (`undefined: AgentIDPrefixCheck` build failure), then implemented to green.

### Rule 1: agent ID prefixes
- Match contexts mirror `event_literal`'s comparison family (==/!=, case incl. comma lists, map-key, Contains/HasPrefix/HasSuffix/TrimPrefix/TrimSuffix) plus concatenation minting (`"msg_" + x`).
- Anchoring: every context requires the prefix IMMEDIATELY after the opening quote — `"question_"` and `"per_user"`-in-composite-keys do not fire (dedicated no-false-positive test). Prose `//` comments and `_test.go` files excluded (event_literal precedent).
- Known-leaks map (path-keyed) empty at birth; `validateKnownLeaksEntries` meta-validates reason + `#NNNN` issue pointer on every entry (tested with seeded bad/good maps).

### Rule 2: spec coupling markers
- Line-based scan of `sdks/openapi.yaml`: `x-opencode-proxy` key regex (quoted/unquoted) anywhere; three coupling phrases case-insensitive on word boundaries in description values (inline + block-scalar continuation lines, blank-line-safe indentation tracking) EXCEPT under the top-level `info:` block — the anchored allowlist. Missing spec = error, never silent pass.
- Known-leaks map (trimmed-line-keyed, stable across line churn) empty at birth, same meta-validation.

### Registration + docs
- `cmd/repolint/main.go`: `runAgentIDPrefix` + `runSpecCouplingMarker` (same reporting shape as `runEventLiteral`).
- `design/0049` §7: discipline rules 7 (ID lexicon) and 8 (spec markers) documented as enforced-by-repolint.
- `README-LLM.md` Agent Session Contract discipline list: rule 2 rewritten (full lexicon, enforcement, allowlist incl. the `pkg/mcp/server.go` per-file entry) + new rule 6 (spec markers).

---

## Key Decisions

1. **`pkg/mcp/server.go` is allowlisted PER-FILE — the flagged conflict.** #1305's issue text lists four allowed paths and does NOT cover `pkg/mcp/server.go:325,341`, which carries `case strings.HasPrefix(requestID, "que_"/"per_")` — the MCP `run_resolve` dispatch fast-path explicitly sanctioned at the #1302 4a-2 r3 review (PR #1363; `pkg/agent/opencode/dialect.go:42-45` documents "the MCP server's dispatch is a prefix FAST-PATH … dispatch, never validation"). A known-leaks entry would violate the zero-at-birth mandate, and failing the real tree violates the birth-state assertion. Resolution: a per-file entry in `agentIDPrefixAllowedPaths` with dated rationale + issue pointers — narrow by construction (`pkg/mcp/other.go` still fails; dedicated test `TestAgentIDPrefixCheck_SiblingDirStillFlagged`), NOT a directory widen. **Conflict flagged here, in the PR description, and in the final report.** Retire when the dispatch moves behind the dialect seam.
2. **Emission exemption for argument-position literals.** `sdks/canary/go/scenarios/d-session-msg/main.go:97` passes `"ses_nonexistent00000000000000"` (a negative probe) — not a match/mint context; exempt per `event_literal`'s `TestEventLiteralCheck_EmissionNotFlagged` precedent. Validated: birth-state real-tree test green without firing on it.
3. **Concatenation minting IS flagged** (event_literal has no such context): minting agent-format IDs platform-side is design/0049 discipline rule 2's leak class; on the real tree the only concat sites are inside `cmd/workspace-agentd/` (ledger `"msg_" + entryID`, faultmatrix fixture IDs) — zero birth violations.
4. **Phrase scope: ALL descriptions outside `info:`, not only response/component ones.** Property/tag/operation descriptions are the same client-facing admission surface; stricter is safer and green on the real tree. Factual agent mentions that match NO phrase (e.g. openapi.yaml:439 "Disposes the current opencode process…") are not flagged — the banned content is the coupling ADMISSION (three phrases), not the agent's name.
5. **`agentIDPrefixKnownLeaks` path-keyed vs `specCouplingKnownLeaks` line-keyed:** paths are stable for Go matches; spec line NUMBERS churn, so the trimmed violating line is the key (monotonic-progress contract preserved: any NEW line fails).

---

## Rule 7 — Assumptions stated and validated

| # | Assumption | Validation |
|---|---|---|
| A1 | Comparison-context scoping catches exactly `pkg/mcp/server.go:325,341` outside allowed paths on the real tree | Grep inventory (`"que_"/"per_"/"ses_"/"msg_"` quoted-literal search, non-test, all dirs) + `TestAgentIDPrefixCheck_RealRepo_BirthStateClean` green with the per-file allowlist |
| A2 | The `pkg/mcp/server.go` dispatch is reviewer-sanctioned | `pkg/agent/opencode/dialect.go:42-45` header comment; PR #1363 (#1302 4a-2 review chain; r1 review corrected the pointer from #1371) |
| A3 | Canary's probe literal is not a matching context | Emission-precedent test; birth-state test green |
| A4 | `pkg/agent/opencode/testdata/` fixtures are non-Go (the explicit entry is documentation-only) | `find testdata -name "*.go"` → 0 |
| A5 | Line-based YAML scan suffices (key regex line-local; phrases line-local; block scalars indentation-tracked) | Fixture tests incl. multi-line block scalars, quoted keys, info-block boundary, blank-line neutrality |
| A6 | CI wiring = registration in `main()` | `ci.yml:94 run: make repolint` → `Makefile:249` `./bin/repolint`; binary runs prove both rules execute with no flags; `release.yml:126` + pre-commit hook use the same binary |
| A7 | `_test.go`/comment exclusions + Go-only scope mirror `event_literal` | Precedent; frontend grep shows only test fixtures (`.test.tsx`) carry prefix literals today — Go-only scope documented as the shared limitation |

**Mutation (non-vacuousness) proofs:** removing the `pkg/mcp/server.go` allowlist entry makes the birth-state test FAIL with `pkg/mcp/server.go:325 (que_)` (observed, then restored); the spec fixture tests seed violating lines in spec copies and fail-closed on a missing spec.

---

## Blockers

None. (Pre-existing: two sibling NNNN_ worklog sentinels on origin/main tripped `TestLive_Worklogs_NoDuplicates` mid-session; the renumber bot landed `00682f9e` — branch rebased onto it, test green.)

---

## Tests Run

- RED: `go test -run 'TestAgentIDPrefix|TestSpecCoupling' ./pkg/repolint/` → build failure (rules absent) — TDD Rule 0 satisfied.
- GREEN: same command → `ok` (25 new tests + subtests).
- `go test -race -timeout 600s ./pkg/repolint/...` → `ok` (after rebase onto `00682f9e`).
- Binary integration: `./bin/repolint` real tree → exit 0 (`ok agent ID prefixes (0 known leak(s) tolerated)`, `ok spec coupling markers (0 known leak(s) tolerated)`); `./bin/repolint -repo <scratch>` with a handler `que_` HasPrefix + `x-opencode-proxy` key + `tracks upstream` description → exit 1 with `api/internal/handlers/leaky.go:6`, `sdks/openapi.yaml:10`, `sdks/openapi.yaml:11`.
- `make repolint` → all checks passed.
- golangci-lint → clean.

---

## Review round 1 (CHANGES_REQUESTED) — corrections

All four findings reproduced, fixed red-first (failing tests written before each behavior change):

1. **`info:` exemption was block-wide** → narrowed to the `info.description` KEY itself (direct child of `info:`); `info.license`/`info.contact` descriptions now held to the phrase bans (`TestSpecCouplingMarkerCheck_InfoSubBlockDescriptionsNotExempt` — red before the fix). Plain multi-line (folded, non-block) scalars checked on first line only — documented as a known boundary.
2. **Const/regex laundering escaped** → added declaration/assignment (`const quePrefix = "que_"`), assignment-carried alternation (`= "^(que|per)_"`), compile-anchored (`MustCompile("^que_")`), and compile-alternation (`(que|msg)_`) contexts (`TestAgentIDPrefixCheck_ConstAndRegexLaundering` — red before the fix). Real-tree birth-state test still green (zero false positives with the widened scope).
3. **Trailing/`/* */` prose false-positives** → `isCommentLine` skips `//`, `/*`, `*`-prefixed lines; `stripTrailingComment` cuts at the earliest `//`/`/*` (documented trade: a `//` inside a URL string can suppress matches on the remainder — a rare miss, never a false positive) (`TestAgentIDPrefixCheck_TrailingAndBlockCommentsNotFlagged` — red before the fix).
4. **Integration legs were manual-only** → automated in `pkg/repolint/binary_integration_test.go`: `TestRepolintMain_RegistersNewRules` (static wiring pin — silent deregistration fails), `TestCIWorkflow_RunsRepolintLint` (ci.yml + release.yml `make repolint` step pin), `TestRepolintBinary_InvokesNewRules` (builds the binary; scratch tree → exit non-zero with file:line for BOTH rules; real tree → exit 0 with both ok-lines; skipped under `-short`).
5. **Provenance miscitation** → the #1302 4a-2 review that sanctioned the `pkg/mcp/server.go` dispatch is PR **#1363** (verified: its r2 review discusses `pkg/mcp/server.go:315,331` prefix routing), not #1371 (the 4b SDK-sync PR). Corrected in `agent_id_prefix.go`, `cmd/repolint/main.go`, this worklog, and the PR description.



- Reviewer rounds on the PR; retire the `pkg/mcp/server.go` per-file allowlist when the MCP dispatch moves behind the dialect seam (post-epic-71; tracked in the allowlist comment).

---

## Files Modified

- `pkg/repolint/agent_id_prefix.go` (new)
- `pkg/repolint/agent_id_prefix_test.go` (new)
- `pkg/repolint/spec_coupling_marker.go` (new)
- `pkg/repolint/spec_coupling_marker_test.go` (new)
- `cmd/repolint/main.go` (register both checks)
- `design/0049_2026-08-09_agent-session-contract.md` (§7 rules 7-8)
- `README-LLM.md` (Agent Session Contract discipline rules 2 + 6)
- `worklogs/0949_2026-09-15_repolint-prefix-and-marker-rules.md` (new)
