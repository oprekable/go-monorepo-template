DEADCODE_BIN := $(shell command -v deadcode 2> /dev/null)

.PHONY: check-deadcode
check-deadcode:
ifndef DEADCODE_BIN
	@echo "Tool 'deadcode' not available. Installing..."
	@go install golang.org/x/tools/cmd/deadcode@latest
	@echo "Installation 'deadcode' finished ."
endif

.PHONY: deadcode
deadcode: check-deadcode ## Run deadcode
	@deadcode $(filter-out $@,$(MAKECMDGOALS))

.PHONY: go-deadcode
go-deadcode: check-deadcode ## Run deadcode per modules
	@find . -name "go.mod" -exec echo "==> deadcode test {}" \;  -execdir deadcode -test ./... \;
	@find . -name "go.mod" -exec echo "==> deadcode {}" \;  -execdir deadcode ./... \;