
GOCYCLO_BIN := $(shell command -v gocyclo 2> /dev/null)

.PHONY: check-gocyclo
check-gocyclo:
ifndef GOCYCLO_BIN
	@echo "Tool 'gocyclo' not available. Installing..."
	@go install github.com/fzipp/gocyclo/cmd/gocyclo@latest
	@echo "Installation 'gocyclo' finished ."
endif

.PHONY: gocyclo
gocyclo: go-mod-tidy check-gocyclo ## Run govulncheck
	@gocyclo $(filter-out $@,$(MAKECMDGOALS))

.PHONY: go-gocyclo
go-gocyclo: go-mod-tidy check-gocyclo ## Run gocyclo per modules
	@find . -name "go.mod" -exec echo "==> gocyclo {}" \; -execdir gocyclo . \;