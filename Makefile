.DEFAULT_GOAL := lint
.PHONY: lint fmt test

lint: ## gofmt, go vet and the tests, as CI runs them
	@test -z "$$(gofmt -l .)" || (gofmt -l . && exit 1)
	go vet ./...
	go test ./...

test: ## The tests
	go test ./...

fmt: ## Format
	gofmt -w .
