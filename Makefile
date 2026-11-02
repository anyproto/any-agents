.PHONY: build test

build:
	go build -o bin/any-agents .

test:
	go test ./...
