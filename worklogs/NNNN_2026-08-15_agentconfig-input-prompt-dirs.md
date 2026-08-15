# Worklog: AgentConfigInput gains first-class prompt/dirs sources (US-65.9 increment 2)

**Date:** 2026-08-15
**PR:** #859 (stacked on #857)
**Story:** first increment of US-65.9 "agent config render ownership" (#860)

---

## Context

End-state ownership model for the agent config (see #860): the
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
  increment 3 (#860).

## Review round 1 corrections (applied in-PR)

- The first version's clear-semantics test was vacuous (double-nested
  decode — its assertions could never fail; the reviewer
  mutation-proved it). Rewritten with correct decode shapes over the
  realistic post-boot-normalize file state.
- Clear/replace are now authoritative over prior renders: non-nil
  AdminPrompt strips the writer-owned `build.prompt` from the captured
  agent section; non-nil AllowedDirs strips exactly the tracked
  injected keys from `permissions.external_directory` (user-authored
  entries — deny rules, own allows, bare-string policies — preserved).
  `injectedDirs` is seeded from the side-car load and recovered from a
  previously rendered mode block, so a writer constructed over a
  rendered file (every real pod after #857) honors clears.
- `AllowedDirs` inputs are sanitized (empty dropped, deduped) and
  defensively copied — parity with the side-car path; no caller-slice
  aliasing.
- Rollback now captures and restores `agentRaw`/`modeRaw`/`injectedDirs`;
  pinned by a same-package failed-rebuild test asserting the sources.
- `TestAgentConfigInput_NilFieldsMeanLeaveUnchanged` extended to the
  two new fields.
- Worklog renamed to the `NNNN_` pre-merge sentinel; TBD refs → #859/#860.

## Verification

- Nine promptdirs tests: replace/clear/nil over side-car AND over
  rendered files, sanitization, caller-slice isolation, failed-rebuild
  rollback (scalars + captured raws), the production side-car+rendered
  configuration (union semantics), and sibling-build-field preservation.
- Round-2 additions: the side-car load UNIONs the recovered injected-dirs
  set (the first version overwrote it — a prior-lifetime /data/* would
  have resurrected on clear; mutation-verified); the preservation
  comments corrected to the actual fail-closed semantics (user-authored
  map-form allows are swept; deny/ask and bare-string survive;
  ambiguity dissolves in increment 3).
- All tests mutation-verified: deleting the Apply handling, the strip
  calls, the rollback restores, or the union each fails its test.
- Full `./pkg/agent/...` + `./cmd/workspace-agentd/` (incl. e2e) pass.
- `go build ./...` clean; gofmt clean.
