
GARBLE_BIN := $(shell command -v garble 2> /dev/null)

.PHONY: check-garble
check-garble:
ifndef GARBLE_BIN
	@echo "Tool 'garble' not available. Installing..."
	@go install mvdan.cc/garble@latest
	@echo "Installation 'garble' finished ."
endif