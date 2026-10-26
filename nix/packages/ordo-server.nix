# `ordo worker start` uploads os.Executable(), which resolves past the wrapper
# script to the static Go binary that wrapProgram keeps as .ordo-wrapped.
{ pkgs, perSystem, ... }:
pkgs.symlinkJoin {
  name = "ordo-server";
  paths = [ perSystem.self.default ];
  nativeBuildInputs = [ pkgs.makeWrapper ];
  postBuild = ''
    wrapProgram $out/bin/ordo --set-default ORDO_SKOPEO ${perSystem.nix2container.skopeo-nix2container}/bin/skopeo
  '';
  meta = perSystem.self.default.meta // {
    description = "Ordo with nix2container's skopeo for {\"nix\": ...} images";
  };
}
