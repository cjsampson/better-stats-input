.PHONY: help
help: ## Describe useful make targets
	@grep -E '^[a-zA-Z0-9_/-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ": .*?## "}; {printf "%-50s %s\n", $$1, $$2}'

.PHONY: run-input
run-input: ## run the websocket input client
	go run *.go
