// Package schema derives the JSON Schemas of job requests and workflows from the server's types.
package schema

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/r3clab/Ordo/internal/image"
	"github.com/r3clab/Ordo/internal/server"
	"github.com/r3clab/Ordo/internal/spec"
)

const draft = "https://json-schema.org/draft/2020-12/schema"

func Names() []string { return []string{"request", "workflow"} }

func file(name string) string { return name + ".schema.json" }

func build(name string) (*jsonschema.Schema, error) {
	opts := &jsonschema.ForOptions{TypeSchemas: map[reflect.Type]*jsonschema.Schema{reflect.TypeFor[image.Source](): imageSchema()}}
	var s *jsonschema.Schema
	var err error
	switch name {
	case "request":
		if s, err = jsonschema.For[server.Request](opts); err == nil {
			refineRequest(s)
			s.Title = "Ordo job request"
			s.Description = "One job; ordo submit reads one per line and requires its id."
		}
	case "workflow":
		if s, err = jsonschema.For[server.Workflow](opts); err == nil {
			refineWorkflow(s)
			s.Title = "Ordo workflow"
		}
	default:
		return nil, fmt.Errorf("unknown schema %q; choose one of %v", name, Names())
	}
	if err != nil {
		return nil, err
	}
	s.Schema = draft
	return s, nil
}

func Generate(name string) ([]byte, error) {
	s, err := build(name)
	if err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	return append(data, '\n'), err
}

func Write(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, name := range Names() {
		data, err := Generate(name)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, file(name)), data, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func imageSchema() *jsonschema.Schema {
	closed := func(properties map[string]*jsonschema.Schema) *jsonschema.Schema {
		return &jsonschema.Schema{Type: "object", Properties: properties, Required: slices.Sorted(maps.Keys(properties)), AdditionalProperties: &jsonschema.Schema{Not: &jsonschema.Schema{}}}
	}
	return &jsonschema.Schema{
		Description: `A local Podman reference, {"nix": INSTALLABLE} for a nix2container image the server builds, or {"archive": FILE, "id": "sha256:IMAGE_ID"} for an OCI archive on the server.`,
		OneOf: []*jsonschema.Schema{
			{Type: "string", MinLength: jsonschema.Ptr(1)},
			closed(map[string]*jsonschema.Schema{"nix": {Type: "string", MaxLength: jsonschema.Ptr(image.MaxInstallableBytes), Pattern: image.InstallablePattern}}),
			closed(map[string]*jsonschema.Schema{"archive": {Type: "string", MinLength: jsonschema.Ptr(1)}, "id": {Type: "string", Pattern: spec.ImageIDPattern}}),
		},
	}
}

func at(s *jsonschema.Schema, path ...string) *jsonschema.Schema {
	for _, step := range path {
		switch step {
		case "[]":
			s = s.Items
		case "{}":
			s = s.AdditionalProperties
		default:
			s = s.Properties[step]
		}
		if s == nil {
			panic(fmt.Sprintf("schema has no %v", path))
		}
	}
	return s
}

func names() *jsonschema.Schema {
	return &jsonschema.Schema{Type: "string", Pattern: spec.NamePattern}
}

func variables() *jsonschema.Schema {
	return &jsonschema.Schema{Type: "string", Pattern: spec.EnvNamePattern, Not: &jsonschema.Schema{Pattern: "^" + spec.ReservedEnvPrefix}}
}

func enum(values []string) []any {
	out := make([]any, len(values))
	for i, v := range values {
		out[i] = v
	}
	return out
}

func refineRequest(s *jsonschema.Schema) {
	at(s, "id").Pattern = spec.NamePattern
	at(s, "inputs").PropertyNames = names()
	resources := at(s, "resources")
	at(resources, "cores").Minimum = jsonschema.Ptr(1.0)
	at(resources, "memory_mib").Minimum = jsonschema.Ptr(1.0)
	at(resources, "gpus").Minimum = jsonschema.Ptr(0.0)
	at(s, "env").PropertyNames = variables()
	at(s, "env", "{}").MaxLength = jsonschema.Ptr(spec.MaxValueBytes)
	at(s, "env", "{}").Pattern = spec.EnvValuePattern
	at(s, "secrets").PropertyNames = variables()
	at(s, "secrets", "{}").MaxLength = jsonschema.Ptr(spec.MaxValueBytes)
	at(s, "secrets", "{}").Pattern = spec.SecretValuePattern
	at(s, "network").Enum = enum(spec.Networks())
	at(s, "cap_add").UniqueItems = true
	at(s, "cap_add", "[]").Enum = enum(spec.Capabilities())
	at(s, "services").UniqueItems = true
	at(s, "services", "[]").Pattern = spec.NamePattern
	at(s, "timeout_seconds").Minimum = jsonschema.Ptr(0.0)
	at(s, "timeout_seconds").Maximum = jsonschema.Ptr(float64(spec.MaxTimeoutSeconds))
	for _, field := range []string{"task", "infrastructure"} {
		at(s, "retry", field).Minimum = jsonschema.Ptr(0.0)
		at(s, "retry", field).Maximum = jsonschema.Ptr(float64(server.MaxRetry))
	}
	at(s, "match", "arch").Enum = enum(spec.Architectures())
	at(s, "match", "min_gpu_memory_mib").Minimum = jsonschema.Ptr(0.0)
	at(s, "match", "tags").UniqueItems = true
	at(s, "match", "tags", "[]").Pattern = spec.NamePattern
}

func refineWorkflow(s *jsonschema.Schema) {
	at(s, "id").Pattern = spec.NamePattern
	at(s, "id").MaxLength = jsonschema.Ptr(server.MaxWorkflowID)
	tasks := at(s, "tasks")
	tasks.Type, tasks.Types = "array", nil
	tasks.MinItems, tasks.MaxItems = jsonschema.Ptr(1), jsonschema.Ptr(server.MaxWorkflowTasks)
	step := at(tasks, "[]")
	refineRequest(step)
	at(step, "id").MaxLength = jsonschema.Ptr(server.MaxStepID)
	step.Required = append(slices.DeleteFunc(step.Required, func(field string) bool { return field == "image" }), "id")
	at(step, "from").PropertyNames = names()
	matrix := at(step, "matrix")
	matrix.PropertyNames = names()
	at(matrix, "{}").MinItems, at(matrix, "{}").MaxItems = jsonschema.Ptr(1), jsonschema.Ptr(server.MaxWorkflowTasks)
}
