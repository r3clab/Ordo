{ inputs, ... }:
{
  # Discarding the output dependency keeps only the .drv in a written task file's
  # closure, so writing tasks does not build their images; the server builds them
  # when the job is submitted.
  image = drv: "${builtins.unsafeDiscardOutputDependency drv.drvPath}^*";

  toJSONL = inputs.nixpkgs.lib.concatMapStrings (line: builtins.toJSON line + "\n");
}
