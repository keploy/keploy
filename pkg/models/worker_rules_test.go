package models

import (
	"encoding/base64"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"go.keploy.io/server/v3/pkg/models/mysql"
)

func httpMock(method, url string) *Mock {
	return &Mock{Kind: HTTP, Spec: MockSpec{HTTPReq: &HTTPReq{Method: Method(method), URL: url}}}
}

func mysqlMock(dest string, statements ...string) *Mock {
	m := &Mock{Kind: MySQL, Spec: MockSpec{Metadata: map[string]string{"destAddr": dest}}}
	for i, s := range statements {
		var msg interface{} = &mysql.QueryPacket{Query: s}
		if i%2 == 1 {
			msg = &mysql.StmtPreparePacket{Query: s}
		}
		m.Spec.MySQLRequests = append(m.Spec.MySQLRequests, mysql.Request{PacketBundle: mysql.PacketBundle{Message: msg}})
	}
	return m
}

func redisMock(binary bool, commands ...[]string) *Mock {
	var b strings.Builder
	for _, c := range commands {
		b.WriteString("*" + strconv.Itoa(len(c)) + "\r\n")
		for _, a := range c {
			b.WriteString("$" + strconv.Itoa(len(a)) + "\r\n" + a + "\r\n")
		}
	}
	data, typ := b.String(), "utf-8"
	if binary {
		data, typ = base64.StdEncoding.EncodeToString([]byte(data)), "binary"
	}
	return &Mock{Kind: REDIS, Spec: MockSpec{GenericRequests: []Payload{{Message: []OutputBinary{{Type: typ, Data: data}}}}}}
}

// A rule names the calls it was written for and no others, for every kind it
// reads: an HTTP call by method, host (any port unless it names one) and path;
// a MySQL or Postgres mock by every statement it records; a Redis mock by
// every command's name and key; any mock by its destination.
func TestAWorkerMockRuleMatchesTheCallsItNames(t *testing.T) {
	jwks := WorkerMockRule{Kind: "Http", Match: map[string]string{"method": "GET", "host": "auth.example.com", "path": "/.well-known/jwks.json"}}
	schema := WorkerMockRule{Kind: "MySQL", Match: map[string]string{"query": "^SHOW FULL FIELDS FROM `[a-z_]+`$"}}
	pgSchema := WorkerMockRule{Kind: "PostgresV3", Match: map[string]string{"query": "^SELECT .* FROM pg_catalog"}}
	redisKey := WorkerMockRule{Kind: "Redis", Match: map[string]string{"command": "get", "key": "^jwks:"}}
	dest := WorkerMockRule{Kind: "Mongo", Match: map[string]string{"destination": "config-db.internal:*"}}

	for _, tc := range []struct {
		name string
		rule WorkerMockRule
		mock *Mock
		want bool
	}{
		{"the JWKS GET", jwks, httpMock("GET", "https://auth.example.com/.well-known/jwks.json"), true},
		{"the JWKS GET on a port", jwks, httpMock("get", "http://AUTH.example.com:8443/.well-known/jwks.json"), true},
		{"another path", jwks, httpMock("GET", "https://auth.example.com/token"), false},
		{"another method", jwks, httpMock("POST", "https://auth.example.com/.well-known/jwks.json"), false},
		{"another host", jwks, httpMock("GET", "https://evil.example.com/.well-known/jwks.json"), false},
		{"a host glob", WorkerMockRule{Kind: "Http", Match: map[string]string{"host": "*.example.com"}}, httpMock("GET", "https://auth.example.com/x"), true},
		{"a rule with a port, another port", WorkerMockRule{Kind: "Http", Match: map[string]string{"host": "auth.example.com:443"}}, httpMock("GET", "https://auth.example.com:8443/x"), false},
		{"a path regex", WorkerMockRule{Kind: "Http", Match: map[string]string{"pathRegex": "^/v[0-9]+/config$"}}, httpMock("GET", "https://cfg/v2/config"), true},
		{"an HTTP rule, a MySQL mock", jwks, mysqlMock("db:3306", "SELECT 1"), false},

		{"a schema read", schema, mysqlMock("db:3306", "SHOW FULL FIELDS FROM `users`"), true},
		{"a schema read, prepared", schema, mysqlMock("db:3306", "SHOW FULL FIELDS FROM `users`", "SHOW   FULL\nFIELDS FROM `users`"), true},
		{"a schema read and an INSERT in one mock", schema, mysqlMock("db:3306", "SHOW FULL FIELDS FROM `users`", "INSERT INTO users VALUES (1)"), false},
		{"a mock with no statement", schema, mysqlMock("db:3306"), false},

		{"a Postgres catalog read", pgSchema, &Mock{Kind: PostgresV3, Spec: MockSpec{PostgresV3: &PostgresV3Spec{Type: PostgresV3TypeQuery, Query: &PostgresV3QuerySpec{SQLNormalized: "SELECT a FROM pg_catalog.pg_type"}}}}, true},
		{"a Postgres session mock", pgSchema, &Mock{Kind: PostgresV3, Spec: MockSpec{PostgresV3: &PostgresV3Spec{Type: PostgresV3TypeSession}}}, false},

		{"a Redis GET of the key", redisKey, redisMock(false, []string{"GET", "jwks:main"}), true},
		{"a Redis GET of the key, kept as binary", redisKey, redisMock(true, []string{"GET", "jwks:main"}), true},
		{"a Redis GET of another key", redisKey, redisMock(false, []string{"GET", "session:9"}), false},
		{"a Redis SET of the key", redisKey, redisMock(false, []string{"SET", "jwks:main", "v"}), false},
		{"a pipelined GET and SET", redisKey, redisMock(false, []string{"GET", "jwks:main"}, []string{"SET", "jwks:main", "v"}), false},

		{"a destination", dest, &Mock{Kind: Mongo, Spec: MockSpec{Metadata: map[string]string{"destAddr": "config-db.internal:27017"}}}, true},
		{"another destination", dest, &Mock{Kind: Mongo, Spec: MockSpec{Metadata: map[string]string{"destAddr": "orders-db.internal:27017"}}}, false},
		{"no destination recorded", dest, &Mock{Kind: Mongo}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.rule.Matches(tc.mock); got != tc.want {
				t.Fatalf("Matches = %v, want %v", got, tc.want)
			}
		})
	}
}

