VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: build web web-deps test check notices clean

build: web
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o corral ./cmd/corral

web-deps:
	cd web && pnpm install --frozen-lockfile

web: web-deps
	cd web && pnpm run build

test:
	go vet ./...
	go test ./...

check: test web-deps
	cd web && pnpm run typecheck
	python3 scripts/gen-notices.py --check

# Regenerate the third-party notices after changing dependencies.
notices: web-deps
	python3 scripts/gen-notices.py

clean:
	rm -f corral
	find web/dist -mindepth 1 ! -name .gitkeep -delete
