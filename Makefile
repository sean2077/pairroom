VERSION ?= $(shell tr -d '\r\n' < VERSION)
DIST ?= dist
COMMIT ?= $(shell git rev-parse HEAD 2>/dev/null || printf dev)
BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
VERSION_PKG := github.com/sean2077/pairroom/internal/version
LDFLAGS := -s -w -X '$(VERSION_PKG).Commit=$(COMMIT)' -X '$(VERSION_PKG).BuildDate=$(BUILD_DATE)'

.PHONY: build test race vet fmt check cover run demo smoke release package clean

build:
	mkdir -p $(DIST)
	go build -buildvcs=false -trimpath -ldflags="$(LDFLAGS)" -o $(DIST)/pairroom ./cmd/pairroom

test:
	go test -count=1 ./...

race:
	go test -race -count=1 ./...

vet:
	go vet ./...

fmt:
	gofmt -w $$(find . -name '*.go' -type f -not -path './.git/*')

cover:
	go test -count=1 -coverprofile=.coverage ./...
	go tool cover -func=.coverage

check: test race vet
	@test -z "$$(gofmt -l $$(find . -name '*.go' -type f -not -path './.git/*'))" || { echo 'Go files are not gofmt-clean'; gofmt -l $$(find . -name '*.go' -type f -not -path './.git/*'); exit 1; }
	@if command -v node >/dev/null 2>&1; then node --check internal/server/assets/app.js && node --check internal/server/assets/richtext.js; fi
	@go list -m all | grep -qx 'github.com/sean2077/pairroom'
	@git diff --check

run:
	go run ./cmd/pairroom serve --repo .

demo:
	go run ./cmd/pairroom serve --repo . --mock --no-browser

smoke:
	bash scripts/mock-e2e.sh

release:
	VERSION=$(VERSION) COMMIT=$(COMMIT) BUILD_DATE=$(BUILD_DATE) DIST=$(DIST) bash scripts/release.sh

package: release

clean:
	rm -rf $(DIST) .coverage