// A rule that reads a key its kind does not, or whose regex does not compile,
// marks nothing: a recording never widens a rule it cannot read.
func TestAWorkerMockRuleItCannotReadMatchesNothing(t *testing.T) {
	for _, r := range []WorkerMockRule{
		{Kind: "Http", Match: map[string]string{"query": ".*"}},
		{Kind: "MySQL", Match: map[string]string{"query": "("}},
		{Kind: "Http"},
		{Kind: "Http", Match: map[string]string{"path": ""}},
	} {
		if r.Matches(httpMock("GET", "https://a/b")) || r.Matches(mysqlMock("db:3306", "SELECT 1")) {
			t.Fatalf("rule %+v matched", r)
		}
	}
}

// Validation names what is wrong with a rule and a rule set, and accepts the
// rules a user writes.
func TestWorkerMockRulesAreValidated(t *testing.T) {
	ok := []WorkerMockRule{
		{ID: "jwks", Kind: "Http", Match: map[string]string{"method": "GET", "host": "auth.example.com", "path": "/.well-known/jwks.json"}},
		{Kind: "MySQL", Match: map[string]string{"query": "^SHOW FULL FIELDS FROM "}},
	}
	if p := ValidateWorkerMockRules(ok); len(p) != 0 {
		t.Fatalf("valid rules reported %v", p)
	}
	for _, tc := range []struct {
		rule WorkerMockRule
		want string
	}{
		{WorkerMockRule{Kind: "Http", Match: map[string]string{"query": "x"}}, "is not one a Http rule reads"},
		{WorkerMockRule{Kind: "MySQL", Match: map[string]string{"query": "("}}, "is not a regex"},
		{WorkerMockRule{Kind: "MySQL", Match: map[string]string{"query": ".*"}}, "matches everything"},
		{WorkerMockRule{Kind: "Http", Match: map[string]string{"host": "*"}}, "only match key matches everything"},
		{WorkerMockRule{Kind: "Http"}, "matches on nothing"},
		{WorkerMockRule{Match: map[string]string{"destination": "a:1"}}, "names no kind"},
		{WorkerMockRule{ID: "Bad ID", Kind: "Http", Match: map[string]string{"path": "/x"}}, "is not lower-case"},
		{WorkerMockRule{Kind: "MySQL", Match: map[string]string{"query": strings.Repeat("a", 513)}}, "longer than 512"},
		{WorkerMockRule{Kind: "Http", Match: map[string]string{"path": "["}}, "is not a glob"},
	} {
		got := strings.Join(tc.rule.Validate(), "; ")
		if !strings.Contains(got, tc.want) {
			t.Fatalf("Validate(%+v) = %q, want it to say %q", tc.rule, got, tc.want)
		}
	}
	dup := []WorkerMockRule{ok[0], {ID: "jwks", Kind: "Http", Match: map[string]string{"path": "/x"}}}
	if p := strings.Join(ValidateWorkerMockRules(dup), "; "); !strings.Contains(p, `have one id, "jwks"`) {
		t.Fatalf("two rules with one id: %q", p)
	}
	many := make([]WorkerMockRule, MaxWorkerMockRules+1)
	for i := range many {
		many[i] = WorkerMockRule{Kind: "Http", Match: map[string]string{"path": "/p" + strings.Repeat("x", i)}}
	}
	if p := strings.Join(ValidateWorkerMockRules(many), "; "); !strings.Contains(p, "at most 50") {
		t.Fatalf("too many rules: %q", p)
	}
}

