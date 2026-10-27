{ inputs, ... }:
let
  inherit (inputs.nixpkgs) lib;
  schema = name: builtins.fromJSON (builtins.readFile ../../schema/${name}.schema.json);
  fields = (schema "request").properties;
  workflowFields = (schema "workflow").properties;
  resources = fields.resources.properties;
  namePattern = fields.id.pattern;
  envPattern = fields.env.propertyNames.pattern;
  reservedEnvPattern = fields.env.propertyNames.not.pattern;
  networks = fields.network.enum;
  capabilities = fields.cap_add.items.enum;
  maxTimeoutSeconds = fields.timeout_seconds.maximum;
  maxRetry = fields.retry.properties.task.maximum;
  maxWorkflowID = workflowFields.id.maxLength;
  maxStepID = workflowFields.tasks.items.properties.id.maxLength;
  maxTasks = workflowFields.tasks.maxItems;

  require = cond: message: cond || throw "ordo: ${message}";
  validName = s: builtins.isString s && builtins.match namePattern s != null;
  atLeast = low: n: builtins.isInt n && n >= low;
  between =
    low: high: n:
    atLeast low n && n <= high;
  optional = check: value: value == null || check value;
  distinct = values: lib.unique values == values;
  distinctNames = names: distinct names && builtins.all validName names;
  compact = lib.filterAttrs (_: value: value != null);

  # Discarding the output dependency keeps only the .drv in a written task file's
  # closure, so writing tasks does not build their images; the server builds them
  # when the job is submitted.
  imageRef =
    image:
    if lib.isDerivation image then
      { nix = "${builtins.unsafeDiscardOutputDependency image.drvPath}^*"; }
    else
      image;

  request =
    {
      id,
      image ? null,
      archive ? null,
      args ? null,
      inputs ? null,
      cores,
      memoryMiB,
      gpus ? null,
      timeoutSeconds ? null,
      env ? null,
      network ? null,
      capAdd ? null,
      services ? null,
      retry ? null,
      tags ? null,
    }:
    assert require (validName id) "task id ${builtins.toJSON id} must match ${namePattern}";
    assert require (
      atLeast resources.cores.minimum cores && atLeast resources.memory_mib.minimum memoryMiB
    ) "${id}: cores and memoryMiB must be positive integers";
    assert require (optional (atLeast resources.gpus.minimum) gpus)
      "${id}: gpus must be a non-negative integer";
    assert require (optional (between 0 maxTimeoutSeconds) timeoutSeconds)
      "${id}: timeoutSeconds must be 0..${toString maxTimeoutSeconds}";
    assert require (optional (builtins.all builtins.isString) args) "${id}: args must be strings";
    assert require (optional (
      s: builtins.all validName (builtins.attrNames s)
    ) inputs) "${id}: input names must match ${namePattern}";
    assert require (optional (
      vars:
      builtins.all (
        name:
        builtins.match envPattern name != null && builtins.match "${reservedEnvPattern}.*" name == null
      ) (builtins.attrNames vars)
    ) env) "${id}: env names must match ${envPattern} and not ${reservedEnvPattern}";
    assert require (optional (
      n: builtins.elem n networks
    ) network) "${id}: network must be one of ${toString networks}";
    assert require (optional (
      caps: distinct caps && builtins.all (c: builtins.elem c capabilities) caps
    ) capAdd) "${id}: capAdd allows only distinct ${toString capabilities}";
    assert require (optional distinctNames services) "${id}: services must be distinct names";
    assert require (optional distinctNames tags) "${id}: tags must be distinct names";
    assert require (optional (
      r: builtins.all (n: between 0 maxRetry n) (builtins.attrValues r)
    ) retry) "${id}: retry counts must be 0..${toString maxRetry}";
    compact {
      inherit
        id
        archive
        args
        inputs
        env
        network
        services
        tags
        ;
      image = if image == null then null else imageRef image;
      resources = compact {
        inherit cores gpus;
        memory_mib = memoryMiB;
      };
      timeout_seconds = timeoutSeconds;
      cap_add = capAdd;
      retry =
        if retry == null then
          null
        else
          compact {
            task = retry.task or null;
            infrastructure = retry.infrastructure or null;
          };
    };
in
{
  inherit imageRef;

  mkTask =
    args:
    assert require (args.image or null != null) "${args.id or "task"}: mkTask needs an image";
    request args;

  mkStep =
    {
      dependsOn ? null,
      from ? null,
      matrix ? null,
      ...
    }@args:
    assert require (
      builtins.stringLength (args.id or "") <= maxStepID
    ) "step id ${args.id or ""} exceeds ${toString maxStepID} characters";
    request (
      removeAttrs args [
        "dependsOn"
        "from"
        "matrix"
      ]
    )
    // compact {
      inherit from matrix;
      depends_on = dependsOn;
    };

  mkWorkflow =
    {
      id,
      image ? null,
      archive ? null,
      tasks,
    }:
    assert require (
      validName id && builtins.stringLength id <= maxWorkflowID
    ) "workflow id ${id} must match ${namePattern} within ${toString maxWorkflowID} characters";
    assert require (
      builtins.length tasks >= 1 && builtins.length tasks <= maxTasks
    ) "workflow ${id} needs 1..${toString maxTasks} tasks";
    assert require (distinct (map (task: task.id) tasks)) "workflow ${id} repeats a task id";
    compact {
      inherit id archive tasks;
      image = if image == null then null else imageRef image;
    };

  toJSONL = tasks: lib.concatMapStrings (task: builtins.toJSON task + "\n") tasks;
}
