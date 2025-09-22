# linkerdev Makefile

.PHONY: help release clean build

help: ## Show this help message
	@echo "linkerdev - Available targets:"
	@echo ""
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2}'

release: ## Create a new release (usage: make release VERSION=v0.1.0-alpha6)
	@if [ -z "$(VERSION)" ]; then \
		echo "Error: VERSION is required. Usage: make release VERSION=v0.1.0-alpha6"; \
		exit 1; \
	fi
	@echo "🚀 Creating release $(VERSION)..."
	@echo "📝 Updating version in main.go..."
	sed -i.bak 's/var version = ".*"/var version = "$(VERSION)"/' cmd/linkerdev/main.go
	rm cmd/linkerdev/main.go.bak
	@echo "📝 Updating version in relay main.go..."
	sed -i.bak 's/var version = ".*"/var version = "$(VERSION)"/' cmd/relay/main.go
	rm cmd/relay/main.go.bak
	@echo "📝 Committing version changes..."
	git add cmd/linkerdev/main.go cmd/relay/main.go
	git commit -m "Release $(VERSION)"
	@echo "🏷️  Creating tag $(VERSION)..."
	git tag $(VERSION)
	@echo "📤 Pushing commit and tag..."
	git push origin main
	git push origin $(VERSION)
	@echo "🎉 Release $(VERSION) completed!"

build: ## Build the linkerdev CLI locally
	@echo "🔨 Building linkerdev CLI..."
	cd cmd/linkerdev && go build -o ../../bin/linkerdev .
	cd cmd/relay && go build -o ../../bin/linkerdev-relay .
	@echo "✅ Build complete! Binaries in bin/"

clean: ## Clean build artifacts
	@echo "🧹 Cleaning build artifacts..."
	rm -rf bin/
	@echo "✅ Clean complete!"

test: ## Run tests
	@echo "🧪 Running tests..."
	go test ./...
	@echo "✅ Tests complete!"
