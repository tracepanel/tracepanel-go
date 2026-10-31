.DEFAULT_GOAL := lint
.PHONY: lint fmt test e2e integrity

lint: ## gofmt, go vet and the tests, as CI runs them
	@test -z "$$(gofmt -l .)" || (gofmt -l . && exit 1)
	go vet ./...
	go test -race ./...

test: ## The tests
	go test ./...

e2e: ## An app against the real agent: TRACEPANEL_AGENT_BIN, a sibling build of the agent, or the one the SDK downloads
	cd e2e && go vet ./... && go test -race -count=1 ./...

integrity: ## The checksums of AgentVersion's binaries, once it is on npm (AGENT=0.9.4: of another version)
	go run ./internal/integrity $(if $(AGENT),-version $(AGENT))

fmt: ## Format
	gofmt -w .
