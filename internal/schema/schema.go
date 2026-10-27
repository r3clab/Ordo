// Package schema derives the JSON Schemas of job requests and workflows from the server's types.
package schema

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/r3clab/Ordo/internal/image"
	"github.com/r3clab/Ordo/internal/server"
	"github.com/r3clab/Ordo/internal/spec"
)

const draft = "https://json-schema.org/draft/2020-12/schema"

func Names() []string { return []string{"request", "workflow"} }

func file(name string) string { return name + ".schema.json" }

func build(name string) (*jsonschema.Schema, error) {
	var s *jsonschema.Schema
	var err error
	switch name {
	case "request":
		if s, err = jsonschema.For[server.Request](nil); err == nil {
			refineRequest(s)
			s.Title = "Ordo job request"
			s.Description = "One job; each line ordo submit reads is a job request, which needs an id, or a workflow."
		}
	case "workflow":
		if s, err = jsonschema.For[server.Workflow](nil); err == nil {
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
	at(s, "image").MaxLength = jsonschema.Ptr(image.MaxInstallableBytes)
	at(s, "image").Pattern = image.InstallablePattern
	at(s, "inputs").PropertyNames = names()
	resources := at(s, "resources")
	at(resources, "cores").Minimum = jsonschema.Ptr(1.0)
	at(resources, "memory_mib").Minimum = jsonschema.Ptr(1.0)
	at(resources, "gpus").Minimum = jsonschema.Ptr(0.0)
	at(s, "env").PropertyNames = variables()
	at(s, "env", "{}").MaxLength = jsonschema.Ptr(spec.MaxValueBytes)
	at(s, "env", "{}").Pattern = spec.EnvValuePattern
	at(s, "secrets").UniqueItems = true
	at(s, "secrets").Items = variables()
	at(s, "network").Enum = enum(spec.Networks())
	at(s, "cap_add").UniqueItems = true
	at(s, "cap_add", "[]").Enum = enum(spec.Capabilities())
	for _, field := range []string{"services", "tags"} {
		at(s, field).UniqueItems = true
		at(s, field, "[]").Pattern = spec.NamePattern
	}
	at(s, "timeout_seconds").Minimum = jsonschema.Ptr(0.0)
	at(s, "timeout_seconds").Maximum = jsonschema.Ptr(float64(spec.MaxTimeoutSeconds))
	for _, field := range []string{"task", "infrastructure"} {
		at(s, "retry", field).Minimum = jsonschema.Ptr(0.0)
		at(s, "retry", field).Maximum = jsonschema.Ptr(float64(server.MaxRetry))
	}
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
	step.Required = append(step.Required, "id")
	at(step, "from").PropertyNames = names()
}
