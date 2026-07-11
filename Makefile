# Go parameters
GOCMD=go
GOBUILD=$(GOCMD) build
GOCLEAN=$(GOCMD) clean
GOTEST=$(GOCMD) test
GOGET=$(GOCMD) get
GOMOD=$(GOCMD) mod
BINARY_NAME=llmsafespaces
BINARY_UNIX=$(BINARY_NAME)_unix

# Build targets
.PHONY: all build clean test cover lint fmt fmt-check imports imports-check vet generate deepcopy \
        helm-lint helm-template helm-template-debug helm-install-dry-run helm-package helm-render \
        helm-chart-test helm-deploy \
        openapi-validate \
        repolint repolint-build chart-sync-migrations install-hooks \
        relay-bin \
        check tools-install \
        gitleaks govulncheck trivy-fs trivy-config security-scan \
        migration-roundtrip migration-fk-cascade migration-idempotent migration-data-cleanup migration-safety migration-safety-docker \
        test-full cover-floor mutation \
        release-tag release-verify-changelog

all: test build

build:
	$(GOBUILD) -o $(BINARY_NAME) -v ./api/cmd/api

clean:
	$(GOCLEAN)
	rm -f $(BINARY_NAME)
	rm -f $(BINARY_UNIX)

test:
	$(GOTEST) -v ./...

cover:
	$(GOTEST) -coverprofile=coverage.out ./...
	$(GOCMD) tool cover -html=coverage.out

lint:
	golangci-lint run

fmt:
	$(GOCMD) fmt ./...

# fmt-check: verify gofmt has been run. Used by pre-commit and CI to
# block PRs that contain unformatted Go. Lists offending files and
# exits non-zero. To fix locally: `make fmt`.
fmt-check:
	@unformatted=$$(gofmt -l . | grep -v '/node_modules/' || true); \
	if [ -n "$$unformatted" ]; then \
		echo "gofmt: the following files are not formatted:"; \
		echo "$$unformatted"; \
		echo ""; \
		echo "Run 'make fmt' to fix."; \
		exit 1; \
	fi

imports:
	@which goimports >/dev/null 2>&1 || $(GOCMD) install golang.org/x/tools/cmd/goimports@latest
	goimports -w $$(find . -name '*.go' -not -path './frontend/node_modules/*' -not -path './sdks/*/node_modules/*')

# imports-check: verify goimports has been run (import grouping +
# unused-import removal). Same enforcement model as fmt-check.
imports-check:
	@which goimports >/dev/null 2>&1 || $(GOCMD) install golang.org/x/tools/cmd/goimports@latest
	@bad=$$(goimports -l . | grep -v '/node_modules/' || true); \
	if [ -n "$$bad" ]; then \
		echo "goimports: the following files have wrong imports:"; \
		echo "$$bad"; \
		echo ""; \
		echo "Run 'make imports' to fix."; \
		exit 1; \
	fi

vet:
	$(GOCMD) vet ./...

generate:
	$(GOCMD) generate ./...

deepcopy:
	chmod +x ./hack/update-deepcopy.sh
	./hack/update-deepcopy.sh

# Cross compilation
build-linux:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GOBUILD) -o $(BINARY_UNIX) -v ./api/cmd/api

build-linux-arm64:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GOBUILD) -o $(BINARY_UNIX)-arm64 -v ./api/cmd/api

# relay-proxy: standalone relay binary for OCI/GCP VMs (Epic 42)
relay-bin:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GOBUILD) -o deploy/relay-proxy-arm64 ./cmd/relay-proxy/
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GOBUILD) -o deploy/relay-proxy-amd64 ./cmd/relay-proxy/

docker-build:
	docker build -t $(BINARY_NAME):latest .

docker-run:
	docker run --rm -p 8080:8080 $(BINARY_NAME):latest

# ---------------------------------------------------------------------------
# Helm chart targets
# ---------------------------------------------------------------------------
HELM=helm
CHART_DIR=helm
RELEASE_NAME?=llmsafespaces
RELEASE_NS?=llmsafespaces

