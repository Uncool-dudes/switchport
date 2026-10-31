# Roadmap

Where switchport is going. These are plans, not commitments, and order may
change. Design detail lives in [SPEC.md](SPEC.md), and each item below is tracked
in [GitHub Issues](https://github.com/Uncool-dudes/switchport/issues).

## Now: harden v0.1

v0.1 is released from `main` with a Nix flake install. It runs on one Mac with
a real app over :443, and its tests pass on macOS and Debian.

Known issues:

- `.localhost` does not resolve on Linux without systemd-resolved, so non-browser
  clients fail there. Find a default that works without it.
- Absolute `Location` redirects to a backend's own address leave switchport,
  because the proxy does not rewrite them.
- `--h2c` fails against a backend that serves TLS, because the flag skips
  detection.
- Next.js Server Actions probably fail on TLDs other than `.localhost`: `Origin`
  is rewritten to `localhost:<port>` while `X-Forwarded-Host` keeps the public
  name. Not yet reproduced.
- A plain server that never answers the TLS probe holds its first request for up
  to 3 seconds.
- TLS backends that require client certificates are untested.
- Restarting the daemon drops open WebSockets, including hot reload.

Still to validate:

- Hot reload in Chrome, Safari, and Firefox, and Firefox certificate trust.
- macOS firewall behavior on :443 with an unsigned binary.
- `switchport service install` on macOS and Linux.
- `switchport setup --tld`.
- NixOS, older macOS, and Linux with systemd-resolved.
- Behavior across sleep, wake, and VPN changes.

Also to do:

- Unit tests for `internal/trust`.
- Docs for moving an app onto a switchport URL: cookies, OAuth redirect URIs,
  CORS allowlists, and dev server settings that open the old URL.
- Docs for fixed-port dev servers across worktrees.

## Next: v0.2

- **Docker and Compose:** the daemon watches for containers with a
  `switchport.name` label and routes their published ports.
- **Project config:** a committed file of names, commands, and ports. Commands
  from a repo never run until approved, like `direnv allow`.
- **`switchport up` and `switchport logs`:** start a stack and merge its logs,
  as a thin layer, since `process-compose` and `overmind` already do this well.
- **Start on first request, stop when idle:** for approved config only.
- **Dashboard:** routes, status, and logs at `https://switchport.localhost`.

To settle before v0.2:

- How containers reach host apps by name.
- Remote development over SSH: whether the proxy runs on the remote host or
  forwards from the laptop.
- Signed and notarized macOS binaries.
- Install options beyond Nix: Homebrew, `go install`, and an npm wrapper.
- Whether ports stay stable per name across restarts.
- Whether the config format can match portless closely enough to ease migration.

## Later

- Request inspector with replay.
- MCP server for route lookup, aliases, and diagnostics.
- Tailscale sharing.
- LAN access, opt-in, with `.local` names through mDNS.
- Aliases that point at remote hosts.
- Raw TCP routes, such as names for Postgres and Redis.
- Import from `portless.json` and `.localias.yaml`.
- Windows.
- Framework presets, as data only: a name mapped to a command template.

## Not planned

- Production use.
- Framework-specific logic in the core.
