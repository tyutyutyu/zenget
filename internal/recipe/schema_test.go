package recipe

import (
	"os"
	"strings"
	"testing"
)

// TestPublicJSONSchemaMirrorsGoContract guards the public recipe schema in
// schemas/recipe-v1.schema.json against drift from the strict Go decoder in
// this package. Go's standard library has no JSON Schema validator, so this
// test pins the load-bearing vocabulary (field names, enum values, constants)
// by direct inspection; semantic parity is enforced by review and by the
// registry CI validating real recipes against the schema.
func TestPublicJSONSchemaMirrorsGoContract(t *testing.T) {
	data, err := os.ReadFile("../../schemas/recipe-v1.schema.json")
	if err != nil {
		t.Fatalf("read public recipe schema: %v", err)
	}
	text := string(data)

	assertContains := func(fragments ...string) {
		t.Helper()
		for _, fragment := range fragments {
			if !strings.Contains(text, fragment) {
				t.Errorf("public recipe schema is missing %q", fragment)
			}
		}
	}

	assertContains(
		`"const": 1`,
		`"schema_version"`,
		`"repository"`,
		`"^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$"`,
		`"supported_systems"`,
		`"asset_selector"`,
		`"archive_binary"`,
		`"target_name"`,
		`"checksum"`,
		`"substring"`,
		`"regex"`,
		`"exact"`,
		`"raw"`,
		`"sha256sum"`,
		`"linux/amd64"`,
		`"linux/arm64"`,
		`"darwin/arm64"`,
		`"windows/amd64"`,
	)

	for _, tag := range []string{"version", "type", "pattern"} {
		assertContains(`"` + tag + `"`)
	}
}
