# Changelog

Notable changes to Corral. Versions follow [Semantic Versioning](https://semver.org);
before 1.0, a minor version may change configuration or behaviour (said here when it does).

## [Unreleased]

### Added
- Session tabs on desktops and tablets: every running session is a tab in the top bar, kept
  connected (up to 8) so switching is instant; `+` opens another; Ctrl+Tab
  and Ctrl+Shift+Tab in the installed app. A tapped notification opens as a
  tab of a window that has them.
- The composer completes `@` files, `/` commands (the agents' own and those
  on the host) and `$` Codex skills, and keeps a history of what was sent.
- Agent alerts say what the agent asks, answer with its first or last choice
  from the alert where buttons are shown (once, while nothing changed), and
  are replaced once the agent no longer waits (also when its pane or session
  closes). At most 10 alerts a minute per user.
- A tapped agent alert brings herdr to that agent's pane (its workspace and
  tab too); on a phone it also leaves another session open in front for it.
- Uploads from the terminal or the composer show their progress and can be
  stopped; closing the file panel asks first while files are still going up.

### Changed
- The herdr panel of workspaces and agents is gone: herdr's own sidebar
  lists them. A button in the top bar, shown while agents wait for you,
  answers one at once or picks among several.
- Ending a session from its tab goes to the next tab, not the host list; with
  tabs, recording, watchers and a slow link show next to them.
- The key bar's default layout puts the arrows before Fn, so all four are in
  sight on a phone. Secondary text, placeholders and the dark theme's buttons
  meet a 4.5:1 contrast; touch targets in the composer and file panel are larger.
- Clearer wording in menus and forms ("Browse files", "Port forwarding",
  "Password each time"); the composer's history no longer lists unsent drafts.

### Fixed
- The terminal fitted one row and a column or two too many: its last row
  (Claude Code's bottom line) was partly hidden under the key bar or the
  window's edge.
- After a newline, input methods that space Chinese from Latin text
  (WeType) no longer put a space before what is typed next.
- The composer keeps its draft when you leave the terminal and come back,
  and no longer capitalises or "corrects" commands on iOS.
- On touch screens the terminal no longer leaves an empty strip on the right
  (room kept for a scrollbar that only shows while scrolling).
- The settings page's font size steps one point at a time (it could jump the
  other way); a port of 0 is refused instead of becoming 22; an invite link
  that could not be checked offers to retry instead of saying it is invalid.

## [0.1.0] - 2026-10-20

The first release.

### Terminal
- Sessions run on the gateway and survive the browser sleeping, losing signal
  or switching devices; with tmux or herdr on the host they also survive
  gateway restarts. Opening a host offers its tmux and herdr sessions, a new
  tmux session or a plain shell.
- A window that attaches late (or reconnects after missing too much output)
  gets the terminal modes programs set once at their start: mouse reporting,
  bracketed paste, the alternate screen, cursor keys, the cursor's style, and
  requests to tell keys apart.
- Several devices can have one session open; the one you act on sets the size.
- Copy mode, colour schemes, font sizes, a composer for longer input, Chinese
  and other input methods, an installable app (PWA), clipboard writes from the
  host (OSC 52).

### Phones and tablets
- A key bar you arrange: keys pinned on the left and a scrolling row, built-in
  keys and keys of your own sending a sequence (tmux's `send-keys` way or
  Moshi's), a long press with its own sequence on most keys, a picker and a
  preview in words.
- A shortcut panel (long press on Ctrl): Ctrl combinations, herdr actions (tabs,
  splits, panes, zoom), tmux prefix keys, and Claude Code and Codex commands;
  it opens on the page of the agent in herdr's focused pane.
- Taps reach the program without bringing up the keyboard; tapping where you
  type (the cursor's line, Claude Code's input box) or the keyboard button
  does. Hardware keyboards type at once.
- The home page shows running sessions as cards with their screen in small.

### Coding agents
- Paste, drop or pick an image: it is uploaded and its path typed (`@path` for
  Gemini CLI). Shift+Enter is a newline; Ctrl+Enter reaches programs that tell
  it apart (Claude Code's "send now").
- With herdr: a panel of workspaces and agents, who is waiting, switching,
  one pane full size, answering a waiting agent from the panel or its
  notification.
- Push notifications and webhooks when an agent needs you or finishes, on the
  terminal bell or a desktop notification from the host.

### Around the terminal
- A file panel: browse, upload, download, create, rename, delete, insert a path.
- Port forwarding of web servers on the host, private or shared; VS Code in the
  browser (code-server, installed on the host on demand).
- Jump hosts (one hop, the jump host's key checked).

### Accounts and security
- Users and administrators, each with their own hosts and credentials; SSH
  passwords and keys are encrypted with a master key kept apart from the
  database; an audit log.
- Two-step sign-in (TOTP, recovery codes), sign-in throttling, sessions ended
  when an account is disabled.
- Session recording (optional, 30 days) and read-only watching by people the
  owner adds.
- English and Chinese interface.

[0.1.0]: https://github.com/ihainan/corral/releases/tag/v0.1.0
