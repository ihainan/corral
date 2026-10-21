# Contributing

Thank you for helping. Bug reports, ideas and pull requests are all welcome;
for anything large, open an issue first so we can agree on the shape of it.
Security problems go to [SECURITY.md](SECURITY.md), not to issues.

## Setting up

The [Develop](README.md#develop) section of the README has the commands: a test
SSH server in a container, the web interface built once, the gateway run with
`go run`. You need Go (the version in `go.mod`), Node.js 22 with corepack, and
Docker for the test server.

## Before sending a change

- `make check` passes: Go vet and tests (including the security suite), the
  web type check, and the third-party notices.
- Behaviour is covered by a test where one is practical (Go tests under
  `internal/…`); a change to how the browser behaves says in the pull request
  how it was tried.
- Interface changes are tried on a phone, a tablet and a desktop window, in
  the light and the dark theme, with real-looking data (long names, many
  items). Corral is used on all three alike. If you can, try Safari on an
  iPhone and an iPad, whose keyboard and focus rules differ from desktop
  browsers'.
- Text shown to users is in both languages, in `web/src/i18n.ts` (the type
  check fails when a key is missing in one). Write the English as plainly as
  you can.
- Settings and anything stored are validated on the gateway as strictly as in
  the browser; anything coming from a host (terminal output, file names, herdr
  state) is treated as untrusted.
- No secrets, internal host names or addresses, or personal data in code,
  tests, screenshots or commit history.

## Dependencies

Corral is Apache-2.0 and its releases bundle their dependencies, so new ones
must have a permissive licence (MIT, BSD, Apache-2.0, ISC and the like; no GPL
or AGPL). Run `make notices` after adding or updating one and commit the result.
Prefer a few lines of code over a dependency for small things.

## Commits and pull requests

- Commit messages in English: a short subject saying what changes, then why,
  for the reader of `git log` a year from now.
- One topic per pull request; unrelated clean-ups separately.
- Code comments explain what is not obvious from the code: the reason, the
  constraint, the case it handles.

## Releases

Maintainers tag `v<version>` after adding its section to
[CHANGELOG.md](CHANGELOG.md); the release workflow builds the binaries and the
image and publishes them.
