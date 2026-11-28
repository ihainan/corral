# Security

This document describes Corral's security model, operational boundaries, host helper daemon guarantees, and known limitations.

Report security vulnerabilities privately rather than opening a public issue: see [SECURITY.md](../SECURITY.md).

## Core precautions

- **Keep the master key and the database apart.** Stored SSH credentials (and optional translation API keys) are encrypted with `CORRAL_MASTER_KEY`; anyone with both the key and the SQLite database can decrypt them. Back up `CORRAL_DATA_DIR` separately from the master key (keep the key in a secret store or a file excluded from database backups).
- **The gateway acts with its users' SSH credentials.** Run Corral on a machine you trust as much as the hosts behind it.
- **Restrict SSH destinations on shared gateways.** By default, users may connect anywhere except loopback (`127.0.0.0/8`, `::1/128`) and always-denied ranges, including the public internet and the gateway machine's non-loopback addresses. On a multi-user gateway, set `CORRAL_ALLOWED_NETWORKS` to the networks containing your hosts (for example `10.0.0.0/8,100.64.0.0/10` for a LAN and Tailscale) so signed-in users cannot use the gateway's network position to probe other addresses or ports.
- **Inform users before enabling session recording.** Recordings capture terminal screen output and are stored unencrypted under `CORRAL_DATA_DIR/recordings`.

## Authentication and sessions

- Passwords are hashed with Argon2id. Failed sign-in attempts throttle the target account and are rate-limited per client IP address (configure `CORRAL_TRUSTED_PROXIES` behind a reverse proxy so real client addresses are seen).
- Two-step sign-in (TOTP with recovery codes) can be enabled per user, and administrators can reset it if a device is lost.
- Passwords set by an administrator (when creating an account directly or resetting a password) must be changed at the user's next sign-in before accessing the application, unless the administrator unticks that option.
- Disabling an account or resetting its password as an administrator immediately terminates all of its browser logins, open terminals, port forwards, and VS Code instances, revokes its push subscriptions, and cleans up its `corrald` host helper state.
- Changing your own password signs out your other browsers. Signing out ends only the current browser's login.
- Terminals otherwise keep running until closed or detached longer than `CORRAL_DETACH_TIMEOUT` (default `24h`).

## Network controls and jump hosts

- Hostnames are resolved once, every returned IP address is checked against `CORRAL_ALLOWED_NETWORKS` and `CORRAL_DENIED_NETWORKS`, and Corral dials the verified IP directly so DNS rebinding cannot swap the target. If a name resolves to any forbidden address, the entire lookup is rejected.
- Always-denied ranges include unspecified addresses (`0.0.0.0/8`, `::/128`), link-local ranges (`169.254.0.0/16`, `fe80::/10`, blocking most cloud metadata endpoints), Alibaba Cloud metadata (`100.100.100.200/32`), Azure WireServer (`168.63.129.16/32`), AWS IPv6 metadata (`fd00:ec2::254/128`), multicast, reserved, and documentation ranges. IPv6 addresses embedding an IPv4 address via NAT64, SIIT, 6to4, Teredo, or IPv4-compatible encodings are held to the rules for the embedded IPv4 address too.
- **Jump hosts.** `CORRAL_ALLOWED_NETWORKS` applies to every connection the gateway opens itself, including the jump host. The hop from a jump host to the target behind it originates on the jump host, where the user can already log in; only addresses that are never valid hosts (link-local, multicast, unspecified) are refused there, and only when written as literal IP addresses (hostnames are resolved by the jump host).
- **Forwarded ports and VS Code.** Served on sibling subdomains of `CORRAL_FORWARD_DOMAIN`. Over HTTPS, the main site sets a `__Host-` session cookie, preventing forwarded applications from reading or overwriting the gateway login. Every request to a forwarded subdomain is authorized against the signed-in user.

## Gateway restarts

Terminals end when the gateway stops, while `tmux` and `herdr` sessions keep running on their hosts. On restart (unless `CORRAL_RESTORE_SESSIONS=off`), the gateway reattaches sessions that were open within the last hour for users still signed in somewhere, using only the host's saved credential and pinned host key (never a prompted password), and only if the session still exists on the host—it never creates new sessions. Each outcome is logged in the audit log as `session.restore`.

## Recordings and session watching

