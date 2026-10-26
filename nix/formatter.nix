{
  flake,
  inputs,
  pkgs,
  ...
}:
let
  rumdlConfig = (pkgs.formats.toml { }).generate "rumdl.toml" {
    MD013 = {
      reflow = true;
      reflow-mode = "sentence-per-line";
    };
  };
  treefmt = inputs.treefmt-nix.lib.evalModule pkgs {
    projectRootFile = "flake.nix";
    settings.global.excludes = [
      "vendor/**"
      "internal/server/web/**"
      "schema/**"
      "ui/node_modules/**"
      "ui/package-lock.json"
    ];

    programs.nixfmt.enable = true;
    programs.nixfmt.package = pkgs.nixfmt-rs;
    programs.deadnix.enable = true;
    programs.statix.enable = true;
    programs.gofmt.enable = true;
    programs.gofmt.package = pkgs.go;
    programs.rumdl-format.enable = true;
    programs.typos.enable = true;
    programs.jsonfmt.enable = true;
    programs.yamlfmt.enable = true;

    settings.formatter.rumdl-format.options = [
      "--config"
      "${rumdlConfig}"
    ];
  };
  formatter = treefmt.config.build.wrapper;
in
formatter
// {
  passthru = formatter.passthru // {
    tests.check = treefmt.config.build.check flake;
  };
}
