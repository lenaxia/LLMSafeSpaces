# Epic-65 status truth fix: status lines, README-LLM contract section, Relay Config Subsystem rewrite

## Session Overview

- **PR:** #858 (`docs/epic-65-status` → `main`)
- **Scope:** docs-only truth sweep of epic-65 status claims and the agent-session-contract / relay-config sections of `README-LLM.md`
- **Duration:** 2026-08-15 04:31Z → 08:28Z (~4h, 10 commits, 11 review rounds)
- **Trigger:** found while planning follow-up work against the board during the 2026-08-15 incident review — the epic-65 README still said `Status: Definition (not yet in implementation)` although US-65.1–65.7 merged 2026-08-09→2026-08-11 UTC (PRs #691–#727).

## What changed

1. **Epic README status line** (`design/stories/epic-65-agent-session-contract/README.md`): `Definition` → `Implementation — US-65.1–65.7 merged (PRs #691–#727, 2026-08-09→2026-08-11 UTC); US-65.8 in progress (history path done; SSE, OpenAPI/SDKs, mobile remain).` Also the `pkg/agent/agent.go` seam reference `:31` → `:91` (`AgentRuntime`'s current line).
2. **README-LLM contract section** (`:370/:372`): "in definition" → "in implementation; US-65.8 in progress"; `pkg/session/` is not "to be created" (10 source files exist).
3. **README-LLM `:398/:400`** — stale future tense ("when the contract lands, those hacks are deleted") replaced with per-hack current truth, each attribution verified against git history and the tree:
   - patch-stripping behavior removed **pre-epic** (2026-05-26, `3c0b1d52`); US-65.5 deleted only the stale `proxy_filter_test.go` artifact
   - legacy history parsing off the live request path since **US-65.4** (#716 wired the adapter unconditionally; #721 made `GetHistory` adapter-first — tree comment `proxy_handlers.go:356` "Adapter path (US-65.4)"); survives only as an adapter-nil fallback
   - relay-config fragilities contained behind `Apply` in **US-65.1**
   - question/permission **event translation on the SSE path remains live** behind the opencode dialect (`proxy_events.go:196` guard; dialect unconditionally wired at `app.go:192`) pending the adapter SSE migration
   - frontend opencode-shape SSE parsing goes when US-65.8 lands
4. **Relay Config Subsystem section** rewritten from the deleted pre-`Apply` writer world (`cmd/workspace-agentd/agent_config_writer.go`, `newAgentConfigWriter`/`SetRelay`/`SetProviders`/`Rebuild`) to the `agent.AgentConfigWriter` seam:
   - writers table: `ConfigWriter.Apply(AgentConfigInput)` (`pkg/agent/opencode/configwriter.go`); boot normalize (#857), pre-boot relay, relay injection, **credential reloads that stage llm-provider secrets** (Apply gated by `formatted != nil`, `secrets.go:809` — `FormatProviders` returns nil for provider-less batches)
   - boot sequence: materialize (`Materialize` (resets tmpfs, then re-applies) → `EnrichProviders` → `FlushProviders` → `applyMCPServersToConfig` (Epic 53) → `applyWorkspaceConfig` → conditional `applyRelayConfigPreBoot`) → `ensureBootAgentConfig` empty-input normalize → injector (~T+7s) with the `HasRelay()` short-circuit (`skipped_pre_boot_applied`)
   - reload sequence: `Lock` → `Materialize` → **cache write (`:783`)** → `EnrichProviders` → `FormatProviders` → `Apply` → `Unlock`; step 2 gates the restart: llm-provider batches → `StageCredentials` (no reboot); reboot only for `env-secret`/`api-key` batches (`shouldRestart`), via `makeSessionAwareRestartDecision` (immediate-if-idle else deferred, bounded)
   - `#443` cache paragraph corrected: the cache is written right after `Materialize` succeeds so it survives later-step (`FormatProviders`/`Apply`) 500s
   - `#852` correctly scoped to the injector's kill path only (reload's session-aware deferral predates it)
5. **Stories index** (`design/stories/README.md:89`): epic-65 row "Definition only" → "In implementation" with the merge window.
6. **PR-body discipline:** body kept in sync with the diff each round (squash-merge message defaults to title+body — round-2's wrong date would have been re-enshrined).

## Review-round history (the institutional-memory part)

| Round | Blocking finding | Resolution |
|---|---|---|
| 1 | replacement date 2026-08-13 false (merges were 08-09→08-11 UTC); 2 other stale sites unswept; "US-65.8 pending" imprecise; correction-history parenthetical | dates fixed; README-LLM:370/372 + stories index swept; "in progress (history path done; …)" wording; parenthetical dropped |
| 2 | `:400` stale future tense ("when the contract lands") contradicted the corrected header 30 lines up | rewritten with per-hack attribution |
| 3 | "backend hacks deleted in US-65.5" overstated — history parsing + question/permission translation still in tree | scoped to live request path (later rounds refined further) |
| 4 | question/permission translation is NOT adapter-nil fallback — it runs live on the SSE event path (dialect-guarded); relay-config attribution over-swept | live-dialect wording; per-hack attribution split |
| 5 | (canary red herring: S-RATE-LIMIT 429-body failure is pre-existing on main — #861's fix; also main's NNNN sentinel from #852 broke repolint briefly, fixed by #865) | n/a — no doc change |
| 6 | history live-path removal was **US-65.4** (#716/#721), not US-65.5; linked Relay Config Subsystem section still documented the deleted pre-`Apply` writer | attribution fixed; whole subsystem section rewritten |
| 7 | `#852` mis-scoped to reload restart; pre-boot relay runs in the **materialize process**, not as agentd boot step; cache-write arrow misplaced vs `FormatProviders` | all three corrected |
| 8 | cache write precedes `FormatProviders` (design intent: survives later-step failure); M1–M4 minors (injector skip clause, no `ModelSelection` caller, all 3+1 pre-boot-relay conditions, `makeSessionAwareRestartDecision` + `EnrichProviders`) | arrows + adjacent `#443` paragraph; **all minors fixed same pass** (strategy change: address everything, not just blocking, to stop round-promotion) |
| 9/10 | reload restart presented as unconditional — `shouldRestart` gates to env-secret/api-key batches; table "every credential reload" over-broad; boot `EnrichProviders` missing; phantom explicit `reset()` step | restart gating + `StageCredentials` path; table scoped; enrich added; "Materialize(batch) (resets tmpfs, then re-applies)" |
| 11 | worklog mandatory (README-LLM:777–793; no triviality exemption; 3h56m/10 commits/10 rounds) — all technical content verified | this file |

**Drifts surfaced for the codebase (beyond docs):** the provider-less reload batch drops the entire `Apply` including MCP staging (tracked in #860); `stripVerboseQuery`/openapi `verbose` residue vs US-65.5's done-when; US-65.6 PartType repolint rule absent; stale `app.go:202-203` comment; `proxy_input.go` REST endpoints still on the dialect; stories index rows for epics 53/63/64/66 still say "Definition only".

## Verification

- Every code claim in the rewritten sections was re-verified against `origin/main` with file:line evidence (the AI reviewer independently re-verified each round; round 11: "No factual error remains in the diff").
- Docs-only: no tests affected; full CI green on the final commit except the pre-existing S-RATE-LIMIT canary (fails on main; fix is open PR #861).
- Merge-window timestamps cross-checked via squash-merge committer dates in the pre-reseed history (`git log v0.14.4`) and `gh pr view` — the skeptical pass's lone refutation (timezone non-conversion) was itself rejected.
