# Convenience targets. Everything here runs with the Go toolchain only.
.PHONY: all build test vet fmt run smoke clean lint image

BIN := bin/relayd

all: vet test build

build:
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o $(BIN) ./cmd/relayd

test:
	@test ! -f testdata/bank/usage.json || { echo "testdata/bank is dirty; run 'make clean-fixture'"; exit 1; }
	go test ./... -count=1 -race

clean-fixture:
	rm -f testdata/bank/usage.json

vet:
	go vet ./...

fmt:
	gofmt -l -w .

run: build
	ALLOW_LOOPBACK=1 RELAY_SALT=dev go run ./cmd/relayd

smoke: build
	@rm -f testdata/bank/usage.json
	./scripts/smoke.sh

image:
	docker build -t archx-eval-relay:dev .

clean:
	rm -rf bin
