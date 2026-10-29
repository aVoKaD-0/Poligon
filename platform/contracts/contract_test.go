package contracts

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

func TestContractFixtures(t *testing.T) {
	for _, kind := range []string{"execution-manifest", "assessment"} {
		b, e := Files.ReadFile("schemas/" + kind + ".json")
		if e != nil {
			t.Fatal(e)
		}
		var document any
		if e = json.Unmarshal(b, &document); e != nil {
			t.Fatal(e)
		}
		compiler := jsonschema.NewCompiler()
		if e = compiler.AddResource(kind+".json", document); e != nil {
			t.Fatal(e)
		}
		schema, e := compiler.Compile(kind + ".json")
		if e != nil {
			t.Fatal(e)
		}
		fixtures, e := filepath.Glob("fixtures/" + kind + "*.json")
		if e != nil || len(fixtures) == 0 {
			t.Fatal("missing golden fixture")
		}
		for _, path := range fixtures {
			b, e = os.ReadFile(path)
			if e != nil {
				t.Fatal(e)
			}
			var fixture any
			if e = json.Unmarshal(b, &fixture); e != nil {
				t.Fatal(e)
			}
			if e = schema.Validate(fixture); e != nil {
				t.Fatalf("%s: %v", path, e)
			}
		}
	}
}
func TestOpenAPIHasUniqueOperations(t *testing.T) {
	b, e := os.ReadFile("openapi/platform.v1.json")
	if e != nil {
		t.Fatal(e)
	}
	var api struct {
		Version string `json:"openapi"`
		Paths   map[string]map[string]struct {
			ID        string         `json:"operationId"`
			Responses map[string]any `json:"responses"`
		} `json:"paths"`
	}
	if e = json.Unmarshal(b, &api); e != nil {
		t.Fatal(e)
	}
	if api.Version != "3.1.0" {
		t.Fatal("OpenAPI 3.1 required")
	}
	ids := map[string]bool{}
	for _, methods := range api.Paths {
		for _, op := range methods {
			if ids[op.ID] || op.ID == "" || op.Responses == nil {
				t.Fatal("invalid operation", op.ID)
			}
			ids[op.ID] = true
		}
	}
	if len(ids) != 56 {
		t.Fatalf("expected complete REST surface, got %d operations", len(ids))
	}
}
