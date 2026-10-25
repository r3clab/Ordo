{
  description = "Small nix2container image for Ordo integration tests";

  inputs = {
    nixpkgs.url = "github:nixos/nixpkgs/nixpkgs-unstable";
    nix2container.url = "github:nlewo/nix2container";
    nix2container.inputs.nixpkgs.follows = "nixpkgs";
  };

  outputs =
    { nixpkgs, nix2container, ... }:
    {
      packages = nixpkgs.lib.genAttrs [ "x86_64-linux" "aarch64-linux" ] (
        system:
        let
          pkgs = nixpkgs.legacyPackages.${system};
        in
        {
          image = nix2container.packages.${system}.nix2container.buildImage {
            name = "localhost/ordo-smoke";
            config = {
              Entrypoint = [ "${pkgs.bash}/bin/bash" ];
              Cmd = [
                "-c"
                "printf 'ordo stdout\\n'; printf 'ordo stderr\\n' >&2"
              ];
              Env = [ "PATH=${pkgs.coreutils}/bin" ];
            };
          };
        }
      );
    };
}