// A rule with no id is named by its kind and match, the same way each time and
// for each spelling of its map, and differently from another rule; the first
// matching rule of a set names the mark.
func TestAWorkerMockRuleIsNamedByWhatItMatches(t *testing.T) {
	a := WorkerMockRule{Kind: "Http", Match: map[string]string{"method": "GET", "path": "/a"}}
	b := WorkerMockRule{Kind: "Http", Match: map[string]string{"path": "/a", "method": "GET"}}
	c := WorkerMockRule{Kind: "Http", Match: map[string]string{"method": "GET", "path": "/b"}}
	if a.EffectiveID() != b.EffectiveID() || !strings.HasPrefix(a.EffectiveID(), "r-") || a.EffectiveID() == c.EffectiveID() {
		t.Fatalf("ids %q %q %q", a.EffectiveID(), b.EffectiveID(), c.EffectiveID())
	}
	if (WorkerMockRule{ID: "jwks", Kind: "Http"}).EffectiveID() != "jwks" {
		t.Fatal("a rule's own id is not its id")
	}
	id, ok := MatchWorkerMockRules([]WorkerMockRule{c, {ID: "any-a", Kind: "Http", Match: map[string]string{"path": "/a"}}, a}, httpMock("GET", "http://h/a"))
	if !ok || id != "any-a" {
		t.Fatalf("matched %q, %v; want the first matching rule, any-a", id, ok)
	}
	if _, ok := MatchWorkerMockRules([]WorkerMockRule{c}, httpMock("GET", "http://h/a")); ok {
		t.Fatal("no rule matches, yet one was reported")
	}
}

// A Redis payload a client garbled, or crafted, is read as no command: a bulk
// length near the int limit, one past the payload, a negative one. It must
// never panic, as the matcher runs in the record loop.
func TestARedisRuleReadsAMalformedPayloadAsNoCommand(t *testing.T) {
	r := WorkerMockRule{Kind: "Redis", Match: map[string]string{"command": "get"}}
	for _, raw := range []string{
		"*1\r\n$9223372036854775807\r\nGET\r\n",
		"*1\r\n$99\r\nGET\r\n",
		"*1\r\n$-5\r\nGET\r\n",
		"*2\r\n$3\r\nGET",
		"*99999999999999999999\r\n",
		"garbage",
	} {
		m := &Mock{Kind: REDIS, Spec: MockSpec{GenericRequests: []Payload{{Message: []OutputBinary{{Type: "utf-8", Data: raw}}}}}}
		if r.Matches(m) {
			t.Fatalf("the payload %q matched", raw)
		}
	}
}

// A rule written in keploy.yml arrives with its match keys lowercased
// (viper): matching and validation compare keys without case.
func TestAWorkerMockRuleComparesItsKeysWithoutCase(t *testing.T) {
	r := WorkerMockRule{Kind: "Http", Match: map[string]string{"pathregex": "^/v[0-9]+/config$", "METHOD": "get"}}
	if p := r.Validate(); len(p) != 0 {
		t.Fatalf("Validate = %v", p)
	}
	if !r.Matches(httpMock("GET", "https://cfg/v2/config")) {
		t.Fatal("a rule with lowercased keys did not match")
	}
	if (WorkerMockRule{Kind: "Http", Match: map[string]string{"pathRegex": "^/a$"}}).EffectiveID() != (WorkerMockRule{Kind: "Http", Match: map[string]string{"pathregex": "^/a$"}}).EffectiveID() {
		t.Fatal("one rule spelled in two cases has two ids")
	}
}

// A misspelled kind, and a pattern that matches everything however it is
// written, are refused; a host rule naming the default port matches a URL that
// omits it.
func TestWorkerMockRulesRefuseWhatWouldMisfire(t *testing.T) {
	for _, tc := range []struct {
		rule WorkerMockRule
		want string
	}{
		{WorkerMockRule{Kind: "http", Match: map[string]string{"path": "/x"}}, `is spelled "Http"`},
		{WorkerMockRule{Kind: "postgresv3", Match: map[string]string{"destination": "db:5432"}}, `is spelled "PostgresV3"`},
		{WorkerMockRule{Kind: "MySQL", Match: map[string]string{"query": "."}}, "matches everything"},
		{WorkerMockRule{Kind: "MySQL", Match: map[string]string{"query": "(?s)."}}, "matches everything"},
		{WorkerMockRule{Kind: "Http", Match: map[string]string{"host": "**"}}, "only match key matches everything"},
		{WorkerMockRule{Kind: "Mongo", Match: map[string]string{"destination": "*:*"}}, "only match key matches everything"},
	} {
		if got := strings.Join(tc.rule.Validate(), "; "); !strings.Contains(got, tc.want) {
			t.Fatalf("Validate(%+v) = %q, want it to say %q", tc.rule, got, tc.want)
		}
	}
	for _, k := range []string{"Couchbase", "Pulsar", "PostgresV2", "DNS"} {
		cb := WorkerMockRule{Kind: k, Match: map[string]string{"destination": "cb.internal:11210"}}
		if p := cb.Validate(); len(p) != 0 {
			t.Fatalf("a destination rule of kind %s (one this tree or an enterprise parser records) was refused: %v", k, p)
		}
		if !cb.Matches(&Mock{Kind: Kind(k), Spec: MockSpec{Metadata: map[string]string{"destAddr": "cb.internal:11210"}}}) {
			t.Fatalf("a destination rule of kind %s did not match its call", k)
		}
	}
	// As the recorder stores an HTTP/1 call: the request line's URL (no
	// scheme, no host), the Host header without the default port, and the
	// address the connection reached.
	recorded := func(port string) *Mock {
		return &Mock{Kind: HTTP, Spec: MockSpec{
			HTTPReq:  &HTTPReq{Method: "GET", URL: "/jwks", Header: map[string]string{"Host": "auth.example.com"}},
			Metadata: map[string]string{"destAddr": "10.0.0.5:" + port},
		}}
	}
	tls := WorkerMockRule{Kind: "Http", Match: map[string]string{"host": "auth.example.com:443", "path": "/jwks"}}
	plain := WorkerMockRule{Kind: "Http", Match: map[string]string{"host": "auth.example.com:80", "path": "/jwks"}}
	if !tls.Matches(recorded("443")) || tls.Matches(recorded("80")) {
		t.Fatal("a :443 rule must match the call dialled on 443, whose Host header names no port, and not the one dialled on 80")
	}
	if !plain.Matches(recorded("80")) || plain.Matches(recorded("443")) {
		t.Fatal("a :80 rule must match the call dialled on 80 and not the one dialled on 443")
	}
	if !(WorkerMockRule{Kind: "Http", Match: map[string]string{"host": "auth.example.com:*", "path": "/jwks"}}).Matches(recorded("443")) {
		t.Fatal("a port glob must match the dialled port of a call whose Host header names none")
	}
	if !(WorkerMockRule{Kind: "Http", Match: map[string]string{"host": "auth.example.com", "path": "/jwks"}}).Matches(recorded("8443")) {
		t.Fatal("a rule without a port must match the host on any port")
	}
	// Without the connection's address, a URL that names its scheme tells the
	// default port.
	if !tls.Matches(httpMock("GET", "https://auth.example.com/jwks")) || tls.Matches(httpMock("GET", "http://auth.example.com/jwks")) {
		t.Fatal("with no destAddr, a :443 rule must match an https URL without a port and not an http one")
	}
	if tls.Matches(httpMock("GET", "https://auth.example.com:8443/jwks")) {
		t.Fatal("a :443 rule matched another port")
	}
}

