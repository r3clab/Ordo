{ flake, pkgs, ... }:
let
  inherit (flake.lib) image toJSONL;
  unbuildable = image (pkgs.runCommand "unbuildable-image" { } "exit 1");
  resources = {
    cores = 1;
    memory_mib = 128;
  };
  tasks = [
    {
      id = "grep-1";
      image = unbuildable;
      args = [
        "--max-time"
        "1h"
      ];
      resources = {
        cores = 2;
        memory_mib = 40960;
      };
      timeout_seconds = 7200;
      env.MODE = "eval";
      network = "egress";
      cap_add = [ "NET_ADMIN" ];
      services = [ "model" ];
      retry.task = 1;
      tags = [ "l40s" ];
      secrets = [ "MODEL_TOKEN" ];
    }
    {
      id = "installable";
      image = "/srv/evaluation#images.klee";
      inherit resources;
    }
  ];
  workflow = {
    id = "eval";
    tasks = [
      {
        id = "prep";
        image = unbuildable;
        inherit resources;
      }
      {
        id = "run";
        image = unbuildable;
        inherit resources;
        depends_on = [ "prep" ];
        from.corpus = "prep";
      }
    ];
  };
  jsonl = pkgs.writeText "tasks.jsonl" (toJSONL (tasks ++ [ workflow ]));
in
pkgs.runCommand "ordo-lib-check" { nativeBuildInputs = [ pkgs.check-jsonschema ]; } ''
  test "$(wc -l < ${jsonl})" -eq ${toString (builtins.length tasks + 1)}
  head -n ${toString (builtins.length tasks)} ${jsonl} | split -l 1 --additional-suffix=.json - task-
  tail -n 1 ${jsonl} > workflow.json
  check-jsonschema --schemafile ${../../schema/request.schema.json} task-*.json
  check-jsonschema --schemafile ${../../schema/workflow.schema.json} workflow.json
  touch "$out"
''
