# U-8 — rpc.md / rpc-commands.md history: were commands ever REMOVED or renamed?

Method: page the GitHub commits API for both paths (full history), then diff
the documented command-set (`###` headings, normalized) across every
doc-truth-up / restructure commit. Evidence files: `u8-commit-history.txt`
(the raw subject list).

## Commit-count history (2025-12 → 2026-11-14)

- `docs/rpc.md`: 50 commits (pre-split: everything lived here)
- `docs/rpc-commands.md`: 3 commits since the 2026-09-22 split (#9898)

## Command-set diffs (the operative check)

```
pre-truthup1 (17ed3fa60~1) vs truthup1 (17ed3fa60, 2025-12-31 "match actual implementation"):
    → NO heading-set difference
#9898~1 vs #9898 (2026-09-22, the rpc.md → rpc.md+rpc-commands.md+json.md split):
    → the split REORGANIZED headings (event docs moved to json.md); no command lost
#9898 vs v1.5.2 (2026-11-10, the pin):
    → IDENTICAL: 34 commands
```

The pinned 34-command set (v1.5.2 rpc-commands.md): abort, abort_bash,
abort_retry, bash, clear_queue, clone, compact, cycle_model,
cycle_thinking_level, export_html, follow_up, fork, get_available_models,
get_available_thinking_levels, get_commands, get_entries, get_fork_messages,
get_last_assistant_text, get_messages, get_session_stats, get_state,
get_tree, new_session, prompt, set_auto_compaction, set_auto_retry,
set_follow_up_mode, set_model, set_session_name, set_steering_mode,
set_thinking_level, steer, switch_session.

## Removals found in the ENTIRE history (none are RPC commands)

- `484d7e06b` 2026-01-01 — "Consolidate session events: **remove
  session_before_new/session_new**, add reason field to switch events"
  (EXTENSION session events, not RPC wire commands)
- `df3f5f41c` 2026-01-11 — "Rename /branch command to /fork" (TUI slash
  command, not RPC)
- `e54dff7ef` 2026-02-03 — SlashCommandSource "template"→"prompt"
  (extension API rename)

Everything else is additive (get_entries 2026-06-11, set_session_name
2026-01-30, get_commands 2026-01-28, clear_queue 2026-08-25, disposition
2026-09-25, thinking levels 2026-07) or semantic fixes with docs updated
in-lockstep (#11746/#11752/#11812 shutdown semantics, 2026-11-03/04).

## Post-pin drift watch (one commit since v1.5.2)

- `ede360e4d` 2026-11-13 — rpc-commands.md `get_last_assistant_text`
  description clarified (skips aborted/tool-only turns): **doc-only**, the
  command set and response shape unchanged. The S6 bump-gate covers it on
  the next pin refresh.

## Ruling input

The RPC command surface has never shed or renamed a command across its
documented history; evolution is additive + lifecycle-semantics fixes
(the two-week shutdown churn the scoping report flagged, §7). The S6
fixture regime (pin + golden JSONL + bump gate) is calibrated correctly:
diff-on-bump, no handshake needed.
