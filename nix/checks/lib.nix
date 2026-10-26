{ flake, pkgs, ... }:
let
  inherit (flake.lib) mkTask mkStep mkWorkflow;
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
      match.arch = "amd64";
      match.tags = [ "l40s" ];
    })
    (mkTask {
      id = "local";
      image = "localhost/klee:1";
      cores = 1;
      memoryMiB = 128;
    })
  ];
  workflow = mkWorkflow {
    id = "eval";
    image = unbuildable;
    tasks = [
      (mkStep {
        id = "prep";
        cores = 1;
        memoryMiB = 128;
      })
      (mkStep {
        id = "run";
        cores = 1;
        memoryMiB = 128;
        dependsOn = [ "prep" ];
        from.corpus = "prep";
        matrix.program = [
          "grep"
          "sed"
        ];
      })
    ];
  };
  requests = map (task: pkgs.writeText "${task.id}.json" (builtins.toJSON task)) tasks;
  valid = {
    id = "ok";
    image = "localhost/klee:1";
    cores = 1;
    memoryMiB = 128;
  };
  step = {
    id = "s";
    cores = 1;
    memoryMiB = 128;
  };
  invalid = {
    id = mkTask (valid // { id = "bad id"; });
    image = mkTask (removeAttrs valid [ "image" ]);
    cores = mkTask (valid // { cores = 0; });
    gpus = mkTask (valid // { gpus = -1; });
    timeout = mkTask (valid // { timeoutSeconds = 365 * 24 * 60 * 60 + 1; });
    env = mkTask (valid // { env."1BAD" = "x"; });
    reservedEnv = mkTask (valid // { env.NVIDIA_VISIBLE_DEVICES = "all"; });
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
    tag = mkTask (valid // { match.tags = [ "bad tag" ]; });
    retry = mkTask (valid // { retry.task = 11; });
    stepId = mkStep (step // { id = pkgs.lib.strings.replicate 33 "s"; });
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
  check-jsonschema --schemafile ${../../schema/request.schema.json} ${toString requests}
  check-jsonschema --schemafile ${../../schema/workflow.schema.json} ${pkgs.writeText "workflow.json" (builtins.toJSON workflow)}
  touch "$out"
''
