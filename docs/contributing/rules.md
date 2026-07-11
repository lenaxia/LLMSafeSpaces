# Engineering Rules

These are the critical guidelines and hard rules that govern every change to LLMSafeSpaces. They apply equally to human and AI contributors. There are no exceptions and no "good enough" exits.

The rules are ordered. Rule 0 (TDD) is the foundation; the rest build on it.

---

## Rule 0: Test Driven Development (TDD)

**MANDATORY.** Write tests *before* writing functional code. Always.

```
Correct workflow:
1. Write test
2. Run test (must fail)
3. Write minimal code to pass
4. Run test (must pass)
5. Refactor if needed
```

**Test requirements (all mandatory — none optional):**

- [x] Multiple happy path tests
- [x] Multiple unhappy path tests (errors, invalid inputs, boundary failures, dependency failures)
- [x] Edge case coverage
- [x] End-to-end integration tests that exercise the real wiring (router → service → K8s/DB/Redis or fakes thereof) — unit tests alone are not sufficient
- [x] Always use `-timeout` when running tests
- [x] Tests must pass before marking work complete

??? example "Definition of done"
    A task is **not** done until it has been demonstrated to be integrated properly via passing e2e/integration tests. "It compiles", "unit tests pass", or "it works in isolation" do not satisfy this requirement. Code that is built but never wired into the live request path is incomplete work.

**Rationale.** TDD produces code that is testable by construction, catches regressions immediately, and forces you to think about interface and behavior before implementation. The integration-test requirement catches the most common failure mode in this codebase: code that works in isolation but is never wired into the live request path.

See [Testing](testing.md) for the patterns and helpers.

---

## Rule 1: Type Safety First

**Always:**

- Define strongly-typed structs for all data structures.
- Create domain types for related fields (see `pkg/types/types.go`).
- Use Go types for all CRD specs and statuses.

**Never:**

- Use `map[string]interface{}` for structured data.
- Use `interface{}` when the type is known.
- Pass untyped data between functions.

Maps are acceptable only when parsing external JSON/YAML with unknown structure — and even then, convert to a typed struct immediately.

??? example "Why"
    Untyped data forces every reader to re-derive the shape from context, defeats the compiler's ability to catch mistakes, and makes refactoring a guessing game. The CRD types (`pkg/apis/llmsafespaces/v1/`) and API transfer objects (`pkg/types/`) are intentionally separate — see [Concepts → CRD type ownership](../getting-started/concepts.md#crd-type-ownership).

---

## Rule 2: Idiomatic Go

- Follow Go conventions throughout.
- Use the `(value, error)` multiple return pattern.
- Avoid global state.
- Create custom error types for domain-specific errors (see `api/internal/errors/errors.go`).
- Prefer minimal concurrency; add it only when there is clear, measurable benefit.

**Rationale.** Idiomatic code is readable to any Go developer, plays well with the toolchain (`gofmt`, `golangci-lint`, the race detector), and integrates naturally with the Kubernetes ecosystem the project depends on.

---

## Rule 3: Explicit Over Implicit

- Explicit error handling — no swallowed errors.
- Explicit type declarations.
- No magic or hidden behavior.

!!! warning "Swallowed errors are bugs"
    `_ = err` (or worse, dropping the return) hides failures that surface later as mysterious behavior. If an error is genuinely safe to ignore, document why in a comment. Otherwise handle it.

---

## Rule 4: Code Quality

Every change must be:

| Principle | Requirement |
|-----------|-------------|
| **SOLID** | Single responsibility, open/closed, Liskov-substitutable, interface-segregated, dependency-inverted |
| **Robust** | Handles failures, partial states, and adversarial inputs without corruption |
| **Reliable** | Deterministic, repeatable, no flaky behavior |
| **Maintainable** | Clear naming, small functions, obvious data flow; the next reader should not need a map |
| **Scalable** | No hidden O(n²) loops, no per-request allocations of expensive resources, no global locks on hot paths |
| **Performant** | Measure before optimizing; do not pessimise (unnecessary copies, N+1 queries, synchronous I/O on hot paths) |
| **Secure** | Input validated, outputs sanitized, secrets never logged, least-privilege by default |
| **Not over-engineered** | No speculative abstractions, no premature generalization, no frameworks-for-the-sake-of-frameworks |
| **Not overly complex** | Prefer the simplest design that satisfies the requirement; if a junior engineer cannot read it, simplify |
| **Idiomatic** | Follow the conventions of the language and the surrounding codebase (see Rule 2) |
| **Faithful to the ask** | Meet the spirit AND the letter of the requirement; do not solve a different problem because it is easier |

