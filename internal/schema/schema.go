// Package schema derives the JSON Schema of job requests from the server's types.
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

func Names() []string { return []string{"request"} }

func file(name string) string { return name + ".schema.json" }

func build(name string) (*jsonschema.Schema, error) {
	if name != "request" {
		return nil, fmt.Errorf("unknown schema %q; choose one of %v", name, Names())
	}
	s, err := jsonschema.For[server.Request](nil)
	if err != nil {
		return nil, err
	}
	refineRequest(s)
	s.Title = "Ordo job request"
	s.Description = "One job, a line of ordo submit, which writes its results into out. A job of one step may give that step's fields at the top level instead of in steps."
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

// The server resolves relative paths against nothing, so ordo submit makes them absolute first.
const absolute = "^/"

func refineRequest(s *jsonschema.Schema) {
	at(s, "out").Pattern = absolute
	at(s, "inputs").PropertyNames = names()
	at(s, "inputs", "{}").Pattern = absolute
	steps := at(s, "steps")
	steps.Type, steps.Types = "array", nil
	steps.MinItems, steps.MaxItems = jsonschema.Ptr(1), jsonschema.Ptr(server.MaxSteps)
	refineStep(at(steps, "[]"))
	refineStep(s)
	s.Required = []string{"out"}
	s.OneOf = []*jsonschema.Schema{{Required: []string{"steps"}}, {Required: []string{"image", "resources"}}}
	for _, field := range []string{"task", "infrastructure"} {
		at(s, "retry", field).Minimum = jsonschema.Ptr(0.0)
		at(s, "retry", field).Maximum = jsonschema.Ptr(float64(server.MaxRetry))
	}
}

func refineStep(s *jsonschema.Schema) {
	at(s, "name").Pattern = spec.NamePattern
	at(s, "image").MaxLength = jsonschema.Ptr(image.MaxInstallableBytes)
	at(s, "image").Pattern = image.InstallablePattern
	at(s, "entrypoint").MinItems = jsonschema.Ptr(1)
	at(s, "entrypoint").PrefixItems = []*jsonschema.Schema{{Type: "string", MinLength: jsonschema.Ptr(1)}}
	at(s, "workdir").Pattern = absolute
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
}