helm-lint:
	$(HELM) lint $(CHART_DIR)

helm-template:
	$(HELM) template $(RELEASE_NAME) $(CHART_DIR) -n $(RELEASE_NS)

helm-template-debug:
	$(HELM) template $(RELEASE_NAME) $(CHART_DIR) -n $(RELEASE_NS) --debug

# Renders against the live cluster's API server. Requires kubeconfig.
helm-install-dry-run:
	$(HELM) install $(RELEASE_NAME) $(CHART_DIR) -n $(RELEASE_NS) --create-namespace --dry-run

helm-package:
	$(HELM) package $(CHART_DIR) -d dist/

# helm-render: lint + template the chart against the bundled defaults
# (values.yaml). Catches:
#   - syntax errors / missing template files
#   - undefined values referenced by templates
#   - invalid Helm chart structure (missing Chart.yaml, etc.)
# Output is discarded; we only care about the exit code. Pre-commit
# and CI use this; for debugging use helm-template or
# helm-template-debug to see the rendered manifests.
helm-render:
	$(HELM) lint $(CHART_DIR)
	$(HELM) template $(RELEASE_NAME) $(CHART_DIR) -n $(RELEASE_NS) >/dev/null

# helm-chart-test: run the Go-based chart rendering tests (chart_test.go).
# These tests render manifests via `helm template` and assert structural
# invariants that `helm-render` (lint + template) cannot catch — e.g. that
# the MCP namespace uses .Release.Namespace, that probes use tcpSocket, that
# additionalHosts includes the /api path, etc.
#
# Requires helm on PATH. Silently skips if helm is absent (see chart_test.go).
# Run by CI in both the `test` and `test-full` jobs (helm installed there).
# Also run by `make check` so local contributors catch regressions before push.
helm-chart-test:
	$(GOTEST) ./helm/...

