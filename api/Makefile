.PHONY: build test clean run docker-build docker-push lint generate

# Variables
APP_NAME := agent-api
VERSION := $(shell git describe --tags --always --dirty)
COMMIT_SHA := $(shell git rev-parse HEAD 2>/dev/null || echo unknown)
BUILD_TIME := $(shell date -u '+%Y-%m-%d_%H:%M:%S')
LDFLAGS := -ldflags "-X github.com/lenaxia/llmsafespaces/pkg/version.Version=$(VERSION) -X github.com/lenaxia/llmsafespaces/pkg/version.CommitSHA=$(COMMIT_SHA) -X github.com/lenaxia/llmsafespaces/pkg/version.BuildTime=$(BUILD_TIME)"
DOCKER_REPO := llmsafespaces

# Go commands
build:
	go build $(LDFLAGS) -o bin/$(APP_NAME) ./cmd/api

test:
	go test -v -race -cover ./...

clean:
	rm -rf bin/
	rm -rf dist/

run:
	go run $(LDFLAGS) ./cmd/api

# Docker commands
docker-build:
	docker build -t $(DOCKER_REPO)/$(APP_NAME):$(VERSION) -t $(DOCKER_REPO)/$(APP_NAME):latest .

docker-push:
	docker push $(DOCKER_REPO)/$(APP_NAME):$(VERSION)
	docker push $(DOCKER_REPO)/$(APP_NAME):latest

# Development tools
lint:
	golangci-lint run ./...

generate:
	go generate ./...

# Database migrations
# Use the libpq KV connection-string form, NOT a postgres:// URL. Make's
# $(VAR) substitution is literal with no URL-encoding, so a URL embeds the
# password raw and breaks when DB_PASSWORD contains URL-reserved chars
# (/ ? # @ : % + =) — common when exported from `openssl rand -base64`.
# The KV form splits on whitespace and '=' and never needs encoding.
# Same fix as the Helm chart's migration Job (PR #437, issue #424).
migrate-up:
	migrate -path ./migrations -database "host=$(DB_HOST) port=$(DB_PORT) user=$(DB_USER) password=$(DB_PASSWORD) dbname=$(DB_NAME) sslmode=disable" up

migrate-down:
	migrate -path ./migrations -database "host=$(DB_HOST) port=$(DB_PORT) user=$(DB_USER) password=$(DB_PASSWORD) dbname=$(DB_NAME) sslmode=disable" down

# Help
help:
	@echo "Available targets:"
	@echo "  build         - Build the application"
	@echo "  test          - Run tests"
	@echo "  clean         - Remove build artifacts"
	@echo "  run           - Run the application locally"
	@echo "  docker-build  - Build Docker image"
	@echo "  docker-push   - Push Docker image to registry"
	@echo "  lint          - Run linters"
	@echo "  generate      - Run code generation"
	@echo "  migrate-up    - Run database migrations up"
	@echo "  migrate-down  - Run database migrations down"
