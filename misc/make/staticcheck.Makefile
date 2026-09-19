STATICCHECK_BIN := $(shell command -v staticcheck 2> /dev/null)

.PHONY: check-staticcheck
check-staticcheck:
ifndef STATICCHECK_BIN
	@echo "Tool 'staticcheck' not available. Installing..."
	@go install honnef.co/go/tools/cmd/staticcheck@latest
	@echo "Installation 'staticcheck' finished ."
endif

.PHONY: staticcheck
staticcheck: check-staticcheck ## Run staticcheck
	@staticcheck $(filter-out $@,$(MAKECMDGOALS))

.PHONY: go-staticcheck
go-staticcheck: check-staticcheck ## Run staticcheck per modules
	@find . -name "go.mod" -exec echo "==> staticcheck {}" \;  -execdir staticcheck ./... \;