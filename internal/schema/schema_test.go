package schema

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/r3clab/Ordo/internal/image"
	"github.com/r3clab/Ordo/internal/server"
)

func validator(t *testing.T, name string) func(string) error {
	t.Helper()
	s, err := build(name)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := s.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	return func(document string) error {
		var instance any
		if err := json.Unmarshal([]byte(document), &instance); err != nil {
			t.Fatal(err)
		}
		return resolved.Validate(instance)
	}
}

func TestRequestSchemaMatchesValidation(t *testing.T) {
	validate := validator(t, "request")
	base := `"id":"grep-1","resources":{"cores":2,"memory_mib":4096}`
	for _, valid := range []string{
		`{` + base + `,"image":{"nix":"/nix/store/x.drv^*"},"args":["--max-time","1h"],"env":{"MODE":"eval"},"network":"egress","cap_add":["NET_ADMIN"],"timeout_seconds":3600,"retry":{"task":2,"infrastructure":0},"tags":["l40s"]}`,
		`{` + base + `,"image":{"nix":"/src#images.grep"},"secrets":["TOKEN"],"inputs":{"corpus":"/data"}}`,
	} {
		if err := validate(valid); err != nil {
			t.Errorf("rejected %s: %v", valid, err)
		}
		var r server.Request
		if err := json.Unmarshal([]byte(valid), &r); err != nil || r.Validate() != nil {
			t.Errorf("the schema accepts what Go rejects: %s: %v %v", valid, err, r.Validate())
		}
	}
	for _, invalid := range []string{
		`{"resources":{"cores":1,"memory_mib":1}}`,
		`{` + base + `,"image":{"nix":"/src#x"},"color":"red"}`,
		`{` + base + `,"image":{"nix":"x","tag":"y"}}`,
		`{` + base + `,"image":{"nix":"--option"}}`,
		`{` + base + `,"image":{}}`,
		`{` + base + `,"image":"localhost/klee:1"}`,
		`{` + base + `,"image":{"archive":"/image.tar","id":"sha256:` + strings.Repeat("a", 64) + `"}}`,
		`{` + base + `,"image":{"nix":"/src#x"},"network":"host"}`,
		`{` + base + `,"image":{"nix":"/src#x"},"cap_add":["SYS_ADMIN"]}`,
		`{` + base + `,"image":{"nix":"/src#x"},"cap_add":["NET_ADMIN","NET_ADMIN"]}`,
		`{` + base + `,"image":{"nix":"/src#x"},"env":{"1BAD":"x"}}`,
		`{` + base + `,"image":{"nix":"/src#x"},"env":{"NVIDIA_VISIBLE_DEVICES":"all"}}`,
		`{` + base + `,"image":{"nix":"/src#x"},"secrets":{"TOKEN":"x"}}`,
		`{` + base + `,"image":{"nix":"/src#x"},"secrets":["TOKEN","TOKEN"]}`,
		`{` + base + `,"image":{"nix":"/src#x"},"secrets":["1BAD"]}`,
		`{` + base + `,"image":{"nix":"/src#x"},"secrets":["NVIDIA_X"]}`,
		`{` + base + `,"image":{"nix":"/src#x"},"retry":{"task":11}}`,
		`{` + base + `,"image":{"nix":"/src#x"},"timeout_seconds":31536001}`,
		`{"id":"bad id","image":{"nix":"/src#x"},"resources":{"cores":1,"memory_mib":1}}`,
		`{"id":"a","image":{"nix":"/src#x"},"resources":{"cores":0,"memory_mib":1}}`,
		`{` + base + `,"image":{"nix":"/src#x"},"tags":["l40s","l40s"]}`,
		`{` + base + `,"image":{"nix":"/src#x"},"tags":["bad tag"]}`,
		`{` + base + `,"image":{"nix":"/src#a\nb"}}`,
		`{` + base + `,"image":{"nix":"/` + strings.Repeat("x", image.MaxInstallableBytes) + `"}}`,
		`{` + base + `,"image":{"nix":"/src#x"},"env":{"MODE":"a\u0000b"}}`,
	} {
		if err := validate(invalid); err == nil {
			t.Errorf("accepted %s", invalid)
		}
		var r server.Request
		if err := server.DecodeJSON(strings.NewReader(invalid), &r); err == nil && r.Validate() == nil {
			t.Errorf("Go accepts what the schema rejects: %s", invalid)
		}
	}
}

func TestWorkflowSchemaRequiresImagesAndBoundsTasks(t *testing.T) {
	validate := validator(t, "workflow")
	step := `{"id":"run","image":{"nix":"/src#images.grep"},"resources":{"cores":1,"memory_mib":128},"depends_on":["prep"],"from":{"corpus":"prep"}}`
	if err := validate(`{"id":"eval","tasks":[` + step + `]}`); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{
		`{"id":"eval","tasks":[]}`,
		`{"id":"` + strings.Repeat("w", 25) + `","tasks":[` + step + `]}`,
		`{"id":"eval","tasks":[{"image":{"nix":"/src#x"},"resources":{"cores":1,"memory_mib":128}}]}`,
		`{"id":"eval","tasks":[{"id":"run","resources":{"cores":1,"memory_mib":128}}]}`,
		`{"id":"eval","image":{"nix":"/src#x"},"tasks":[` + step + `]}`,
		`{"id":"eval","tasks":[{"id":"run","image":{"nix":"/src#x"},"resources":{"cores":1,"memory_mib":128},"matrix":{"program":["grep"]}}]}`,
	} {
		if err := validate(invalid); err == nil {
			t.Errorf("accepted %s", invalid)
		}
	}
}

func TestGenerateIsStable(t *testing.T) {
	for _, name := range Names() {
		first, err := Generate(name)
		if err != nil {
			t.Fatal(err)
		}
		second, _ := Generate(name)
		if string(first) != string(second) || !json.Valid(first) {
			t.Fatalf("%s schema is not a stable JSON document", name)
		}
	}
	if _, err := Generate("job"); err == nil {
		t.Fatal("generated an unknown schema")
	}
}
