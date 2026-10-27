# squall developer tasks. Run `make help` for a list.

SHELL := /bin/bash
.DEFAULT_GOAL := help

# Development tools are pinned in tools/go.mod and run with `go tool`.
GOTOOL := go tool -modfile=tools/go.mod

.PHONY: help
help: ## Show this help
	@awk 'BEGIN {FS = ":.*##"} /^[a-zA-Z_-]+:.*##/ {printf "  \033[36m%-10s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

.PHONY: check
check: lint vuln test ## Run linters, the vulnerability check and tests

.PHONY: lint
lint: ## Lint and check formatting
	$(GOTOOL) golangci-lint run ./...
	cd sdktest && go tool -modfile=../tools/go.mod golangci-lint run ./...

.PHONY: fmt
fmt: ## Format code
	$(GOTOOL) golangci-lint fmt ./...
	cd sdktest && go tool -modfile=../tools/go.mod golangci-lint fmt ./...

.PHONY: vuln
vuln: ## Check dependencies for known vulnerabilities
	$(GOTOOL) govulncheck ./...
	cd sdktest && go tool -modfile=../tools/go.mod govulncheck ./...

.PHONY: test
test: ## Run tests with the race detector
	go test -race -shuffle=on -cover ./...
	cd sdktest && go test -race -shuffle=on -coverpkg=github.com/parallelworks/squall/... ./...

.PHONY: bench
bench: ## Run benchmarks
	go test -run '^$$' -bench . -benchmem ./...

.PHONY: fuzz
fuzz: ## Fuzz the filter parsers and the bucket servers for a minute each
	go test -run '^$$' -fuzz '^FuzzParseFilter$$' -fuzztime 60s ./gcp/compute/
	go test -run '^$$' -fuzz '^FuzzParseFilter$$' -fuzztime 60s ./gcp/lustre/
	go test -run '^$$' -fuzz '^FuzzServeHTTP$$' -fuzztime 60s ./aws/s3/
	go test -run '^$$' -fuzz '^FuzzServeHTTP$$' -fuzztime 60s ./gcp/storage/

.PHONY: tidy
tidy: ## Tidy go.mod files
	go mod tidy
	cd tools && go mod tidy
	cd sdktest && go mod tidy
