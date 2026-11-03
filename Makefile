.PHONY: build test dist

VERSION ?= $(shell git describe --tags --always --dirty)
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64

build:
	go build -o bin/any-agents .

test:
	go test ./...

# dist cross-compiles one archive per platform plus checksums.txt.
dist:
	rm -rf dist && mkdir -p dist
	for p in $(PLATFORMS); do \
	  os=$${p%/*}; arch=$${p#*/}; \
	  CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags='-s -w' -o dist/any-agents . && \
	  tar -C dist -czf dist/any-agents_$(VERSION)_$${os}_$${arch}.tar.gz any-agents && rm dist/any-agents || exit 1; \
	done
	cd dist && sha256sum *.tar.gz > checksums.txt
