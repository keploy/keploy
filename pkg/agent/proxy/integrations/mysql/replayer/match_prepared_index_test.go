package replayer

import (
	"math/rand"
	"slices"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
)

// TestFoldKeyIsEqualFold: preparedIndex files a recorded EXECUTE under the
// folded identity of its query, and a live EXECUTE looks up its own, so two
// queries the matcher takes as the same statement (strings.EqualFold) must
// fold alike, and two it does not, differently.
func TestFoldKeyIsEqualFold(t *testing.T) {
	pairs := [][2]string{
		{"SELECT v FROM kv WHERE id = ?", "select V from KV where ID = ?"},
		{"k", "K"},                // Kelvin sign folds with k
		{"s", "ſ"},                // long s folds with s
		{"İ", "i"},                // dotted capital I does not fold with i
		{"\xff", "\xfe"},          // invalid bytes both read as RuneError
		{"\xff", "�"},             // as does RuneError itself
		{"a\xffb", "A�B"},         // inside ASCII
		{"Σ", "ς"},                // capital sigma, final sigma
		{"σ", "ς"},                // sigma, final sigma
		{"ab", "abc"},             // lengths differ
		{"ß", "ss"},               // sharp s has no simple fold to ss
		{"ẞ", "ß"},                // capital sharp s folds with sharp s
		{"SELECT ?", "SELECT ? "}, // trailing space
	}
	alphabet := []rune{'a', 'A', 'k', 'K', 'K', 's', 'S', 'ſ', 'σ', 'Σ', 'ς', 'i', 'I', 'İ', 'ı', '�', '?', ' '}
	r := rand.New(rand.NewSource(1))
	word := func() string {
		var b strings.Builder
		for n := r.Intn(4); n >= 0; n-- {
			if r.Intn(12) == 0 {
				b.WriteByte(0x80 + byte(r.Intn(0x40))) // an invalid byte
				continue
			}
			b.WriteRune(alphabet[r.Intn(len(alphabet))])
		}
		return b.String()
	}
	for i := 0; i < 20000; i++ {
		pairs = append(pairs, [2]string{word(), word()})
	}
	for _, p := range pairs {
		if got, want := foldKey(p[0]) == foldKey(p[1]), strings.EqualFold(p[0], p[1]); got != want {
			t.Fatalf("foldKey(%q) == foldKey(%q) is %v, strings.EqualFold is %v", p[0], p[1], got, want)
		}
	}
}

// TestReadsQueryAttributesIsLowerCaseSearch: readsQueryAttributes answers as
// strings.Contains(strings.ToLower(query), ...) does, for every spelling.
func TestReadsQueryAttributesIsLowerCaseSearch(t *testing.T) {
	for _, q := range []string{
		"",
		"SELECT 1",
		"SELECT mysql_query_attribute_string('a')",
		"SELECT MYSQL_QUERY_ATTRIBUTE_STRING('a')",
		"select Mysql_Query_Attribute_Strin('a')",
		"select mysql_query_attribute_strinG",
		"mmysql_query_attribute_string",
		"SELECT MYSQL_QUERY_ATTRIBUTE_STRING('é')",
		"SELECT MYSQL_QUERY_ATTRIBUTE_STRİNG('a')", // dotted capital I lower-cases to i
		"SELECT KMYSQL_QUERY_ATTRIBUTE_STRING",
		"SELECT \xffmysql_query_attribute_string",
	} {
		want := strings.Contains(strings.ToLower(q), "mysql_query_attribute_string")
		if got := readsQueryAttributes(q); got != want {
			t.Errorf("readsQueryAttributes(%q) = %v, want %v", q, got, want)
		}
	}
}

