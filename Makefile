GO ?= go

.PHONY: build vet test test-race lint check integration tidy

build:
	$(GO) build ./...

vet:
	$(GO) vet ./...

# Unit tests only. Tests needing live teratestnet are behind the `integration` tag.
test:
	$(GO) test ./...

test-race:
	$(GO) test -race ./...

lint:
	golangci-lint run

# The full gate CI enforces.
check: tidy build vet test-race lint

# Requires a funded teratestnet wallet; see docs.
integration:
	$(GO) test -tags=integration -count=1 -v ./...

tidy:
	$(GO) mod tidy
