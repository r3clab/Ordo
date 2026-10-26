{ pkgs, ... }:
let
  inherit (pkgs) lib;
  src = lib.fileset.toSource {
    root = ../../ui;
    fileset = lib.fileset.difference ../../ui (lib.fileset.maybeMissing ../../ui/node_modules);
  };
  committed = lib.fileset.toSource {
    root = ../../internal/server/web;
    fileset = ../../internal/server/web;
  };
in
pkgs.buildNpmPackage {
  pname = "ordo-ui-check";
  version = "0";
  inherit src;
  npmDepsHash = "sha256-UEplGqDaVaTxOp7tK43udM2UK/Nm2yY99JQ1+1rC1bE=";

  buildPhase = ''
    runHook preBuild
    npm run check
    npm run build -- --outDir "$PWD/dist"
    runHook postBuild
  '';

  installPhase = ''
    if ! diff -r ${committed} dist; then
      echo "internal/server/web is stale: run 'npm run build' in ui/ and commit the result" >&2
      exit 1
    fi
    touch "$out"
  '';
}
