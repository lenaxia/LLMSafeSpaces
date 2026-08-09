# Worklog: US-65.6 — Repolint Import-Rule Enforcement

**Date:** 2026-08-09
**Session:** US-65.6 — build-time invariant that agent-specific knowledge stays behind the seam
**Status:** Complete

---

## Objective

Add a repolint rule forbidding `pkg/agent/opencode/` imports outside the construction/wiring layer (api/internal/app/, cmd/*) and the in-pod agentd binary. The boundary US-65.1 contained must be enforced, not just hoped for. The rule locks in the seam so the next opencode quirk cannot land back in platform code as another `SetX`+`Rebuild`+`restart()` block — exactly the leak shape Rule 12 says a single-method seam prevents.

---

## Work Completed

### New rule: `pkg/repolint/agent_import.go`

- `AgentImportCheck(root)` walks every non-test `.go` file, parses imports (`parser.ImportsOnly`), and flags any file that imports `pkg/agent/opencode/` from outside `agentImportAllowedPrefixes` (`api/internal/app/`, `cmd/workspace-agentd/`, `cmd/controller/`, `controller/cmd/`) unless listed in `agentImportKnownLeaks`.
- `agentImportKnownLeaks` is a dated, commented allowlist. Each entry cites the story that retires it. Adding to this list without a worklog + story citation is the moral equivalent of `// TODO: fix later` and is rejected at review.
- `KnownLeakCount()` exposes the current leak total so the success message surfaces the tech debt while it is non-zero: `ok    agent-import boundary (5 known leak(s) tolerated pending US-65.4)`. The number visibly tightens when each leak retires.
- Test files (`_test.go`) are excluded: tests of the opencode package itself and tests that exercise the concrete client against a fake server are legitimate. The boundary protects production code shape.

### Wired into `cmd/repolint/main.go`

- `runAgentImport(root)` added next to `runGinSetMode`; failures accumulate into the existing `failures` counter. Repolint is invoked by `.githooks/pre-commit` and the Lint job in `.github/workflows/ci.yml`, so the rule is now in both gates — no separate CI wiring needed.

### Test coverage (`pkg/repolint/agent_import_test.go`)

- Real-repo scan passes with the documented leaks tolerated.
- A new violation in a temp repo IS flagged (`api/internal/handlers/evil.go` triggers the failure).
- Allow-set entries (app.go, agentd main, controller main) are NOT flagged.
- Test files are NOT flagged (legitimate use in opencode's own tests + handler tests with fakes).
- `TestKnownLeaksStillMatchReality` fails closed in both directions: missing entry → real leak flagged; stale entry → orphan detected. This is the anti-rot guard — the list shrinks as leaks retire, never silently stays.

### PartType review-flag (spec second rule)

Spec asked: "new `PartType` constants require a linked design-doc update (flags the diff for review)." The in-package `TestPartTypeCountIsExactlyFive` already pins the count. Building a second Go-source-parsing repolint rule would duplicate that check (Rule 4 — over-engineering). Instead, I improved the test's failure message to explicitly cite the contract-change procedure: *"amend design/0049 §4.3 + Epic 65 US-65.2 first, OR revert the constant. There is no third option."* The flag fires at `go test`, which is already a CI gate.

---

## Key Decisions

1. **Codify existing leaks, don't fix them inline.** Rule 7 validation: I initially categorized 3 of the 5 leaks as "trivial to fix now" (error sentinel, two init() Register calls). On inspection:
   - `opencode.ErrNoRunningPod` is a `*pkgerrors.StatusError` defined in opencode. Re-exporting via pkg/agent creates a cycle (opencode imports pkg/agent for the AgentRuntime interface it implements). Fixing requires moving the canonical sentinel definition — a real refactor, not a 1-liner.
   - The `init() { opencode.Register() }` pattern in `workspace_service.go` and `controller.go` exists because there's no single explicit wiring point. Moving the registration out of init() changes boot ordering; tests may rely on the implicit side-effect. Centralizing boot wiring is itself part of Epic 65's construction-layer work.
   - Net: codifying all 5 with explicit retirement criteria is the honest move. The rule's "Done when" is "a NEW leak fails repolint" — that is met today. Fixing the existing leaks is US-65.4 / US-65.6-followup scope.

2. **Test files excluded.** Production code shape is what the boundary protects. Tests of `pkg/agent/opencode/` itself and tests that wire the concrete client against an `httptest.Server` fake are legitimate (e.g., `agent_reload_e2e_test.go` exercises the real opencode client against a stub — the fake is the test seam, not a production coupling).

3. **`knownLeaks` is dated and tagged with exit criteria.** Each entry names the story that retires it (US-65.4 for the C2 Client users, US-65.6-followup for the sentinel/init cases). The success message shows the count so reviewers see the debt while it exists. `TestKnownLeaksStillMatchReality` makes the list fail-closed against rot.

4. **No PartType repolint rule.** The in-package unit test is the right level; a second source-parsing repolint check duplicates it. Improved the failure message instead. Documented this trade-off (faithful to Rule 4) in the worklog so the choice is auditable.

---

## Adversarial Self-Review (Rule 11)

| # | Finding | Class | Resolution |
|---|---------|-------|------------|
| F1 | `repoRoot` test helper collided with `sequence_test.go:1262`'s existing helper | Real (build break) | **Fixed** — removed mine; reuse existing. |
| F2 | 3 leaks initially missed by my own categorization (`models_handler.go`, `workspace_service.go`, `controller.go`) — rule (correctly) flagged them | Real (rule works as intended) | **Fixed** — added to `agentImportKnownLeaks` with proper exit criteria (US-65.6-followup). |
| F3 | `isTestFile` had `len(path)-9:` for an 8-char suffix (`_test.go`) — off-by-one, flagged legitimate test files as leaks | Real (test bug) | **Fixed** — `len(suffix)` parameterization. |
| F4 | `agent_import.go` failed gofmt after the Const→Map edit | Real (formatter) | **Fixed** — `gofmt -w`. |
| F5 | Could the rule be bypassed with `//go:build ignore` files or vendored copies? | Acceptable | Build-ignored files don't compile, so they don't cause coupling. Vendored paths are excluded by `isExcludedPath`. |
| F6 | Exit code on failure verified? | Validated | `echo $?` after a synthetic new leak returns 1 (not the `0` shown when piping through `tail`). |

---

## Tests Run

- `go vet ./pkg/repolint/... ./cmd/repolint/... ./pkg/session/...` — clean
- `go test -timeout 30s -count=1 ./pkg/repolint/... ./pkg/session/...` — PASS
- `go build ./...` — clean
- `./bin/repolint` against real repo — `ok    agent-import boundary (5 known leak(s) tolerated pending US-65.4)`
- Synthetic new-leak test in a temp repo — correctly flagged with exit code 1

---

## Files Modified

**Created:**
- `pkg/repolint/agent_import.go`
- `pkg/repolint/agent_import_test.go`

**Modified:**
- `cmd/repolint/main.go` (runAgentImport added + wired into the failures accumulator)
- `pkg/session/part_test.go` (improved `TestPartTypeCountIsExactlyFive` failure message to cite the design-doc amendment procedure)

---

## Open Items / Follow-ups

- **US-65.6-followup** (new, ~1 day): when created, retires 3 of the 5 knownLeaks:
  - Move `ErrNoRunningPod` canonical definition from `pkg/agent/opencode/agent_client.go` to `pkg/agent` (breaks the cycle that forces the re-export to live in opencode/); opencode's own sentinel becomes `var ErrNoRunningPod = agent.ErrNoRunningPod`.
  - Replace `init() { opencode.Register() }` in `workspace_service.go` and `controller.go` with explicit registration at `app.New` / `controller` main boot (Rule 3 — Explicit Over Implicit).
- **US-65.4** retires the remaining 2 (the concrete `*opencode.Client` users in the proxy handlers).

When both land, `agentImportKnownLeaks` must be empty; the rule then enforces the design's full allow-set with no exceptions.

---

## Next Steps

US-65.6 is complete. Pivoting to Epic 63 (Inboard Session Queue / V2 session API) per user direction. US-63.1 (verification spike — confirm bundled opencode serves V2 prompt+interrupt end-to-end) is the first item; it de-risks the entire epic and settles findings F13–F17 against a real binary in ~1 day.