# helm-deploy: upgrade the release on the live cluster. Enforces:
#   1. Local branch is synced with origin/main (prevents deploying stale
#      chart files — the incident cause from worklog 0140 where migration
#      000014 was missing from the ConfigMap because the local repo was
#      behind the CI-built image).
#   2. CRDs are applied before helm upgrade (Helm 3 does not upgrade CRDs
#      in crds/ on upgrade).
#   3. Chart lint passes.
#
# Usage:
#   make helm-deploy                                          # defaults
#   make helm-deploy RELEASE_NS=default IMAGE_TAG=sha-abc1234 # override
#   make helm-deploy HELM_FLAGS="--set frontend.enabled=true"  # extra flags
#
# Local overrides (gitignored): create helm/values.local.yaml
# to persist cluster-specific values (e.g. redis.host, mcp.enabled=false).
# The file is automatically included on every helm-deploy when present.
IMAGE_TAG?=
HELM_FLAGS?=
LOCAL_VALUES_FILE := $(CHART_DIR)/values.local.yaml
LOCAL_VALUES_FLAG := $(if $(wildcard $(LOCAL_VALUES_FILE)),-f $(LOCAL_VALUES_FILE))
helm-deploy:
	@echo "== helm-deploy: checking git sync =="
	@git fetch origin main --quiet
	@LOCAL=$$(git rev-parse HEAD) && REMOTE=$$(git rev-parse origin/main) && \
	  if [ "$$LOCAL" != "$$REMOTE" ]; then \
	    echo "ERROR: local HEAD ($$LOCAL) != origin/main ($$REMOTE)"; \
	    echo "Run 'git pull --ff-only' first. Deploying stale chart files"; \
	    echo "causes missing migrations and broken deployments."; \
	    exit 1; \
	  fi
	$(if $(LOCAL_VALUES_FLAG),@echo "== helm-deploy: using local overrides $(LOCAL_VALUES_FILE) ==",@echo "WARNING: $(LOCAL_VALUES_FILE) not found — deploying with chart defaults only. Create this file to persist cluster-specific overrides (redis host, ingress, rbac scope, etc.)")
	@echo "== helm-deploy: applying CRDs =="
	kubectl apply -f $(CHART_DIR)/crds/
	@echo "== helm-deploy: linting chart =="
	$(HELM) lint $(CHART_DIR) $(LOCAL_VALUES_FLAG)
	@echo "== helm-deploy: upgrading release $(RELEASE_NAME) in $(RELEASE_NS) =="
	$(HELM) upgrade $(RELEASE_NAME) $(CHART_DIR) -n $(RELEASE_NS) \
	  $(LOCAL_VALUES_FLAG) \
	  $(if $(IMAGE_TAG),--set api.image.tag=$(IMAGE_TAG) \
	                      --set controller.image.tag=$(IMAGE_TAG) \
	                      --set frontend.image.tag=$(IMAGE_TAG) \
	                      --set runtimeEnvironments.base.image.tag=$(IMAGE_TAG)) \
	  $(HELM_FLAGS)
	@echo "== helm-deploy: waiting for rollout =="
	@kubectl -n $(RELEASE_NS) rollout status deploy/$(RELEASE_NAME)-api --timeout=120s || true
	@kubectl -n $(RELEASE_NS) rollout status deploy/$(RELEASE_NAME)-controller --timeout=120s || true
	@echo "== helm-deploy: verifying cluster CRDs match chart (worklog 0465) =="
	@$(MAKE) repolint-build >/dev/null
	@./bin/repolint -cluster-drift || { \
	  echo "ERROR: deployed CRDs drift from chart YAMLs after kubectl apply."; \
	  echo "       The kubectl apply step at the start of helm-deploy should have"; \
	  echo "       resolved this. If you see this message, the apply was rejected"; \
	  echo "       (often because Helm's hook order or RBAC interfered)."; \
	  echo "       Investigate before proceeding — workspace activate paths will"; \
	  echo "       silently lose pruned fields like spec.suspend."; \
	  exit 1; \
	}
	@echo "== helm-deploy: done =="

# ---------------------------------------------------------------------------
# OpenAPI validation
# ---------------------------------------------------------------------------
openapi-validate:
	$(MAKE) -C sdks validate

# ---------------------------------------------------------------------------
# Repository layout lint (migration numbering, worklog numbering, chart drift)
# ---------------------------------------------------------------------------
# repolint: lint checks invoked by .githooks/pre-commit and CI. Catches the
# failure modes that have caused production incidents:
#   - duplicate database migration version numbers (silent skip on cluster)
#   - non-sequential migration version numbers (gap = deleted migration)
#   - duplicate worklog numbers (history confusion)
#   - drift between api/migrations/ and helm/migrations/
#   - drift between Go CRD struct fields and chart CRD openAPIV3Schema
#     (apiserver silently drops unknown fields; see worklog 0118-0119)
# See pkg/repolint/sequence_test.go and pkg/repolint/crd_drift_test.go for
# the regression cases and worklog 0098 for the originating incident.
repolint: repolint-build
	./bin/repolint

# repolint-build: build the repolint binary WITHOUT running checks. Used by
# .githooks/post-rewrite, which needs the binary available even when the
# worklog sequence is currently broken (the exact state the hook fixes).
# The plain `repolint` target builds THEN runs checks, so it returns
# non-zero on a worklog collision — making it useless as a build step
# inside the very hook that resolves the collision.
repolint-build:
	$(GOBUILD) -o bin/repolint ./cmd/repolint

# chart-sync-migrations: copy api/migrations/*.sql into helm/migrations/.
# Run this every time you add a migration so the Helm-bundled copy stays in
# sync with the canonical one. The pre-commit hook will fail if you forget.
chart-sync-migrations:
	$(GOBUILD) -o bin/repolint ./cmd/repolint
	./bin/repolint -fix-drift

