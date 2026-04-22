package models

import "testing"

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
