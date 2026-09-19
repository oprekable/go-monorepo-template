GOLANGCI_LINT_BIN := $(shell command -v golangci-lint 2> /dev/null)

.PHONY: check-golangci-lint
check-golangci-lint:
ifndef GOLANGCI_LINT_BIN
	@echo "Tool 'golangci-lint' not available. Installing..."
	@go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest
	@echo "Installation 'golangci-lint' finished ."
endif

.PHONY: golangci-lint
golangci-lint: check-golangci-lint ## Run golangci-lint
	@golangci-lint $(filter-out $@,$(MAKECMDGOALS))

.PHONY: go-lint
go-lint: check-golangci-lint ## Run golangci-lint (linter and run) per modules
	@golangci-lint linters
	@golangci-lint cache clean
	@find . -name "go.mod" -execdir golangci-lint run -c $(CURDIR)/.golangci.yml ./... --fix \;