# Deployment

This document covers deploying Corral with Docker, Docker Compose, or standalone binaries, configuring a reverse proxy, enabling forwarded ports and VS Code, and preparing target hosts.

## Distribution

Corral ships as a single static binary or container image with the web interface embedded and a SQLite database stored on disk.

- Container images (`linux/amd64` and `linux/arm64`): `ghcr.io/ihainan/corral`
- Standalone Linux binaries (`amd64` and `arm64`) with checksums: [GitHub Releases](https://github.com/ihainan/corral/releases)

The container image runs as user `corral` (uid `10001`), sets `CORRAL_LISTEN=0.0.0.0:8080`, `CORRAL_DATA_DIR=/data`, and `CORRAL_CORRALD_DIR=/usr/local/share/corral/corrald`, and bundles the `corrald` host helper binaries for `linux/amd64` and `linux/arm64`.

## Docker

Generate a 32-byte master key once and store it outside your database backups:

```sh
(umask 077; docker run --rm ghcr.io/ihainan/corral genkey > corral.key)
docker run -d --name corral --stop-timeout 30 \
  -p 127.0.0.1:8080:8080 \
  -v corral-data:/data \
  -e CORRAL_PUBLIC_URL=https://ssh.example.com \
  -e CORRAL_MASTER_KEY="$(cat corral.key)" \
  ghcr.io/ihainan/corral
docker logs corral
```

On first start, Corral logs a one-time setup link (`/setup#<token>`, valid until restart). Open that link to create the first administrator account, then add users from **Users** (by invitation link or by creating an account directly).

Pass `--stop-timeout 30` so the container has time to drain requests and record open sessions before stopping.

## Docker Compose

Generate the master key into `.env` alongside `compose.yaml`:

```sh
(umask 077; echo "CORRAL_MASTER_KEY=$(docker run --rm ghcr.io/ihainan/corral genkey)" > .env)
```

Example `compose.yaml`:

```yaml
services:
  corral:
    image: ghcr.io/ihainan/corral:latest
    restart: unless-stopped
    stop_grace_period: 30s  # time to record open sessions so they reopen after restart
    ports: ["127.0.0.1:8080:8080"]
    environment:
      CORRAL_PUBLIC_URL: https://ssh.example.com
      CORRAL_MASTER_KEY: ${CORRAL_MASTER_KEY:?}
      CORRAL_TRUSTED_PROXIES: 172.16.0.0/12  # reverse proxy network as the container sees it
    volumes:
      - corral-data:/data

volumes:
  corral-data:
```

Pin a specific release tag (for example `ghcr.io/ihainan/corral:0.2.0`) if you want explicit upgrades.

If you use a host bind mount (`./data:/data`) instead of a named volume, make the directory writable by uid `10001`:

```sh
mkdir data && sudo chown 10001 data
```

## Building the container image

The `Dockerfile` builds the frontend, `corral`, and both `corrald` architectures on the build host without emulation. It requires a positive integer `RELEASE` build argument (typically the commit timestamp):

```sh
docker build \
  --build-arg VERSION="$(git describe --tags --always --dirty)" \
  --build-arg RELEASE="$(git log -1 --format=%ct)" \
  --build-arg COMMIT="$(git rev-parse --short=12 HEAD)" \
  -t corral .
```

To build for another architecture:

```sh
docker buildx build --platform linux/arm64 \
  --build-arg VERSION="$(git describe --tags --always --dirty)" \
  --build-arg RELEASE="$(git log -1 --format=%ct)" \
  --build-arg COMMIT="$(git rev-parse --short=12 HEAD)" \
  -t corral .
```

## Reverse proxy

Serve Corral over HTTPS behind a reverse proxy. The proxy must:

- Pass WebSocket upgrades (`Upgrade` and `Connection` headers).
- Disable response buffering so terminal output streams immediately.
- Allow long-lived connections (for example `86400s` read and send timeouts).
- Allow request bodies at least as large as `CORRAL_UPLOAD_MAX_MB` (default `100`).
- Forward `Host`, `X-Forwarded-For`, and `X-Forwarded-Proto`.

See [`deploy/nginx.conf.example`](../deploy/nginx.conf.example) for a working nginx configuration.

Set `CORRAL_TRUSTED_PROXIES` to the reverse proxy's IP address or CIDR so sign-in rate limiting and the audit log record real client addresses. When left unset, Corral ignores `X-Forwarded-For`.

## Multiple domains

To serve the same Corral instance on additional domains (for example an internal hostname alongside an external one), list them in `CORRAL_EXTRA_URLS`:

- Every extra URL must use the same scheme (`https://` or `http://`) as `CORRAL_PUBLIC_URL`.
- Non-ASCII hostnames must be written in punycode (`xn--`) form.
- None of the extra URLs may fall under `CORRAL_FORWARD_DOMAIN`.
- Each domain has its own sign-in cookie. Invitation links use the domain they were created from; forwarded ports and VS Code remain on `CORRAL_PUBLIC_URL`'s side.

## Forwarded ports and VS Code

Port forwarding and browser-based VS Code (`code-server`) are served on subdomains of `CORRAL_FORWARD_DOMAIN`:

- Forwarded ports: `<port>-<id>.<domain>`
- VS Code: `code-<id>.<domain>`

To enable them:

1. Point a wildcard DNS record (`*.<domain>`) and a wildcard TLS certificate at your reverse proxy.
2. Route those wildcard hostnames to Corral alongside the main site.
3. Set `CORRAL_FORWARD_DOMAIN` (requires an `https://` `CORRAL_PUBLIC_URL`, except during loopback-only local development).

Every request to a forwarded subdomain is checked against the signed-in user. Because the main site uses `__Host-` cookies over HTTPS, forwarded applications on sibling subdomains cannot read or overwrite the main site's session cookie.

### Offline or air-gapped VS Code installation

By default, when a user opens VS Code on a host, the host downloads `code-server` from GitHub Releases and verifies its SHA-256 digest before unpacking. When the interface language is Chinese, the host also fetches the Chinese language pack from [Open VSX](https://open-vsx.org) and checks its pinned digest.

For hosts without internet access, place the release archives in a directory on the gateway and point `CORRAL_CODE_SERVER_DIR` to it:

- `code-server-<version>-linux-amd64.tar.gz` and/or `code-server-<version>-linux-arm64.tar.gz` (matching `CORRAL_CODE_SERVER_VERSION`, default `4.138.0`)
- Optionally, `MS-CEINTL.vscode-language-pack-zh-hans-1.131.0.vsix` for Chinese UI support

The gateway verifies the digests and uploads the files to the host over SSH.

## Target hosts

Nothing needs to be installed on target hosts for Corral to work:

- **Multiplexers (`tmux` or `herdr`)**: When [tmux](https://github.com/tmux/tmux) or [herdr](https://herdr.dev) is present on a host, opening the host lists its existing sessions (including stopped `herdr` sessions, which restart on open) and offers to create a new one. Sessions running inside `tmux` or `herdr` survive gateway restarts and can also be attached locally. Hosts with neither open a plain login shell.
- **SFTP subsystem**: The file browser and image/file uploads require OpenSSH's SFTP subsystem (enabled by default in standard OpenSSH installations).
- **Agents workbench**: Reading coding agent conversations and process metadata requires Linux hosts with `/proc`.
- **Optional host helper daemon (`corrald`)**: Users can opt in per host to install `corrald` under their own system account (`~/.local/lib/corral/`). It serves agent status and conversation reads over a local Unix domain socket forwarded through the existing SSH connection, reducing SSH session channel usage and providing immediate status updates. It requires no root privileges, opens no network ports, makes no outbound connections, and exits after 10 minutes with no connected gateway. Administrators can disable it globally with `CORRAL_CORRALD_DISABLED=on`.

## Restarts and session restoration

Terminals close when the gateway stops, while `tmux` and `herdr` sessions keep running on their hosts. Unless `CORRAL_RESTORE_SESSIONS=off` is set, Corral automatically reattaches sessions on startup that:

- Were open within the last hour before shutdown.
- Belong to users who still have a valid browser login.
- Use a saved credential and pinned host key (hosts that prompt for a password are never restored automatically).
- Still exist on the target host (Corral never creates a new multiplexer session during restore).

Each restore attempt is recorded in the audit log as `session.restore`. Give the process enough shutdown time (`--stop-timeout 30` or `stop_grace_period: 30s`) so active sessions are recorded cleanly before exit.
