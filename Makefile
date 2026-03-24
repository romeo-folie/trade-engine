.DEFAULT_GOAL := build

BINARY       := te
BUILD_DIR    := .
MODULE       := github.com/romeo-folie/trade-engine
VERSION      := 0.1.0
LDFLAGS      := -ldflags "-X main.version=$(VERSION)"
FRONTEND_DIR := web/frontend
EMBED_DIR    := internal/web/dist

.PHONY: frontend-build
frontend-build: ## Build the React frontend and copy dist to the Go embed target
	cd $(FRONTEND_DIR) && npm install --silent && npm run build
	rm -rf $(EMBED_DIR)
	cp -r $(FRONTEND_DIR)/dist $(EMBED_DIR)
	@echo "Frontend dist synced → $(EMBED_DIR)"

.PHONY: build
build: frontend-build ## Build the te binary (includes frontend build)
	go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY) ./cmd/te

.PHONY: build-go-only
build-go-only: ## Build the te binary without rebuilding the frontend
	go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY) ./cmd/te

.PHONY: install
install: build ## Build and install te to /usr/local/bin
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
migrate: build-go-only ## Apply pending database migrations
	./$(BINARY) migrate

.PHONY: clean
clean: ## Remove build artifacts
	rm -f $(BINARY)

.PHONY: tidy
tidy: ## Tidy go.mod and go.sum
	go mod tidy

.PHONY: help
help: ## Show this help
	@grep -E '^[a-zA-A_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  %-20s %s\n", $$1, $$2}'
