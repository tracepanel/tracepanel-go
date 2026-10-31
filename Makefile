.DEFAULT_GOAL := lint
.PHONY: lint fmt test tidy e2e integrity

# The core module, then each integration's: a module of its own, tagged <dir>/vX.Y.Z at a release.
MODULES := . pgxtrace gormtrace gintrace echotrace echo5trace redistrace zaptrace zerologtrace grpctrace kafkatrace

lint: ## gofmt, go vet and the tests of every module, as CI runs them
	@test -z "$$(gofmt -l .)" || (gofmt -l . && exit 1)
	@for module in $(MODULES); do \
		echo "== $$module"; \
		(cd $$module && go vet ./... && go test -race ./...) || exit 1; \
	done

test: ## The tests of every module
	@for module in $(MODULES); do (cd $$module && go test ./...) || exit 1; done

tidy: ## go mod tidy in every module
	@for module in $(MODULES); do (cd $$module && go mod tidy) || exit 1; done

e2e: ## An app against the real agent: TRACEPANEL_AGENT_BIN, a sibling build of the agent, or the one the SDK downloads
	cd e2e && go vet ./... && go test -race -count=1 ./...

integrity: ## The checksums of AgentVersion's binaries, once it is on npm (AGENT=0.9.5: of another version)
	go run ./internal/integrity $(if $(AGENT),-version $(AGENT))

fmt: ## Format
	gofmt -w .
