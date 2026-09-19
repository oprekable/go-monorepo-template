FIELDALIGNMENT_BIN := $(shell command -v fieldalignment 2> /dev/null)

.PHONY: check-fieldalignment
check-fieldalignment:
ifndef FIELDALIGNMENT_BIN
	@echo "Tool 'fieldalignment' not available. Installing..."
	@go install golang.org/x/tools/go/analysis/passes/fieldalignment/cmd/fieldalignment@latest
	@echo "Installation 'fieldalignment' finished ."
endif

.PHONY: fieldalignment
fieldalignment: check-fieldalignment ## Run fieldalignment
	@fieldalignment $(filter-out $@,$(MAKECMDGOALS))

.PHONY: go-fieldalignment
go-fieldalignment: check-fieldalignment ## Run fieldalignment per modules
	@find . -name "go.mod" -exec echo "==> fieldalignment {}" \;  -execdir fieldalignment -fix ./... \;