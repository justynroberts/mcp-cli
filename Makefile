BINARY   := mcp-cli
PKG      := github.com/justynroberts/mcp-cli
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  := -s -w -X $(PKG)/internal/cli.Version=$(VERSION)
DIST     := dist

# Portability is the point: CGO off everywhere, so every target is a single
# static binary with no runtime dependencies.
export CGO_ENABLED = 0

PLATFORMS := \
	darwin/arm64 darwin/amd64 \
	linux/amd64 linux/arm64 linux/arm \
	windows/amd64 windows/arm64

.PHONY: build
build: ## Build ./mcp-cli for this machine
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) .

.PHONY: install
install: ## Install mcp-cli into $$GOBIN (or $$GOPATH/bin)
	go install -trimpath -ldflags "$(LDFLAGS)" .

.PHONY: test
test: ## Run all tests
	go test ./...

.PHONY: test-race
test-race: ## Run all tests under the race detector
	go test -race ./...

.PHONY: cover
cover: ## Run tests with a coverage summary
	go test -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -1

.PHONY: lint
lint: ## gofmt + go vet
	@test -z "$$(gofmt -l . | tee /dev/stderr)" || (echo "run: gofmt -w ." && exit 1)
	go vet ./...

.PHONY: tidy
tidy: ## Tidy go.mod/go.sum
	go mod tidy

.PHONY: release
release: ## Cross-compile every supported platform into dist/
	@rm -rf $(DIST) && mkdir -p $(DIST)
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; ext=""; \
		[ "$$os" = "windows" ] && ext=".exe"; \
		echo "  $$os/$$arch"; \
		GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "$(LDFLAGS)" \
			-o $(DIST)/$(BINARY)-$$os-$$arch$$ext . || exit 1; \
	done
	@ls -lh $(DIST)

.PHONY: clean
clean: ## Remove build artefacts
	rm -rf $(BINARY) $(DIST) coverage.out

.PHONY: help
help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'

.DEFAULT_GOAL := help
