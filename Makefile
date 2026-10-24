.PHONY: build build-init test vet fmtcheck lint verify

build:
	go build -o ./bin/paddock ./cmd/paddock

build-init:
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o ./bin/paddock-init ./cmd/paddock

test:
	go test -race ./...

vet:
	go vet ./...

fmtcheck:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

lint: vet fmtcheck

verify: fmtcheck vet test build build-init
