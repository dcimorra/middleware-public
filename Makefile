# The four things CI runs, so they can be run the same way at a desk.
# `release-local` is what .github/workflows/release.yml does, on one
# machine, for when a release has to be cut without GitHub.

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  = -s -w -X main.Version=$(VERSION)

.PHONY: build test lint release-local clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o middleware ./cmd/middleware

test:
	go test -race ./...

lint:
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then echo "gofmt:"; echo "$$out"; exit 1; fi
	go vet ./...
	@test "$$(go list -m all | wc -l)" = "1" || { echo "third-party dependency found in go.mod"; exit 1; }

release-local: lint test
	rm -rf dist && mkdir -p dist
	for arch in amd64 arm64; do \
	  CGO_ENABLED=0 GOOS=linux GOARCH=$$arch go build -trimpath -ldflags "$(LDFLAGS)" -o dist/middleware-linux-$$arch ./cmd/middleware && \
	  (cd dist && sha256sum middleware-linux-$$arch > middleware-linux-$$arch.sha256); \
	done
	ls -l dist

clean:
	rm -rf middleware dist
