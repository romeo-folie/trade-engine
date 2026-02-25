.DEFAULT_GOAL := build

BINARY    := te
BUILD_DIR := .
MODULE    := github.com/romeo-folie/trade-engine
VERSION   := 0.1.0
LDFLAGS   := -ldflags "-X main.version=$(VERSION)"

.PHONY: build
build: ## Build the te binary
	go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY) ./cmd/te

.PHONY: install
install: build ## Install te to /usr/local/bin
	install -m 755 $(BINARY) /usr/local/bin/$(BINARY)
	@echo "Installed: /usr/local/bin/$(BINARY)"

.PHONY: test
test: ## Run all tests
	go test ./... -v -timeout 60s

.PHONY: test-short
test-short: ## Run tests without integration tests
	go test ./... -short -timeout 30s

.PHONY: lint
lint: ## Run go vet and staticcheck
	go vet ./...
	@which staticcheck > /dev/null 2>&1 && staticcheck ./... || echo "staticcheck not installed, skipping"

.PHONY: migrate
migrate: build ## Apply pending database migrations
	./$(BINARY) migrate

.PHONY: clean
clean: ## Remove build artifacts
	rm -f $(BINARY)

.PHONY: tidy
tidy: ## Tidy go.mod and go.sum
	go mod tidy

.PHONY: help
help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  %-18s %s\n", $$1, $$2}'
