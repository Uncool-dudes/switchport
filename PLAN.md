# switchport v0.1 plan

## Primary goal

A developer runs `switchport run api -- <any server command>` and reaches that server at a trusted `https://api.localhost`, in any language, with no port chosen or remembered, and nothing left behind when it stops.

"Done" means the v0.1 acceptance criteria in `SPEC.md` pass on macOS and Linux. Scope is `SPEC.md` "v0.1: core" and nothing else.

## Working rules

- Each phase runs in a fresh session. Read this file, the named `SPEC.md` sections, and the named spike code first.
- `spike/` is reference code. Copy from it, then reshape to the package boundaries here. Never import it. Leave it unchanged.
- No external Go modules in the main module. The `grpc-go` check in Phase 9 lives in `spike/`, which has its own `go.mod`.
- One commit per coherent change, per `commits.md`. Do not push unless asked.
- Build with `go build -o /dev/null ./...` or `go vet ./...`. A plain `go build ./...` drops a binary in the repo root.
- Stop and ask if a phase needs a decision this plan or the spec does not cover.

## Decisions taken for this plan

The spec lists these as open. They are defaults so work can start, and each is cheap to change before release.

- **Daemon start**: `run` starts the daemon if it is not running.
- **Name conflict**: fail with the owner's PID and the fix. `--force` replaces.
- **Ports**: a free ephemeral port on each start, not stable per name.
- **State directory**: `SWITCHPORT_STATE_DIR`, otherwise `~/.switchport`. Avoids the macOS `Application Support` path, which contains a space.
- **License**: still open. Blocks a public release only.

## Package boundaries

Each package hides one decision likely to change. Interfaces stay small, with tests at the package surface.

| Package | Hides | Surface |
| --- | --- | --- |
| `internal/route` | Name rules, route shapes | `Name`, `App`, `Alias`, `Route` |
| `internal/proc` | `ps`, `lsof`, `/proc`, selection policy | `Tracker`, `Discover`, `Listener` |
| `internal/registry` | File format, locking, liveness | `Open`, `Add`, `Remove`, `List` |
| `internal/proxy` | Forwarding, errors, hold, Host rules | `New(lookup, opts)` returning `http.Handler` |
| `internal/ca` | Key storage, minting | `Load`, `GetCertificate`, `Root` |
| `internal/trust` | OS trust stores | `Install`, `Remove`, `Installed` |
| `internal/daemon` | Listeners, reload, lifecycle | `Run`, `Ensure` |
| `internal/cli` | Commands and flags | `Main(ctx, args) int` |

`route.Name` is a value object with a validating constructor. `App` and `Alias` are distinct types, so an alias cannot carry a PID. `proxy` receives a lookup function and never imports `registry`.

## Phase 0: Documentation discovery (complete)

> **How this serves the primary goal:** every later phase builds on verified APIs, so no session loses time on calls that do not exist.

Checked against Go 1.27.3 source and `api/go1.N.txt`. The build target is Go 1.26, and Go 1.26.10 source itself was not read.

**Allowed APIs**

- Process: `syscall.SysProcAttr{Setpgid, Foreground, Ctty}`, `exec.Cmd.Cancel`, `exec.Cmd.WaitDelay`, `signal.NotifyContext`.
- Files: `syscall.Flock` with `LOCK_EX`, `LOCK_NB`, `LOCK_UN`; `os.CreateTemp`, `os.Rename`.
- Proxy: `httputil.ReverseProxy{Rewrite, ErrorHandler, ModifyResponse}`, `ProxyRequest.SetXForwarded`.
- HTTP: `http.Protocols` with `SetHTTP1`, `SetHTTP2`, `SetUnencryptedHTTP2`; `Server.Protocols`, `Transport.Protocols`.
- TLS: `tls.Config.GetCertificate`, `ClientHelloInfo.ServerName`, `tls.Certificate.Leaf`.
- x509: `CreateCertificate`, `PermittedDNSDomains`, `PermittedDNSDomainsCritical`.
- Logs and tests: `slog.NewTextHandler`, `slog.NewJSONHandler`, `testing/synctest` (`Test`, `Wait`), `T.Context`, `T.Chdir`.

**Do not use**

