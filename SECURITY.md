# Security

Corral holds SSH credentials and opens terminals on its users' hosts, so its
security matters more than most web apps'. Thank you for looking.

## Supported versions

The latest release, and `main`. Fixes are not backported to older releases
before 1.0.

## Reporting a vulnerability

Please report it privately, not in a public issue or pull request:
**[report a vulnerability](https://github.com/ihainan/corral/security/advisories/new)**
(GitHub's private vulnerability reporting).

Include what you can of:

- what an attacker can do, and what they need first (an account? which role?
  a position on the network? a host they control?);
- the steps or a proof of concept, against a fresh install if possible;
- the version (`corral version`, or the commit) and the configuration that
  matters (reverse proxy, `CORRAL_*` settings);
- any idea of a fix.

You should hear back within a week. We will agree on a date for disclosure,
normally once a fixed release is out, and credit you in the advisory unless you
prefer otherwise.

## Out of scope

These are how Corral is meant to work, described in the README's
[Security](README.md#security) section:

- administrators see the audit log and everyone's recordings; recordings are
  stored unencrypted;
- whoever controls the gateway (and its master key) can reach every host its
  users can;
- the hop from a jump host to the host behind it is made by the jump host and
  is not limited by `CORRAL_ALLOWED_NETWORKS`;
- the limitations listed under [Known limitations](README.md#known-limitations).

A way around any of these protections is in scope, of course.
