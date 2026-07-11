# Development Workflow

How to get a local LLMSafeSpaces environment running, run the tests, and iterate. If you're new to the project, read the [Contributing overview](index.md) and the [Engineering Rules](rules.md) first.

## Prerequisites

| Tool | Version | Why |
|------|---------|-----|
| [Go](https://go.dev/) | 1.25+ | API service, controller, all Go binaries |
| [Docker](https://www.docker.com/) | any recent | `kind` runs on it; image builds |
| [kind](https://kind.sigs.k8s.io/) | 0.20+ | Local Kubernetes cluster |
| [kubectl](https://kubernetes.io/docs/reference/kubectl/) | 1.28+ | Talk to the cluster |
| [Helm](https://helm.sh/) | 3.12+ | Deploy the chart |
| [Node.js](https://nodejs.org/) | 22+ | Frontend (React + Vite) |

Optional but recommended: `jq` (for working with API responses in the shell), `make`.

Verify:

```bash
go version          # go1.25+
docker version
kind version        # kind v0.20+
kubectl version --client --short
helm version --short
node --version      # v22+
```

## Clone

```bash
git clone https://github.com/lenaxia/LLMSafeSpaces.git
cd LLMSafeSpaces
```

## Install pre-commit hooks

Run this **immediately** after cloning. It is not optional.

```bash
make install-hooks
```

Every commit then runs: `repolint`, `gofmt`, `goimports`, `golangci-lint`, and `helm-render` checks. Without hooks installed, broken commits reach CI and waste time — install them once and forget about it.

## Bootstrap a local cluster

The `local/` directory has three scripts that drive a complete `kind`-based environment:

```bash
cd local

# Creates a kind cluster, builds all images, deploys the chart
./bootstrap.sh
```

This takes ~5 minutes the first time (image builds dominate). When it finishes, you should see:

```bash
$ kubectl get pods -n llmsafespaces
NAME                                READY   STATUS    RESTARTS   AGE
llmsafespaces-api-xxx               1/1     Running   0          2m
llmsafespaces-controller-xxx        1/1     Running   0          2m
llmsafespaces-frontend-xxx          1/1     Running   0          2m
postgres-xxx                        1/1     Running   0          2m
redis-xxx                           1/1     Running   0          2m
```

Port-forward the API to reach it from your host:

```bash
kubectl port-forward -n llmsafespaces svc/llmsafespaces-api 8080:8080 &
```

The API is now at `http://localhost:8080`. From here, follow the [Quickstart](../getting-started/quickstart.md) to register a user and drive a session.

### Run the e2e suite

```bash
cd local

# Set LLM_* env vars to enable the prompt round-trip
# and patch-part stripping checks.
LLM_BASE_URL=https://your-llm/v1 \
LLM_API_KEY=sk-... \
LLM_MODEL=default \
./test.sh
```

### Tear down

```bash
./local/teardown.sh
```

Removes the kind cluster and all resources.

## Run tests locally

```bash
# All Go tests, with race detector and timeout (the canonical command)
go test -timeout 90s -race ./...

# Or via make
make test
```

!!! tip "Always use `-timeout`"
    `go test ./...` without a timeout can hang forever on a stuck test. The canonical invocation is `go test -timeout 90s -race ./...`. The `-race` flag is mandatory — race conditions are bugs.

See [Testing](testing.md) for the full testing strategy: TDD workflow, table-driven tests, mocks, the cover floor, envtest, and the e2e suite.

## Run the linter

```bash
make lint
```

Runs `golangci-lint` (and other linters via the pre-commit hooks). Lint must pass before merge.

## Build

```bash
# API, controller, all Go binaries
make build

# Cross-compile for Linux amd64
make build-linux

# Docker images
docker build -f api/Dockerfile -t llmsafespaces/api:dev .
docker build -f controller/Dockerfile -t llmsafespaces/controller:dev .
docker build -f runtimes/base/Dockerfile -t llmsafespaces/runtime-base:dev runtimes/base
docker build -f frontend/Dockerfile -t llmsafespaces/frontend:dev frontend
```

## The Go module proxy gotcha

If `proxy.golang.org` is unreachable — common in sandboxed or air-gapped dev environments — module downloads fail. Use `GOPROXY=direct` to download modules directly from source repositories (GitHub, etc.):

```bash
# Download all modules (bypassing proxy.golang.org and sum.golang.org)
GOPROXY=direct GONOSUMCHECK=* GONOSUMDB=* go mod download

# Run tests with direct proxy
GOPROXY=direct GONOSUMCHECK=* GONOSUMDB=* go test -timeout 120s -short ./...

# Build with direct proxy
GOPROXY=direct GONOSUMCHECK=* GONOSUMDB=* go build ./...
```

This works whenever the source repos (e.g. `github.com`) are reachable, even if the Go module proxy is not.

## Iterating on the frontend

The frontend is a React 19 + TypeScript + Vite SPA under `frontend/`.

```bash
cd frontend
npm install
npm run dev      # Vite dev server with hot reload
```

The dev server proxies API requests to the backend. Point it at your port-forwarded API (default `http://localhost:8080`).

### Frontend tests

```bash
cd frontend

# Unit tests (vitest)
npm test

# E2E (Playwright)
npx playwright test
```

See [Testing](testing.md) for the frontend test stack.

## Running the controller locally

```bash
cd controller

# Build
go build -o bin/manager .

# Run against the current kubeconfig (e.g. your kind cluster)
go run ./main.go --enable-leader-election=false

# Install CRDs into the cluster
bash scripts/install-crds.sh
```

## Database migrations

```bash
cd api
make migrate-up     # apply
make migrate-down   # rollback
```

## Code generation

When you modify CRD types in `pkg/apis/llmsafespaces/v1/*_types.go`, regenerate the DeepCopy implementations:

```bash
make deepcopy
# Verify and commit the generated file
git add pkg/apis/llmsafespaces/v1/zz_generated.deepcopy.go
```

`pkg/types/types.go` contains API transfer objects only — no generated deepcopy. Manual `DeepCopy` methods are implemented only where needed (types passed by pointer across goroutine boundaries).

## Common commands cheat sheet

```bash
# --- Root module ---
go mod tidy                     # Tidy dependencies
make test                       # Run all tests
go test -timeout 30s -race -v ./...   # Verbose with race detector
make cover                      # Coverage
make fmt                        # Format
make vet                        # Static analysis
make lint                       # Lint
make build                      # Build API binary
make build-linux                # Cross-compile for Linux amd64
make docker-build               # Docker build

# --- API service (from api/) ---
cd api && make build            # Build
cd api && make run              # Run locally
cd api && make migrate-up       # Migrate up
cd api && make migrate-down     # Rollback

# --- Controller ---
cd controller && go build -o bin/manager .
cd controller && go run ./main.go --enable-leader-election=false

# --- Code generation ---
make deepcopy                   # Regenerate DeepCopy methods
```

## During work

1. Write tests first — TDD, always ([Rule 0](rules.md#rule-0-test-driven-development-tdd)).
2. Use strongly-typed structs ([Rule 1](rules.md#rule-1-type-safety-first)).
3. Commit at each logical unit of work with a descriptive message.
4. Run `make test && make lint` before pushing.

## After completing work

1. Run all tests: `make test` or `go test -timeout 30s -race ./...`.
2. Run the linter: `make lint`.
3. Verify tests pass.
4. **Write a worklog entry** — see [Worklogs](worklogs.md).
5. Commit, push, open a PR, and enter the review-iterate-approve-merge cycle described in the [Contributing overview](index.md).

## Next

- [Engineering Rules](rules.md) — the bar every change must meet
- [Testing](testing.md) — TDD, table-driven tests, mocks, the cover floor
- [Worklogs](worklogs.md) — institutional memory
