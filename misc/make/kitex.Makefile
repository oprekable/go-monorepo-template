KITEX_BIN := $(shell command -v kitex 2> /dev/null)

.PHONY: check-kitex
check-kitex:
ifndef KITEX_BIN
	@echo "Tool 'kitex' not available. Installing..."
	@go install github.com/cloudwego/kitex/tool/cmd/kitex@latest
	@echo "Installation 'kitex' finished ."
endif

.PHONY: kitex
kitex: check-kitex ## Run kitex
	@kitex $(filter-out $@,$(MAKECMDGOALS))