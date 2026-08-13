.PHONY: build test race vet fmt check run demo release package clean

build:
	mkdir -p dist
	go build -trimpath -o dist/pairroom ./cmd/pairroom

test:
	go test ./...

race:
	go test -race ./...

vet:
	go vet ./...

fmt:
	gofmt -w $$(find . -name '*.go' -type f)

check: test race vet
	@if command -v node >/dev/null 2>&1; then node --check internal/server/assets/app.js; fi

run:
	go run ./cmd/pairroom serve --repo .

demo:
	go run ./cmd/pairroom serve --repo . --mock --no-browser

release:
	mkdir -p dist
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o dist/pairroom-linux-amd64 ./cmd/pairroom
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o dist/pairroom-windows-amd64.exe ./cmd/pairroom
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -trimpath -ldflags='-s -w' -o dist/pairroom-darwin-arm64 ./cmd/pairroom
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o dist/pairroom-darwin-amd64 ./cmd/pairroom
	cd dist && sha256sum pairroom-* > SHA256SUMS

package:
	mkdir -p dist
	tar -czf dist/pairroom-source.tar.gz --exclude='./dist' --exclude='./.git' .

clean:
	rm -rf dist .coverage