# install-hooks: wire .githooks/ into git's hook path. Run once per fresh
# clone. After this, every `git commit` runs `make repolint` and rejects the
# commit on failure, and every `git rebase` / `git commit --amend` runs
# the post-rewrite hook which auto-renumbers any worklog that collides
# with origin/main (the failure mode behind the long string of
# "chore: fix worklog number collision" commits in this repo's history).
install-hooks:
	git config core.hooksPath .githooks
	chmod +x .githooks/pre-commit
	chmod +x .githooks/post-rewrite
	@echo "Installed: git core.hooksPath = .githooks"
	@echo "Pre-commit hook runs repolint on every commit (worklog collisions auto-fix)."
	@echo "Post-rewrite hook auto-renumbers worklogs after every rebase / --amend."

# ---------------------------------------------------------------------------
# Quality gates (Epic 19: pre-merge automation)
# ---------------------------------------------------------------------------
# tools-install: install the developer tools the gates rely on. Run once
# per fresh clone, or after a Go-toolchain upgrade. Idempotent.
tools-install:
	$(GOCMD) install golang.org/x/tools/cmd/goimports@latest
	$(GOCMD) install github.com/client9/misspell/cmd/misspell@latest
	@which golangci-lint >/dev/null 2>&1 || \
		$(GOCMD) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest
	@echo "Tools installed: goimports, misspell, golangci-lint"
	@echo "Other tools (helm, gitleaks, govulncheck, trivy) are checked"
	@echo "by the relevant gates and installed on demand."

# check: run all the pre-merge quality gates locally. Mirrors what CI
# will block on. Use this before pushing to avoid CI round-trips.
#   - fmt-check     : gofmt is clean
#   - imports-check : goimports is clean
#   - vet           : go vet finds nothing
#   - lint          : golangci-lint finds nothing
#   - helm-render   : chart lints and renders
#   - repolint      : migration/worklog/chart-drift sequence checks
check: fmt-check imports-check vet lint helm-render helm-chart-test repolint
	@echo ""
	@echo "All quality gates passed."

# pre-commit-fix: auto-fix the mechanical issues that pre-commit blocks on,
# and re-stage the modified files so the next `git commit` succeeds.
#
# Use this when pre-commit fails on:
#   - gofmt           (alignment, indentation)
#   - goimports       (import grouping / unused imports)
#   - misspell        (US-locale spelling: "behaviour" → "behavior", etc)
#   - chart drift     (api/migrations/ ↔ helm/migrations/)
#   - staticcheck S1016 (struct → struct conversion idiom)
#
# Does NOT auto-fix:
#   - errcheck / bodyclose / sqlclosecheck (semantic; need code changes)
#   - duplicate migration numbers (load-bearing; need human rename decision)
#   - CRD drift (need Go ↔ chart schema reconciliation)
#   - gitleaks findings (rotate the secret + remove from diff)
#
# After running, only the staged files that pre-commit had complained about
# are added back; we DO NOT touch unstaged user changes.
pre-commit-fix:
	@echo "== pre-commit-fix: snapshot staged files =="
	@staged=$$(git diff --cached --name-only --diff-filter=ACM | grep -E '\.(go|sql)$$' || true); \
	if [ -z "$$staged" ]; then \
		echo "No Go/SQL files staged; only chart-drift and worklog-fix will run."; \
	fi; \
	echo "== gofmt =="; \
	$(GOCMD) fmt ./... >/dev/null; \
	echo "== goimports =="; \
	$(MAKE) -s imports >/dev/null; \
	echo "== misspell =="; \
	which misspell >/dev/null 2>&1 || $(GOCMD) install github.com/client9/misspell/cmd/misspell@latest; \
	misspell -w -locale US $$(find . -name '*.go' -not -path './frontend/node_modules/*' -not -path './sdks/*/node_modules/*') >/dev/null 2>&1 || true; \
	echo "== chart-sync-migrations =="; \
	$(MAKE) -s chart-sync-migrations >/dev/null; \
	echo "== fix-worklogs =="; \
	$(GOBUILD) -o bin/repolint ./cmd/repolint >/dev/null 2>&1; \
	./bin/repolint -fix-worklogs; \
	echo "== restage modified files =="; \
	if [ -n "$$staged" ]; then \
		echo "$$staged" | xargs -r git add; \
	fi; \
	git add helm/migrations/ 2>/dev/null || true; \
	git add worklogs/ 2>/dev/null || true; \
	echo ""; \
	echo "Auto-fixes applied and re-staged. Re-run 'git commit' to retry."

