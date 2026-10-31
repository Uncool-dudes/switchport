# switchport spec

Named HTTPS URLs for any local dev server, in any language, from one binary.

Status: v0.1 core implemented on macOS and Linux, with the checks under "Validation before v0.1" still open. Spikes live in `spike/` (see Appendix A).

## Summary

`switchport run api -- go run ./cmd/api` gives the process a free port and a stable URL at `https://api.localhost`. Nobody picks or remembers a port. Other processes find it by name, in a browser, in `curl`, or from Go or Python code.

## Positioning

The direct comparison is [portless](https://github.com/vercel-labs/portless) (0.15.13 as of this draft). It is capable and well known, and it leads with the JavaScript audience. Competing for that audience head-on is not the plan. switchport targets polyglot and backend stacks, which portless serves least well.

| | portless 0.15.13 | switchport target |
| --- | --- | --- |
| Runtime | Node 24+ | Single static binary |
| Framework knowledge | Injects flags per framework | None in core |
| Non-browser name resolution | `/etc/hosts` sync | OS handles `.localhost`; embedded DNS for custom TLDs |
| Wildcard subdomains | Proxy only; hosts file cannot express them | Works for every client, on both |
| HTTP/2 to backend, gRPC | Not found in `proxy.ts` | Supported |
| Start on first request | Not found in source reviewed | Planned |
| Docker Compose | Most-upvoted open issue | Planned |
| Windows | Supported | Deferred |
| Worktrees, tunnels, LAN, MCP | Shipped | Worktrees first, others later |

portless entries marked "not found" come from reading its core source files, not from running it. Recheck them before making any public claim.

## Principles

- **Framework agnostic.** The core never contains a framework name. Behavior comes from a generic contract (see D5).
- **Explicit over discovered.** The core accepts an explicit list of apps. Workspace discovery stays outside it until two real sources justify an interface.
- **Nothing to learn for the common case.** One command, then a URL.
- **Fail with the fix.** Every error names the cause and the command that resolves it.
- **Crash-safe.** `kill -9`, power loss, and concurrent starts never corrupt state.
- **Scriptable.** Every read command has JSON output, for scripts and agents.

## Glossary

- **App**: a process tree started by `switchport`, with a name and a port.
- **Alias**: a static route to a port `switchport` did not start. Persists until removed.
- **Route**: a mapping from a hostname to a backend address. Owned by one app or alias.
- **Registry**: the persistent set of routes.
- **Daemon**: the long-running user process that holds the proxy and the DNS responder.
- **Proxy**: the part of the daemon that terminates TLS and forwards by hostname.
- **TLD**: the suffix for route hostnames, by default `localhost` (see D2).
- **Process tree**: a root process and all its descendants, tracked by parent pid.
- **Worktree prefix**: the branch-derived subdomain added in a linked git worktree.
- **Stale route**: a route whose process is dead.
- **Port contract**: the rules for how an app learns or reveals its port (see D5).

## Goals

- Run any command under a name and reach it at `https://<name>.<tld>`.
- Trusted local HTTPS with no browser warnings, set up once.
- Name resolution for every client on the machine, not only browsers.
- Worktrees run side by side with no collisions and no config.
- Containers and other external services are reachable the same way as apps.
- WebSockets, SSE, and gRPC work through the proxy.
- Single binary with no runtime dependency.

## Non-goals

Deferred:

- Windows support.
- Public tunnels (ngrok, Cloudflare).
- Workspace discovery for Turborepo, pnpm, or Nx. These work today without integration, because each package's `dev` script can call `switchport run` and the monorepo tool runs those scripts as usual.
- Framework presets. If added later they are data, a name mapped to a command template, with no framework logic in code.

Rejected:

- Production use.
- Framework-specific logic in the core.

## Features

Source names the tool a feature comes from. "Unverified" marks behavior known only from a one-line README mention, or a design not yet tested.

### v0.1: core

**Run wrapper** (portless)

- `switchport run <name> -- <command>` assigns a port, registers a route, starts the child, and removes the route on exit.
- The wrapper tracks the whole process tree, not a process group. `pnpm` starts its child in a new group, so a group kill leaves the server running.
- Signals go to every tracked process, children first.
- With a terminal attached, the child gets the foreground (`SysProcAttr.Foreground`). Without it, a child in a new group hangs when it reads stdin.
- If `<name>` is omitted, it comes from the directory name.
- Acceptance: after Ctrl+C on `switchport run api -- go run ./cmd/api`, the route is gone and no descendant process remains.

**Port contract** (D5)

- Set `PORT` in the child env, substitute `{port}` in the command, and observe listening sockets as a fallback.
- Acceptance: Vite, Next.js, Astro, Flask, Django, and uvicorn are each reachable with no framework-specific code in switchport. Passed in the spike.

**Child environment** (portless, extended)

- Inject `PORT`, `SWITCHPORT_URL`, and the CA path through `NODE_EXTRA_CA_CERTS`, `SSL_CERT_FILE`, and `REQUESTS_CA_BUNDLE`.
- Never override a value the user already set.
- Acceptance: a Node or Python child calling another app by its `https://` URL needs no manual TLS setup.

**Service URL lookup** (portless `get`)

- `switchport get <name>` prints a URL, applying the worktree prefix when in a linked worktree. `--no-worktree` skips it.
- A prefixed lookup falls back to the main-checkout route when the worktree has no such app, and the output says so on stderr.
- `--wait <duration>` blocks until the route exists, for startup ordering.

**Aliases** (portless `alias`)

- `switchport alias <name> <port>` adds a static route. `--remove` deletes it. `--force` replaces an existing route.
- Acceptance: an alias survives a reboot and a daemon restart.

**Listing** (portless `list`)

- `switchport list` shows name, URL, backend, PID, type, and worktree. `--json` is stable and versioned.

**Worktree prefix** (portless)

- In a linked worktree the sanitized branch name becomes a subdomain: `fix-ui.api.localhost`.
- Detection compares `git rev-parse --git-dir` with `--git-common-dir`.
- Acceptance: one service runs from two checkouts at once with no flags.

**Local HTTPS** (portless, spike)

- Generate a per-machine CA with X.509 name constraints limited to the configured TLDs.
- Mint leaf certificates on first handshake, held in memory only, so there is no renewal logic and no leaf keys on disk.
- Never mint a wildcard directly under a TLD. macOS rejects `*.test`.
- Acceptance: Safari, Chrome, Firefox, `curl`, Go, Node, and Python connect with no warnings.

**Name resolution** (D2)

- The default TLD needs no setup: macOS 26.2 resolves `*.localhost` to loopback itself.
- For a custom TLD, the daemon answers `*.<tld>` on a loopback UDP port and `/etc/resolver/<tld>` points at it.
- Acceptance: `dscacheutil -q host -a name anything.localhost` returns a loopback address, and a custom TLD resolves from libc, Python, Node, and Go.

**Proxy behavior** (portless, spike)

- HTTP/2 to clients. HTTP/1.1 to backends by default.
- Use HTTP/2 cleartext to the backend when a request has `Content-Type: application/grpc*`. A route-level `--h2c` flag forces it. Automatic selection is unverified.
- WebSockets over HTTP/1.1 Upgrade. Extended CONNECT stays off, the Go default, so browsers use a separate HTTP/1.1 connection.
- Set `X-Forwarded-For`, `-Proto`, `-Host`, and `-Port`. Strip client-supplied values.
- Host handling follows D7. Return 508 on a forwarding loop.
- Hold requests in the handler for a short budget while a backend starts, tied to the request context.
- Return 404 for unknown routes, 502 for unreachable backends, and 421 when `Host` is outside the configured TLDs. Serve HTML when `Accept` includes `text/html`, otherwise JSON.
- The 404 page lists active routes.

**Registry** (portless, improved)

- JSON with a schema version, written to a temp file and renamed, guarded by `flock`. The kernel releases `flock` when a process dies, so no lock expiry logic is needed.
- portless has an open issue about `routes.json` being corrupted when a client is killed. This design must not reproduce it.
- Acceptance: 50 concurrent starts lose no routes. Killing a writer at any point leaves a valid file.

**Stale route cleanup** (portless)

- Each app route records the root PID and known descendants. Reads drop routes whose processes are all dead.
- `switchport prune` stops orphaned process trees and removes their routes.

**Setup and teardown** (puma-dev, portless)

- `switchport setup` does CA trust in one guided step, plus the resolver file only when a custom TLD is used. Each privileged step is shown before it runs.
- `switchport uninstall` removes the CA from the keychain, the resolver file, the service, and the state directory.
- Acceptance: uninstall leaves no trace.

**Diagnostics** (portless `doctor`, localias `debug config`)

- `switchport doctor` is read-only. It checks the daemon, routes, name resolution through the system resolver, CA trust, and port binding, and prints a fix for each failure.
- `dig` ignores `/etc/resolver`, so it must not be used for this check.

**Platforms**

- macOS and Linux. Linux resolver integration uses `resolvectl domain` with systemd-resolved; other Linux setups are documented as manual.

### v0.2: differentiators

**Docker and Compose** (portless top request)

- Containers cannot use port observation: in the spike, a `docker run -p` listener is not in the CLI's process tree. Docker needs this separate path.
- The daemon watches Docker for containers with a `switchport.name` label and creates routes from their published ports. It uses the Docker Engine API over the socket through `net/http`, so no SDK dependency is needed.
- Works with Docker Desktop, OrbStack, and Colima, or `doctor` says why not.
- Containers calling host apps are a separate problem; see Open questions.

**Project config** (localias, portless)

- A committed file declares app names, commands, and ports.
- Lookup order: `--config`, env var, current directory, git root, XDG path. `switchport config` prints the active path.
- Commands in a repo file are never run until approved, like `direnv allow`, because a cloned repo could otherwise execute code on an incoming request.

**Stack start** (hotel, informed)

- `switchport up` starts apps from project config and merges logs. `switchport logs <name>` follows one.
- Scope check first: `process-compose` and `overmind` already do this. Prefer a thin layer.

**Start on first request and idle stop** (hotel, puma-dev; unverified)

- A request to a declared but stopped app starts it, and the proxy holds the request until ready.
- Idle apps stop after a configured period.
- Approved project config only.

**Dashboard** (hotel, portless open request)

- `https://switchport.localhost` shows routes, status, and logs.

### Later

- **Request inspector**: recent requests per route with headers and timing, and replay. Not seen in portless or the other tools reviewed.
- **MCP server** (portless): route lookup, aliases, and diagnostics for agents.
- **Tailscale sharing** (portless): publish through `tailscale serve`, with cleanup on exit.
- **LAN access** (portless, localias): opt-in, with `.local` names through mDNS.
- **Remote targets** (hotel): an alias pointing at a remote host.
- **Raw TCP routes**: names for Postgres and Redis. HTTP aliases cannot carry these protocols. Needs design, possibly per-name loopback addresses.
- **Migration**: import `portless.json` and `.localias.yaml`.
- **Windows**.
- **Discovery sources and presets**, under the rules in Principles.

## Design decisions

### D1. Data plane: Go stdlib (decided, pending review)

- The proxy, TLS, and certificate minting use only the Go standard library.
- The spike passed in 318 lines with no external modules and a 6.7M binary.
- Rejected: Caddy as a separate process, which adds a process to supervise.
- Rejected: `certmagic`, whose value is ACME and renewal, neither of which applies here.
- Fallback: embedded Caddy (`caddy.Load`), with one custom `http.handlers` module, if protocol work grows beyond what is reasonable to own. Its embedding path is undocumented and it adds 168 `go.mod` entries.
- Rust remains possible with `hyper`, `rustls`, and `rcgen`, at roughly 1.5 to 2 times the code. Pingora's macOS support is unverified.

### D2. Name resolution: `.localhost` default, embedded DNS (leaning)

The default TLD is `localhost`, because framework host checks decide this. Tested with Host preserved:

| Server | `.test` | `.localhost` |
| --- | --- | --- |
| Vite 8, Astro 7 | 403 `This host is not allowed` | Works |
| Django 6 | 400 | Works |
| Next.js 16 | Page works, HMR refused by Origin check | Works |
| Flask, uvicorn | Works | Works |

- `.localhost` works with no config in every server tested.
- On macOS 26.2, `*.localhost` resolves to `::1` and `127.0.0.1` from libc `getaddrinfo`, `dscacheutil`, Python, Node, and Go with and without cgo. No hosts entry or resolver file is involved, and `a.b.localhost` works, so wildcards need no setup.
- `/etc/resolver/localhost` is pointless there: `scutil --dns` lists it, but no query reaches the configured server. mDNSResponder answers `.localhost` itself. So the OS, not switchport, owns those answers.
- Embedded DNS is needed only for custom TLDs. With `/etc/resolver/<tld>`, every runtime above resolved through it.
- Older macOS releases and Linux distributions are unverified. `doctor` must detect a failing `.localhost` lookup and suggest a custom TLD.
- Clients may try `::1` first, so the proxy must serve both IPv4 and IPv6 loopback.
- `.test` stays available through `--tld`, using the rewrite in D7.
- Hosts file sync is rejected: root on each write, no wildcards, and conflicts with tools like `hostctl`.
- Limits to document: `dig` does not see these names, containers do not resolve them, and `GODEBUG=netdns=go` bypasses `/etc/resolver`.

### D3. Registry: shared file (leaning)

- The CLI keeps working when the daemon is down, so `list` and `doctor` work when most needed.
- The daemon watches the file.
- Rejected: daemon API only, which makes every command depend on a healthy daemon.

### D4. Daemon privilege and low ports (decided for macOS)

- The daemon always runs as the user, never as root. This rules out the root-owned files reported in a portless issue, and means on-demand start can safely spawn children.
- Spike result on macOS, uid 501: `127.0.0.1:443` and `[::1]:443` are denied, `:443` and `0.0.0.0:443` succeed.
- So the daemon binds a wildcard address and closes non-loopback peers in `Accept`, before TLS.
- The macOS firewall may prompt on this bind. Behavior for an unsigned binary needs testing.
- Linux: `setcap` on the binary, or `net.ipv4.ip_unprivileged_port_start`. With neither, fall back to a high port and print it.

### D5. Port contract (spiked)

1. **`PORT` env**: always set.
2. **`{port}` placeholder**: any occurrence in the command is replaced. Example: `switchport run web -- vite --port {port}`.
3. **Observation**: runs alongside from the start, with no timeout to wait through. It watches the process tree's listening sockets. The assigned port wins at once. Otherwise it waits a short settle window, then picks the first listener that answers HTTP.

The route uses the observed address, because Vite and Astro bound only `[::1]` in the spike.

Spike results:

- Six real servers worked, and five launchers of Vite: `pnpm exec`, `pnpm run`, `npm run`, `npx`, and a shell pipeline.
- Vite, Astro, Flask, Django, and uvicorn ignored `PORT` and were found anyway. Two Vites took 5173 and 5174 and both routed correctly.
- A raw port opened before the HTTP port is skipped, and a raw-only app still routes, marked non-HTTP.
- Macs pay 88 to 112ms per poll through `ps` and `lsof`, against 0.8ms through `/proc` on Linux. Discovery took about 0.6 to 2 seconds. Polling stops once a route is found.

Known limits:

- A hardcoded port already in use makes the child exit. The wrapper reports that exit.
- Two HTTP listeners are ambiguous, and the first wins. `--port` pins one.
- A process that leaves the tree is lost. Astro 7 backgrounds itself when it detects an AI agent, and needs `--ignore-lock` there. This does not apply to a normal terminal.
- A tracked pid can in theory be reused after a process exits.

### D6. Spawning: only with approval

- `switchport run` spawns from the user's shell.
- The daemon spawns only for on-demand start, only from approved config.

### D7. Host and Origin handling (leaning)

- For `.localhost`, preserve `Host` and `Origin`. Every tested server accepts them.
- For other TLDs, send `Host: localhost:<port>` to the backend, and keep the public name in `X-Forwarded-Host`. With this, all six servers tested accepted `.test`.
- Next.js also checks `Origin`, so `Origin` is rewritten too. Its refusal is a raw `Unauthorized`, which the proxy reports as 502.
- This rewrite disables a real protection. Vite and Next.js block foreign hosts and origins to stop malicious pages reaching dev servers. Rewrite `Origin` only when it belongs to a configured TLD, and pass any other value through unchanged so the backend still refuses it. This filter is not implemented in the spike.
- Apps that build absolute URLs from `Host` will see `localhost`, unless they read `X-Forwarded-Host`.

## Security

- **Bind**: the listener is wildcard on macOS, so the loopback filter in `Accept` is a security boundary and needs tests. LAN mode is explicit and opt-in.
- **CA key**: stored with `0600` permissions, per machine, and never synced. Anyone who can read it can issue certificates this machine trusts for the constrained TLDs.
- **Name constraints**: the spike confirmed `security verify-cert` rejects `api.example.com` from a CA constrained to `test`. Firefox and Chrome verifiers still need checking.
- **Trust**: an explicit step. `uninstall` removes it.
- **Browser-reachable dev services**: a stable name and a trusted certificate make local services easier for a malicious page to target. Do not add permissive CORS headers. The Origin rule in D7 is required, not optional.
- **Multi-user machines**: registry and state are owned by the user with `0700` on the directory. The proxy must not route for another user's registry.
- **Project config**: never executed until approved (see Project config).
- **Input**: route names are validated as DNS labels. The DNS responder bounds parsing and never answers outside configured TLDs.
- **No telemetry** by default.

## Open questions

Must answer before finalizing:

- License.
- Should `run` start the daemon automatically, or require `setup` first? portless auto-starts, and the first-run experience is the main comparison.
- Should ports be stable per name across restarts? Stable ports help tools that cache them. Random ports expose hidden dependencies.
- What happens when a name is already in use: fail, replace, or suffix?

Should answer before v0.2:

- How do containers reach host apps by name, given they cannot see `/etc/resolver`? Options include `host.docker.internal` with a `Host` header, or running DNS for the Docker network.
- Remote development over SSH, which is an open portless issue. Does the proxy run on the remote host, or forward from the laptop?
- Signed and notarized macOS binaries, and their effect on firewall prompts.
- Distribution: Homebrew, a Nix flake, `go install`, npm wrapper for JS users.
- How does the tool behave across sleep and wake, and when a VPN changes DNS?
- Does a config format exist that matches portless closely enough to ease migration?

## Validation before v0.1

Done: the Origin filter (Next.js refuses a foreign `Origin` through the proxy, and Vite refuses a WebSocket without its token whatever the origin), `grpc-go` bidirectional streaming through the daemon (25 full-duplex round trips, in `spike/grpccheck`), automatic h2c selection, name constraint on `localhost` under `security verify-cert`, the Linux trust store on Debian, and Node and Python children calling sibling HTTPS URLs.

Open, all needing a person or a machine this work did not have:

- `switchport setup` on macOS, which needs a keychain approval. Then browser trust, including Firefox and whether it needs enterprise roots.
- HMR in real Chrome, Safari, and Firefox. Handshakes and frames are tested, but no browser has run.
- The real :443 bind, including macOS firewall behavior with an unsigned binary. All tests used high ports.
- `switchport service install` on macOS and Linux. The definitions are written but not loaded by any test.
- `.localhost` on older macOS. On Linux, glibc did not resolve `x.localhost` in a Debian bookworm container without systemd-resolved, while `curl` handles it itself. Check systemd-resolved hosts, and consider a default that works without it.
- `/etc/resolver/<tld>` through `setup --tld`. The mechanism was proven by hand, but the command needs root.

## Appendix A: Spike results

Code is in `spike/`. Go 1.27.3. macOS arm64 with `-race`, Linux arm64 in Docker. Framework fixtures in `.fw/` were installed by hand and are not committed.

Framework versions: Vite 8.3.5, Next.js 16.4.3, Astro 7.4.4, Django 6.1.2.

### Proxy (macOS)

| Area | Result |
| --- | --- |
| Forwarding | HTTP/2 client to HTTP/1.1 backend, spoofed `X-Forwarded-For` removed |
| WebSocket | 101 and two-way traffic. Vite, Astro, and Next.js HMR handshakes returned real frames |
| Extended CONNECT | Not advertised by default (`http2xconnect=1` enables it) |
| SSE | Streams unbuffered |
| Request hold | POST with body held 737ms until the backend started |
| Client disconnect | Hold stops with the request context |
| h2c backend | `TE: trailers` and trailers pass through, simulated gRPC only |
| Certificates | 50 concurrent handshakes, 1 mint, 53ms total |
| Foreign SNI | Handshake refused |
| Route churn | 200 requests during concurrent add and delete, no race |
| DNS | Stdlib UDP responder served a custom TLD through `/etc/resolver` to libc, `dscacheutil`, Python, Node, and Go, with and without cgo |
| `.localhost` | Resolved by macOS 26.2 itself; `/etc/resolver/localhost` was ignored, with no queries reaching the responder |
| Loopback filter | Non-loopback peer dropped, loopback accepted |
| macOS verifier | `*.test` rejected, `*.myapp.test` accepted, constrained CA rejects foreign names |

### Observation (macOS and Linux)

15 scenarios on each platform: `PORT` honored, fixed port on `127.0.0.1`, `0.0.0.0`, and `::1`, random port, `go run` grandchild, shell pipeline, 2-second delay, raw port before HTTP, two HTTP ports, immediate exit, never listening, non-HTTP only, port in use, and daemonized process.

### Findings that changed the design

- A saved pid can be reused by an unrelated process, so `prune` compares the recorded start time before signaling.
- `dig` and the system resolver differ on `.localhost`, and Debian containers without systemd-resolved do not resolve it at all.
- macOS 26.2 resolves `*.localhost` itself, so the default path needs neither DNS nor root.
- `pnpm` runs its child in a new process group, so group-based lookup and kill both fail. Track the process tree.
- Vite, Astro, Django, and Next.js refuse `.test` in some way. This changed the default TLD and added D7.
- Vite and Astro bind only `[::1]`, so an assumed `127.0.0.1` fails.
- A child in a new process group hangs reading the terminal unless given the foreground.
- Retrying in `Transport.DialContext` leaks after client disconnect, because the dial context is detached from the request. Hold in the handler instead.
- macOS allows an unprivileged :443 bind on wildcard addresses only.
- `SetXForwarded` does not set `X-Forwarded-Port`.
- A foreign SNI fails at the handshake, so 421 applies only when `Host` differs from SNI.

## Appendix B: Implementation map

| Component | Go |
| --- | --- |
| Reverse proxy | `httputil.ReverseProxy` with `Rewrite` |
| HTTP/2 | `net/http`, `http.Protocols` for h2c |
| TLS and SNI | `tls.Config.GetCertificate` |
| Certificates | `crypto/x509` |
| Trust | `security add-trusted-cert`, platform equivalents on Linux |
| Child process | `os/exec`, `syscall.SysProcAttr{Setpgid: true, Foreground: true}` |
| Process tree | `ps` on macOS, `/proc` on Linux |
| Listeners | `lsof` on macOS, `/proc/net/tcp{,6}` on Linux |
| Liveness | `syscall.Kill(pid, 0)` |
| DNS | Hand-written A-record responder over `net.PacketConn` |
| Registry | `encoding/json`, `syscall.Flock`, `os.Rename` |
| Docker | Engine API over the socket with `net/http` |
| Packaging | `buildGoModule` |

## Appendix C: Sources

- portless 0.15.13: README, https://portless.sh/changelog, open issues, and `packages/portless/src` (`proxy.ts`, `certs.ts`, `routes.ts`, `hosts.ts`, `cli.ts`) at https://github.com/vercel-labs/portless
- localias: README, https://github.com/peterldowns/localias
- hotel: README, https://github.com/typicode/hotel (last pushed 2023-10)
- puma-dev: README, https://github.com/puma/puma-dev (last pushed 2024-05)
- Caddy: source at https://github.com/caddyserver/caddy, v2.11.7
- Go 1.27.3 source, and Go 1.25.3 docs through Context7
