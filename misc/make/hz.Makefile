HZ_BIN := $(shell command -v hz 2> /dev/null)

.PHONY: check-hz
check-hz:
ifndef HZ_BIN
	@echo "Tool 'hz' not available. Installing..."
	@go install github.com/cloudwego/hertz/cmd/hz@latest
	@echo "Installation 'hz' finished ."
endif

.PHONY: hz
hz: check-hz ## Run hz
	@hz $(filter-out $@,$(MAKECMDGOALS))