- **Recordings** capture whatever the terminal screen displayed: echoed input appears, while passwords typed at prompts that disable echo do not. Owners can view their own recordings; administrators can view everyone's and delete recordings. Watching a recording is recorded in the audit log, and terminals display an indicator while recording is active.
- **Read-only watching** shares a live terminal screen (including recent scrollback) with users added by the session owner. Watchers cannot type, resize, browse files, or view the session owner's agent conversation logs.

## Host helper daemon (`corrald`)

The optional `corrald` helper daemon reduces SSH session channel pressure and delivers immediate agent status updates:

- Installed only after explicit per-host user consent against a verified capability manifest hash.
- Runs strictly under the user's own system account with no root privileges, no listening network ports, and no outbound internet access; communicates only over a local Unix domain socket forwarded through the existing SSH connection.
- Accepts only predefined user-initiated operations (reading process/agent status, conversation logs, and pane screens; sending text/keys and answering prompt cards); never executes arbitrary shell commands.
- Does not modify agent configuration files, shell rc files, `PATH`, `crontab`, systemd units, or `~/.ssh/`.
- Logs operations and errors only (`corrald.log`, capped at 5 MB with one backup), never conversations or screen contents.
- Exits automatically after 10 minutes without a connected gateway. Any future release that expands its read/action/write scope requires renewed user consent.
- Administrators can disable `corrald` globally with `CORRAL_CORRALD_DISABLED=on`.

## Known limitations

- Two devices typing into the same terminal session at the same time interleave input, as with any shared terminal.
- In a plain shell, composer `@` completion reads from the home directory (in `tmux` and `herdr`, it reads from the terminal's current working directory).
- In a directory where other users may rename entries (world-writable without the sticky bit), an upload's staging directory can be swapped by another user between steps; such a directory already permits tampering with anything placed in it.
- The file browser lists the first 5,000 entries returned by the host in a directory (folders first, sorted by name) and indicates when more exist.
- Answering a waiting agent checks the pane's screen right before typing, but `herdr` cannot check and type atomically: an answer entered directly on the host in that same moment is not detected.
- After a gateway restart, open `tmux` and `herdr` tabs reopen automatically in their previous positions; hosts configured to ask for a password each time are not restored (the UI indicates this), and plain shells end with the gateway. Each browser window tracks its own open-tab record, so a tab closed in one window while another was hidden or on another page may reopen in the other window after a restart.
- The Agents workbench reads conversations for Claude Code, Codex, OpenCode, Antigravity CLI, and Devin CLI running in `tmux` or `herdr` on Linux hosts (process discovery relies on `/proc`). Only the session owner can read agent conversations; read-only watchers see only the terminal screen because conversation logs hold full history beyond what is on screen.
- Installing the optional Claude Code question hook from the UI reads and writes `~/.claude/settings.json` only (custom `CLAUDE_CONFIG_DIR` paths are supported for reading conversations, not for the hook installer).
- In `herdr` panes, prompts and questions are answered directly inside the conversation view or reply dialog; in `tmux`, questions an agent asks via its own tool show as waiting and are answered in the terminal (permission prompts are not written to the log, so `tmux` shows the agent as working). The sidebar shows live agent statuses for `herdr` sessions and the currently viewed `tmux` session; other `tmux` sessions are listed without status.
- Sending a message from the conversation view verifies right before typing that the expected agent process is still in the foreground of that pane, but check-and-type is not atomic on the host: if the process exits in those few milliseconds, the input lands in the shell. Stop and "send now" press their keys after the gateway's check, so a process exiting or a pane changing hands in between still receives the keystroke. In `tmux`, verifying the pane belongs to the session and sending keys are two separate `tmux` commands, so a window moved to another session in between still receives the keys. Keys from the touch key bar go directly to the terminal rather than through prompt cards.
- Permission cards display the prompt as the agent draws it on the screen; when the layout cannot be parsed reliably, the card falls back to showing the raw screen excerpt. The permission mode reflects the latest log entry (a change made since appears with the next message).
- Queued messages shown above the composer reflect what the log recorded for the current process since it started; a process of the same session restarted within one second of the previous one cannot be distinguished (host boot time has one-second resolution).
- Multiple tabs viewing the same `tmux` or `herdr` session are ordered by when they were opened; closing one can shift the positions of the other tabs for that session.
- Claude Code's suggested next prompt is read from greyed placeholder text in its terminal input box; another program rendering a matching input box could also be offered in the composer, though it is only inserted as a draft and never sent automatically.
