INFISICAL_BIN := $(shell command -v infisical 2> /dev/null)

.PHONY: check-infisical
check-infisical:
ifndef INFISICAL_BIN
	@echo "Tool 'infisical' not available. Installing..."
	@npm install -g @infisical/cli
	@echo "Installation 'infisical' finished ."
endif