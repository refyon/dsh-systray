# Security Policy

## Reporting a vulnerability

Please report security issues **privately**, not through a public issue:

- Preferred: GitHub's [private vulnerability reporting](https://github.com/refyon/dsh-systray/security/advisories/new)
  (Security → Advisories → Report a vulnerability).
- Alternative: open a minimal issue asking for a private channel, without any details of the flaw.

Please include: affected version (the `vX.Y.Z` you downloaded, or `dev`), platform, what an attacker
can achieve, and the smallest reproduction you have. Expect an initial reply within a few days —
this is a single-maintainer project maintained in spare time, so please allow for that.

## Supported versions

Only the latest release is supported. Fixes are shipped as a new tag; there are no backports to
older releases.

## Scope notes

Things worth reporting:

- The local Web UI being reachable beyond loopback, or a token/auth bypass (`port`, `trustedHosts`).
- Silent modification of system state outside the app's own data: autostart registry entry,
  shell/`PATH` changes, files written outside the app directories.
- Credential handling: the app stores no GitHub token itself (the `gh` CLI keeps it in the OS
  credential store), and `account.json` holds the account session token with `0600` permissions.
  Anything that leaks these, or writes them somewhere looser, is in scope.
- Update path: accepting an update package whose checksum does not match `SHA256SUMS.txt`, or
  fetching update metadata over an unauthenticated channel that can be swapped for a malicious one.
- Anything that lets a *web page* drive the local service (the app starts `dsh web` on loopback and
  the UI is protected by a per-launch token).

Not vulnerabilities by design — already documented in the README:

- The macOS build is **not notarized** and the Windows build is **not code-signed**, so the OS warns
  on first run and the user must explicitly allow it. macOS gates on the `com.apple.quarantine`
  attribute: downloading through a browser sets it and Gatekeeper blocks the launch, while a copy
  transferred through Windows (NTFS cannot store the attribute) runs without any check. The README
  documents the one-time `xattr` command that clears it.
- The app is not sandboxed: by design it writes to `~/.dsh`, to its own config directory, and to the
  autostart location when that setting is on.
- Outdated bundled runtime (Node.js / pnpm) versions are not treated as vulnerabilities in this app;
  report them upstream instead.
- Findings that require an attacker to already control the user's machine or account.

## Disclosure

Please keep the report private until a fix is released. Credit is given in the release notes unless
you prefer to stay anonymous.
