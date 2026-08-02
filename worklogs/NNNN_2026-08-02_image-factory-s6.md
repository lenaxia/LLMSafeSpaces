# Worklog NNNN — Image Factory S6: LLM failure explainer

**Date:** 2026-08-02
**Scope:** S6 — wires the platform's existing LLM infrastructure into the
failure explainer interface defined in S5.

## Summary

Implemented `llmExplainer` — calls the in-cluster LLM (LiteLLM/vLLM) to
explain build failures in plain language + attribute them to a specific
extension when possible. Full degradation mode: LLM down/timeout/parse-
failure → fallback string, callback proceeds normally.

## What was built

- `imagefactory_explainer.go` — `llmExplainer` implementing `failureExplainer`.
  OpenAI-compatible `/v1/chat/completions` call; structured JSON response
  (explanation + attributedExtension). 15s timeout (callback path, not
  user request). All error paths degrade to fallback.
- `config.go` — `ImageFactory` config section (ImageRepo, CallbackURL,
  LLMExplainer.BaseURL/Model/APIKey).
- `app.go` — wires `NewLLMExplainer` when `LLMExplainer.BaseURL` is set.

## Tests (9 cases)

- Disabled when BaseURL or Model empty → fallback
- Happy path (test server returns structured JSON → parsed correctly)
- LLM unreachable → fallback
- LLM returns non-JSON → raw content used as explanation
- LLM returns empty explanation → fallback
- LLM returns 500 → fallback
- LLM returns empty choices → fallback
- Prompt includes extensions + log tail

All -race clean, fmt/vet clean, full api/... builds.
