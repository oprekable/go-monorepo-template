WIRE_BIN := $(shell command -v wire 2> /dev/null)

.PHONY: check-wire
check-wire:
ifndef WIRE_BIN
	@echo "Tool 'wire' not available. Installing..."
	@go install github.com/google/wire/cmd/wire@latest
	@echo "Installation 'wire' finished ."
endif

.PHONY: wire
wire: check-wire ## Run wire
	@wire $(filter-out $@,$(MAKECMDGOALS))

.PHONY: go-wire
go-wire: go-mod-tidy check-wire ## Run wire
	@wire $(filter-out $@,$(MAKECMDGOALS))