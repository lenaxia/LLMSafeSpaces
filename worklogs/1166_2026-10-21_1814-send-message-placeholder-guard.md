# #1814 — send_message placeholder-class guard (2026-10-21)

## Problem
The mid-write emission-corruption mode replaces a drafted send_message
body with a literal placeholder token as the call fires; the sender
rarely notices, and every such delivery costs the recipient a turn.

## Changes
1. cmd/workspace-agentd/mcp_tools.go — isPlaceholderClassPayload +
   the exact-match reject set {placeholder, placeholder-removed,
   <placeholder>, todo, tbd, asdf, qwerty} (trimmed, case-folded);
   mcpSendMessage rejects with the re-compose guidance BEFORE any
   delivery. Narrow by charter: NOT a length heuristic — "ok"/"x"/
   "test"/"ping"/probe traffic and sentences merely CONTAINING a
   placeholder word pass untouched.
2. cmd/workspace-agentd/mcp_server.go — the send_message tool
   description hardened per the issue text ("complete intended
   content / re-compose / placeholder-class payloads rejected").

## Red-first evidence
TestMCPSendMessage_PlaceholderClassRejected: 14/14 cases red against
the pre-guard tree (the tokens DELIVERED today), green after. The
pass-set (11 payloads incl. "ok", "x", "test", "placeholder text I
meant to send", "TODO: fix this later") and the whitespace-variant
set (3) pin the narrow-exact-match contract.

## Checklist
- [x] Unit pins: placeholder-class rejected with guidance; ok/x/probe
      and containment cases pass (28 subtests total).
- [x] Tool description updated.
- [x] CHANGELOG [Unreleased].
- [ ] Adversarial subagent verify before push.
- [ ] Push + PR (never self-merge).
