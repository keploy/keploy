package models

import (
	"testing"

	yamlLib "gopkg.in/yaml.v3"
)

// TestMockNamePostgresV3Constants pins the exact string values of the
// PostgresV3 Mock.Name constants. Mock.Name participates in hit-count
// indexing, dedup, and by-name lookups across MockManager; drifting
// these strings silently splits the pool. The integrations-repo
// recorder is expected to migrate to these constants in a follow-up
// commit so the string literal and the constant remain identical.
func TestMockNamePostgresV3Constants(t *testing.T) {
	if MockNamePostgresV3Query != "PostgresV3Query" {
		t.Fatalf("MockNamePostgresV3Query: want %q, got %q", "PostgresV3Query", MockNamePostgresV3Query)
	}
	if MockNamePostgresV3Session != "PostgresV3Session" {
		t.Fatalf("MockNamePostgresV3Session: want %q, got %q", "PostgresV3Session", MockNamePostgresV3Session)
	}
}

// TestPostgresV3QuerySpec_BackwardCompat_ScopeSilentlyIgnored asserts
// that old YAML recordings containing the retired `scope` key still
// unmarshal cleanly into PostgresV3QuerySpec after the field was
// removed from the struct. gopkg.in/yaml.v3 is non-strict by default,
// so unknown keys are silently skipped; this test pins that behaviour
// so a future switch to Decoder.KnownFields(true) would fail loudly.
//
// Derived from a real fkppl debug-bundle fragment
// (/tmp/auto-replay-issue/...mocks.yaml) which still stamps
// `scope: session` on 72 per-test Postgres mocks.
func TestPostgresV3QuerySpec_BackwardCompat_ScopeSilentlyIgnored(t *testing.T) {
	// Minimal old-shape YAML: carries the retired `scope` key
	// alongside lifetime / sqlAstHash / invocationId. Fields we do NOT
	// care about for this assertion (response / bindValues / ...) are
	// omitted — zero-values round-trip fine.
	const oldYAML = `
class: select
lifetime: perTest
scope: session
sqlAstHash: sha256:a66bf255653587dd476dfd0524632134bc0623f293ee93d2bf2666447cd2b4d1
sqlNormalized: "select 1"
invocationId: inv-1
`

	var spec PostgresV3QuerySpec
	if err := yamlLib.Unmarshal([]byte(oldYAML), &spec); err != nil {
		t.Fatalf("yaml.Unmarshal returned error on old recording with scope: field — "+
			"backward-compat broke. err=%v", err)
	}
	if spec.Class != "select" {
		t.Fatalf("Class: want %q, got %q", "select", spec.Class)
	}
	if spec.Lifetime != "perTest" {
		t.Fatalf("Lifetime: want %q, got %q", "perTest", spec.Lifetime)
	}
	if spec.SQLAstHash == "" {
		t.Fatalf("SQLAstHash: want non-empty, got empty — known keys stopped parsing")
	}
	if spec.InvocationID != "inv-1" {
		t.Fatalf("InvocationID: want %q, got %q", "inv-1", spec.InvocationID)
	}
}
