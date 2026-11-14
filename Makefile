.PHONY: build image test test-cli fuzz-cli test-vm fuzz-vm property e2e

build:
	go build -o bin/hk ./cmd/hk

image:
	python3 images/build.py

test:
	go test -race ./...

test-cli:
	go test -race ./cmd/hk

fuzz-cli:
	go test ./cmd/hk -run '^$$' -fuzz '^FuzzCreateArgs$$' -fuzztime=10s -parallel=2
	go test ./cmd/hk -run '^$$' -fuzz '^FuzzNameArg$$' -fuzztime=10s -parallel=2

test-vm:
	go test -race ./internal/vm

fuzz-vm: property
	go test ./internal/vm -run '^$$' -fuzz '^FuzzReadRecord$$' -fuzztime=10s -parallel=2
	go test ./internal/vm -run '^$$' -fuzz '^FuzzImageIntegrity$$' -fuzztime=10s -parallel=2
	go test ./internal/vm -run '^$$' -fuzz '^FuzzProxyReply$$' -fuzztime=10s -parallel=2

property:
	go test ./internal/vm -run '^$$' -fuzz '^FuzzNameConfinement$$' -fuzztime=10s -parallel=2

e2e:
	go test -race -tags=e2e ./tests/e2e -v -count=1 -timeout=5m
