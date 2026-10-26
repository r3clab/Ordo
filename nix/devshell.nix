{
  inputs,
  perSystem,
  pkgs,
  ...
}:
perSystem.devshell.mkShell {
  imports = [
    "${inputs.devshell}/extra/git/hooks.nix"
    "${inputs.devshell}/extra/language/c.nix"
  ];

  language.c.compiler = pkgs.stdenv.cc;

  packages = [
    pkgs.go
    pkgs.gopls
    pkgs.golangci-lint
    pkgs.nodejs
    perSystem.self.formatter
  ];

  env = [
    {
      name = "GOTOOLCHAIN";
      value = "local";
    }
  ];

  devshell.motd = "";

  git.hooks.enable = true;
  git.hooks.pre-commit.text = ''
    set -e
    ${perSystem.self.formatter}/bin/treefmt --fail-on-change
    ${pkgs.golangci-lint}/bin/golangci-lint run ./...
  '';
}
