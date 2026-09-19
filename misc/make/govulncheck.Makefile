GOVULNCHECK_BIN := $(shell command -v govulncheck 2> /dev/null)

.PHONY: check-govulncheck
check-govulncheck:
ifndef GOVULNCHECK_BIN
	@echo "Tool 'govulncheck' not available. Installing..."
	@go install golang.org/x/vuln/cmd/govulncheck@latest
	@echo "Installation 'govulncheck' finished ."
endif

.PHONY: govulncheck
govulncheck: check-govulncheck ## Run govulncheck
	@govulncheck $(filter-out $@,$(MAKECMDGOALS))

.PHONY: go-govulncheck
go-govulncheck: check-govulncheck ## Run govulncheck per modules
	@find . -name "go.mod" -exec echo "==> govulncheck {}" \; -execdir govulncheck -show verbose ./... \;