// TestExecuteComparisonAllocatesNothing: comparing a recorded EXECUTE with the
// live one, which the scan does for every candidate of the live statement,
// allocates nothing while the debug log is off. Lower-casing both queries to
// look for the attribute function, and building the debug lines' fields,
// allocated on every candidate.
func TestExecuteComparisonAllocatesNothing(t *testing.T) {
	const q = "SELECT id, name, payload FROM small WHERE id = ?"
	bundle := func(v string) mysql.PacketBundle {
		return mysql.PacketBundle{
			Header: &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: 16}, Type: "COM_STMT_EXECUTE"},
			Message: &mysql.StmtExecutePacket{Status: 0x17, StatementID: 1, IterationCount: 1, ParameterCount: 1,
				Parameters: []mysql.Parameter{{Type: 254, Value: v}}},
		}
	}
	live := bundle("k1")
	log := zap.NewNop()
	for _, tc := range []struct {
		name     string
		recorded mysql.PacketBundle
		def      bool
	}{
		{"definitive", bundle("k1"), true},
		{"parameters drifted", bundle("k2"), false},
	} {
		def, _, _ := matchStmtExecutePacketQueryAware(log, tc.recorded, live, q, q, 1, "m", nil)
		if def != tc.def {
			t.Fatalf("%s: definitive = %v, want %v", tc.name, def, tc.def)
		}
		if n := testing.AllocsPerRun(200, func() {
			matchStmtExecutePacketQueryAware(log, tc.recorded, live, q, q, 1, "m", nil)
		}); n != 0 {
			t.Errorf("%s: %v allocations per comparison, want 0", tc.name, n)
		}
	}
}

// TestOwnedPreparedKeysFileWhatPreparedKeysFilesUnnamed: an exact view files
// the mocks its test owns by preparedIndex's OwnedKeys, which reads only
// whether a mock holds an EXECUTE recorded without its query. It must file
// under keyExecuteUnnamed exactly the mocks preparedKeys files there, and
// under no other key.
func TestOwnedPreparedKeysFileWhatPreparedKeysFilesUnnamed(t *testing.T) {
	req := func(typ string, msg interface{}, stored string) mysql.Request {
		r := mysql.Request{PacketBundle: mysql.PacketBundle{Header: &mysql.PacketInfo{Header: &mysql.Header{}, Type: typ}, Message: msg}}
		r.SetStoredQuery(stored)
		if stored == " " {
			r.Meta = map[string]string{mysql.MetaQuery: " "}
		}
		return r
	}
	exec := &mysql.StmtExecutePacket{StatementID: 1}
	prep := &mysql.StmtPreparePacket{Query: "SELECT 1"}
	query := &mysql.QueryPacket{Query: "SELECT 1"}
	cases := map[string][]mysql.Request{
		"named EXECUTE":                      {req(comStmtExecute, exec, "SELECT ?")},
		"unnamed EXECUTE":                    {req(comStmtExecute, exec, "")},
		"EXECUTE whose query is blank":       {req(comStmtExecute, exec, " ")},
		"EXECUTE with no packet":             {req(comStmtExecute, nil, "")},
		"EXECUTE with another packet":        {req(comStmtExecute, query, "")},
		"PREPARE then unnamed EXECUTE":       {req(comStmtPrepare, prep, ""), req(comStmtExecute, exec, "")},
		"PREPARE then named EXECUTE":         {req(comStmtPrepare, prep, ""), req(comStmtExecute, exec, "SELECT 1")},
		"COM_QUERY":                          {req(comQuery, query, "")},
		"COM_QUERY typed as an EXECUTE":      {req(comStmtExecute, query, "")},
		"no header":                          {{PacketBundle: mysql.PacketBundle{Message: exec}}},
		"named, then unnamed EXECUTE":        {req(comStmtExecute, exec, "SELECT ?"), req(comStmtExecute, exec, "")},
		"COM_STMT_CLOSE":                     {req(comStmtClose, &mysql.StmtClosePacket{StatementID: 1}, "")},
		"an unnamed EXECUTE of an HTTP mock": nil,
	}
	unnamed := 0
	for name, reqs := range cases {
		m := &models.Mock{Kind: models.MySQL, Spec: models.MockSpec{MySQLRequests: reqs}}
		if reqs == nil {
			m = &models.Mock{Kind: models.HTTP, Spec: models.MockSpec{MySQLRequests: []mysql.Request{req(comStmtExecute, exec, "")}}}
		}
		want := slices.Contains(preparedKeys(m), keyExecuteUnnamed)
		got := ownedPreparedKeys(m)
		if want {
			unnamed++
		}
		if want != slices.Contains(got, keyExecuteUnnamed) || len(got) > 1 || (len(got) == 1 && got[0] != keyExecuteUnnamed) {
			t.Errorf("%s: OwnedKeys %q, preparedKeys files it under %s: %v", name, got, keyExecuteUnnamed, want)
		}
	}
	if unnamed != 4 {
		t.Fatalf("precondition: preparedKeys filed %d of the cases under %s, want 4", unnamed, keyExecuteUnnamed)
	}
}
