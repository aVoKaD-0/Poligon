package contracts

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestManifestFixture(t *testing.T) {
	schemas, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("fixtures/execution-manifest.v1.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := Validate(schemas.Manifest, raw); err != nil {
		t.Fatal(err)
	}
}

func TestPinnedPlatformFiles(t *testing.T) {
	root := os.Getenv("POLIGON_PLATFORM_TEST_DIR")
	if root == "" {
		t.Skip("set POLIGON_PLATFORM_TEST_DIR to the pinned platform/ directory")
	}
	for local, upstream := range map[string]string{
		"controlv1/control.pb.go":                "gen/controlv1/control.pb.go",
		"controlv1/control_grpc.pb.go":           "gen/controlv1/control_grpc.pb.go",
		"proto/poligon/control/v1/control.proto": "contracts/proto/poligon/control/v1/control.proto",
		"schemas/agent.json":                     "contracts/schemas/agent.json",
		"schemas/scenario.json":                  "contracts/schemas/scenario.json",
		"schemas/artifact-ref.json":              "contracts/schemas/artifact-ref.json",
		"schemas/execution-manifest.json":        "contracts/schemas/execution-manifest.json",
		"fixtures/execution-manifest.v1.json":    "contracts/fixtures/execution-manifest.v1.json",
	} {
		expected, err := os.ReadFile(filepath.Join(root, upstream))
		if err != nil {
			t.Fatal(err)
		}
		actual, err := os.ReadFile(local)
		if err != nil {
			t.Fatal(err)
		}
		norm := func(b []byte) []byte { return bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n")) }
		if !bytes.Equal(norm(expected), norm(actual)) {
			t.Errorf("%s differs from Platform", local)
		}
	}
}