- Added in Go 1.27: `synctest.Sleep`, `encoding/json/v2`, `Server.DisableClientPriority`, `Server.MaxHeaderValueCount`, `tls.ConnectionState.LocalCertificate`.
- Deprecated: `ReverseProxy.Director`.
- Does not exist: `(*os.Root).CreateTemp`, `os.Flock`, a public `Protocols.SetHTTP3`, any atomic-write helper in `os`.

**Traps**

- `Foreground` needs `Ctty` as a descriptor in the parent process, which differs from `Setctty`. It implies `Setpgid`.
- A `Transport` with both HTTP/1 and `UnencryptedHTTP2` does not use h2c for `http://`. h2c needs its own `Transport` (`src/net/http/transport.go:2053`).
- Go documents no atomicity promise for `os.Rename`, and I found no stated guarantee that `flock` is released on process death. Prove both with tests rather than assuming them.
- `encoding/json` v1 runs on v2 internals under the 1.27 toolchain. Keep golden tests for `--json` and the registry, and run on Go 1.26 as well.
- `tls.Certificate.Leaf` can be nil on hand-built certificates, so set it when minting.

## Phase 1: Process tracking and port discovery

> **How this serves the primary goal:** "any server command" depends on finding the port without framework knowledge, and "nothing left behind" depends on tracking the full process tree.

**Implement**

- `internal/route`: copy no code. Write `Name` per `SPEC.md` "Security" (DNS label rules, dotted names allowed), with `App`, `Alias`, `Route`.
- `internal/proc`: copy `Tracker`, `Listener`, `Discover`, `choose`, `probeHTTP` from `spike/observe.go:19-200`.
- Copy platform files from `spike/observe_darwin.go` and `spike/observe_linux.go`.
- Port the scenarios from `spike/observe_test.go:69-226`, using a Go fixture server so tests do not need Python.
- Make Discover's assigned-port preference and settle window parameters, as in `DiscoverOpts`.

**References**: `SPEC.md` D5; Appendix A "Findings".

**Verify**

- `go test -race ./internal/proc` passes on macOS.
- The same suite passes on Linux in `golang:1.26-bookworm`.
- A test confirms a child in a new process group is still found. Use a fixture that calls `Setpgid`, reproducing `pnpm`.

**Anti-pattern guards**

- No process-group lookup or group kill anywhere.
- No framework names in code. `rg -i 'vite|next|astro|django' internal cmd` returns nothing.
- `lsof` must keep `-a`, or it returns every listener on the machine.

## Phase 2: `run` wrapper

> **How this serves the primary goal:** this is the command the user types, and it owns correct startup, terminal behavior, and shutdown.

**Implement**

- `internal/cli` with stdlib `flag`. Copy the `run` shape from `cmd/loadout/main.go` in the `loadout` repo (`signal.NotifyContext`, exit code return).
- `switchport run [name] [--port n] -- <command>`. Name defaults to the directory name.
- Set `PORT`, substitute `{port}`, start the child, run `Discover`, and print the discovered address. The URL comes in Phase 5.
- Give the child the terminal foreground when stdin is a TTY, and restore it on exit. See `spike/ttyrun/main.go`.
- Forward signals to the tree. Escalate to `SIGKILL` after a grace period. Exit with the child's code.

**References**: `SPEC.md` "Run wrapper", "Port contract".

**Verify**

- Ctrl+C under a pty stops the server, with no descendant left. Adapt the pty harness used for `ttyrun`.
- A child that reads stdin works under a pty.
- A child's exit code reaches the shell.
- Exercise real `pnpm exec vite` by hand, or through `spike/.fw` fixtures.

**Anti-pattern guards**

- No signal sent to a negative PID.
- `{port}` substitution is text only, with no flag parsing or tool detection.
- No broad process matching such as `pkill -f`. Signal only tracked PIDs.

## Phase 3: Proxy handler

> **How this serves the primary goal:** this is what makes the URL reach the server, including hot reload, streaming, and gRPC.

**Implement**

- `internal/proxy`: copy `rewrite`, `waitReady`, `writeError`, `backendError`, and `ServeHTTP` from `spike/proxy.go:200-297`.
- Take a lookup function instead of the spike's route map.
- Implement D7, including the part the spike lacks: rewrite `Origin` only when it belongs to a configured TLD. Pass any other `Origin` through unchanged.
- Preserve `Host` and `Origin` on `.localhost`. Rewrite on other TLDs.
- Keep one `Transport` for HTTP/1 and one for h2c. Choose h2c on `Content-Type: application/grpc*` or a route flag. Automatic selection is new, so test it.
- Add the route list to the 404 page.

