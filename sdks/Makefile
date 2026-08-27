# SDK generation and validation targets
.PHONY: validate generate-all generate-ts generate-python generate-go generate-java sdk-check clean contract-test contract-test-mock

SPEC := openapi.yaml

# Validate the OpenAPI spec using go-based validator (no npm required)
validate:
	@echo "Validating OpenAPI specification..."
	@go run ./validate/main.go $(SPEC)
	@echo "✓ OpenAPI spec is valid"

# sdk-check is the CI-enforceable core of E9 (Epic 67): everything that can
# gate a merge without live infrastructure — spec validity, the spec's own
# structural tests, and the bidirectional spec↔router parity contract.
# Per-language compile + wire-level usage tests run in the sdk-contract CI
# job (.github/workflows/ci.yml) and locally per toolchain.
sdk-check: validate
	@cd validate && go test -timeout 60s ./...
	@cd .. && go test -timeout 240s -run TestOpenAPIRouterContract ./api/internal/server
	@echo "✓ SDK surface is current (spec valid + router parity holds)"

# Generate all SDKs (placeholder — implemented in US-14.3 through US-14.6)
generate-all: validate generate-ts generate-python generate-go generate-java

generate-ts: validate
	@echo "TypeScript SDK generation not yet implemented (US-14.3)"

generate-python: validate
	@echo "Python SDK generation not yet implemented (US-14.4)"

generate-go: validate
	@echo "Go SDK generation not yet implemented (US-14.5)"

generate-java: validate
	@echo "Java SDK generation not yet implemented (US-14.6)"

# contract-test runs every Hurl file in tests/contract/ against a running API
# instance specified by BASE_URL + TOKEN. Used in live / staging validation.
# Usage: make contract-test BASE_URL=http://localhost:8080 TOKEN=lsp_xxx
contract-test:
	@test -n "$(BASE_URL)" || (echo "BASE_URL is required" && exit 1)
	@test -n "$(TOKEN)" || (echo "TOKEN is required" && exit 1)
	@for f in tests/contract/*.hurl; do \
		echo "→ $$f"; \
		hurl --variable base_url=$(BASE_URL) --variable token=$(TOKEN) $$f || exit 1; \
	done
	@echo "✓ All contract tests passed"

# contract-test-mock spins up a Prism mock from the OpenAPI spec and runs
# every Hurl file against it. Validates that the SDKs' expected requests
# match the spec without requiring a live API. Requires npx (npm).
contract-test-mock:
	@command -v hurl >/dev/null 2>&1 || { echo "hurl not installed — see https://hurl.dev/docs/installation.html"; exit 1; }
	@command -v npx  >/dev/null 2>&1 || { echo "npx not installed — requires Node.js"; exit 1; }
	@echo "Starting Prism mock server from $(SPEC) on :4010..."
	@npx --yes @stoplight/prism-cli mock $(SPEC) --port 4010 > /tmp/prism.log 2>&1 & PRISM_PID=$$!; \
		ready=0; \
		for i in $$(seq 1 60); do \
			curl -s -o /dev/null http://localhost:4010/ && { ready=1; break; }; \
			kill -0 $$PRISM_PID 2>/dev/null || { echo "Prism died during startup:"; cat /tmp/prism.log; exit 1; }; \
			sleep 1; \
		done; \
		[ $$ready -eq 1 ] || { echo "Prism not listening on :4010 after 60s:"; cat /tmp/prism.log; kill $$PRISM_PID; exit 1; }; \
		echo "Prism ready (pid $$PRISM_PID). Running Hurl suite..."; \
		for f in tests/contract/*.hurl; do \
			echo "→ $$f"; \
			hurl --variable base_url=http://localhost:4010 --variable token=mock-token $$f || { kill $$PRISM_PID; exit 1; }; \
		done; \
		kill $$PRISM_PID; \
		echo "✓ All contract tests passed against Prism mock"

clean:
	rm -rf typescript/ python/ go/ java/
