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
	@echo "📝 Creating release branch..."
	git checkout -b release/$(VERSION)
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
	@echo "📤 Pushing branch and tag..."
	git push origin release/$(VERSION)
	git push origin $(VERSION)
	@echo "✅ Release $(VERSION) created successfully!"
	@echo "🔄 Switching back to main..."
	git checkout main
	@echo "📝 Merging release branch..."
	git merge release/$(VERSION) --no-ff -m "Merge release $(VERSION)"
	@echo "📤 Pushing main..."
	git push origin main
	@echo "🧹 Cleaning up release branch..."
	git branch -d release/$(VERSION)
	git push origin --delete release/$(VERSION)
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