### Comments and self-documentation

- **No comments unless strictly necessary and timeless.** Code is self-documenting through clear naming.
- Incorrect or outdated comments must be removed or corrected.

**Rationale.** The PR review rubric (see the [Contributing overview](index.md)) scores every dimension 1–10 and requires a 9+ on each. These principles are what the rubric measures.

---

## Rule 5: Zero Technical Debt

- Do not create adapters for backwards compatibility.
- Remove legacy code.
- Implement the full final solution.
- Never hack tests to pass — fix the root cause.

!!! danger "No pre-existing errors are acceptable"
    "Pre-existing" is not an excuse. If you encounter errors, warnings, or broken behavior in the codebase — even if you did not introduce them — fix them. We are the only ones working on this codebase; every error is our responsibility. Leave the codebase in a zero-error state after every session.

**Rationale.** In a codebase where the next reader is often an AI agent with a fresh context window, technical debt compounds rapidly. A single tolerated error becomes a pattern, becomes an assumption, becomes a bug. The zero-debt rule keeps the codebase legible.

---

## Rule 6: Uncertainty Protocol

If uncertain about correct behavior: **ask the user.** Do not guess, assume, or implement workarounds.

**Rationale.** Silent guesses that turn out wrong are the most expensive class of bug — they look intentional, ship, and resist correction because "that's how it was built." Asking costs a few minutes; a wrong guess costs hours of rework and erodes trust.

---

## Rule 7: Assumptions — State, Then Validate

Every non-trivial change rests on assumptions about the system (data shape, caller behavior, library semantics, deployment environment, ordering, concurrency, error modes, etc.). These assumptions cause most production bugs when they go unstated and unchecked.

### Mandatory protocol

1. **State assumptions up front.** Before writing code, list every assumption the change relies on. Write them in the worklog, the PR description, or a comment block at the top of the design discussion. "It is obvious" is not an excuse — write it down.
2. **Validate every assumption.** For each one, identify how you will prove it true:
    - Read the relevant source/spec/doc
    - Run a query, probe the running cluster, or write a quick test
    - Check git history or existing tests
    - Ask the user if it cannot be validated mechanically
3. **If you cannot validate it, do not rely on it.** Either find a way to validate it, redesign so the assumption is unnecessary, or ask the user. Never proceed on an unvalidated assumption.
4. **Record the validation result.** In the worklog, next to each assumption, record what proved it (e.g. "verified via `pkg/kubernetes/client_test.go:142`" or "confirmed by `kubectl get sandbox -o yaml` on cluster X").
5. **Treat failed validations as findings.** A disproved assumption is a bug or design flaw. Surface it; do not work around it silently.

!!! info "Why this rule is non-negotiable"
    The most common failure mode in this codebase has been silent assumption drift — code that "should work" because someone assumed a behavior that was never true. Documented examples exist in worklogs 0030 and 0333. Stating and validating assumptions up front is the single highest-leverage discipline for preventing this class of bug.

---

## Rule 8: Understand the Architecture First

Before making any change, read the relevant design document(s). Understand how the change fits the overall data flow. **Never modify code without knowing why.**

Key documents by area:

| Area | Document |
|------|----------|
| **V2 Architecture** | `design/0021_2026-05-21_evolution-v2.md` (authoritative) |
| V2 Implementation stories | `design/stories/` |
| Security model | `design/0027_2026-05-24_security-policy-v21.md`, `design/0021 §9` |
| Inference relay fleet | `design/stories/epic-42-multi-cloud-inference-relay/README.md` |
| Master KEK hardening | `design/stories/epic-50-master-kek-hardening/README.md` |
| Tenant isolation (gVisor + quotas) | `design/stories/epic-51-tenant-isolation/README.md` |
| System overview (V1, reference) | `design/archive/v1/0001_2025-03-05_architecture.md` |
| Controller + CRDs (V1, reference) | `design/archive/v1/0003_2025-03-05_controller.md` |
| Runtime environments (V1, reference) | `design/archive/v1/0007_2025-03-05_runtimeenv.md` |
| Network policies (V1, reference) | `design/archive/v1/0020_2025-03-05_network.md` |