var longSelect = "SELECT " + strings.Repeat("`users`.`column_with_a_long_name`, ", 20) + "`users`.`id` FROM `users` WHERE `users`.`id` = ?"

// longSelectPrefix is the regex a rule of longSelect holds: its longest
// quoted prefix within a rule's 512 characters, never cutting an escape.
var longSelectPrefix = func() string {
	q := "^"
	for _, r := range longSelect {
		piece := regexp.QuoteMeta(string(r))
		if len(q)+len(piece) > maxWorkerRuleRegex {
			break
		}
		q += piece
	}
	return q
}()

// A rule suggested from a recorded mock names that mock's call, narrowly: it
// matches the mock, and does not match a different call of the same kind. A
// mock whose call keploy cannot read is named by its destination; a call
// keploy reads but cannot name in a rule is refused, never widened to its
// destination (every call to that database).
func TestARuleSuggestedFromAMockNamesItsCall(t *testing.T) {
	pg := func(stmt string) *Mock {
		return &Mock{Kind: PostgresV3, Spec: MockSpec{PostgresV3: &PostgresV3Spec{Type: PostgresV3TypeQuery, Query: &PostgresV3QuerySpec{SQLNormalized: stmt}}}}
	}
	mongo := &Mock{Kind: Mongo, Spec: MockSpec{Metadata: map[string]string{"destAddr": "mongo:27017"}}}
	for _, row := range []struct {
		name  string
		mock  *Mock
		other *Mock // a different call of the same kind
		want  map[string]string
	}{
		{"HTTP", httpMock("get", "http://auth.example.com/.well-known/jwks.json?x=1"), httpMock("GET", "http://auth.example.com/other"),
			map[string]string{"method": "GET", "host": "auth.example.com", "path": "/.well-known/jwks.json"}},
		{"HTTP with glob characters in its path", httpMock("GET", "http://api/a*b/[c]"), httpMock("GET", "http://api/axxb/c"),
			map[string]string{"method": "GET", "host": "api", "path": `/a\*b/\[c\]`}},
		{"MySQL", mysqlMock("db:3306", "SHOW FULL FIELDS FROM `users`"), mysqlMock("db:3306", "SHOW FULL FIELDS FROM `users` LIKE 'id'"),
			map[string]string{"query": "^SHOW FULL FIELDS FROM `users`$"}},
		{"HTTP with an upper-case host", httpMock("GET", "http://Auth.Example.COM/jwks"), httpMock("GET", "http://other.example.com/jwks"),
			map[string]string{"method": "GET", "host": "auth.example.com", "path": "/jwks"}},
		{"MySQL too long for a whole rule, by its longest prefix", mysqlMock("db:3306", longSelect), mysqlMock("db:3306", "DELETE FROM orders"),
			map[string]string{"query": longSelectPrefix}},
		{"MySQL with bytes that are not UTF-8", mysqlMock("db:3306", "SELECT * FROM t WHERE b = _binary'\xff\xfe'"), mysqlMock("db:3306", "SELECT * FROM t WHERE b = _binary'ab'"),
			map[string]string{"query": `^SELECT \* FROM t WHERE b = _binary'\x{FFFD}\x{FFFD}'$`}},
		{"Redis reading several keys", redisMock(false, []string{"GET", "a"}, []string{"GET", "b"}), redisMock(false, []string{"GET", "session:123"}),
			map[string]string{"command": "GET", "key": "^(?:a|b)$"}},
		{"Redis with a key that is not UTF-8", redisMock(false, []string{"GET", "k\xff"}), redisMock(false, []string{"GET", "k"}),
			map[string]string{"command": "GET", "key": `^k\x{FFFD}$`}},
		{"Redis AUTH, without its password", redisMock(false, []string{"AUTH", "hunter2"}), redisMock(false, []string{"GET", "hunter2"}),
			map[string]string{"command": "AUTH"}},
		{"Redis HELLO, without its credentials", redisMock(false, []string{"HELLO", "3", "AUTH", "user", "hunter2"}), redisMock(false, []string{"GET", "3"}),
			map[string]string{"command": "HELLO"}},
		{"Postgres", pg("SELECT * FROM users WHERE id = $1"), pg("SELECT * FROM orders WHERE id = $1"),
			map[string]string{"query": `^SELECT \* FROM users WHERE id = \$1$`}},
		{"Redis", redisMock(false, []string{"GET", "jwks:v1"}), redisMock(false, []string{"GET", "jwks:v1:old"}),
			map[string]string{"command": "GET", "key": "^jwks:v1$"}},
		{"another kind, by its destination", mongo, &Mock{Kind: Mongo, Spec: MockSpec{Metadata: map[string]string{"destAddr": "other:27017"}}},
			map[string]string{"destination": "mongo:27017"}},
	} {
		t.Run(row.name, func(t *testing.T) {
			rule, err := SuggestWorkerMockRule(row.mock)
			if err != nil {
				t.Fatal(err)
			}
			if len(rule.Match) != len(row.want) {
				t.Fatalf("rule %+v; want match %v", rule, row.want)
			}
			for k, v := range row.want {
				if rule.Match[k] != v {
					t.Fatalf("rule %+v; want match %v", rule, row.want)
				}
			}
			if !rule.Matches(row.mock) {
				t.Fatalf("rule %+v does not match the mock it came from", rule)
			}
			if rule.Matches(row.other) {
				t.Fatalf("rule %+v also matches a different call", rule)
			}
		})
	}
	if _, err := SuggestWorkerMockRule(&Mock{Kind: Mongo, Name: "mock-9"}); err == nil {
		t.Fatal("a mock with no readable call and no destination got a rule")
	}
	// A mock whose invalid bytes became U+FFFD on the way (JSON does) gives
	// the rule, and the rule id, of the mock as recorded: a server derives it
	// again at a merge.
	raw, err := SuggestWorkerMockRule(mysqlMock("db:3306", "SELECT * FROM t WHERE b = _binary'\xff'"))
	if err != nil {
		t.Fatal(err)
	}
	cleaned, err := SuggestWorkerMockRule(mysqlMock("db:3306", "SELECT * FROM t WHERE b = _binary'\uFFFD'"))
	if err != nil {
		t.Fatal(err)
	}
	if raw.EffectiveID() != cleaned.EffectiveID() {
		t.Fatalf("rules %v and %v of one mock have different ids", raw.Match, cleaned.Match)
	}
	// A prefix cut never splits a character or an escape, and stays within a
	// rule's length, whatever the statement's characters.
	for _, unit := range []string{"é", "日本", "\xff", "*"} {
		stmt := "SELECT '" + strings.Repeat(unit, 600) + "'"
		rule, err := SuggestWorkerMockRule(mysqlMock("db:3306", stmt))
		if err != nil {
			t.Fatalf("a long statement of %q: %v", unit, err)
		}
		if q := rule.Match["query"]; len(q) > maxWorkerRuleRegex || !utf8.ValidString(q) {
			t.Fatalf("a long statement of %q gives a %d-character or invalid regex", unit, len(q))
		}
	}
	for name, m := range map[string]*Mock{
		// An alternation would also match a mock of SET NAMES alone.
		"MySQL with several statements": mysqlMock("db:3306", "SET NAMES utf8mb4", "SHOW FULL FIELDS FROM users"),
		"MySQL with no statement":       mysqlMock("db:3306"),
		"Redis with two commands":       redisMock(false, []string{"GET", "a"}, []string{"SET", "a", "1"}),
	} {
		if rule, err := SuggestWorkerMockRule(m); err == nil {
			t.Fatalf("%s: got rule %+v; want it refused, not widened", name, rule)
		}
	}
}