# pre-commit-fix-strict: like pre-commit-fix but ALSO runs the gates after
# fixing, so you can confirm the commit will now go through. Slower (~30s
# golangci-lint run) but no surprises.
pre-commit-fix-strict: pre-commit-fix
	@echo ""
	@echo "== verifying gates =="
	@$(MAKE) -s repolint
	@$(MAKE) -s fmt-check
	@$(MAKE) -s imports-check
	@$(MAKE) -s lint
	@echo ""
	@echo "All gates pass. 'git commit' should succeed now."

# recover-stash: dig dangling commits out of git's lost-and-found and
# print which ones contain Go/SQL/worklog/markdown files. Used when a
# rebase + stash dance lost untracked files (the failure mode in
# worklog 0123). Read-only — never modifies anything.
#
# Once you find the SHA, recover individual files with:
#   git show <sha>:path/to/file > path/to/file
recover-stash:
	@echo "Scanning git fsck --lost-found for dangling commits..."
	@for sha in $$(git fsck --lost-found 2>/dev/null | grep "dangling commit" | awk '{print $$3}'); do \
		files=$$(git show --stat $$sha 2>/dev/null | grep -E '\.(go|sql|md|tsx?|yaml)$$' | head -8 || true); \
		if [ -n "$$files" ]; then \
			echo ""; \
			echo "=== $$sha ==="; \
			git log --oneline -1 $$sha 2>/dev/null; \
			echo "$$files"; \
		fi; \
	done
	@echo ""
	@echo "To recover a file from a SHA above:"
	@echo "  git show <sha>:path/to/file > path/to/file"

# ---------------------------------------------------------------------------
# Security scanners (Epic 19, PR B)
# ---------------------------------------------------------------------------
# Three complementary scanners:
#   gitleaks    -- secrets in working tree (test fixtures allow-listed
#                  via .gitleaks.toml)
#   govulncheck -- Go vulnerability database; only fails on CALLED vulns
#   trivy fs    -- multi-language CVEs (npm, pip, mvn, go.mod, ...)
#   trivy config-- K8s manifest + Dockerfile misconfig
#
# Run individually for fast feedback or all of them via `security-scan`.

gitleaks:
	@which gitleaks >/dev/null 2>&1 || { \
		echo "gitleaks not installed; install from https://github.com/gitleaks/gitleaks"; \
		exit 1; }
	gitleaks detect --redact -c .gitleaks.toml --no-banner

govulncheck:
	@which govulncheck >/dev/null 2>&1 || $(GOCMD) install golang.org/x/vuln/cmd/govulncheck@latest
	govulncheck ./...

trivy-fs:
	@which trivy >/dev/null 2>&1 || { \
		echo "trivy not installed; install from https://github.com/aquasecurity/trivy"; \
		exit 1; }
	trivy fs --severity HIGH,CRITICAL --exit-code 1 \
		--skip-dirs frontend/node_modules \
		--skip-dirs sdks/typescript/node_modules \
		--skip-dirs sdks/vscode-llmsafespaces/node_modules \
		--ignorefile .trivyignore \
		.

