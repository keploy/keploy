package models

// StatementKey names the statement a mock records, for the kinds whose clients
// can prepare a statement before they run it: a PostgreSQL client may prepare
// its statements at app start and run most of them only inside tests. It is ""
// for a mock that records none, and for every other kind (a kind whose clients
// prepare ahead adds its key here).
//
// The agent's on-disk store keeps, for each key, the first per-test mock
// recorded with it as the set's statement catalog, which the replay stages
// describe-only (DiskMocks.LoadStatements, integrations.StatementCatalog): a
// parser answers a prepare from it when no mock it may serve then holds the
// statement, and never serves it as a call's answer.
func StatementKey(m *Mock) string {
	if m == nil {
		return ""
	}
	switch m.Kind {
	case PostgresV3:
		s := m.Spec.PostgresV3
		if s == nil || s.Type != PostgresV3TypeQuery || s.Query == nil {
			return ""
		}
		if s.Query.SQLAstHash != "" {
			return s.Query.SQLAstHash
		}
		if s.Query.SQLNormalized != "" {
			return "sql:" + s.Query.SQLNormalized
		}
	}
	return ""
}