// Two mocks of one call have one key, whatever the host's case; another
// path, statement, destination or command is another call.
func TestTheCallAMockRecordsIsItsKey(t *testing.T) {
	same := [][2]*Mock{
		{httpMock("GET", "http://auth.example.com/jwks"), httpMock("GET", "http://Auth.Example.COM/jwks")},
		{mysqlMock("db:3306", "SHOW FULL FIELDS FROM users"), mysqlMock("db:3306", "SHOW FULL FIELDS FROM users")},
	}
	for _, p := range same {
		if WorkerMockCallKey(p[0]) != WorkerMockCallKey(p[1]) {
			t.Fatalf("one call, two keys: %q and %q", WorkerMockCallKey(p[0]), WorkerMockCallKey(p[1]))
		}
	}
	other := [][2]*Mock{
		{httpMock("GET", "http://auth.example.com/jwks"), httpMock("GET", "http://auth.example.com/other")},
		{httpMock("GET", "http://auth.example.com/jwks"), httpMock("POST", "http://auth.example.com/jwks")},
		{mysqlMock("db:3306", "SHOW FULL FIELDS FROM users"), mysqlMock("db:3306", "SHOW FULL FIELDS FROM reports")},
		{mysqlMock("db:3306", "SELECT 1"), mysqlMock("db2:3306", "SELECT 1")},
		{redisMock(false, []string{"GET", "a"}), redisMock(false, []string{"GET", "b"})},
	}
	for _, p := range other {
		if WorkerMockCallKey(p[0]) == WorkerMockCallKey(p[1]) {
			t.Fatalf("two calls, one key %q", WorkerMockCallKey(p[0]))
		}
	}
}

