# Configuration

This document lists every runtime environment variable Corral reads, along with defaults, validation rules, and behavior.

All runtime configuration is supplied through environment variables. Only `CORRAL_PUBLIC_URL` and `CORRAL_MASTER_KEY` (or `CORRAL_MASTER_KEY_FILE`) are required.

## Core

| Variable | Default | Meaning |
|---|---|---|
| `CORRAL_PUBLIC_URL` | (required) | Absolute `http://` or `https://` URL users open (for example `https://ssh.example.com`). Used for origin checks, cookies, setup links, and forwarded-port authentication. |
| `CORRAL_EXTRA_URLS` | — | Additional URLs for the same site, comma-separated (for example an external domain alongside an internal one). Must use the same scheme as `CORRAL_PUBLIC_URL`, have no path/query/fragment/credentials, use ASCII (`xn--`) hostnames, and not fall under `CORRAL_FORWARD_DOMAIN`. Passes origin checks. Invite links follow the domain they were created from; forwarded ports and VS Code stay on `CORRAL_PUBLIC_URL`. Each domain has its own sign-in cookie. |
| `CORRAL_MASTER_KEY` / `CORRAL_MASTER_KEY_FILE` | (required) | 32 random bytes, base64-encoded (generate with `corral genkey`). Encrypts stored SSH credentials and translation API keys. If both are set, `CORRAL_MASTER_KEY_FILE` wins. |
| `CORRAL_LISTEN` | `127.0.0.1:8080` (`0.0.0.0:8080` in the container image) | TCP address the HTTP server binds to. |
| `CORRAL_DATA_DIR` | `./data` (`/data` in the container image) | Directory storing the SQLite database and session recordings. Created with mode `0700` if missing. |
| `CORRAL_DEFAULT_LANGUAGE` | browser language (`en` for webhooks) | Default UI language (`zh-CN` or `en`) for users who have not selected one, and language used for webhook notifications. |

## Network access and proxies

| Variable | Default | Meaning |
|---|---|---|
| `CORRAL_ALLOWED_NETWORKS` | everywhere except loopback | Comma-separated IP addresses or CIDRs that SSH targets may resolve to. When unset or blank, any address is allowed except loopback (`127.0.0.0/8`, `::1/128`) and the always-denied ranges below; the gateway machine's non-loopback addresses are not refused. When set, only the listed networks are permitted (include `127.0.0.1/32` explicitly if loopback access is desired). |
| `CORRAL_DENIED_NETWORKS` | — | Extra comma-separated IP addresses or CIDRs to refuse. Always refused regardless of `CORRAL_ALLOWED_NETWORKS`: `0.0.0.0/8`, `::/128`, link-local (`169.254.0.0/16`, `fe80::/10`, covering most cloud instance metadata services), Alibaba Cloud metadata (`100.100.100.200/32`), Azure WireServer (`168.63.129.16/32`), AWS IPv6 metadata (`fd00:ec2::254/128`), multicast (`224.0.0.0/4`, `ff00::/8`), reserved (`240.0.0.0/4`), and documentation ranges (`192.0.2.0/24`, `198.51.100.0/24`, `203.0.113.0/24`, `2001:db8::/32`, `3fff::/20`). IPv6 addresses carrying an embedded IPv4 address via known translations (NAT64 `64:ff9b::/96` and `64:ff9b:1::/48`, SIIT `::ffff:0:0:0/96`, 6to4 `2002::/16`, Teredo `2001::/32`, IPv4-compatible `::/96`) are checked against the rules for that IPv4 address as well (for `64:ff9b:1::/48`, where the prefix length is unknown, every RFC 6052 reading is checked). Network-specific NAT64 prefixes are not detected automatically; on such networks, restrict `CORRAL_ALLOWED_NETWORKS` explicitly. |
| `CORRAL_TRUSTED_PROXIES` | — | Comma-separated IP addresses or CIDRs of reverse proxies whose `X-Forwarded-For` header is trusted. When empty, no proxy is trusted. |

## Sessions and uploads

