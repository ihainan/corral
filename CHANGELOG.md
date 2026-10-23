# Changelog

Notable changes to Corral. Versions follow [Semantic Versioning](https://semver.org);
before 1.0, a minor version may change configuration or behaviour (said here when it does).

## [Unreleased]

### Added
- A conversation view (the speech-bubbles button) for Claude Code in tmux or
  herdr on Linux hosts: the session's own log as turns, the agent's answers
  formatted (lists, tables, code to copy), the tool calls between them folded
  into one line per run and opened on demand, edits shown as the lines they
  change. It reads the whole history, which herdr's panes do not keep for a
  program on the alternate screen, and follows the pane in front. It is off
  until turned on, per session; messages are still typed in the composer.
  Only the session's owner sees it. A bar over it names the agent (herdr's
  workspace and the pane's title) and what it is doing; when it waits for
  an answer a strip at the bottom opens the reply dialog. What the
  composer sends shows until the log has it, and on touch screens the
  composer opens with the view. When the gateway loses herdr, its last
  statuses are no longer shown as current.
- Tab keys in the installed app: ⌘T opens a new tab (Ctrl+Shift+T off a Mac,
  where Ctrl+letters stay with the programs); on a Mac or an iPad ⌘1–⌘8 pick
  a tab by its place, ⌘9 the last, ⌘⇧[ and ⌘⇧] the previous and next. No key
  closes a tab, as that ends its session. In a browser tab these stay the
  browser's.
- Opening a host offers a new herdr session (named by the gateway, as a new
  tmux session is) and lists stopped herdr sessions too, which start again
  when opened; before, herdr was offered only while a session of it ran.
- The shortcut panel: herdr's new workspace and close pane (asked first, for
  the pane focused then), Ctrl+\ (quit), Claude Code's task list (^T), Codex's
  mode (⇧Tab), transcript (^T) and editing the last message (Esc Esc).

### Changed
- The settings page is grouped by what a setting is about: terminal, keyboard
  and composer, touch screens (the key bar and taps), notifications and the
  account. Display name, password and two-step sign-in are rows like the
  others; the password is changed in a dialog. The page is a little wider on
  large screens, and on phones the Enter key's choice fills the row.
- Scrolling with a trackpad, in a program that takes the mouse (Claude Code's
  fullscreen view, herdr, tmux, vim), goes as far as the fingers move: a
  report per line, as native terminals send, instead of one per event at a
  third of the distance. A mouse wheel's notch is one report as before. On
  touch screens a flick slows as iOS lists do and carries further, and the
  scroll goes to the pane under the finger (herdr's split panes).

### Fixed
- On iOS with WeType (and input methods like it), the letter typed after
  another no longer comes up capitalised.
- The "drop to upload" hint goes away when a file dragged over the terminal
  is taken away again or the drag is given up (it stayed until a reload).
- On a Mac the composer's key hints read Return and Shift+Return: the ↩ and
  ⇧ symbols came out small and low beside the text.
- Ctrl+Tab (and the installed app's other tab keys) goes one tab per press:
  held a little long, the key's repeat went on a tab more each time.

## [0.2.0] - 2026-10-21

Upgrading from 0.1.0 needs no configuration change; the database is updated
when the new version starts.

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
- Programs that follow the terminal's colours (Claude Code's "Auto" theme,
  herdr for its panes) are told when the terminal scheme turns dark or light
  (mode 2031), so they switch with it.
- Once agents are seen in a terminal, alerts are offered for the device (a
  button that asks the browser), when it has not been asked yet; at most three
  times, a week apart.
- Uploads from the terminal or the composer show their progress and can be
  stopped; closing the file panel asks first while files are still going up.
- Terminal schemes can follow the app's appearance with a scheme of your
  choice for each (Match app: one for dark, one for light).
- Agents that finished and were not looked at since (herdr's "done") show on
  their session's tab and card.
- Two fingers moved together on a touch screen switch to the next or
  previous session (sideways) or herdr workspace (up and down); spread or
  pinched they still size the font.
- A long paste into the composer shows as a label ("[Pasted text #1, 120
  lines]") and is sent in full.
- Administrators can create an account directly (Users → Create directly):
  a user name and a first password to hand over. Such an account, and one
  whose password an administrator reset, must choose its own password at its
  next sign-in before anything else (an option of both).
- The composer offers Claude Code's suggested next prompt (the greyed text in
  its input box): as its placeholder, taken with Tab (a button on touch screens).
- A tmux or herdr session the gateway's restart (an update) or a lost host
  connection ended is opened again by itself, after a 3-second countdown in
  the tab in front (at once in the tabs behind it), once the gateway answers
  again. A session someone ended, or a plain shell, is not.

### Changed
- The composer stays open or closed per session across reloads (a new session
  follows the last choice), and its draft survives a reload of the same
  browser tab (kept in that tab only, gone when it is closed).
- On a computer, Enter in the composer sends and Shift+Enter starts a new
  line, as in the agents' own input boxes; a setting turns Enter back into a
  new line (⌘/Ctrl+Enter sends). On phones and tablets Enter stays a new line.
  The keys are shown by the send button.
  "Insert" is now "Type in".
- Text being composed by an input method is shown in the terminal's colours,
  underlined, instead of a black box.
- The herdr panel of workspaces and agents is gone: herdr's own sidebar
  lists them. A button in the top bar, shown while agents wait for you,
  answers one at once or picks among several.
- Session cards on the home page show the whole screen, scaled to fit, instead
  of its bottom left corner (a large herdr screen showed only part of its
  sidebar).
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
  (WeType, Sogou; on iOS and macOS) no longer put a space before what is
  typed next.
- The composer keeps its draft when you leave the terminal and come back,
  and no longer capitalises or "corrects" commands on iOS.
- Chinese full-width punctuation no longer gets half an em of extra space
  after it, which could push the end of a line under the next column (Chrome
  on macOS with PingFang trims such punctuation when it is measured).
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

[Unreleased]: https://github.com/ihainan/corral/compare/v0.2.0...HEAD
[0.2.0]: https://github.com/ihainan/corral/releases/tag/v0.2.0
[0.1.0]: https://github.com/ihainan/corral/releases/tag/v0.1.0
