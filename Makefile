VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
RELEASE ?= $(shell git log -1 --format=%ct 2>/dev/null || echo 0)
COMMIT ?= $(shell git rev-parse --short=12 HEAD 2>/dev/null || echo unknown)

.PHONY: build corrald web web-deps test check notices clean codex-check codex-bench codex-contract codex-browser

build: web
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o corral ./cmd/corral

corrald:
	mkdir -p dist/corrald/linux-amd64 dist/corrald/linux-arm64
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w -X main.version=$(VERSION) -X main.release=$(RELEASE) -X main.commit=$(COMMIT)" -o dist/corrald/linux-amd64/corrald ./cmd/corrald
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "-s -w -X main.version=$(VERSION) -X main.release=$(RELEASE) -X main.commit=$(COMMIT)" -o dist/corrald/linux-arm64/corrald ./cmd/corrald

web-deps:
	cd web && pnpm install --frozen-lockfile

web: web-deps
	cd web && pnpm run build

test:
	go vet ./...
	go test ./...

check: test web-deps
	cd web && pnpm run typecheck
	cd web && pnpm run test
	python3 scripts/gen-notices.py --check
	cd web && node scripts/check-styles.mjs

codex-check:
	python3 -B -m unittest discover -s scripts -p 'test_codex_*.py'
	python3 -B scripts/codex_checks.py

codex-bench:
	go test ./internal/convo -run '^$$' -bench '^BenchmarkCodexFeed$$' -benchmem -count=5

codex-contract:
	python3 -B scripts/codex_contract.py --output "$${CORRAL_CONTRACT_OUTPUT:?set CORRAL_CONTRACT_OUTPUT to a new output directory}"

codex-browser:
	python3 -B scripts/codex_browser.py --output "$${CORRAL_BROWSER_OUTPUT:?set CORRAL_BROWSER_OUTPUT to a new output directory}"

# Regenerate the third-party notices after changing dependencies.
notices: web-deps
	python3 scripts/gen-notices.py

clean:
	rm -f corral
	find web/dist -mindepth 1 ! -name .gitkeep -delete
