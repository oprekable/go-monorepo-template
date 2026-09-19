THRIFTGO_BIN := $(shell command -v thriftgo 2> /dev/null)

.PHONY: check-thriftgo
check-thriftgo:
ifndef THRIFTGO_BIN
	@echo "Tool 'thriftgo' not available. Installing..."
	@go install github.com/cloudwego/thriftgo@latest
	@echo "Installation 'thriftgo' finished ."
endif

.PHONY: thriftgo
thriftgo: check-thriftgo ## Run thriftgo
	@thriftgo $(filter-out $@,$(MAKECMDGOALS))