**References**: `SPEC.md` "Proxy behavior", D7; `spike/proxy_test.go:79-420`.

**Verify**

- Port the forwarding, WebSocket, SSE, hold, disconnect, h2c, and status tests.
- New test: a foreign `Origin` reaches the backend unchanged, and Vite or Next.js refuses it.
- New test: gRPC content type selects h2c with no route flag.

**Anti-pattern guards**

- Hold in the handler on `r.Context()`, never in `Transport.DialContext`.
- Do not assume `127.0.0.1`. Dial the observed address.
- Do not add permissive CORS headers.
- Do not enable `http2xconnect`.

## Phase 4: Registry and read commands

> **How this serves the primary goal:** stable names, lookup between services, and clean recovery after a crash all rest on this state.

**Implement**

- `internal/registry`: JSON with a schema version, written through `os.CreateTemp` in the same directory then `os.Rename`, guarded by `syscall.Flock` on a separate lock file. See `src/cmd/go/internal/lockedfile/internal/filelock/filelock_unix.go`.
- Directory mode `0700`, files `0600`.
- Drop stale routes on read, using root PID and known descendants.
- Commands: `list [--json]`, `get [--no-worktree] [--wait d]`, `alias [--remove] [--force]`, `prune`.
- Worktree prefix: compare `git rev-parse --git-dir` with `--git-common-dir`, then sanitize the branch into a DNS label.
- Wire `run` to register on start and remove on exit.

**References**: `SPEC.md` "Registry", "Stale route cleanup", "Service URL lookup", "Aliases", "Worktree prefix", D3.

**Verify**

- 50 concurrent processes add routes and none are lost.
- A writer killed with `SIGKILL` mid-write leaves a valid file and a free lock. This is the proof the Phase 0 trap asks for.
- After `kill -9` on a wrapper, the next CLI call clears the route.
- An alias survives a restart.
- Golden test for `list --json`.
- `prune` under a real linked worktree.

**Anti-pattern guards**

- Never write the registry in place.
- No lock expiry logic. `flock` must be proven sufficient by the test above.
- Do not read the file outside the lock when writing.

## Phase 5: Daemon, plain HTTP end to end

> **How this serves the primary goal:** this joins the pieces, so a name resolves to a running server for the first time.

**Implement**

- `internal/daemon`: serve `proxy` from registry reads. Watch by polling the file's mtime, since stdlib has no file watcher.
- `Ensure` starts the daemon detached if it is not running, guarded by a lock so two `run` calls start one daemon.
- Bind per `SPEC.md` D4: wildcard on macOS with `loopbackOnly` from `spike/proxy.go:221-234`, loopback on Linux.
- Add `switchport daemon status|stop`, with a log file in the state directory.
- `run` now prints the URL.
- HTTP on a high port for this phase.

**References**: `SPEC.md` D3, D4.

**Verify**

- `switchport run` then `curl http://api.localhost:<port>` works from a fresh state.
- A new route is live within a second.
- Daemon restart keeps routes. Two simultaneous `run` calls start one daemon.
- Loopback filter drops a LAN peer, per `spike/proxy_test.go:518`.

**Anti-pattern guards**

- The daemon never runs as root.
- The daemon spawns no apps in v0.1. See D6.
- The CLI's `list` and `doctor` must work when the daemon is down.

## Phase 6: Local HTTPS

> **How this serves the primary goal:** this delivers the "trusted `https://`" half of the goal, on the default port.

**Implement**

- `internal/ca`: copy `newCA`, `leaf`, and `getCertificate` from `spike/proxy.go:38-198`.
- Persist the CA key with `0600`. Leaf certificates stay in memory. Set `Leaf` when minting.
- Name-constrain to configured TLDs.
- `internal/trust`: macOS `security add-trusted-cert` and removal. Linux per distro, with an explicit error when unsupported. Copy behavior from portless `certs.ts`, and read that file first.
- `switchport trust`. Serve :443 with :80 redirecting.

**References**: `SPEC.md` "Local HTTPS", D4, "Security".

**Verify**

