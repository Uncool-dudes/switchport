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

Nix flakes are the only supported install for now, on Apple silicon macOS and
Linux. Nixpkgs no longer builds for Intel Macs.

```sh
nix profile add github:Uncool-dudes/switchport
switchport setup
```

Older Nix calls this `nix profile install`. To try it first, use `nix run
github:Uncool-dudes/switchport -- doctor`.

In a flake configuration, such as home-manager or NixOS:

```nix
{
  inputs.switchport.url = "github:Uncool-dudes/switchport";

  # then, in a module
  nixpkgs.overlays = [ inputs.switchport.overlays.default ];
  home.packages = [ pkgs.switchport ];
}
```

The package needs Go 1.26, so don't make its `nixpkgs` input follow an older
channel.

After upgrading, run `switchport daemon stop` so the next command starts the
new daemon.

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
  `switchport run web -- vite --port {port}`. A dev server configured with a fixed port and
  `strictPort` ignores `PORT`; pass the allocated port through the package script instead:
  `switchport run web -- pnpm dev --port {port}`.
- A command with several ports routes to the first that answers HTTP. Use
  `--port` to choose.
- In a linked git worktree the branch becomes a subdomain, such as
  `fix-ui.api.localhost`, so checkouts never collide.
- Commands receive `SWITCHPORT_URL`, plus `NODE_EXTRA_CA_CERTS`, `SSL_CERT_FILE`,
  and `REQUESTS_CA_BUNDLE` unless already set, so they can call each other over
  HTTPS.
- gRPC works with no flag. `--h2c` forces HTTP/2 cleartext to other backends.
- Backends that serve HTTPS themselves are detected, and their certificates are
  not verified, since backends are always on loopback.
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

With home-manager on macOS, use a launchd agent, with the overlay above:

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
wildcard address, so it binds one and closes non-loopback peers before TLS.

On Linux it falls back to port 8443, because capabilities cannot be set on a
binary in the Nix store. To use 443, allow it system-wide:

```sh
sudo sysctl net.ipv4.ip_unprivileged_port_start=443
```

On NixOS, set `boot.kernel.sysctl."net.ipv4.ip_unprivileged_port_start" = 443`.

## NixOS

`switchport setup` cannot change the system trust store on NixOS. Copy the
public certificate next to your configuration and declare it:

```nix
security.pki.certificateFiles = [ ./switchport-ca.pem ];
```

```sh
cp ~/.switchport/ca.pem switchport-ca.pem
```

The certificate is safe to commit. The key stays in `~/.switchport`.

Docker containers do not run under `switchport run`. Publish a port and use
`switchport alias db 5432`.

State lives in `~/.switchport`, or `SWITCHPORT_STATE_DIR`.

See [ROADMAP.md](ROADMAP.md) for what is next, [SPEC.md](SPEC.md) for the design,
and `spike/` for the experiments behind it.

## Status

Early. Used on macOS 26, and tested on Debian. NixOS and other Linux
distributions are untested.

## License

MIT
