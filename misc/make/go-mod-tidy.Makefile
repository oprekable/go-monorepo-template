.PHONY: go-mod-tidy
go-mod-tidy: ## Run go mod tidy
	@go work sync
	@find . -name "go.mod" -exec echo "==> Tidying {}" \; -execdir go mod tidy \;
