VERSION ?= $(shell tr -d '\r\n' < VERSION)
DIST ?= dist
COMMIT ?= $(shell git rev-parse HEAD 2>/dev/null || printf dev)
BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
PYTHON ?= $(shell if command -v python3 >/dev/null 2>&1; then printf python3; elif command -v python >/dev/null 2>&1; then printf python; else printf python3; fi)
VERSION_PKG := github.com/sean2077/pairroom/internal/version
LDFLAGS := -s -w -X '$(VERSION_PKG).Commit=$(COMMIT)' -X '$(VERSION_PKG).BuildDate=$(BUILD_DATE)'
GOEXE ?= $(shell go env GOEXE)
GOBIN ?= $(shell go env GOBIN)
ifeq ($(strip $(GOBIN)),)
GOBIN := $(shell go env GOPATH)/bin
endif

.PHONY: build install test race vet fmt check agent-contract release-contract cover run demo smoke release package clean

build:
	mkdir -p $(DIST)
	go build -buildvcs=false -trimpath -ldflags="$(LDFLAGS)" -o $(DIST)/pairroom ./cmd/pairroom

install:
	GOBIN="$(GOBIN)" go install -buildvcs=false -trimpath -ldflags="$(LDFLAGS)" ./cmd/pairroom
	@printf 'Installed PairRoom %s to %s\n' "$(VERSION)" "$(GOBIN)/pairroom$(GOEXE)"
	@if command -v pairroom >/dev/null 2>&1; then \
		printf 'PATH resolves pairroom to %s\n' "$$(command -v pairroom)"; \
	else \
		printf 'Note: %s is not currently discoverable as pairroom on PATH.\n' "$(GOBIN)"; \
	fi

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

check: test race vet agent-contract release-contract
	@test -z "$$(gofmt -l $$(find . -name '*.go' -type f -not -path './.git/*'))" || { echo 'Go files are not gofmt-clean'; gofmt -l $$(find . -name '*.go' -type f -not -path './.git/*'); exit 1; }
	@if command -v node >/dev/null 2>&1; then node --check internal/server/assets/app.js && node --check internal/server/assets/richtext.js; fi
	@go list -m all | grep -qx 'github.com/sean2077/pairroom'
	@git diff --check

agent-contract:
	"$(PYTHON)" .agents/tools/generate-subagents.py --check
	@test -L CLAUDE.md && test "$$(readlink CLAUDE.md)" = AGENTS.md

release-contract:
	"$(PYTHON)" scripts/test_extract_changelog.py
	@notes="$$(mktemp)"; trap 'rm -f "$$notes"' 0 1 2 3 15; \
		"$(PYTHON)" scripts/extract-changelog.py --changelog CHANGELOG.md --tag "v$(VERSION)" --output "$$notes"; \
		test -s "$$notes"

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
	@test "$(DIST)" = dist || { echo 'clean only accepts the default DIST=dist' >&2; exit 1; }
	rm -rf -- "$(CURDIR)/dist" "$(CURDIR)/.coverage"