- Port the mint-once, foreign SNI, and verifier tests.
- `security verify-cert` accepts `a.localhost` from a CA constrained to `localhost`. The spike only checked `test`, so this is a real question. If it fails, drop the constraint for `localhost` and record why in `SPEC.md`.
- After `switchport trust`, `curl https://api.localhost` works with no flags.

**Anti-pattern guards**

- Never mint a wildcard directly under a TLD.
- No leaf keys on disk.
- `trust` always runs as an explicit step, never as a side effect of `run`.
- Do not bind `127.0.0.1:443` on macOS. It fails unprivileged.

## Phase 7: Child environment

> **How this serves the primary goal:** a Node or Python service must reach another service's HTTPS URL without manual TLS setup.

**Implement**

- Inject `SWITCHPORT_URL`, `NODE_EXTRA_CA_CERTS`, `SSL_CERT_FILE`, and `REQUESTS_CA_BUNDLE`.
- Never override a value the user set.
- `SSL_CERT_FILE` replaces default roots for some tools, so point it at a bundle of system roots plus the CA, and test it.

**References**: `SPEC.md` "Child environment".

**Verify**

- A Node and a Python child each `GET` a sibling's `https://` URL with no extra config.
- A preset `NODE_EXTRA_CA_CERTS` survives.
- A child can still reach a public HTTPS site through `SSL_CERT_FILE`.

**Anti-pattern guards**

- No per-framework env logic.

## Phase 8: Setup, teardown, diagnostics, custom TLDs

> **How this serves the primary goal:** this makes first run and removal safe, and turns failures into a fix the user can run.

**Implement**

- `switchport setup`: guided CA trust, showing each privileged step first.
- `switchport uninstall`: remove CA, service, resolver files, and state.
- `switchport service install`: launchd on macOS, systemd user unit on Linux. Add a home-manager example to the README.
- `switchport doctor`: read-only checks with a fix per failure. Test names through the system resolver, not `dig`. If `.localhost` lookups fail, suggest a custom TLD.
- Custom TLDs: copy `answerDNS` and `serveDNS` from `spike/proxy.go:299-360`, served only when configured. `setup --tld test` writes `/etc/resolver/test`, or uses `resolvectl domain` on Linux.

**References**: `SPEC.md` "Setup and teardown", "Diagnostics", "Name resolution", D2.

**Verify**

- `uninstall` leaves no keychain entry, resolver file, service, or state directory.
- `doctor` reports each seeded failure: daemon down, CA untrusted, port taken.
- `.test` works end to end, with Vite and Django through the D7 rewrite.
- DNS parser test with truncated and malformed packets.

**Anti-pattern guards**

- `doctor` changes nothing.
- No hidden `sudo`. Print each privileged step first.
- Never write `/etc/hosts`.
- Never create `/etc/resolver/localhost`. macOS ignores it.

## Phase 9: Verification

> **How this serves the primary goal:** this proves the goal for real users, including the parts that automated tests cannot reach.

**Automated**

- `go test -race ./...` on macOS, and on Linux in Docker, on Go 1.26.
- `go vet ./...`, `gofmt -l .`, `golangci-lint run`, `nix build`.
- `rg -i 'vite|next|astro|django|flask' internal cmd` returns nothing.
- `go list -deps ./cmd/switchport` shows no non-stdlib modules.
- No em or en dashes in `SPEC.md`, `README.md`, or `PLAN.md`.

**Remaining `SPEC.md` validation**

- HMR in real Chrome, Safari, and Firefox with Vite and Next.js. Edit a file and watch the page update.
- `grpc-go` through the proxy, including bidirectional streaming, in `spike/`.
- Firefox trust, including whether it needs enterprise roots.
- Backends served over TLS, or an explicit decision to defer.
- `.localhost` resolution on older macOS and on Linux with and without systemd-resolved.
- macOS firewall behavior on the wildcard :443 bind with an unsigned binary.

**Acceptance walkthrough**

- Fresh machine state, then `setup`, then each v0.1 acceptance criterion in `SPEC.md`, then `uninstall`.
- Update `SPEC.md` with every result, and README with real usage.

## Out of scope for v0.1

Docker and Compose, project config, `up`, on-demand start, dashboard, MCP, Tailscale, LAN mode, tunnels, Windows, workspace discovery, presets, raw TCP routes, request inspector.
