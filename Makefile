BIN      ?= portwake
IMAGE    ?= ghcr.io/byebyebruce/portwake
PKG      := ./cmd/portwake
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  := -s -w -X main.version=$(VERSION)
ARGS     ?=

.DEFAULT_GOAL := build
.PHONY: build run tui test race integration cover vet fmt check tidy dist docker clean help

build: ## Build the binary
	go build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN) $(PKG)

run: build ## Run with logs (extra flags via ARGS="--web 127.0.0.1:9001")
	./$(BIN) $(ARGS)

tui: build ## Run with the terminal dashboard
	./$(BIN) --tui $(ARGS)

test: ## Run unit tests (fake compose runner)
	go test ./...

race: ## Run tests with the race detector
	go test -race -count=1 ./...

integration: ## Run the integration test against real docker
	go test -tags integration -count=1 -run Real -v ./internal/app/

cover: ## Test coverage report (coverage.html)
	go test -coverpkg=./internal/... -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out -o coverage.html
	go tool cover -func=coverage.out | tail -1

vet: ## go vet (incl. integration build tag)
	go vet ./...
	go vet -tags integration ./...

fmt: ## Format code
	gofmt -w .

check: ## Fail if code is unformatted, then vet and race-test
	@test -z "$$(gofmt -l .)" || (echo "unformatted files:"; gofmt -l .; exit 1)
	$(MAKE) vet race

tidy: ## go mod tidy
	go mod tidy

dist: ## Cross-compile into dist/ (darwin/linux, amd64/arm64)
	@mkdir -p dist
	@for os in darwin linux; do for arch in amd64 arm64; do \
		echo "dist/$(BIN)-$(VERSION)-$$os-$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags '$(LDFLAGS)' \
			-o dist/$(BIN)-$(VERSION)-$$os-$$arch $(PKG) || exit 1; \
	done; done

docker: ## Build the Docker image ($(IMAGE):$(VERSION))
	docker build -t $(IMAGE):$(VERSION) .

clean: ## Remove build artifacts
	rm -rf $(BIN) dist coverage.out coverage.html

help: ## List targets
	@grep -E '^[a-z-]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-12s %s\n", $$1, $$2}'