// A rule may also name what the call answered (response), to pick one of a
// call's copies: the HTTP body, the MySQL result as JSON, the Redis reply.
// It must name the call too, and a kind whose answer keploy does not read
// (PostgresV3) refuses it; a rule derived from a mock never holds it.
func TestARuleCanNameTheResponse(t *testing.T) {
	newKey := httpMock("GET", "http://auth/jwks")
	newKey.Spec.HTTPResp = &HTTPResp{StatusCode: 200, Body: `{"keys":[{"kid":"2026-11"}]}`}
	oldKey := httpMock("GET", "http://auth/jwks")
	oldKey.Spec.HTTPResp = &HTTPResp{StatusCode: 200, Body: `{"keys":[{"kid":"2026-10"}]}`}
	rule := WorkerMockRule{Kind: "Http", Match: map[string]string{"path": "/jwks", "response": `"kid":"2026-11"`}}
	if problems := rule.Validate(); len(problems) > 0 {
		t.Fatal(problems)
	}
	if !rule.Matches(newKey) || rule.Matches(oldKey) {
		t.Fatalf("the response did not pick the copy: new %v, old %v", rule.Matches(newKey), rule.Matches(oldKey))
	}

	redisOK := redisMock(false, []string{"AUTH", "x"})
	redisOK.Spec.GenericResponses = []Payload{{Message: []OutputBinary{{Type: "utf-8", Data: "+OK\r\n"}}}}
	if r := (WorkerMockRule{Kind: "Redis", Match: map[string]string{"command": "AUTH", "response": `^\+OK`}}); !r.Matches(redisOK) {
		t.Fatal("a Redis reply was not read")
	}

	for name, bad := range map[string]WorkerMockRule{
		"response alone":    {Kind: "Http", Match: map[string]string{"response": "kid"}},
		"postgres response": {Kind: "PostgresV3", Match: map[string]string{"query": "^SELECT", "response": "x"}},
		"matching all":      {Kind: "Http", Match: map[string]string{"path": "/jwks", "response": ".*"}},
		"not a regex":       {Kind: "Http", Match: map[string]string{"path": "/jwks", "response": "("}},
	} {
		if len(bad.Validate()) == 0 {
			t.Fatalf("%s: validated", name)
		}
	}
	derived, err := SuggestWorkerMockRule(newKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := derived.Match["response"]; ok {
		t.Fatalf("a derived rule %v names the response", derived.Match)
	}
}

// A MySQL rule's response reads the result as JSON: a column name of the
// schema read names the copy.
func TestAMySQLRuleReadsTheResultAsJSON(t *testing.T) {
	m := mysqlMock("db:3306", "SHOW FULL FIELDS FROM users")
	m.Spec.MySQLResponses = []mysql.Response{{PacketBundle: mysql.PacketBundle{Header: &mysql.PacketInfo{Type: "TextResultSet"}, Message: map[string]any{"columns": []string{"Field"}, "rows": [][]string{{"sign_in_count"}}}}}}
	if r := (WorkerMockRule{Kind: "MySQL", Match: map[string]string{"query": "^SHOW FULL FIELDS FROM users$", "response": "sign_in_count"}}); !r.Matches(m) {
		t.Fatalf("the result %q was not read", workerMockResponseText(m))
	}
	if r := (WorkerMockRule{Kind: "MySQL", Match: map[string]string{"query": "^SHOW FULL FIELDS FROM users$", "response": "zipcode"}}); r.Matches(m) {
		t.Fatal("a column the result lacks matched")
	}
}

// A tracer's comment (dd-trace's sqlcommenter, a fresh traceparent in each
// statement) is not part of the call: two copies tagged differently are one
// call, and a rule derived from one names the other. A hint, a versioned
// comment and a quoted "/*" are the statement's own.
func TestATracersCommentIsNotPartOfTheCall(t *testing.T) {
	tagged := func(tp string) *Mock {
		return mysqlMock("db:3306", "SHOW FULL FIELDS FROM `users` /*dde='prod',ddps='web',traceparent='00-"+tp+"-01'*/")
	}
	a, b := tagged("aaaa"), tagged("bbbb")
	if WorkerMockCallKey(a) != WorkerMockCallKey(b) {
		t.Fatalf("one call tagged twice, two keys: %q and %q", WorkerMockCallKey(a), WorkerMockCallKey(b))
	}
	rule, err := SuggestWorkerMockRule(a)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(rule.Match["query"], "traceparent") {
		t.Fatalf("the derived rule quotes the tracer's comment: %q", rule.Match["query"])
	}
	for _, m := range []*Mock{b, mysqlMock("db:3306", "/* rails:app */ SHOW FULL FIELDS FROM `users`")} {
		if !rule.Matches(m) {
			t.Fatalf("rule %v does not match %q", rule.Match, mysqlStatements(m))
		}
	}
	if rule.Matches(mysqlMock("db:3306", "SHOW FULL FIELDS FROM `reports` /*traceparent='00-cccc-01'*/")) {
		t.Fatal("the rule matches another table's schema read")
	}

	for q, want := range map[string]string{
		"SELECT/*x*/1":                          "SELECT 1",
		"SELECT /*+ MAX_EXECUTION_TIME(1) */ 1": "SELECT /*+ MAX_EXECUTION_TIME(1) */ 1",
		"SELECT /*!40001 SQL_NO_CACHE */ 1":     "SELECT /*!40001 SQL_NO_CACHE */ 1",
		"SELECT '/* kept */' /* gone */":        "SELECT '/* kept */'",
		`SELECT 'it\'s /*' /* gone */`:          `SELECT 'it\'s /*'`,
		"SELECT `a/*b` FROM t /* gone */":       "SELECT `a/*b` FROM t",
		"SELECT 1 /* unterminated":              "SELECT 1 /* unterminated",
		"SELECT /*M!100100 STRAIGHT_JOIN */ 1":  "SELECT /*M!100100 STRAIGHT_JOIN */ 1",
		"-- it's\nSELECT '/* kept */' FROM t":   "-- it's SELECT '/* kept */' FROM t",
		"# don't\nSELECT 1 /* gone */":          "# don't SELECT 1",
		"SELECT 1 --x /* kept */":               "SELECT 1 --x",
	} {
		if got := collapseSpace(stripSQLComments(q)); got != want {
			t.Errorf("stripSQLComments(%q) = %q, want %q", q, got, want)
		}
	}
}

// A mock whose call keploy cannot name is a call of its own, so a rule that
// names it by destination alone marks every such mock, as it did before
// rules marked one copy per call; a mock whose call it can name is keyed by
// all of it: an HTTP request's query and body too.
func TestACallKeployCannotNameIsItsOwn(t *testing.T) {
	mongo := func(name string) *Mock {
		return &Mock{Name: name, Kind: Mongo, Spec: MockSpec{Metadata: map[string]string{"destAddr": "mongo:27017"}}}
	}
	execute := func(name string) *Mock {
		m := mysqlMock("db:3306")
		m.Name = name
		return m
	}
	for name, p := range map[string][2]*Mock{
		"two Mongo mocks":         {mongo("mock-1"), mongo("mock-2")},
		"two unnamed MySQL calls": {execute("mock-3"), execute("mock-4")},
	} {
		if WorkerMockCallKey(p[0]) == WorkerMockCallKey(p[1]) {
			t.Fatalf("%s: one key %q", name, WorkerMockCallKey(p[0]))
		}
	}
	body := func(b string) *Mock {
		m := httpMock("POST", "http://auth/oauth/token")
		m.Spec.HTTPReq.Body = b
		return m
	}
	for name, p := range map[string][2]*Mock{
		"two queries": {httpMock("GET", "http://cfg/config?tenant=a"), httpMock("GET", "http://cfg/config?tenant=b")},
		"two bodies":  {body(`{"scope":"a"}`), body(`{"scope":"b"}`)},
	} {
		if WorkerMockCallKey(p[0]) == WorkerMockCallKey(p[1]) {
			t.Fatalf("%s: one key %q", name, WorkerMockCallKey(p[0]))
		}
	}
	// Two mocks of one POST, built apart: one key.
	first, again := body(`{"scope":"a"}`), body(`{"scope":"a"}`)
	if WorkerMockCallKey(first) != WorkerMockCallKey(again) {
		t.Fatal("one POST, two keys")
	}
}

// An HTTP/2 mock's answer is read from its own response; a MySQL row's value
// is read as it is, & and < included; a rule whose request keys match
// everything matches on the response alone, and is refused.
func TestTheResponseKeyReadsEveryKind(t *testing.T) {
	h2 := &Mock{Kind: HTTP2, Spec: MockSpec{HTTP2Req: &HTTP2Req{Method: "GET", URL: "/v1/keys", Authority: "auth:443"},
		HTTP2Resp: &HTTP2Resp{StatusCode: 200, Body: `{"kid":"2026-11"}`}}}
	if r := (WorkerMockRule{Kind: "Http2", Match: map[string]string{"path": "/v1/keys", "response": "2026-11"}}); !r.Matches(h2) {
		t.Fatalf("an HTTP/2 response %q was not read", workerMockResponseText(h2))
	}
	m := mysqlMock("db:3306", "SELECT name FROM partners")
	m.Spec.MySQLResponses = []mysql.Response{{PacketBundle: mysql.PacketBundle{Message: map[string]any{"rows": [][]string{{"Smith & <Sons>"}}}}}}
	if r := (WorkerMockRule{Kind: "MySQL", Match: map[string]string{"query": "^SELECT name FROM partners$", "response": "Smith & <Sons>"}}); !r.Matches(m) {
		t.Fatalf("the row %q was not read as written", workerMockResponseText(m))
	}
	for _, r := range []WorkerMockRule{
		{Kind: "Http", Match: map[string]string{"host": "*", "response": "kid"}},
		{Kind: "Http", Match: map[string]string{"host": "*", "destination": "*:*", "response": "kid"}},
	} {
		if !strings.Contains(strings.Join(r.Validate(), "; "), "response alone") {
			t.Fatalf("rule %v is not refused as matching on the response alone: %v", r.Match, r.Validate())
		}
	}
	if p := (WorkerMockRule{Kind: "Http", Match: map[string]string{"host": "*", "destination": "*:*"}}).Validate(); !strings.Contains(strings.Join(p, "; "), "matches everything") {
		t.Fatalf("a rule whose every key matches everything was not refused: %v", p)
	}
}

// A Postgres call is its normalized statement and the values it was made
// with: Rails' schema read of each table is one statement whose $1 names the
// table, so two tables' reads are two calls, and two copies of one table's
// read are one.
func TestAPostgresCallIsItsStatementAndItsValues(t *testing.T) {
	read := func(name, table string) *Mock {
		return &Mock{Name: name, Kind: PostgresV3, Spec: MockSpec{Metadata: map[string]string{"destAddr": "db:5432"}, PostgresV3: &PostgresV3Spec{Type: PostgresV3TypeQuery,
			Query: &PostgresV3QuerySpec{SQLNormalized: "SELECT a.attname FROM pg_attribute a WHERE a.attrelid = $1::regclass", Literals: PostgresV3Cells{{Value: table}}}}}}
	}
	if WorkerMockCallKey(read("mock-1", "users")) == WorkerMockCallKey(read("mock-2", "orders")) {
		t.Fatal("two tables' schema reads are one call")
	}
	if WorkerMockCallKey(read("mock-1", "users")) != WorkerMockCallKey(read("mock-3", "users")) {
		t.Fatal("two copies of one table's schema read are two calls")
	}
	binds := read("mock-4", "")
	binds.Spec.PostgresV3.Query.Literals = nil
	binds.Spec.PostgresV3.Query.BindValues = PostgresV3Cells{{Value: "users"}}
	other := read("mock-5", "")
	other.Spec.PostgresV3.Query.Literals = nil
	other.Spec.PostgresV3.Query.BindValues = PostgresV3Cells{{Value: "orders"}}
	if WorkerMockCallKey(binds) == WorkerMockCallKey(other) {
		t.Fatal("two calls bound to two tables are one call")
	}
}

// PostgreSQL's comments are its own: any -- starts one, # does not, block
// comments nest, a backslash in a string is a character, and a dollar-quoted
// string is kept whole.
func TestPostgresCommentsAreReadAsPostgresReadsThem(t *testing.T) {
	for q, want := range map[string]string{
		"SELECT a --see /* x\nFROM t /* tag */":   "SELECT a --see /* x FROM t",
		"SELECT /* a /* b */ c */ 1":              "SELECT 1",
		"SELECT 1 # 2 /* gone */":                 "SELECT 1 # 2",
		`SELECT 'C:\' /* gone */`:                 `SELECT 'C:\'`,
		"SELECT $f$ /* kept */ $f$ /* gone */":    "SELECT $f$ /* kept */ $f$",
		"SELECT $$a$$, $1 /*traceparent='00-x'*/": "SELECT $$a$$, $1",
		"SELECT /*+ SeqScan(t) */ 1":              "SELECT /*+ SeqScan(t) */ 1",
		"SELECT /*! 1 */ 2":                       "SELECT 2",
		"SELECT E'\\'' , '/* keep */' FROM t":     "SELECT E'\\'' , '/* keep */' FROM t",
		"SELECT a$b$c FROM t /* x */":             "SELECT a$b$c FROM t",
	} {
		if got := collapseSpace(stripPostgresComments(q)); got != want {
			t.Errorf("stripPostgresComments(%q) = %q, want %q", q, got, want)
		}
	}
}

// A method names no call beside a response: GET plus a body regex would serve
// every GET to any host whose answer matched.
func TestAMethodDoesNotNameTheCallOfAResponseRule(t *testing.T) {
	r := WorkerMockRule{Kind: "Http", Match: map[string]string{"method": "GET", "response": "kid"}}
	if !strings.Contains(strings.Join(r.Validate(), "; "), "response alone") {
		t.Fatalf("rule %v is not refused: %v", r.Match, r.Validate())
	}
	if p := (WorkerMockRule{Kind: "Http", Match: map[string]string{"method": "GET"}}).Validate(); len(p) != 0 {
		t.Fatalf("a method alone, refused: %v", p)
	}
	redis := WorkerMockRule{Kind: "Redis", Match: map[string]string{"command": "GET", "response": "kid"}}
	if !strings.Contains(strings.Join(redis.Validate(), "; "), "response alone") {
		t.Fatalf("rule %v is not refused: %v", redis.Match, redis.Validate())
	}
	if p := (WorkerMockRule{Kind: "Redis", Match: map[string]string{"command": "AUTH"}}).Validate(); len(p) != 0 {
		t.Fatalf("a command alone, refused: %v", p)
	}
	if p := strings.Join((WorkerMockRule{Kind: "Http", Match: map[string]string{"response": ""}}).Validate(), "; "); strings.Contains(p, "response alone") {
		t.Fatalf("an empty response said to match on the response alone: %v", p)
	}
}
