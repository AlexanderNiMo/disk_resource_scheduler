MODULE  := github.com/alex/disk_resource_scheduler
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE)

.DEFAULT_GOAL := help
.PHONY: help build test fmt vet tidy check clean run

help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

build: ## Build the binary into bin/
	@mkdir -p bin && go build -ldflags '$(LDFLAGS)' -o bin/diskd ./cmd/diskd

test: ## Run the tests
	go test ./... -count=1

fmt: ## Format the code
	gofmt -l -w ./cmd ./internal ./e2e

vet: ## Vet the code
	go vet ./...

tidy: ## Tidy the modules
	go mod tidy

check: fmt vet build test ## Format, vet, build and test

run: build ## Run the daemon with config.yaml
	./bin/diskd --config config.yaml

clean: ## Remove the build artefacts
	rm -rf bin
