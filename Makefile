GO_LDFLAGS ?= -X main.version=dev

.PHONY: build build-init check test container-test vet fmtcheck lint verify

build:
	go build -ldflags '$(GO_LDFLAGS)' -o ./bin/paddock ./cmd/paddock

build-init:
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -ldflags '$(GO_LDFLAGS)' -o ./bin/paddock-init ./cmd/paddock

# check runs the cli-contract black-box checker against the built binary: help,
# unknown flags, unknown commands, and missing positionals, as a consumer drives
# them. It reads the surface `paddock schema` prints, so the declaration and the
# behavior the checker asserts cannot drift apart.
check: build
	go run github.com/sdougbrown/cli-contract/cmd/cli-contract check ./bin/paddock

test:
	go test -race ./...

# Starts Apple container VMs and builds images; needs Apple silicon with the
# container service running. On a Linux host with rootful podman it also runs
# the podman engine's real-container tests; set PADDOCK_PODMAN_SUDO=1 there
# when podman is reached through sudo.
container-test:
	PADDOCK_CONTAINER_TESTS=1 PADDOCK_BUILD_TESTS=1 go test -race -count=1 ./internal/engine/applecontainer
	@if command -v podman >/dev/null 2>&1; then \
		PADDOCK_PODMAN_TESTS=1 PADDOCK_BUILD_TESTS=1 go test -race -count=1 ./internal/engine/podman; \
	else echo "podman not found; skipping the podman container tests"; fi

vet:
	go vet ./...

fmtcheck:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

lint: vet fmtcheck

verify: fmtcheck vet test build build-init check
