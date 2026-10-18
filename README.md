# Corral

A self-hosted web SSH client built for phones and tablets first, and for
running terminal coding agents (Claude Code, Codex) from anywhere.

- **Sessions survive your phone.** The gateway holds the SSH connection. Lock
  your phone, lose signal, switch apps — come back and the terminal continues
  exactly where it was. With tmux on the host, sessions also survive gateway
  restarts.
- **The keys your phone lacks.** A key bar with Esc, Ctrl and Alt (tap once or
  lock), Tab and Shift+Tab, arrows, F1–F12 and the symbols that are hard to type.
- **Agent friendly.** Paste, drop or pick an image and it is uploaded to the
  host and its path pasted into the terminal, which is how Claude Code and Codex
  take images. Shift+Enter inserts a newline. A composer field makes input
  methods and dictation work properly.
- **Multi-user.** Accounts by invitation, per-user hosts and keys, credentials
  encrypted at rest and never sent to the browser, an audit log, and an
  allow-list of networks the gateway may reach.

Status: early development.

## Run

```sh
docker build -t corral .
docker run -d --name corral -p 8080:8080 -v corral-data:/data \
  -e CORRAL_PUBLIC_URL=https://ssh.example.com \
  -e CORRAL_MASTER_KEY="$(docker run --rm corral genkey)" \
  corral
docker logs corral   # prints a one-time link for creating the first administrator
```

Serve it over HTTPS behind a reverse proxy that passes WebSocket upgrades
(see `deploy/nginx.conf.example`). Keep the master key out of database backups:
anyone with both can decrypt stored SSH credentials.

## Configuration

| Variable | Default | Meaning |
|---|---|---|
| `CORRAL_PUBLIC_URL` | (required) | The URL users open; used for origin checks and cookies |
| `CORRAL_MASTER_KEY` / `CORRAL_MASTER_KEY_FILE` | (required) | 32 random bytes, base64 (`corral genkey`) |
| `CORRAL_LISTEN` | `127.0.0.1:8080` | Listen address |
| `CORRAL_DATA_DIR` | `./data` | SQLite database location |
| `CORRAL_ALLOWED_NETWORKS` | private ranges | Networks SSH targets may resolve to |
| `CORRAL_DENIED_NETWORKS` | — | Extra networks to refuse (link-local and multicast are always refused) |
| `CORRAL_TRUSTED_PROXIES` | — | Proxies whose `X-Forwarded-For` is believed |
| `CORRAL_DETACH_TIMEOUT` | `24h` | How long a session runs with nobody viewing it |
| `CORRAL_MAX_SESSIONS_PER_USER` | `20` | Open sessions per user |
| `CORRAL_UPLOAD_MAX_MB` | `100` | Upload size limit |
| `CORRAL_LOGIN_IDLE_TIMEOUT` / `CORRAL_LOGIN_MAX_AGE` | `168h` / `720h` | Browser sign-in lifetime |

## Develop

```sh
docker build -t corral-test-sshd test/sshd && docker run -d -p 127.0.0.1:2222:22 corral-test-sshd
make web && go run ./cmd/corral serve      # with the variables above
make check                                  # Go tests (incl. security suite) + type check
```

## License

Apache-2.0
