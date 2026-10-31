{
  description = "switchport: named HTTPS URLs for any local dev server";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixpkgs-unstable";

  outputs =
    { self, nixpkgs }:
    let
      systems = [
        "aarch64-darwin"
        "aarch64-linux"
        "x86_64-linux"
      ];
      forAllSystems = f: nixpkgs.lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});
    in
    {
      packages = forAllSystems (pkgs: rec {
        switchport = pkgs.callPackage ./nix/package.nix {
          version = self.shortRev or self.dirtyShortRev or "dev";
        };
        default = switchport;
      });

      overlays.default = final: _prev: {
        switchport = final.callPackage ./nix/package.nix {
          version = self.shortRev or self.dirtyShortRev or "dev";
        };
      };

      devShells = forAllSystems (pkgs: {
        default = pkgs.mkShell {
          packages = [
            pkgs.go
            pkgs.gopls
            pkgs.golangci-lint
          ];
        };
      });

      formatter = forAllSystems (pkgs: pkgs.nixfmt);
    };
}
