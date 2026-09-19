BUF_BIN := $(shell command -v buf 2> /dev/null)

.PHONY: check-buf
check-buf:
ifndef BUF_BIN
	@echo "Tool 'buf' not available. Installing..."
	@go install github.com/bufbuild/buf/cmd/buf@latest
	@echo "Installation 'buf' finished ."
endif

.PHONY: buf
buf: check-buf ## Run buf
	@buf $(filter-out $@,$(MAKECMDGOALS))