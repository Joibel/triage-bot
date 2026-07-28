# Project settings
BINARY_NAME := triage-bot
MODULE := github.com/Joibel/triage-bot
BUILD_DIR := bin
DOCKER_IMAGE := ghcr.io/joibel/triage-bot

# Version info
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD_TIME := $(shell date -u '+%Y-%m-%dT%H:%M:%SZ')

# Go settings
GOFLAGS := -trimpath
LDFLAGS := -s -w \
	-X $(MODULE)/internal/buildinfo.Version=$(VERSION) \
	-X $(MODULE)/internal/buildinfo.Commit=$(COMMIT) \
	-X $(MODULE)/internal/buildinfo.BuildTime=$(BUILD_TIME)
CGO_ENABLED := 0
TEST_TIMEOUT := 5m

# Pinned tool versions (kept in step with .github/workflows/ci.yml)
GOVULNCHECK_VERSION := v1.6.0

# Source files
GO_FILES := $(shell find . -name '*.go' -type f -not -path './vendor/*' -not -path './.devenv/*')

# Default target
.DEFAULT_GOAL := all

.PHONY: all
all: $(BUILD_DIR)/$(BINARY_NAME) ## Run all checks and build

.PHONY: check
check: fmt lint audit test ## Run all quality checks

$(BUILD_DIR)/$(BINARY_NAME): check $(GO_FILES) go.mod ## Compile the binary with stripped symbols
	CGO_ENABLED=$(CGO_ENABLED) go build $(GOFLAGS) -ldflags="$(LDFLAGS)" -o $@ .

.PHONY: run
run: $(BUILD_DIR)/$(BINARY_NAME) ## Build and execute the binary
	$(BUILD_DIR)/$(BINARY_NAME)

.PHONY: test
test: ## Run tests
	go test -v -timeout $(TEST_TIMEOUT) -race -covermode=atomic ./...

.PHONY: test/cover
test/cover: ## Generate coverage report
	go test -v -timeout $(TEST_TIMEOUT) -race -coverprofile=coverage.out -covermode=atomic -coverpkg=./... ./...
	go tool cover -html=coverage.out -o coverage.html

.PHONY: lint
lint: ## Run golangci-lint
	golangci-lint run

.PHONY: lint/fix
lint/fix: ## Auto-fix lint issues
	golangci-lint run --fix

.PHONY: fmt
fmt: ## Format code
	gofmt -s -w $(GO_FILES)
	goimports -w -local $(MODULE) $(GO_FILES)

.PHONY: vet
vet: ## Run go vet
	go vet ./...

.PHONY: audit
audit: ## Run security and dependency checks
	go mod tidy -diff
	go mod verify
	go run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...

.PHONY: deps
deps: ## Download and tidy dependencies
	go mod download
	go mod tidy

.PHONY: update
update: ## Update dependencies to latest versions
	go get -u ./...
	go mod tidy

.PHONY: docker
docker: ## Build Docker image
	docker build \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		--build-arg BUILD_TIME=$(BUILD_TIME) \
		-t $(DOCKER_IMAGE):$(VERSION) \
		-t $(DOCKER_IMAGE):latest \
		.

.PHONY: docker/push
docker/push: ## Build and push Docker image
	docker buildx build \
		--platform linux/amd64,linux/arm64 \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		--build-arg BUILD_TIME=$(BUILD_TIME) \
		-t $(DOCKER_IMAGE):$(VERSION) \
		-t $(DOCKER_IMAGE):latest \
		--push \
		.

.PHONY: clean
clean: ## Remove build artifacts
	rm -rf $(BUILD_DIR)
	rm -f coverage.out coverage.html

.PHONY: help
help: ## Show available targets
	@grep -E '^[a-zA-Z_/-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-15s\033[0m %s\n", $$1, $$2}'
