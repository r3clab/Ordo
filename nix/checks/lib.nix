{ flake, pkgs, ... }:
let
  inherit (flake.lib)
    mkTask
    mkStep
    mkWorkflow
    toJSONL
    ;
  unbuildable = pkgs.runCommand "unbuildable-image" { } "exit 1";
  tasks = [
    (mkTask {
      id = "grep-1";
      image = unbuildable;
      args = [
        "--max-time"
        "1h"
      ];
      cores = 2;
      memoryMiB = 40960;
      timeoutSeconds = 7200;
      env.MODE = "eval";
      network = "egress";
      capAdd = [ "NET_ADMIN" ];
      services = [ "model" ];
      retry.task = 1;
      tags = [ "l40s" ];
      secrets = [ "MODEL_TOKEN" ];
    })
    (mkTask {
      id = "installable";
      image.nix = "/srv/evaluation#images.klee";
      cores = 1;
      memoryMiB = 128;
    })
  ];
  workflow = mkWorkflow {
    id = "eval";
    tasks = [
      (mkStep {
        id = "prep";
        image = unbuildable;
        cores = 1;
        memoryMiB = 128;
      })
      (mkStep {
        id = "run";
        image = unbuildable;
        cores = 1;
        memoryMiB = 128;
        dependsOn = [ "prep" ];
        from.corpus = "prep";
      })
    ];
  };
  jsonl = pkgs.writeText "tasks.jsonl" (toJSONL tasks);
  valid = {
    id = "ok";
    image.nix = "/srv/evaluation#images.klee";
    cores = 1;
    memoryMiB = 128;
  };
  step = valid // {
    id = "s";
  };
  invalid = {
    id = mkTask (valid // { id = "bad id"; });
    image = mkTask (removeAttrs valid [ "image" ]);
    podmanImage = mkTask (valid // { image = "localhost/klee:1"; });
    archiveImage = mkTask (
      valid
      // {
        image = {
          archive = "/image.tar";
          id = "sha256:${pkgs.lib.strings.replicate 64 "a"}";
        };
      }
    );
    extraImageKey = mkTask (
      valid
      // {
        image = {
          nix = "/src#x";
          archive = "/image.tar";
        };
      }
    );
    emptyInstallable = mkTask (valid // { image.nix = ""; });
    optionInstallable = mkTask (valid // { image.nix = "--option"; });
    multilineInstallable = mkTask (valid // { image.nix = "a\nb"; });
    longInstallable = mkTask (valid // { image.nix = "/${pkgs.lib.strings.replicate 4096 "x"}"; });
    cores = mkTask (valid // { cores = 0; });
    gpus = mkTask (valid // { gpus = -1; });
    timeout = mkTask (valid // { timeoutSeconds = 365 * 24 * 60 * 60 + 1; });
    env = mkTask (valid // { env."1BAD" = "x"; });
    reservedEnv = mkTask (valid // { env.NVIDIA_VISIBLE_DEVICES = "all"; });
    secret = mkTask (valid // { secrets = [ "1BAD" ]; });
    reservedSecret = mkTask (valid // { secrets = [ "NVIDIA_VISIBLE_DEVICES" ]; });
    repeatedSecret = mkTask (
      valid
      // {
        secrets = [
          "TOKEN"
          "TOKEN"
        ];
      }
    );
    secretInEnv = mkTask (
      valid
      // {
        env.TOKEN = "x";
        secrets = [ "TOKEN" ];
      }
    );
    network = mkTask (valid // { network = "host"; });
    capability = mkTask (valid // { capAdd = [ "SYS_ADMIN" ]; });
    repeatedCapability = mkTask (
      valid
      // {
        capAdd = [
          "NET_ADMIN"
          "NET_ADMIN"
        ];
      }
    );
    service = mkTask (valid // { services = [ "bad name" ]; });
    tag = mkTask (valid // { tags = [ "bad tag" ]; });
    retry = mkTask (valid // { retry.task = 11; });
    stepId = mkStep (step // { id = pkgs.lib.strings.replicate 33 "s"; });
    stepImage = mkStep (removeAttrs step [ "image" ]);
    workflowId = mkWorkflow {
      id = pkgs.lib.strings.replicate 25 "w";
      tasks = [ (mkStep step) ];
    };
    noTasks = mkWorkflow {
      id = "w";
      tasks = [ ];
    };
    repeatedTask = mkWorkflow {
      id = "w";
      tasks = [
        (mkStep step)
        (mkStep step)
      ];
    };
  };
  accepted = builtins.attrNames (
    pkgs.lib.filterAttrs (_: value: (builtins.tryEval (builtins.deepSeq value value)).success) invalid
  );
in
assert pkgs.lib.assertMsg (accepted == [ ]) "ordo lib accepted invalid ${toString accepted}";
pkgs.runCommand "ordo-lib-check" { nativeBuildInputs = [ pkgs.check-jsonschema ]; } ''
  test "$(wc -l < ${jsonl})" -eq ${toString (builtins.length tasks)}
  split -l 1 --additional-suffix=.json ${jsonl} task-
  check-jsonschema --schemafile ${../../schema/request.schema.json} task-*.json
  check-jsonschema --schemafile ${../../schema/workflow.schema.json} ${pkgs.writeText "workflow.json" (builtins.toJSON workflow)}
  touch "$out"
''
