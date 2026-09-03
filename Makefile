VERSION ?= $(shell tr -d '\r\n' < VERSION)
DIST ?= dist
COMMIT ?= $(shell git rev-parse HEAD 2>/dev/null || printf dev)
LAST_TAG ?= $(shell git describe --tags --abbrev=0 2>/dev/null || printf unknown)
COMMITS_SINCE_TAG ?= $(shell git rev-list "$(LAST_TAG)..HEAD" --count 2>/dev/null || printf unknown)
BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
PYTHON ?= $(shell if command -v python3 >/dev/null 2>&1; then printf python3; elif command -v python >/dev/null 2>&1; then printf python; else printf python3; fi)
DESKTOP_DIR ?= desktop
DESKTOP_WAILS ?= wails3
ifeq ($(OS),Windows_NT)
DESKTOP_PYTHON ?= python
else
DESKTOP_PYTHON ?= $(PYTHON)
endif
VERSION_PKG := github.com/sean2077/pairroom/internal/version
LDFLAGS := -s -w -X '$(VERSION_PKG).Commit=$(COMMIT)' -X '$(VERSION_PKG).BuildDate=$(BUILD_DATE)' -X '$(VERSION_PKG).LastTag=$(LAST_TAG)' -X '$(VERSION_PKG).CommitsSinceTag=$(COMMITS_SINCE_TAG)'
# Tracked sources in this worktree only. `find .` would also scan sibling
# `.worktrees/*` checkouts and fail `make check` on unrelated dirty files.
GO_FILES := $(shell git ls-files -- '*.go')
GOEXE ?= $(shell go env GOEXE)
GOBIN ?= $(shell go env GOBIN)
ifeq ($(strip $(GOBIN)),)
GOBIN := $(shell go env GOPATH)/bin
endif

.PHONY: build install test race vet fmt check agent-contract release-contract cover run demo smoke release package desktop-build desktop-package clean docs-check

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
	@test "$$(go env CGO_ENABLED)" = 1 || { printf '%s\n' 'make race requires CGO_ENABLED=1 and a Go-supported C compiler on PATH; see docs/DEVELOPMENT.md' >&2; exit 1; }
	go test -race -count=1 ./...

vet:
	go vet ./...

fmt:
	gofmt -w $(GO_FILES)

cover:
	go test -count=1 -coverprofile=.coverage ./...
	go tool cover -func=.coverage

check: test race vet agent-contract release-contract docs-check
	@test -z "$$(gofmt -l $(GO_FILES))" || { echo 'Go files are not gofmt-clean'; gofmt -l $(GO_FILES); exit 1; }
	@if command -v node >/dev/null 2>&1; then \
		node --check internal/server/assets/app.js && \
		node --check internal/server/assets/room-shell.js && \
		node --check internal/server/assets/richtext.js && \
		node --check internal/server/assets/ux.js && \
		node --check internal/service/assets/management.js && \
		node --check internal/service/assets/management-ux.js && \
		node --check scripts/test_room_shell.js && \
		node scripts/test_room_shell.js; \
	fi
	@go list -m all | grep -qx 'github.com/sean2077/pairroom'
	@git diff --check

agent-contract:
	"$(PYTHON)" .agents/tools/generate-subagents.py --check
	@test -L CLAUDE.md && test "$$(readlink CLAUDE.md)" = AGENTS.md

release-contract:
	"$(PYTHON)" scripts/test_extract_changelog.py
	bash scripts/test_install.sh
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
	VERSION=$(VERSION) COMMIT=$(COMMIT) BUILD_DATE=$(BUILD_DATE) LAST_TAG=$(LAST_TAG) COMMITS_SINCE_TAG=$(COMMITS_SINCE_TAG) DIST=$(DIST) bash scripts/release.sh

package: release

desktop-build:
	cd "$(DESKTOP_DIR)" && "$(DESKTOP_PYTHON)" scripts/prepare-build.py && "$(DESKTOP_WAILS)" task build

desktop-package:
	cd "$(DESKTOP_DIR)" && "$(DESKTOP_PYTHON)" scripts/prepare-build.py && "$(DESKTOP_WAILS)" task package

clean:
	@test "$(DIST)" = dist || { echo 'clean only accepts the default DIST=dist' >&2; exit 1; }
	rm -rf -- "$(CURDIR)/dist" "$(CURDIR)/.coverage"

docs-check:
	python3 scripts/docs-check.py
