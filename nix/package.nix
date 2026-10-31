{
  lib,
  buildGoModule,
  version,
}:
buildGoModule {
  pname = "switchport";
  inherit version;

  src = lib.cleanSource ../.;

  vendorHash = null;

  subPackages = [ "cmd/switchport" ];

  ldflags = [
    "-s"
    "-w"
    "-X main.version=${version}"
  ];

  meta = {
    description = "Named HTTPS URLs for any local dev server";
    homepage = "https://github.com/Uncool-dudes/switchport";
    license = lib.licenses.mit;
    mainProgram = "switchport";
    platforms = lib.platforms.linux ++ [ "aarch64-darwin" ];
  };
}
