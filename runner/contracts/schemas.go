package contracts

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

//go:embed schemas/*.json
var files embed.FS

type Schemas struct {
	Agent, Scenario, Manifest *jsonschema.Schema
}

func Load() (Schemas, error) {
	var result Schemas
	compiler := jsonschema.NewCompiler()
	compiler.AssertFormat()
	for _, item := range []struct {
		name   string
		target **jsonschema.Schema
	}{{"agent", &result.Agent}, {"scenario", &result.Scenario}, {"execution-manifest", &result.Manifest}} {
		name := "schemas/" + item.name + ".json"
		b, err := files.ReadFile(name)
		if err != nil {
			return Schemas{}, err
		}
		var schema any
		if err := json.Unmarshal(b, &schema); err != nil {
			return Schemas{}, err
		}
		if err := compiler.AddResource(name, schema); err != nil {
			return Schemas{}, err
		}
		compiled, err := compiler.Compile(name)
		if err != nil {
			return Schemas{}, err
		}
		*item.target = compiled
	}
	return result, nil
}

func Validate(schema *jsonschema.Schema, raw []byte) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return fmt.Errorf("decode schema input: %w", err)
	}
	return schema.Validate(v)
}
