# Copyright (C) 2026 Michael Kao
# SPDX-License-Identifier: AGPL-3.0-or-later

# Makefile for SDK canaries.
# Run from the repository root.

.PHONY: canary-ci canary-go-fast canary-python-fast canary-typescript-fast canary-mcp-fast canary-all canary-build

LLMSAFESPACES_URL ?= http://localhost:8080
LLMSAFESPACES_API_KEY ?=
LLMSAFESPACES_API_KEY_USER2 ?=
LLMSAFESPACES_EMAIL ?=
LLMSAFESPACES_PASSWORD ?=
LLMSAFESPACES_LLM_API_KEY ?=
LLMSAFESPACES_LLM_MODEL ?=

export LLMSAFESPACES_URL LLMSAFESPACES_API_KEY LLMSAFESPACES_API_KEY_USER2
export LLMSAFESPACES_EMAIL LLMSAFESPACES_PASSWORD
export LLMSAFESPACES_LLM_API_KEY LLMSAFESPACES_LLM_MODEL

# Run all ci:fast (Tier 1) canaries — no pod wait, no LLM credentials needed
canary-ci: canary-go-fast canary-python-fast canary-typescript-fast canary-mcp-fast

# ── Go SDK ────────────────────────────────────────────────────────────────────

GO_SHALLOW := s-health s-auth s-auth-config s-logout s-apikey s-user-settings \
              s-ws-crud s-ws-status s-secret-crud s-secret-reveal s-secret-audit \
              s-secret-bindings s-env-vars s-cred-crud s-error-format \
              s-mcp-crud
              s-ownership

GO_DEEP := d-ws-lifecycle d-session-ensure d-session-msg d-prompt-async \
           d-cred-bind d-cred-model-flow d-suspend-resume-session d-sse-events

canary-go-fast:
	@echo "=== Go SDK — ci:fast scenarios ==="
	@for s in $(GO_SHALLOW); do \
		echo "  Running $$s..."; \
		go run ./sdks/canary/go/scenarios/$$s/ || exit 1; \
	done

canary-go-deep:
	@echo "=== Go SDK — deep scenarios ==="
	@for s in $(GO_DEEP); do \
		echo "  Running $$s..."; \
		go run ./sdks/canary/go/scenarios/$$s/ || exit 1; \
	done

canary-go: canary-go-fast canary-go-deep

# ── Python SDK ────────────────────────────────────────────────────────────────

PY_SHALLOW := s_health s_auth s_auth_config s_logout s_apikey s_user_settings \
              s_ws_crud s_ws_status s_secret_crud s_secret_reveal s_secret_audit \
              s_secret_bindings s_env_vars s_cred_crud s_error_format s_ownership
              s_mcp_crud

PY_DEEP := d_ws_lifecycle d_session_ensure d_session_msg d_prompt_async \
           d_cred_bind d_cred_model_flow d_suspend_resume_session d_sse_events

canary-python-fast:
	@echo "=== Python SDK — ci:fast scenarios ==="
	@cd sdks/python && pip install -e ".[dev]" -q
	@for s in $(PY_SHALLOW); do \
		echo "  Running $$s..."; \
		python sdks/canary/python/scenarios/$$s.py || exit 1; \
	done

canary-python-deep:
	@echo "=== Python SDK — deep scenarios ==="
	@for s in $(PY_DEEP); do \
		echo "  Running $$s..."; \
		python sdks/canary/python/scenarios/$$s.py || exit 1; \
	done

canary-python: canary-python-fast canary-python-deep

# ── TypeScript SDK ────────────────────────────────────────────────────────────

TS_SHALLOW := s-health s-auth s-auth-config s-logout s-apikey s-user-settings \
              s-ws-crud s-ws-status s-secret-crud s-secret-reveal s-secret-audit \
              s-secret-bindings s-env-vars s-cred-crud s-error-format s-ownership
              s-mcp-crud

TS_DEEP := d-ws-lifecycle d-session-ensure d-session-msg d-prompt-async \
           d-cred-bind d-cred-model-flow d-suspend-resume-session d-sse-events

canary-typescript-fast:
	@echo "=== TypeScript SDK — ci:fast scenarios ==="
	@cd sdks/typescript && npm ci --silent && npx tsup src/index.ts --format esm,cjs --dts --silent
	@for s in $(TS_SHALLOW); do \
		echo "  Running $$s..."; \
		npx tsx sdks/canary/typescript/scenarios/$$s.ts || exit 1; \
	done

canary-typescript-deep:
	@echo "=== TypeScript SDK — deep scenarios ==="
	@for s in $(TS_DEEP); do \
		echo "  Running $$s..."; \
		npx tsx sdks/canary/typescript/scenarios/$$s.ts || exit 1; \
	done

canary-typescript: canary-typescript-fast canary-typescript-deep

# ── MCP Server ────────────────────────────────────────────────────────────────

canary-mcp-fast:
	@echo "=== MCP Server — ci:fast scenarios ==="
	go build -o /tmp/canary-mcp ./cmd/mcp/
	MCP_BINARY=/tmp/canary-mcp go run ./sdks/canary/mcp/

# ── All canaries ──────────────────────────────────────────────────────────────

canary-all: canary-go canary-python canary-typescript canary-mcp-fast

# ── Build check ───────────────────────────────────────────────────────────────

canary-build:
	@echo "Building Go canary modules..."
	@cd sdks/canary/go && go build ./... && echo "  Go canary: OK"
	@cd sdks/canary/mcp && go build ./... && echo "  MCP canary: OK"
	@echo "Done."

# ── Single scenario ───────────────────────────────────────────────────────────
# Usage: make canary-one SCENARIO=s-auth SDK=go
canary-one:
	@if [ "$(SDK)" = "go" ]; then go run ./sdks/canary/go/scenarios/$(SCENARIO)/; fi
	@if [ "$(SDK)" = "python" ]; then python sdks/canary/python/scenarios/$(SCENARIO).py; fi
	@if [ "$(SDK)" = "typescript" ]; then npx tsx sdks/canary/typescript/scenarios/$(SCENARIO).ts; fi
