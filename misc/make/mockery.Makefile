MOCKERY_BIN := $(shell command -v mockery 2> /dev/null)

.PHONY: check-mockery
check-mockery:
ifndef MOCKERY_BIN
	@echo "Tool 'mockery' not available. Installing..."
	@go install github.com/vektra/mockery/v3@v3.8.0
	@echo "Installation 'mockery' finished ."
endif

.PHONY: go-mockery
go-mockery: check-mockery ## Run mockery
	@find . -name ".mockery.yml" -exec echo "==> mockery {}" \; -execdir mockery \;