| Variable | Default | Meaning |
|---|---|---|
| `CORRAL_DETACH_TIMEOUT` | `24h` | Go duration a terminal session stays alive with no browser attached. |
| `CORRAL_MAX_SESSIONS_PER_USER` | `20` | Maximum concurrent open terminal sessions per user (minimum `1`). |
| `CORRAL_SSH_MAX_SESSIONS` | `9` | Maximum concurrent SSH `"session"` channels (PTY, exec, SFTP, direct single-pane attach) per SSH connection (valid range `4`–`1000`). Defaults to `9`, leaving 1 slot below OpenSSH's default `MaxSessions` of `10`. Forwarded channels (`direct-tcpip`, `direct-streamlocal@openssh.com`) do not count toward this cap. |
| `CORRAL_UPLOAD_MAX_MB` | `100` | Maximum file upload size in MiB (minimum `1`). |
| `CORRAL_LOGIN_IDLE_TIMEOUT` | `168h` | Browser sign-in idle expiry as a Go duration (default 7 days). |
| `CORRAL_LOGIN_MAX_AGE` | `720h` | Maximum browser sign-in lifetime as a Go duration (default 30 days). |
| `CORRAL_RESTORE_SESSIONS` | `on` | Set to `off` to disable automatically reattaching open `tmux` and `herdr` sessions when the gateway starts (and to discard previously recorded open tabs). See [Deployment](deployment.md#restarts-and-session-restoration). |

## Forwarded ports and VS Code

| Variable | Default | Meaning |
|---|---|---|
| `CORRAL_FORWARD_DOMAIN` | — | Parent domain for forwarded ports (`<port>-<id>.<domain>`) and VS Code (`code-<id>.<domain>`). Requires an `https://` `CORRAL_PUBLIC_URL`, except during local development when both `CORRAL_PUBLIC_URL` and `CORRAL_LISTEN` use loopback IP addresses. |
| `CORRAL_CODE_SERVER` | `on` when `CORRAL_FORWARD_DOMAIN` is set, else `off` | `on` or `off`. Enables browser-based VS Code (`code-server`). Setting `on` requires `CORRAL_FORWARD_DOMAIN`. |
| `CORRAL_CODE_SERVER_VERSION` | `4.138.0` | Semver `code-server` release installed on hosts (format `major.minor.patch`). |
| `CORRAL_CODE_SERVER_URL` | GitHub release URL | HTTPS download URL template containing `{version}` and optionally `{arch}` (`amd64` or `arm64`). |
| `CORRAL_CODE_SERVER_SHA256` | built in for `4.138.0` | Trusted archive SHA-256 digests in the form `amd64=<64-hex>,arm64=<64-hex>`. Required when using a version other than `4.138.0`; archives without a trusted digest are never installed. |
| `CORRAL_CODE_SERVER_DIR` | — | Existing gateway directory containing `code-server-{version}-linux-{arch}.tar.gz` (and optionally `MS-CEINTL.vscode-language-pack-zh-hans-1.131.0.vsix` for the Chinese interface), uploaded to hosts that lack internet access. |

## Session recording

| Variable | Default | Meaning |
|---|---|---|
| `CORRAL_RECORDING` | `off` | `on` or `off`. When `on`, records the terminal output of every session as asciicast files under `CORRAL_DATA_DIR/recordings`. |
| `CORRAL_RECORDING_DAYS` | `30` | Retention period in days before old recordings are pruned (valid range `1`–`3650`). |
| `CORRAL_RECORDING_MAX_MB` | `200` | Maximum size in MiB of a single recording before writing stops (valid range `1`–`100000`). |

## Notifications and webhooks

| Variable | Default | Meaning |
|---|---|---|
| `CORRAL_PUSH_HOSTS` | — | Comma-separated extra Web Push service hosts permitted in addition to Apple, Google, Mozilla, and Microsoft. Accepts an exact hostname or `.example.com` for any subdomain beneath it. |
| `CORRAL_WEBHOOK_PREFIXES` | — (webhooks off) | Comma-separated URL prefixes users are permitted to configure for alert webhooks. |

## Host helper daemon (`corrald`)

| Variable | Default | Meaning |
|---|---|---|
| `CORRAL_CORRALD_DIR` | `corrald/` next to the `corral` executable (`/usr/local/share/corral/corrald` in the container image) | Directory containing `linux-amd64/corrald` and `linux-arm64/corrald` (and optionally `manifest.json`). If missing or unreadable, the host helper daemon feature is disabled and Corral uses SSH exec and SFTP directly. |
| `CORRAL_GATEWAY_ID` | hostname of `CORRAL_PUBLIC_URL` | Stable gateway identifier written into per-host `corrald` authorization bindings. Set explicitly if the gateway's public hostname may change while keeping existing host authorizations. |
| `CORRAL_CORRALD_DISABLED` | `off` | Administrator kill-switch (`on`/`true`/`1` or `off`/`false`/`0`) that disables installing or using `corrald` across all hosts. |

## Single sign-on

See [Single sign-on](single-sign-on.md) for setup instructions, security behavior, and provider examples.

| Variable | Default | Meaning |
|---|---|---|
| `CORRAL_LOCAL_LOGIN` | `all` | Who may sign in with a local password: `all`, `admins` (administrators only; everyone else uses SSO), or `off` (nobody; requires `CORRAL_OIDC_ISSUER`). |
| `CORRAL_OIDC_ISSUER` | — (SSO off) | OpenID Connect provider issuer URL. Must be `https://` (`http://` allowed only for `localhost`, `127.0.0.1`, or `::1` during development) with no query, fragment, whitespace, or embedded credentials. Endpoints and JWKS keys are discovered from it. |
| `CORRAL_OIDC_CLIENT_ID` | (required with issuer) | OIDC client ID registered at the provider. |
| `CORRAL_OIDC_CLIENT_SECRET` / `CORRAL_OIDC_CLIENT_SECRET_FILE` | (required with issuer) | OIDC client secret (Corral is a confidential client). Set exactly one of the two; setting both is an error. |
| `CORRAL_OIDC_SCOPES` | `openid profile email` | Scopes requested during sign-in, space- or comma-separated. `openid` is always included. |
| `CORRAL_OIDC_GROUPS_CLAIM` | `groups` | Claim holding the user's groups: a top-level claim name or a dotted path (for example `realm_access.roles`). |
| `CORRAL_OIDC_ALLOWED_GROUPS` | — (anyone the provider signs in) | Comma-separated groups whose members are allowed to sign in. |
| `CORRAL_OIDC_ADMIN_GROUPS` | — (roles managed in Corral) | Comma-separated groups whose members are granted administrator privileges. Evaluated on every SSO sign-in; users in none of these groups become regular members (never demoting the last administrator). |
| `CORRAL_OIDC_BUTTON` | `Sign in with SSO` / `使用单点登录` | Custom label for the SSO sign-in button. |
| `CORRAL_OIDC_END_SESSION` | `false` | `true`/`1`/`yes` or `false`/`0`/`no`. When enabled, signing out of an SSO-created session also redirects to the provider's `end_session_endpoint` if advertised. |

## Development

| Variable | Default | Meaning |
|---|---|---|
| `CORRAL_STATIC_DIR` | — (embedded web UI) | Serve the web interface from this directory on disk instead of the embedded build. Intended for frontend development. |
