VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: build web web-deps test check notices clean codex-check codex-bench codex-contract codex-browser

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