V1 design docs are archived under `design/archive/v1/` and are reference-only — where they conflict with `evolution-v2.md`, V2 wins.

---

## Rule 9: Communication Tone

- Neutral, factual, objective.
- Not sensational or sycophantic.
- Provide honest and critical feedback.
- Validate claims with evidence before stating them.

**Rationale.** Sycophantic feedback ("great work! looks good!") hides real problems. The project depends on reviewers — human and AI — being willing to say "this is wrong, here's why." Tone should be direct but professional.

---

## Rule 10: Never Force Push Without Explicit Permission

**NEVER use `git push --force` or `git push --force-with-lease` unless the user has explicitly told you it is okay to force push.**

Force pushing rewrites shared history and can destroy a collaborator's work. The only acceptable scenarios are:

1. The user directly instructs you: "force push" or "push --force".
2. You are fixing a CI-rejected commit (e.g. repolint worklog numbering) and no other collaborator has pulled the broken commit.
3. You are working on a private branch that no one else has ever pushed to.

!!! tip "Prefer rebase"
    Always prefer `git pull --rebase` + a normal `git push` over force pushing. If you pushed a broken commit, first ask the user if force push is acceptable, describe why it's needed, and wait for confirmation.

---

## Rule 11: Adversarial Self-Review

After implementing any non-trivial change, **before marking it complete**, conduct a structured adversarial review in three phases.

### Phase 1 — Identify weaknesses, gaps, and failure modes

Explicitly ask:

1. **Where are the gaps?** What did the design not cover? What edge cases are unhandled? What requirements were omitted?
2. **Where is it weak?** Which parts are fragile, tightly coupled, or depend on implicit ordering?
3. **Where will it fail?** Under what conditions (concurrency, partial failure, invalid state, resource exhaustion, adversarial input) will the implementation behave unexpectedly?
4. **What did I assume without verifying?** Re-read the assumptions list (Rule 7). For each one, ask: "Did I actually validate this, or did I just believe it?"
5. **What would a skeptical reviewer reject?** If someone with no context read this diff, what would they flag?
6. **Why might this code be wrong?** Take the adversarial view — assume the implementation is incorrect or misses the mark, and prove otherwise.

### Phase 2 — Validate each finding

For every criticism generated in Phase 1:

1. **Is the finding real?** Re-read the code, re-run the test, reproduce the scenario. Do not take findings at face value.
2. **Is it a bug, a design flaw, or a false alarm?**
    - **Real bug:** Fix it before proceeding. Do not defer.
    - **Design flaw:** Surface with proposed remediation. Do not proceed without addressing.
    - **False alarm:** Document why it is not a real issue (one sentence with evidence). Do not silently dismiss.
3. **If uncertain:** Escalate to the user rather than dismissing or guessing.
4. **Only validated findings make it into the record.** Unvalidated claims, guesses, and assumed-but-unverified assertions are discarded. They have no place in a worklog, PR description, or review report.

### Phase 3 — Remediate or document

- Real findings must be fixed with regression tests before the change is complete.
- False alarms must be documented with rationale (one sentence is sufficient).
- The change is not ready until Phase 2 returns zero real findings.

??? info "This is a mandatory validation gate, not introspection"
    This is not optional introspection — it is a mandatory validation gate. Code that has not survived its own adversarial review is not ready for commit.

See also the [Adversarial Assessment](index.md) criteria used during pull request review (expanded in `README-LLM.md` under "PR Review Guide").

---

## Rule 12: Containment Before Abstraction

This codebase depends on external components it does not own — most notably the AI agent that runs inside every workspace (`opencode serve`). The platform's value is the orchestration, isolation, and multi-tenant control layer, *not* the agent loop. The risk is that knowledge of an external component's implementation details bleeds into platform code (API, controller, services), making every future change a jury-rig and every eventual swap a rewrite.

The rule is about *when* to pay for an abstraction — and it is the opposite of "abstract everything early."

### Containment now (cheap, mandatory)

