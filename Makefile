VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: build web test check clean

build: web
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o corral ./cmd/corral

web:
	cd web && pnpm install --frozen-lockfile && pnpm run build

test:
	go vet ./...
	go test ./...

check: test
	cd web && pnpm run typecheck

clean:
	rm -f corral
	find web/dist -mindepth 1 ! -name .gitkeep -delete
