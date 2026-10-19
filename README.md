# Corral

A self-hosted web SSH client built for phones and tablets first, and for
running terminal coding agents (Claude Code, Codex, Gemini CLI…) from anywhere.

Corral is a gateway: it holds the SSH connections and serves a terminal to
the browser. Your phone can sleep, lose signal or switch apps; the session
keeps running and you pick it up where it was — on the same device or another.

## Features

**A terminal that works on a phone**
- The gateway keeps sessions alive while nobody looks; with tmux or
  [herdr](https://herdr.dev) on the host they also
  survive gateway restarts.
- A key bar with Esc, Ctrl and Alt (tap once or lock), Tab and Shift+Tab,
  arrows, F1–F12 and the symbols that are hard to type on a phone keyboard.
- Input methods work: Chinese and other IMEs, dictation, and a composer field
  for writing longer input before sending it.
- Several devices can have one session open; the one you act on decides the
  terminal size, the others follow it.
- A copy mode with native text selection, colour schemes, adjustable fonts,
  installable as an app (PWA).

**Made for coding agents**
- Paste, drop or pick an image: it is uploaded to the host and its path is
  typed into the terminal, which is how Claude Code and Codex take images
  (Ctrl+V works as in a local terminal). Shift+Enter inserts a newline.
- With herdr, a panel lists workspaces and agents, shows who is waiting for
  you, and switches between them.
- Push notifications (and optional webhooks) when an agent in herdr needs
  you or finishes, or when a program rings the terminal bell or sends a
  desktop notification (OSC 9 / OSC 777) — unless you are looking at that
  session anyway.

**Around the terminal**
- A file panel: browse the host, upload (with progress, asking before
  replacing), download, create folders, rename, delete, and insert a path
  into the terminal — starting in the terminal's current folder.
- Open web servers running on the host (dev servers, previews) in the
  browser through port forwarding, privately or shared with other users.
- Open the host in VS Code in the browser ([code-server](https://github.com/coder/code-server),
  installed on the host on demand; not bundled with Corral).

**Several users**
- Accounts by invitation, administrators and members, each with their own
  hosts and keys. SSH passwords and keys are encrypted at rest and never sent
  to the browser.
- Two-step sign-in with an authenticator app, with recovery codes.
- An audit log of sign-ins, connections, file changes and administration.
- Optional session recording (asciicast, played back in the browser) and
  read-only watching of a session the owner shares.
- The gateway connects only to networks you allow (private ranges by
  default), checked after DNS resolution.

Chinese and English user interfaces ship together.

## Run

Corral is one binary (or container image) with an embedded web interface and
a SQLite database. It needs a public URL and a master key:

```sh
docker build -t corral .
docker run -d --name corral -p 127.0.0.1:8080:8080 -v corral-data:/data \
  -e CORRAL_PUBLIC_URL=https://ssh.example.com \
  -e CORRAL_MASTER_KEY="$(docker run --rm corral genkey)" \
  corral
docker logs corral   # prints a one-time link for creating the first administrator
```

Or with Compose, keeping the key in a file outside the data directory:

```yaml
services:
  corral:
    image: corral
    restart: unless-stopped
    ports: ["127.0.0.1:8080:8080"]
    environment:
      CORRAL_PUBLIC_URL: https://ssh.example.com
      CORRAL_MASTER_KEY_FILE: /run/secrets/corral-master-key
      CORRAL_TRUSTED_PROXIES: 172.16.0.0/12   # the reverse proxy, as the container sees it
    volumes:
      - ./data:/data
      - ./master.key:/run/secrets/corral-master-key:ro
```

Open the link from the log to create the first administrator, then invite
everyone else from **Users**.

### Behind a reverse proxy

Serve Corral over HTTPS. The proxy must pass WebSocket upgrades, must not
buffer responses, and should allow long-lived connections and uploads as
large as `CORRAL_UPLOAD_MAX_MB`; see [`deploy/nginx.conf.example`](deploy/nginx.conf.example).
Set `CORRAL_TRUSTED_PROXIES` to the proxy's address so sign-in limits and the
audit log see real client addresses.

### Forwarded ports and VS Code

Both are served on subdomains of `CORRAL_FORWARD_DOMAIN`: `<port>-<id>.<domain>`
for a forwarded port and `code-<id>.<domain>` for VS Code. Point a wildcard DNS
record (`*.<domain>`) and a wildcard certificate at the proxy, and route those
names to Corral as well. Each forward is checked against the signed-in user
on every request; subdomains cannot read the main site's sign-in.

### On the hosts

Nothing needs to be installed. With **tmux** on a host (or **herdr**, chosen
per host), sessions run inside it, so they survive the gateway restarting and
can be attached from elsewhere. The file panel and image uploads need the
host's SFTP subsystem (enabled by default in OpenSSH).

## Configuration

All settings are environment variables.

| Variable | Default | Meaning |
|---|---|---|
| `CORRAL_PUBLIC_URL` | (required) | The URL users open; used for origin checks and cookies |
| `CORRAL_MASTER_KEY` / `CORRAL_MASTER_KEY_FILE` | (required) | 32 random bytes, base64 (`corral genkey`); encrypts stored credentials |
| `CORRAL_LISTEN` | `127.0.0.1:8080` (`0.0.0.0:8080` in the image) | Listen address |
| `CORRAL_DATA_DIR` | `./data` (`/data` in the image) | Database and recordings |
| `CORRAL_ALLOWED_NETWORKS` | private ranges | Networks SSH targets may resolve to |
| `CORRAL_DENIED_NETWORKS` | — | Extra networks to refuse (link-local, multicast and unspecified are always refused) |
| `CORRAL_TRUSTED_PROXIES` | — | Proxies whose `X-Forwarded-For` is believed |
| `CORRAL_DETACH_TIMEOUT` | `24h` | How long a session runs with nobody viewing it |
| `CORRAL_MAX_SESSIONS_PER_USER` | `20` | Open sessions per user |
| `CORRAL_UPLOAD_MAX_MB` | `100` | Upload size limit |
| `CORRAL_LOGIN_IDLE_TIMEOUT` / `CORRAL_LOGIN_MAX_AGE` | `168h` / `720h` | Browser sign-in lifetime |
| `CORRAL_DEFAULT_LANGUAGE` | each browser's language | `zh-CN` or `en` for everyone who has not picked one (and for webhooks) |
| `CORRAL_FORWARD_DOMAIN` | — | Parent domain for forwarded ports and VS Code; needs an `https://` public URL |
| `CORRAL_CODE_SERVER` | `on` with a forward domain | `on` / `off`: VS Code in the browser |
| `CORRAL_CODE_SERVER_VERSION` | `4.138.0` | code-server release installed on hosts |
| `CORRAL_CODE_SERVER_URL` | GitHub release | Download URL template (https) with `{version}` and optionally `{arch}` (`amd64`, `arm64`) |
| `CORRAL_CODE_SERVER_SHA256` | built in for `4.138.0` | Trusted archive digests, `amd64=<sha256>,arm64=<sha256>`; required for other versions |
| `CORRAL_CODE_SERVER_DIR` | — | Gateway directory with `code-server-{version}-linux-{arch}.tar.gz`, uploaded to hosts without internet access |
| `CORRAL_RECORDING` | `off` | `on` records the output of every session |
| `CORRAL_RECORDING_DAYS` | `30` | How long recordings are kept |
| `CORRAL_RECORDING_MAX_MB` | `200` | Size limit of one recording |
| `CORRAL_PUSH_HOSTS` | — | Extra push service host suffixes (besides Apple, Google, Mozilla, Microsoft) |
| `CORRAL_WEBHOOK_PREFIXES` | — (webhooks off) | URL prefixes users may send alert webhooks to |
| `CORRAL_STATIC_DIR` | — (embedded) | Serve the web interface from this directory instead (front-end development) |

## Security

- **Keep the master key and the database apart.** Stored SSH credentials are
  encrypted with the master key; anyone with both can decrypt them. Back up
  `CORRAL_DATA_DIR` and keep the key somewhere else (a secret store, or a file
  that is not in the backup).
- **The gateway acts with the users' SSH credentials.** Anyone who controls
  the gateway can reach every host its users can. Run it on a machine you trust
  as much as the hosts behind it, and keep `CORRAL_ALLOWED_NETWORKS` as narrow
  as your hosts allow.
- **Sign-in.** Passwords are hashed with Argon2id; failed attempts slow an
  account down and are limited per address. Two-step sign-in is per user, and
  administrators can reset it for a lost phone. Disabling an account (or an
  administrator resetting its password) ends all of its browser logins,
  terminals and forwards at once; changing your own password signs out your
  other browsers; signing out ends that browser's login. Terminals otherwise
  keep running until closed or detached for `CORRAL_DETACH_TIMEOUT`.
- **Recordings** contain whatever the screen showed (not what was typed), and
  are kept unencrypted in the data directory. Owners see their own,
  administrators everyone's; watching one is audited. Say so to your users
  when you turn recording on — the terminal tells them it is recorded.
- **Watching** shows the screen, recent output included, to the people the
  owner adds; they cannot type, resize or use anything else.

Report security problems privately to the maintainers rather than in a
public issue.

## Known limitations

- Two devices typing into one session at the same time interleave, as with
  any shared terminal.
- In a folder where other users may rename entries (world-writable without
  the sticky bit), an upload's staging folder can be swapped by them between
  steps; such a folder lets them tamper with anything put there anyway.
- Listing an enormous folder in the file panel reads it whole (for at most
  20 seconds) before showing the first 5000 entries.

## Develop

```sh
docker build -t corral-test-sshd test/sshd && docker run -d -p 127.0.0.1:2222:22 corral-test-sshd
make web && go run ./cmd/corral serve   # with the variables above
make check                              # Go tests (incl. the security suite), type check, notices
make notices                            # after changing dependencies
```

The test SSH server accepts `alice` / `alice-pass`. Third-party licences are
listed in [`web/public/THIRD-PARTY-NOTICES.txt`](web/public/THIRD-PARTY-NOTICES.txt),
which the web interface also serves.

## License

[Apache-2.0](LICENSE)
