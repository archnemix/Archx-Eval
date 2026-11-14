# Convenience targets. Everything here runs with the Go toolchain only.
.PHONY: all build test vet fmt run smoke clean lint image

BIN := bin/relayd

all: vet test build

build:
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o $(BIN) ./cmd/relayd

test:
	go test ./... -count=1 -race

vet:
	go vet ./...

fmt:
	gofmt -l -w .

run: build
	ALLOW_LOOPBACK=1 RELAY_SALT=dev go run ./cmd/relayd

smoke: build
	./scripts/smoke.sh

image:
	docker build -t archx-eval-relay:dev .

clean:
	rm -rf bin
