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
    mainProgram = "switchport";
    platforms = lib.platforms.unix;
  };
}
