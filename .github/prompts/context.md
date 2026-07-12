Repository: LLMSafeSpaces — a Kubernetes-first platform (Go) for running AI agents securely in isolated workspaces. Every workspace runs `opencode serve` as a persistent HTTP server with a PVC-backed persistent filesystem. Single maintainer: @lenaxia.

Key directories:
- api/               — Go API service (Gin) + MCP server; reverse proxy to workspace agents, workspace/credential/secret management
- controller/        — Kubernetes operator (controller-runtime); manages Workspace, RuntimeEnvironment, InferenceRelay CRDs
- runtimes/          — Container images (Python, Node.js, Go); hardened environments with opencode serve and credential injection
- pkg/               — Shared packages (types, kubernetes client, redact, logger, secrets, utilities)
- cmd/               — Top-level binaries (api, mcp, redact, repolint, seal-key, workspace-agentd, relay-router, relay-proxy)
- design/            — Architecture and design documents (EVOLUTION-V2.md is authoritative)
- design/SECURITY.md — Defense-in-depth security model
- .github/workflows/ — CI/CD pipelines

**Before doing anything else: read README-LLM.md at the repo root.** It contains the full architecture overview, coding standards, hard rules, and development workflow. Every response must be consistent with it.

---

## Commands

Post a comment on the issue or PR using any of these commands:

- `/ai` — re-assess the current issue or PR in full (issue responder or full PR re-review)
- `/ai <text>` — address a specific request, e.g. `/ai can you also update the tests for the workspace service?`
- `/review [text]` — explicit PR code review, optionally focused on a specific area
- `/fix <description>` — fix a bug: branch, TDD regression tests, PR, iterate through review until approved, merge
- `/implement <description>` — implement a feature/story: TDD, multi-agent workflow, PR, iterate until approved, merge
- `/test <target>` — write or improve tests: TDD, PR, iterate until approved, merge
- `/analyze [text]` — deep read-only analysis, posts findings as a comment (no code changes)
- `/explain <topic>` — explain code or architecture, posts explanation as a comment (no code changes)
- `/security [text]` — security-focused review against design/SECURITY.md
- `/triage [text]` — triage an issue: categorize, prioritize, suggest labels
- `/design [text]` — iterate on a design document under `design/` before implementing: opens a PR, iterates through review, **holds for `/merge`** (never auto-merges)
- `/merge` — explicitly merge an approved PR (squash). Use after `/design`, or after `/fix`/`/implement`/`/test`/`/security` invoked with `--no-merge`
- `/help` — show full command reference

Text after the command is appended to the prompt for custom tuning. All code-change commands (`/fix`, `/implement`, `/test`, `/security`) follow the review-iterate-approve-merge workflow: branch → PR → auto-review → fix → push → re-review → repeat until approved → merge. Append `--no-merge` to any of them to hold the merge until you post `/merge`. `/design` always holds.

The assistant will be triggered automatically and will read README-LLM.md and the full thread before responding.
