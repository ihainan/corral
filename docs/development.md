# Development

This document covers setting up a local development environment, building binaries and container images, and running Corral's test and validation suites.

See [CONTRIBUTING.md](../CONTRIBUTING.md) before opening a pull request, and [CHANGELOG.md](../CHANGELOG.md) for version history.

## Prerequisites

- Go (version specified in [`go.mod`](../go.mod))
- Node.js 22 with `corepack` enabled (`pnpm` version pinned in [`web/package.json`](../web/package.json))
- Python 3 (for third-party notice verification and Codex validation scripts)
- Docker (for the local test SSH server)

## Running locally

Start the test SSH server, build the web interface once, and run the gateway:

```sh
docker build -t corral-test-sshd test/sshd && docker run -d -p 127.0.0.1:2222:22 corral-test-sshd
make web
export CORRAL_PUBLIC_URL=http://127.0.0.1:8080 \
  CORRAL_ALLOWED_NETWORKS=127.0.0.1/32 \
  CORRAL_MASTER_KEY="$(go run ./cmd/corral genkey)"
go run ./cmd/corral serve
```

The startup log prints the one-time setup link to create the first administrator account. Add the test SSH server as host `127.0.0.1`, port `2222`, user `alice`, password `alice-pass`.

For frontend iteration without rebuilding the Go binary, point `CORRAL_STATIC_DIR` to the built web assets directory (for example `web/dist`).

To test the optional `corrald` host helper daemon locally, build its Linux binaries and point `CORRAL_CORRALD_DIR` to `dist/corrald`:

```sh
make corrald
```

## Building

Build the standalone `corral` binary (which builds and embeds the web UI):

```sh
make build
```

Build the `corrald` host helper binaries (`dist/corrald/linux-amd64/corrald` and `dist/corrald/linux-arm64/corrald`):

```sh
make corrald
```

Build the container image (the `Dockerfile` requires a positive integer `RELEASE` build argument):

```sh
docker build \
  --build-arg VERSION="$(git describe --tags --always --dirty)" \
  --build-arg RELEASE="$(git log -1 --format=%ct)" \
  --build-arg COMMIT="$(git rev-parse --short=12 HEAD)" \
  -t corral .
```

## Checks and tests

Run the full verification suite before submitting a change:

```sh
make check    # go vet, Go tests (incl. security suite), web typecheck, web tests, notices, style checks
make test     # go vet and Go tests only
make notices  # regenerate web/public/THIRD-PARTY-NOTICES.txt after changing dependencies
```

For changes affecting Codex chat mode, run the Codex validation targets as well:

```sh
make codex-check  # automated Codex regression checks
make codex-bench  # synthetic Codex log-parsing benchmarks
```

The [Codex chat validation plan](codex-chat-validation.md) describes the isolated real-CLI contract tests (`make codex-contract`), browser component regressions (`make codex-browser`), and performance measurements, and distinguishes them from physical-device and real-provider testing.

## Third-party notices

Third-party licenses are listed in [`web/public/THIRD-PARTY-NOTICES.txt`](../web/public/THIRD-PARTY-NOTICES.txt), which is also served by the web interface. Run `make notices` whenever Go or npm dependencies change and commit the updated file.
