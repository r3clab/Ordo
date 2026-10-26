{ pkgs, perSystem, ... }:
let
  committed = pkgs.lib.fileset.toSource {
    root = ../../schema;
    fileset = ../../schema;
  };
in
pkgs.runCommand "ordo-schema-check" { nativeBuildInputs = [ perSystem.self.default ]; } ''
  ordo schema --out generated
  if ! diff -r ${committed} generated; then
    echo "schema/ is stale: run 'go run ./cmd/ordo schema --out schema' and commit the result" >&2
    exit 1
  fi
  touch "$out"
''
