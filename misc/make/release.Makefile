# ── Versioning ──────────────────────────────────────────────
# Usage:
#   make release MODULE=pkg/hertz/doer VERSION=v0.1.0   → tag: pkg/hertz/doer/v0.1.0
#   make release MODULE=. VERSION=v0.1.0                  → tag: v0.1.0 (root module)
#   make release-list                                      → show all modules & latest tags
#   make release-list MODULE=pkg/hertz/doer              → show tags for one module

.PHONY: release
release: ## Tag & push a module release (MODULE=<dir> VERSION=v<semver>)
	@if [ -z "$(MODULE)" ] || [ -z "$(VERSION)" ]; then \
		echo "Usage: make release MODULE=<dir> VERSION=v<semver>"; \
		echo "  MODULE=. for root, or relative dir like pkg/hertz/doer"; \
		echo "  VERSION=v0.1.0"; \
		exit 1; \
	fi
	@if ! echo "$(VERSION)" | grep -qE '^v[0-9]+\.[0-9]+\.[0-9]+$$'; then \
		echo "Error: VERSION must match v<major>.<minor>.<patch> (e.g. v0.1.0)"; \
		exit 1; \
	fi
	@MOD="$(MODULE)"; \
	if [ "$$MOD" = "." ]; then \
		TAG="$(VERSION)"; \
	else \
		MOD=$$(echo "$$MOD" | sed 's|^./||; s|/$$||'); \
		if [ ! -f "$$MOD/go.mod" ]; then \
			echo "Error: $$MOD/go.mod not found"; \
			exit 1; \
		fi; \
		TAG="$$MOD/$(VERSION)"; \
	fi; \
	echo "Module:  $$MOD"; \
	echo "Tag:     $$TAG"; \
	echo ""; \
	echo "Cleanup tag..."; \
	git fetch --prune --prune-tags origin; \
	EXISTING=$$(git tag -l "$$TAG" 2>/dev/null); \
	if [ -n "$$EXISTING" ]; then \
		echo "Error: tag $$TAG already exists"; \
		exit 1; \
	fi; \
	echo "Creating tag..."; \
	git tag -a "$$TAG" -m "release $$TAG"; \
	echo "Pushing tag..."; \
	git push origin "$$TAG"; \
	echo ""; \
	echo "✓ Released $$TAG"

.PHONY: release-list
release-list: ## List modules and their latest tags (MODULE=<dir> for one module)
	@if [ -n "$(MODULE)" ] && [ "$(MODULE)" != "." ]; then \
		PREFIX=$$(echo "$(MODULE)" | sed 's|^./||; s|/$$||'); \
		echo "Tags for $$PREFIX:"; \
		git tag -l "$$PREFIX/v*" --sort=-v:refname | grep -E "^$$PREFIX/v[0-9]" || true; \
	elif [ "$(MODULE)" = "." ]; then \
		echo "Tags for root module:"; \
		git tag -l "v*" --sort=-v:refname | grep -v '/'; \
	else \
		echo "Module                          Latest Tag"; \
		echo "──────────────────────────────  ──────────────────────────────────"; \
		printf "%-30s  %s (root)\n" "." "$$(git tag -l 'v*' --sort=-v:refname | grep -v '/' | head -1)"; \
		find . -name "go.mod" -print | sort | while read f; do \
			dir=$$(dirname "$$f" | sed 's|^./||'); \
			latest=$$(git tag -l "$$dir/v*" --sort=-v:refname | grep -E "^$$dir/v[0-9]" | head -1); \
			printf "%-30s  %s\n" "$$dir" "$${latest:-(none)}"; \
		done; \
	fi