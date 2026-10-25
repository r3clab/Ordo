{ pkgs, ... }:
pkgs.buildGoModule (finalAttrs: {
  pname = "ordo";
  version = "0.1.0-dev";
  src = pkgs.lib.fileset.toSource {
    root = ../.;
    fileset = pkgs.lib.fileset.unions [
      ../go.mod
      (pkgs.lib.fileset.maybeMissing ../go.sum)
      (pkgs.lib.fileset.fileFilter (file: file.hasExt "go") ../.)
    ];
  };
  vendorHash = null;
  subPackages = [ "cmd/ordo" ];

  checkPhase = ''
    runHook preCheck
    go test -race ./...
    runHook postCheck
  '';

  passthru.tests.lint = finalAttrs.finalPackage.overrideAttrs (old: {
    pname = "ordo-lint";
    nativeBuildInputs = old.nativeBuildInputs ++ [ pkgs.golangci-lint ];
    buildPhase = ''
      runHook preBuild
      export GOLANGCI_LINT_CACHE="$TMPDIR/golangci-lint"
      golangci-lint run ./...
      runHook postBuild
    '';
    doCheck = false;
    installPhase = ''
      touch "$out"
    '';
  });

  meta = {
    description = "Reproducible evaluations across SSH workers";
    homepage = "https://github.com/r3clab/Ordo";
    license = pkgs.lib.licenses.mit;
    mainProgram = "ordo";
  };
})