- **Keep external-dependency knowledge behind a single seam.** A package, folder, or adapter — not scattered across the codebase. The opencode config-merge semantics, provider-ID model, relay injection, and readyz contract should live in one place. Platform code talks to that seam; it does not know what is behind it.
- **Stop adding new external-component specifics to platform code.** When you write a new feature, ask: "does this line need to know the agent is opencode?" If yes, it belongs behind the seam, not in a service or handler.
- **Containment is not abstraction.** No interface design, no generics, no provider registry. Just a boundary — a folder whose contents are allowed to know the external component, and a small surface the rest of the codebase calls. This is cheap and reversible.

### Do NOT abstract prematurely (the trap)

!!! warning "Single-consumer abstractions encode the wrong shape"
    A single consumer tells you nothing about what the interface should look like. Any abstraction designed against one implementation will encode that implementation's shape as if it were universal — and you will refactor the abstraction itself when the second consumer arrives. **You pay twice.** This is the speculative-abstraction tax Rule 4 prohibits.

    The relay-config subsystem is the canonical example: every accommodation of opencode's config-merge semantics (last-writer-wins, `OPENCODE_CONFIG` always wins, no hot reload, the `agent-config.json` write architecture, the one-shot injector, the 20s stale window) is opencode's behavior leaking into the design. None of those are *our* requirements. Designing an "agent provider" interface today — against opencode only — would freeze that leakage into a contract we'd then have to break.

### Trigger the real abstraction here (pay the big cost)

Abstract when **any one** of these is true:

1. **A second consumer is funded.** The moment a second agent (e.g. Claude Code, a homegrown harness) is scheduled as real work, abstract first, adapter second. A second consumer is the only thing that validates interface shape — writing the second adapter straight onto single-consumer-shaped platform code produces two jury-rigged systems instead of one. This is the highest-leverage moment to spend the money.
2. **The external component forces a rewrite anyway.** If an opencode breaking change forces a relay-config rewrite, absorb the abstraction cost then — the rewrite cost is already sunk.
3. **Pain recurs in the same seam.** A repeated pattern of jury-rigging the *same* area (e.g. multiple worklogs touching `agent-config.json` handling) is evidence that containment has failed. That is a valid abstraction signal even before a second consumer — but the bar is *recurrence*, not a single inconvenience.

### The test for "is this premature?"

Before introducing an interface, adapter, or provider abstraction for an external component, answer:

> *"Do I have at least two concrete consumers, or a forcing rewrite, or demonstrated recurring pain in this seam?"*

If none, contain behind a boundary and stop. The abstraction will be cheaper and more correct when one of those is true.

??? info "Scope"
    This rule is general — it applies to any external dependency whose internals we accommodate (agents, cloud drivers, relay VM binaries, MCP SDKs). The agent provider is the current primary case because the coupling is deepest there.

**Rationale.** Premature abstraction is the most expensive form of technical debt in a codebase that wraps external components. It freezes accidental details into intentional contracts. Containment is the cheap, reversible step that keeps the door open for the right abstraction later.

---

## Summary

| # | Rule | One-line essence |
|---|------|------------------|
| 0 | TDD | Tests first, always; integration tests are the definition of done |
| 1 | Type Safety First | Typed structs; never `map[string]interface{}` for structured data |
| 2 | Idiomatic Go | Follow Go conventions; `(value, error)`; minimal concurrency |
| 3 | Explicit Over Implicit | No swallowed errors, no magic |
| 4 | Code Quality | SOLID, robust, reliable, maintainable, scalable, secure, not over-engineered |
| 5 | Zero Technical Debt | No adapters for back compat; fix pre-existing errors; no hacked tests |
| 6 | Uncertainty Protocol | If unsure, ask — do not guess |
| 7 | Assumptions | State them, validate them, record the proof |
| 8 | Understand Architecture First | Read the design docs before changing code |
| 9 | Communication Tone | Neutral, factual, evidence-based |
| 10 | Never Force Push | Only with explicit permission |
| 11 | Adversarial Self-Review | Three-phase review; zero real findings before complete |
| 12 | Containment Before Abstraction | Contain external coupling now; abstract only with a second consumer or recurring pain |

## Next

- [Testing](testing.md) — how Rule 0 is applied in practice
- [Worklogs](worklogs.md) — where assumptions (Rule 7) and review findings (Rule 11) are recorded
- [Contributing overview](index.md) — the PR workflow that enforces these rules
