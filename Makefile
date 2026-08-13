VERSION ?= 0.3.0
DIST ?= dist

.PHONY: build test race vet fmt check run demo release package clean

build:
	mkdir -p $(DIST)
	go build -buildvcs=false -trimpath -o $(DIST)/pairroom ./cmd/pairroom

test:
	go test ./...

race:
	go test -race ./...

vet:
	go vet ./...

fmt:
	gofmt -w $$(find . -name '*.go' -type f)

check: test race vet
	@test -z "$$(gofmt -l $$(find . -name '*.go' -type f))" || { echo 'Go files are not gofmt-clean'; gofmt -l $$(find . -name '*.go' -type f); exit 1; }
	@if command -v node >/dev/null 2>&1; then node --check internal/server/assets/app.js && node --check internal/server/assets/richtext.js; fi
	@git diff --check

run:
	go run ./cmd/pairroom serve --repo .

demo:
	go run ./cmd/pairroom serve --repo . --mock --no-browser

release:
	mkdir -p $(DIST)
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -buildvcs=false -trimpath -ldflags='-s -w' -o $(DIST)/pairroom-linux-amd64 ./cmd/pairroom
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -buildvcs=false -trimpath -ldflags='-s -w' -o $(DIST)/pairroom-windows-amd64.exe ./cmd/pairroom
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -buildvcs=false -trimpath -ldflags='-s -w' -o $(DIST)/pairroom-darwin-arm64 ./cmd/pairroom
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -buildvcs=false -trimpath -ldflags='-s -w' -o $(DIST)/pairroom-darwin-amd64 ./cmd/pairroom
	cd $(DIST) && sha256sum pairroom-* > SHA256SUMS

package: release
	mkdir -p $(DIST)
	tar -czf $(DIST)/pairroom-v$(VERSION)-source.tar.gz --exclude='./dist' --exclude='./.git' .

clean:
	rm -rf $(DIST) .coverage
