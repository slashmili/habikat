.PHONY: build image test fuzz test-cli fuzz-cli test-vm fuzz-vm fuzz-import fuzz-identity fuzz-fetch integration test-image property e2e

build:
	go build -o bin/hk ./cmd/hk

image:
	python3 images/build.py

test:
	go test -race ./...

fuzz: fuzz-cli fuzz-vm fuzz-import fuzz-identity fuzz-fetch

test-cli:
	go test -race ./cmd/hk

fuzz-cli:
	go test ./cmd/hk -run '^$$' -fuzz '^FuzzCreateArgs$$' -fuzztime=10s -parallel=2
	go test ./cmd/hk -run '^$$' -fuzz '^FuzzNameArg$$' -fuzztime=10s -parallel=2
	go test ./cmd/hk -run '^$$' -fuzz '^FuzzDeleteApproval$$' -fuzztime=10s -parallel=2
	go test ./cmd/hk -run '^$$' -fuzz '^FuzzFetchArgs$$' -fuzztime=10s -parallel=2
	go test ./cmd/hk -run '^$$' -fuzz '^FuzzFetchApproval$$' -fuzztime=10s -parallel=2

test-vm:
	go test -race ./internal/vm

fuzz-vm: property
	go test ./internal/vm -run '^$$' -fuzz '^FuzzReadRecord$$' -fuzztime=10s -parallel=2
	go test ./internal/vm -run '^$$' -fuzz '^FuzzImageIntegrity$$' -fuzztime=10s -parallel=2
	go test ./internal/vm -run '^$$' -fuzz '^FuzzProxyReply$$' -fuzztime=10s -parallel=2

property:
	go test ./internal/vm -run '^$$' -fuzz '^FuzzNameConfinement$$' -fuzztime=10s -parallel=2

integration:
	go test -race -tags=integration ./internal/repository ./internal/vm -v -count=1

test-image:
	python3 -m unittest discover -s images -p 'test_*.py' -v

fuzz-import:
	go test ./internal/vm -run '^$$' -fuzz '^FuzzImportBoundary$$' -fuzztime=10s -parallel=2

fuzz-identity:
	go test ./internal/repository -run '^$$' -fuzz '^FuzzGitIdentityValidation$$' -fuzztime=10s -parallel=2
	go test ./internal/vm -run '^$$' -fuzz '^FuzzGuestGitIdentity$$' -fuzztime=10s -parallel=2

fuzz-fetch:
	go test ./internal/repository -run '^$$' -fuzz '^FuzzFetchManifest$$' -fuzztime=10s -parallel=2
	go test ./internal/repository -run '^$$' -fuzz '^FuzzFetchNamespace$$' -fuzztime=10s -parallel=2
	go test ./internal/vm -run '^$$' -fuzz '^FuzzFetchStatus$$' -fuzztime=10s -parallel=2
	go test ./internal/vm -run '^$$' -fuzz '^FuzzFetchTransferBound$$' -fuzztime=10s -parallel=2

e2e:
	go test -race -tags=e2e ./tests/e2e -v -count=1 -timeout=10m
