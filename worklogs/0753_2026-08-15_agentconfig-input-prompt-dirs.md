# Worklog: AgentConfigInput gains first-class prompt/dirs sources (US-65.9 increment 2)

**Date:** 2026-08-15
**PR:** TBD (stacked on #857)
**Story:** first increment of US-65.9 "agent config render ownership" (issue TBD)

---

## Context

End-state ownership model for the agent config (see issue TBD): the
config file is a rendered artifact of declared sources, and the
`AgentConfigInput` type fully describes those sources. Until this
change, the admin system prompt and allowed external directories were
writer-construction options (`WithAdminPromptPath` /
`WithAllowedDirsPath`) — loaded from the bootstrap side-cars but
invisible to the `Apply` contract. A caller could update providers,
model, relay, and MCP servers through the seam, but revising the
prompt or dirs required reconstructing the writer.

## Changes

- `pkg/agent/agentconfig.go`: `AgentConfigInput` gains `AdminPrompt
  *AdminPromptChange` and `AllowedDirs *AllowedDirsChange` with the
  same pointer semantics as every other source (nil = unchanged;
  non-nil = replace; non-nil empty = clear). Documented bootstrap/
  runtime split: construction seeds from side-car files (the
  materialize staging contract); Apply updates thereafter.
- `pkg/agent/opencode/configwriter.go`: Apply handles both fields;
  rollback captures `adminPrompt`/`allowedDirs` alongside the other
  sources (failed Apply leaves all in-memory state unchanged).
- `HasRelay` already queries writer source state (`w.relay != nil`,
  seeded with a sentinel by `loadExisting`) — the other half of this
  increment's scope was already satisfied; no change needed.

## Not done (deliberately)

- Callers still use the side-car load path at construction; no caller
  yet sends the new fields at runtime (no feature needs it today —
  Rule 12: the seam is complete, usage arrives with the first feature
  that updates prompts at runtime, e.g. live admin-prompt reload).
- The writer remains self-merging (`loadExisting`) — pure render is
  increment 3 (see issue TBD).

## Verification

- 3 new tests: replace-over-side-car, clear, nil-unchanged.
- Full `./pkg/agent/...` + `./cmd/workspace-agentd/` (incl. e2e) pass.
- `go build ./...` clean; gofmt clean.
