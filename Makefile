# Automatically includes all .Makefile files inside the 'misc/make/' directory
-include $(wildcard ./misc/make/*.Makefile)

ifeq ($(OS), Windows_NT)
	HELP_CMD = Select-String "^[a-zA-Z_-]+:.*?\#\# .*$$" "./Makefile" | Foreach-Object { $$_data = $$_.matches -split ":.*?\#\# "; $$obj = New-Object PSCustomObject; Add-Member -InputObject $$obj -NotePropertyName ('Command') -NotePropertyValue $$_data[0]; Add-Member -InputObject $$obj -NotePropertyName ('Description') -NotePropertyValue $$_data[1]; $$obj } | Format-Table -HideTableHeaders @{Expression={ $$e = [char]27; "$$e[36m$$($$_.Command)$${e}[0m" }}, Description
else
	HELP_CMD = grep -h -E '^[a-zA-Z_-]+:.*?\#\# .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?\#\# "}; {printf "\033[36m%-30s\033[0m %s\n", $$1, $$2}'
endif

.DEFAULT_GOAL := help

%:
	@:

.PHONY: help
help: ## Show this help
	@${HELP_CMD}


.PHONY: development-checks
development-checks: go-mod-tidy go-mockery go-lint go-staticcheck go-fieldalignment go-deadcode go-govulncheck ## Download dependencies, install tools, generate codes, linter, code check (use it in code development cycle)


.PHONY: test
test: development-checks ## Run unit tests
	@find . -name "go.mod" -exec echo "==> test {}" \; -execdir sh -c 'go test -gcflags=all=-l -count=1 -p=8 -parallel=8 -race -coverprofile=coverage.out ./... -json | tee report.json' \;