trivy-config:
	@which trivy >/dev/null 2>&1 || { \
		echo "trivy not installed; install from https://github.com/aquasecurity/trivy"; \
		exit 1; }
	trivy config --severity HIGH,CRITICAL --exit-code 1 \
		--skip-dirs frontend/node_modules \
		--skip-dirs sdks/typescript/node_modules \
		--skip-dirs sdks/vscode-llmsafespaces/node_modules \
		--skip-dirs design/stories/epic-17-security-review \
		--skip-dirs local \
		--ignorefile .trivyignore \
		.

# security-scan: run all four scanners. Mirrors the CI security-scan
# workflow exactly. Slow (~30s); use the individual targets for tighter
# loops.
security-scan: gitleaks govulncheck trivy-fs trivy-config
	@echo ""
	@echo "All security scanners passed."

# ---------------------------------------------------------------------------
# Migration safety (Epic 19, PR C)
# ---------------------------------------------------------------------------
# Runs the migration round-trip + FK cascade + idempotency suite from
# .github/workflows/migration-safety.yml, but locally against a Postgres
# you supply via PG* env vars (PGHOST, PGUSER, PGPASSWORD, PGDATABASE).
#
# Setup:
#   docker run -d --rm --name pg-test -p 5432:5432 \
#     -e POSTGRES_USER=llmsafespaces -e POSTGRES_PASSWORD=test \
#     -e POSTGRES_DB=llmsafespaces postgres:16
#   export PGHOST=localhost PGUSER=llmsafespaces PGPASSWORD=test PGDATABASE=llmsafespaces
#   make migration-safety
#
# All three sub-targets re-create the schema from scratch in a single
# database, so they're not parallelizable. Run them in order or use
# the meta target.

migration-roundtrip:
	@command -v psql >/dev/null 2>&1 || { echo "psql not installed"; exit 1; }
	@: $${PGHOST:?must set PG* env vars}
	bash hack/migration-roundtrip.sh

migration-fk-cascade:
	@command -v psql >/dev/null 2>&1 || { echo "psql not installed"; exit 1; }
	@: $${PGHOST:?must set PG* env vars}
	bash api/migrations/test/fk_cascade.sh

migration-idempotent:
	@command -v psql >/dev/null 2>&1 || { echo "psql not installed"; exit 1; }
	@: $${PGHOST:?must set PG* env vars}
	bash hack/migration-idempotent.sh

migration-data-cleanup:
	@command -v psql >/dev/null 2>&1 || { echo "psql not installed"; exit 1; }
	@: $${PGHOST:?must set PG* env vars}
	bash hack/migration-data-cleanup.sh

migration-safety: migration-roundtrip migration-idempotent migration-fk-cascade migration-data-cleanup
	@echo ""
	@echo "All migration safety checks passed."

# migration-safety-docker: run the full migration-safety suite against a
# throwaway postgres:16 container that this target starts and tears down.
# Requires Docker + host-side PostgreSQL client tools (psql, pg_isready).
# Mirrors .github/workflows/migration-safety.yml exactly (all four
# checks), so what runs in CI runs identically here.
#
# Used by .githooks/pre-commit (only when .sql files are staged AND docker
# is available) to give local feedback before push. Skips cleanly when
# docker is absent, so it never blocks a commit on a docker-less machine.
#
# Override the image with LSS_PGIMAGE (defaults to postgres:16). Skip the
# pre-commit invocation with LSS_SKIP_MIGRATION_GATE=1.
migration-safety-docker:
	bash hack/migration-safety-docker.sh

# ---------------------------------------------------------------------------
# Test rigor (Epic 19, PR D)
# ---------------------------------------------------------------------------

# test-full: full test suite (no -short) with race detector. Mirrors
# the `test-full` job in ci.yml. Use this before pushing if you've
# touched anything performance-sensitive.
test-full:
	$(GOTEST) -timeout 600s -race -count=1 ./...

