# Worklog 0660 — Epic 53 Follow-up Hardening

**Date:** 2026-08-02
**Epic:** 53 — MCP Server Integration
**Scope:** Post-merge hardening follow-up addressing LOW-severity review findings + DoD item 15.

## Summary

Addressed all remaining review findings from PR #613's bot review and the follow-up PR #615 review.

## Changes

### SSRF hardening
- Replaced 6-host literal blocklist with proper IP range checking (RFC1918, loopback, link-local, CGNAT, IPv6 ULA) + DNS resolution
- Added `ip.IsUnspecified()` check for `0.0.0.0`/`::` (regression caught by review — old blocklist caught it, IP ranges alone don't)
- Used `net.DefaultResolver.LookupHost(ctx, host)` instead of `net.LookupHost` (noctx linter)

### Kill-switch scope + fail-closed
- Gated all org mutations (`OrgUpdate`, `OrgDelete`) not just `OrgCreate`
- Changed `orgAdminAllowed` to fail-CLOSED on read error (security control that fails open provides no kill capability)

### Env/Header validation
- Env var names validated via `validation.ValidateEnvVarName` (blocks `LD_PRELOAD` etc)
- Header names checked for CRLF injection
- Extracted `validateMCPServerUpdate` so validation runs on the PUT path too (review caught a bypass: create with safe URL, update to metadata endpoint)

### DRY rendering
- Extracted `RenderOpencodeMCPServerEntry` to `pkg/agentd/secrets` — shared between `AgentConfigWriter.rebuild` and boot-time `applyMCPServersToConfig`

### README-LLM.md
- Added "MCP Server Integration" section documenting the as-built system (DoD item 15)

## Tests added
- SSRF: RFC1918 (`10.x`, `192.168.x`, `172.16.x`), loopback, link-local, localhost, `0.0.0.0`
- Env injection: `LD_PRELOAD`, empty name
- Header CRLF injection
- OrgUpdate kill-switch disabled → 403
- OrgDelete kill-switch disabled → 403
- Kill-switch fail-closed on read error → 403
- Update-path SSRF reject
- Update-path env injection reject
