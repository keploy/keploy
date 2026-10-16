package models

import "testing"

// A PostgreSQL query mock is keyed by its statement's hash, or its text when
// it has none; any other mock, or PostgreSQL mock of another type, records no
// statement.
func TestStatementKey(t *testing.T) {
	q := func(hash, sql string) *Mock {
		return &Mock{Kind: PostgresV3, Spec: MockSpec{PostgresV3: &PostgresV3Spec{
			Type: PostgresV3TypeQuery, Query: &PostgresV3QuerySpec{SQLAstHash: hash, SQLNormalized: sql}}}}
	}
	for _, tc := range []struct {
		name string
		m    *Mock
		want string
	}{
		{"query by hash", q("sha256:a", "select 1"), "sha256:a"},
		{"query by text", q("", "select 1"), "sql:select 1"},
		{"query of nothing", q("", ""), ""},
		{"session", &Mock{Kind: PostgresV3, Spec: MockSpec{PostgresV3: &PostgresV3Spec{Type: PostgresV3TypeSession}}}, ""},
		{"http", &Mock{Kind: HTTP}, ""},
		{"nil", nil, ""},
	} {
		if got := StatementKey(tc.m); got != tc.want {
			t.Errorf("%s: StatementKey = %q, want %q", tc.name, got, tc.want)
		}
	}
}
