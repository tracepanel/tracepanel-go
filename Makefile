.DEFAULT_GOAL := lint
.PHONY: lint fmt test tidy

# The core module, then each integration's: a module of its own, tagged <dir>/vX.Y.Z at a release.
MODULES := . pgxtrace gintrace echotrace echo5trace redistrace zaptrace zerologtrace grpctrace kafkatrace

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

fmt: ## Format
	gofmt -w .
