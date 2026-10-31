{
  lib,
  buildGoModule,
  version,
}:
buildGoModule {
  pname = "devroute";
  inherit version;

  src = lib.cleanSource ../.;

  vendorHash = null;

  subPackages = [ "cmd/devroute" ];

  ldflags = [
    "-s"
    "-w"
    "-X main.version=${version}"
  ];

  meta = {
    description = "Named HTTPS URLs for any local dev server";
    mainProgram = "devroute";
    platforms = lib.platforms.unix;
  };
}