# cover-floor: run the coverage-instrumented test suite and assert the
# total coverage is at or above the floor (50%). Mirrors the gate in
# the CI `test` job — run locally to verify before pushing.
cover-floor:
	$(GOTEST) -timeout 300s -race -short \
		-coverprofile=coverage.out \
		-covermode=atomic \
		-coverpkg=./... \
		./...
	@total=$$($(GOCMD) tool cover -func=coverage.out | awk '/^total:/ {print $$3}' | tr -d '%'); \
	if awk -v t="$$total" 'BEGIN { exit !(t < 50) }'; then \
		echo "FAIL: total coverage $${total}% is below the 50.0% floor."; \
		exit 1; \
	fi; \
	echo "OK: total coverage $${total}% (floor 50.0%)"

# mutation: run gremlins mutation testing against the security-critical
# packages. Slow (~5-15 min per package on a laptop). Mirrors the
# nightly mutation.yml workflow. Set TARGET=path/to/pkg to scope.
mutation:
	@which gremlins >/dev/null 2>&1 || { \
		echo "gremlins not installed; install with"; \
		echo "  GOSUMDB=off GOTOOLCHAIN=local go install github.com/go-gremlins/gremlins/cmd/gremlins@v0.6.0"; \
		exit 1; }
	@target=$${TARGET:-pkg/secrets}; \
	echo "Running gremlins on ./$${target} ..."; \
	gremlins unleash ./$${target} --workers 2

# release-verify-changelog: confirm CHANGELOG.md has a section for VERSION.
# Used by release-tag and by CI on tag push.
release-verify-changelog:
	@test -n "$(VERSION)" || { echo "VERSION is required (make release-verify-changelog VERSION=0.3.0)"; exit 1; }
	@grep -E "^## \[$(VERSION)\] - [0-9]{4}-[0-9]{2}-[0-9]{2}" CHANGELOG.md >/dev/null \
		|| { echo "FAIL: CHANGELOG.md is missing a section for v$(VERSION)."; \
		     echo "Add a heading like '## [$(VERSION)] - $$(date -u +%Y-%m-%d)' with the release notes."; \
		     exit 1; }
	@echo "OK: CHANGELOG section for v$(VERSION) found."

# release-tag: cut an annotated release tag and push it. The Release
# workflow on GitHub Actions builds/publishes everything from there.
#
# Usage:
#   make release-tag VERSION=0.3.0
#
# Checks:
#   - VERSION matches semver (X.Y.Z)
#   - main is up to date with origin
#   - CHANGELOG.md has a section for the version
#   - tag doesn't already exist
release-tag:
	@test -n "$(VERSION)" || { echo "VERSION is required (make release-tag VERSION=0.3.0)"; exit 1; }
	@echo "$(VERSION)" | grep -E '^[0-9]+\.[0-9]+\.[0-9]+$$' >/dev/null \
		|| { echo "FAIL: VERSION must be semver X.Y.Z (no leading 'v')."; exit 1; }
	@git rev-parse -q --verify "refs/tags/v$(VERSION)" >/dev/null \
		&& { echo "FAIL: tag v$(VERSION) already exists."; exit 1; } || true
	@$(MAKE) -s release-verify-changelog VERSION=$(VERSION)
	@echo "Fetching origin..."
	@git fetch origin main >/dev/null
	@behind=$$(git rev-list --count HEAD..origin/main); \
	if [ "$$behind" -ne 0 ]; then \
		echo "FAIL: HEAD is $$behind commits behind origin/main. Rebase first."; \
		exit 1; \
	fi
	@echo "Cutting tag v$(VERSION)..."
	@git tag -a "v$(VERSION)" -m "Release v$(VERSION)"
	@echo "Pushing tag v$(VERSION) to origin..."
	@git push origin "v$(VERSION)"
	@echo ""
	@echo "Tag v$(VERSION) pushed. The Release workflow is now running:"
	@echo "  https://github.com/lenaxia/LLMSafeSpaces/actions/workflows/release.yml"
	@echo "Watch for the GitHub Release to appear at:"
	@echo "  https://github.com/lenaxia/LLMSafeSpaces/releases/tag/v$(VERSION)"
