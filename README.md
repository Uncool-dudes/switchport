# switchport

Named HTTPS URLs for any local dev server, in any language, from one binary.

```sh
switchport setup
switchport run api -- go run ./cmd/api
# https://api.localhost
```

No port to pick or remember, and no framework support needed. switchport sets
`PORT`, then watches which port your command's process tree actually opens, so
servers that ignore `PORT` work too.

## Install

```sh
nix profile install github:Uncool-dudes/switchport
```

or `go install github.com/Uncool-dudes/switchport/cmd/switchport@latest`. The
flake also provides `overlays.default` and a dev shell.

## Usage

```sh
switchport run [name] [--port n] [--force] [--h2c] -- <command>
switchport list [--json]
switchport get <name> [--no-worktree] [--wait duration]
switchport alias <name> <port> [--force] [--h2c]
switchport alias --remove <name>
switchport prune
switchport doctor
switchport uninstall
```

- The name defaults to the directory name.
- `{port}` in a command is replaced, for tools that take a port as a flag:
  `switchport run web -- vite --port {port}`.
- A command with several ports routes to the first that answers HTTP. Use
  `--port` to choose.
- In a linked git worktree the branch becomes a subdomain, such as
  `fix-ui.api.localhost`, so checkouts never collide.
- Commands receive `SWITCHPORT_URL`, plus `NODE_EXTRA_CA_CERTS`, `SSL_CERT_FILE`,
  and `REQUESTS_CA_BUNDLE` unless already set, so they can call each other over
  HTTPS.
- gRPC works with no flag. `--h2c` forces HTTP/2 cleartext to other backends.
- `prune` stops commands left running after their wrapper was killed.

`API_URL=$(switchport get api --wait 10s)` finds a service from a script.

## Custom TLDs

`.localhost` needs no DNS setup on macOS 26 and in browsers. Run `switchport
doctor` to check other systems.

```sh
switchport setup --tld test
```

This answers `*.test` from the daemon and adds `/etc/resolver/test`, which needs
root once. Run it before the CA is created, since the CA is limited to its TLDs.

`.test` is not the default because several dev servers reject unknown hosts. On
custom TLDs switchport presents requests to the backend as `localhost`, and
keeps the public name in `X-Forwarded-Host`. An `Origin` from outside your own
TLDs passes through unchanged, so dev servers still refuse it.

## Daemon

`run` starts a user-level daemon if needed. To start it at login instead:

```sh
switchport service install
```

With home-manager on macOS, use a launchd agent:

```nix
launchd.agents.switchport = {
  enable = true;
  config = {
    ProgramArguments = [ "${pkgs.switchport}/bin/switchport" "daemon" "run" ];
    KeepAlive = true;
    RunAtLoad = true;
  };
};
```

The daemon never runs as root. macOS allows an unprivileged :443 only on a
wildcard address, so it binds one and closes non-loopback peers before TLS. On
Linux, give the binary `cap_net_bind_service`, or it falls back to port 8443.

Docker containers do not run under `switchport run`. Publish a port and use
`switchport alias db 5432`.

State lives in `~/.switchport`, or `SWITCHPORT_STATE_DIR`.

See [SPEC.md](SPEC.md) for the design and `spike/` for the experiments behind it.
