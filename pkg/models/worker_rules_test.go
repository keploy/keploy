package models

import (
	"encoding/base64"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
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

func mongoOpMsgMock(name, dest, reqSection, respSection string) *Mock {
	m := &Mock{
		Name: name,
		Kind: Mongo,
		Spec: MockSpec{
			Metadata: map[string]string{"destAddr": dest},
			MongoRequests: []MongoRequest{{
				Message: &MongoOpMessage{Sections: []string{reqSection}},
			}},
		},
	}
	if respSection != "" {
		m.Spec.MongoResponses = []MongoResponse{{
			Message: &MongoOpMessage{Sections: []string{respSection}},
		}}
	}
	return m
}

// TestMongoWorkerMockRulesValidateMatchAndDerive verifies Mongo worker-mock
// rules (db, command, collection, response) including api-server's exact
// ptw-reconcile-poll and load-stale-scan shapes (red on main, where Mongo had
// no protocol keys in workerRuleKeys).
func TestMongoWorkerMockRulesValidateMatchAndDerive(t *testing.T) {
	ptwRule := WorkerMockRule{
		ID:   "ptw-reconcile-poll",
		Kind: "Mongo",
		Match: map[string]string{
			"db":         "api-server",
			"command":    "findAndModify",
			"collection": "ptw_endpoints",
			"response":   `^\{ SectionSingle msg: \{"lastErrorObject":\{"n":\{"\$number(?:Int|Long)":"0"\}[,}]`,
		},
	}
	loadRule := WorkerMockRule{
		ID:   "load-stale-scan",
		Kind: "Mongo",
		Match: map[string]string{
			"db":         "api-server",
			"command":    "update",
			"collection": "loadTestRun",
			"response":   `^\{ SectionSingle msg: \{"n":\{"\$number(?:Int|Long)":"0"\}[,}]`,
		},
	}
	ptwAnyDBRule := WorkerMockRule{
		ID:   "ptw-reconcile-any-db",
		Kind: "Mongo",
		Match: map[string]string{
			"command":    "findAndModify",
			"collection": "ptw_endpoints",
			"response":   `^\{ SectionSingle msg: \{"lastErrorObject":\{"n":\{"\$number(?:Int|Long)":"0"\}[,}]`,
		},
	}

	for _, r := range []WorkerMockRule{ptwRule, loadRule, ptwAnyDBRule} {
		if probs := r.Validate(); len(probs) > 0 {
			t.Fatalf("rule %s failed Validate: %v", r.ID, probs)
		}
	}

	// Negative validation cases for Mongo rules.
	for _, bad := range []struct {
		rule WorkerMockRule
		want string
	}{
		{WorkerMockRule{Kind: "Mongo", Match: map[string]string{"command": "find", "response": `"ok":1`}}, "response alone"},
		{WorkerMockRule{Kind: "Mongo", Match: map[string]string{"db": "api-server", "unknown": "x"}}, "not one a Mongo rule reads"},
		{WorkerMockRule{Kind: "Mongo", Match: map[string]string{"collection": "ptw_endpoints", "response": ".*"}}, "matches everything"},
	} {
		probs := bad.rule.Validate()
		if len(probs) == 0 || !strings.Contains(strings.Join(probs, "; "), bad.want) {
			t.Fatalf("Validate(%+v) = %v, want substring %q", bad.rule, probs, bad.want)
		}
	}

	ptwNoop := mongoOpMsgMock(
		"mock-ptw-noop", "mongo:27017",
		`{ SectionSingle msg: {"findAndModify":"ptw_endpoints","query":{"dirtyAt":{"$lte":{"$numberLong":"1764300000"}}},"$db":"api-server"} }`,
		`{ SectionSingle msg: {"lastErrorObject":{"n":{"$numberInt":"0"},"updatedExisting":false},"value":null,"ok":{"$numberDouble":"1.0"}} }`,
	)
	ptwClaimed := mongoOpMsgMock(
		"mock-ptw-claimed", "mongo:27017",
		`{ SectionSingle msg: {"findAndModify":"ptw_endpoints","query":{"dirtyAt":{"$lte":{"$numberLong":"1764300000"}}},"$db":"api-server"} }`,
		`{ SectionSingle msg: {"lastErrorObject":{"n":{"$numberInt":"1"},"updatedExisting":true},"value":{"_id":"ep-1"},"ok":{"$numberDouble":"1.0"}} }`,
	)
	ptwClaimedWithNullField := mongoOpMsgMock(
		"mock-ptw-claimed-nullfield", "mongo:27017",
		`{ SectionSingle msg: {"findAndModify":"ptw_endpoints","query":{"dirtyAt":{"$lte":{"$numberLong":"1764300000"}}},"$db":"api-server"} }`,
		`{ SectionSingle msg: {"lastErrorObject":{"n":{"$numberInt":"1"},"updatedExisting":true},"value":{"_id":"ep-1","nested":{"value":null}},"ok":{"$numberDouble":"1.0"}} }`,
	)
	ptwWrongDB := mongoOpMsgMock(
		"mock-ptw-otherdb", "mongo:27017",
		`{ SectionSingle msg: {"findAndModify":"ptw_endpoints","query":{},"$db":"other-db"} }`,
		`{ SectionSingle msg: {"lastErrorObject":{"n":{"$numberInt":"0"},"updatedExisting":false},"value":null,"ok":{"$numberDouble":"1.0"}} }`,
	)
	ptwWrongCmd := mongoOpMsgMock(
		"mock-ptw-insert", "mongo:27017",
		`{ SectionSingle msg: {"insert":"ptw_endpoints","documents":[{}],"$db":"api-server"} }`,
		`{ SectionSingle msg: {"lastErrorObject":{"n":{"$numberInt":"0"}},"ok":1} }`,
	)
	ptwWrongColl := mongoOpMsgMock(
		"mock-ptw-othercoll", "mongo:27017",
		`{ SectionSingle msg: {"findAndModify":"other_endpoints","query":{},"$db":"api-server"} }`,
		`{ SectionSingle msg: {"lastErrorObject":{"n":{"$numberInt":"0"}},"ok":1} }`,
	)
	ptwPrefixColl := mongoOpMsgMock(
		"mock-ptw-prefixcoll", "mongo:27017",
		`{ SectionSingle msg: {"findAndModify":"ptw_endpoints2","query":{},"$db":"api-server"} }`,
		`{ SectionSingle msg: {"lastErrorObject":{"n":{"$numberInt":"0"}},"ok":1} }`,
	)
	getMorePtw := mongoOpMsgMock(
		"mock-getmore-ptw", "mongo:27017",
		`{ SectionSingle msg: {"getMore":{"$numberLong":"42"},"collection":"ptw_endpoints","$db":"api-server"} }`,
		`{ SectionSingle msg: {"cursor":{"id":{"$numberLong":"0"},"nextBatch":[]},"ok":{"$numberDouble":"1.0"}} }`,
	)
	dbAggRead := mongoOpMsgMock(
		"mock-db-agg-read", "admin:27017",
		`{ SectionSingle msg: {"aggregate":{"$numberInt":"1"},"pipeline":[{"$currentOp":{}}],"$db":"admin"} }`,
		`{ SectionSingle msg: {"cursor":{"firstBatch":[]},"ok":{"$numberDouble":"1.0"}} }`,
	)
	dbAggWrite := mongoOpMsgMock(
		"mock-db-agg-out", "admin:27017",
		`{ SectionSingle msg: {"aggregate":{"$numberInt":"1"},"pipeline":[{"$out":"dump"}],"$db":"admin"} }`,
		`{ SectionSingle msg: {"ok":{"$numberDouble":"1.0"}} }`,
	)
	loadNoop := mongoOpMsgMock(
		"mock-load-noop", "mongo:27017",
		`{ SectionSingle msg: {"update":"loadTestRun","ordered":true,"$db":"api-server"} }`,
		`{ SectionSingle msg: {"n":{"$numberInt":"0"},"nModified":{"$numberInt":"0"},"ok":{"$numberDouble":"1.0"}} }`,
	)
	loadModified := mongoOpMsgMock(
		"mock-load-mod", "mongo:27017",
		`{ SectionSingle msg: {"update":"loadTestRun","ordered":true,"$db":"api-server"} }`,
		`{ SectionSingle msg: {"n":{"$numberInt":"1"},"nModified":{"$numberInt":"1"},"ok":{"$numberDouble":"1.0"}} }`,
	)
	loadMultiSec := &Mock{
		Name: "mock-load-multisec",
		Kind: Mongo,
		Spec: MockSpec{
			Metadata: map[string]string{"destAddr": "mongo:27017"},
			MongoRequests: []MongoRequest{{
				Message: &MongoOpMessage{Sections: []string{
					`{ SectionSingle identifier: updates , msgs: [ {"q":{"status":"running"},"u":{"$set":{"status":"crashed"}}} ] }`,
					`{ SectionSingle msg: {"update":"loadTestRun","ordered":true,"$db":"api-server"} }`,
				}},
			}},
			MongoResponses: []MongoResponse{{
				Message: &MongoOpMessage{Sections: []string{
					`{ SectionSingle msg: {"n":{"$numberInt":"0"},"nModified":{"$numberInt":"0"},"ok":{"$numberDouble":"1.0"}} }`,
				}},
			}},
		},
	}
	oauthInsert := mongoOpMsgMock(
		"mock-oauth", "mongo:27017",
		`{ SectionSingle msg: {"insert":"oauth_authorization_requests","$db":"api-server"} }`,
		`{ SectionSingle msg: {"n":1,"ok":1} }`,
	)
	opQueryCmd := &Mock{
		Name: "mock-opquery",
		Kind: Mongo,
		Spec: MockSpec{
			Metadata: map[string]string{"destAddr": "mongo:27017"},
			MongoRequests: []MongoRequest{{
				Message: &MongoOpQuery{
					FullCollectionName: "api-server.$cmd",
					Query:              `{"findAndModify":"ptw_endpoints","query":{}}`,
				},
			}},
			MongoResponses: []MongoResponse{{
				Message: &MongoOpReply{Documents: []string{`{"lastErrorObject":{"n":{"$numberInt":"0"}},"value":null,"ok":1}`}},
			}},
		},
	}
	opQueryCmdBodyDBOverrides := &Mock{
		Name: "mock-opquery-db-override",
		Kind: Mongo,
		Spec: MockSpec{
			Metadata: map[string]string{"destAddr": "mongo:27017"},
			MongoRequests: []MongoRequest{{
				Message: &MongoOpQuery{
					FullCollectionName: "api-server.$cmd",
					Query:              `{"findAndModify":"ptw_endpoints","$db":"other-db"}`,
				},
			}},
		},
	}
	opQueryCmdBodyDBMatches := &Mock{
		Name: "mock-opquery-db-matches",
		Kind: Mongo,
		Spec: MockSpec{
			Metadata: map[string]string{"destAddr": "mongo:27017"},
			MongoRequests: []MongoRequest{{
				Message: &MongoOpQuery{
					FullCollectionName: "other-db.$cmd",
					Query:              `{"findAndModify":"ptw_endpoints","$db":"api-server"}`,
				},
			}},
		},
	}
	opQueryEmbeddedMsg := &Mock{
		Name: "mock-opquery-embedded-msg",
		Kind: Mongo,
		Spec: MockSpec{
			Metadata: map[string]string{"destAddr": "mongo:27017"},
			MongoRequests: []MongoRequest{{
				Message: &MongoOpQuery{
					FullCollectionName: "api-server.$cmd",
					Query:              `{"find":"logs","filter":{"text":"msg:{\"findAndModify\":\"ptw_endpoints\"}"}}`,
				},
			}},
			MongoResponses: []MongoResponse{{
				Message: &MongoOpReply{Documents: []string{`{"value":null,"ok":1}`}},
			}},
		},
	}
	opQueryDirect := &Mock{
		Name: "mock-opquery-direct",
		Kind: Mongo,
		Spec: MockSpec{
			Metadata: map[string]string{"destAddr": "mongo:27017"},
			MongoRequests: []MongoRequest{{
				Message: &MongoOpQuery{FullCollectionName: "api-server.ptw_endpoints", Query: `{}`},
			}},
		},
	}
	opQueryMalformed := &Mock{
		Name: "mock-opquery-malformed",
		Kind: Mongo,
		Spec: MockSpec{
			Metadata: map[string]string{"destAddr": "mongo:27017"},
			MongoRequests: []MongoRequest{{
				Message: &MongoOpQuery{FullCollectionName: "api-server.$cmd", Query: `{"findAndModify":"ptw_endpoints",`},
			}},
			MongoResponses: []MongoResponse{{
				Message: &MongoOpReply{Documents: []string{`{"value":null,"ok":1}`}},
			}},
		},
	}
	emptyFirstKey := mongoOpMsgMock(
		"mock-empty-key", "mongo:27017",
		`{ SectionSingle msg: {"":"ptw_endpoints","findAndModify":"ptw_endpoints","$db":"api-server"} }`,
		`{ SectionSingle msg: {"lastErrorObject":{"n":{"$numberInt":"0"}},"value":null,"ok":1} }`,
	)
	truncatedOpMsg := mongoOpMsgMock(
		"mock-truncated", "mongo:27017",
		`{ SectionSingle msg: {"findAndModify":"ptw_endpoints","$db":"api-server",`,
		`{ SectionSingle msg: {"lastErrorObject":{"n":{"$numberInt":"0"}},"value":null,"ok":1} }`,
	)
	multiReqUnparsedSecond := &Mock{
		Name: "mock-multireq-unparsed",
		Kind: Mongo,
		Spec: MockSpec{
			Metadata: map[string]string{"destAddr": "mongo:27017"},
			MongoRequests: []MongoRequest{
				{Message: &MongoOpMessage{Sections: []string{`{ SectionSingle msg: {"findAndModify":"ptw_endpoints","$db":"api-server"} }`}}},
				{Message: &MongoOpUnknown{}},
			},
			MongoResponses: []MongoResponse{{
				Message: &MongoOpMessage{Sections: []string{`{ SectionSingle msg: {"lastErrorObject":{"n":{"$numberInt":"0"}},"value":null,"ok":1} }`}},
			}},
		},
	}
	multiReqMismatchSecond := &Mock{
		Name: "mock-multireq-mismatch",
		Kind: Mongo,
		Spec: MockSpec{
			Metadata: map[string]string{"destAddr": "mongo:27017"},
			MongoRequests: []MongoRequest{
				{Message: &MongoOpMessage{Sections: []string{`{ SectionSingle msg: {"findAndModify":"ptw_endpoints","$db":"api-server"} }`}}},
				{Message: &MongoOpMessage{Sections: []string{`{ SectionSingle msg: {"insert":"oauth_authorization_requests","$db":"api-server"} }`}}},
			},
			MongoResponses: []MongoResponse{{
				Message: &MongoOpMessage{Sections: []string{`{ SectionSingle msg: {"lastErrorObject":{"n":{"$numberInt":"0"}},"value":null,"ok":1} }`}},
			}},
		},
	}
	legacyUpdate := &Mock{
		Name: "mock-legacy-update",
		Kind: Mongo,
		Spec: MockSpec{
			Metadata:      map[string]string{"destAddr": "mongo:27017"},
			MongoRequests: []MongoRequest{{Message: MongoOpUpdate{FullCollectionName: "api-server.loadTestRun"}}},
		},
	}

	for _, tc := range []struct {
		name string
		rule WorkerMockRule
		mock *Mock
		want bool
	}{
		{"ptw no-op matches", ptwRule, ptwNoop, true},
		{"ptw any-db rule matches across dbs", ptwAnyDBRule, ptwWrongDB, true},
		{"ptw claimed endpoint rejected by response regex", ptwRule, ptwClaimed, false},
		{"ptw claimed doc containing nested value:null rejected by anchored lastErrorObject.n regex", ptwRule, ptwClaimedWithNullField, false},
		{"ptw wrong db rejected", ptwRule, ptwWrongDB, false},
		{"ptw wrong command rejected", ptwRule, ptwWrongCmd, false},
		{"ptw wrong collection rejected", ptwRule, ptwWrongColl, false},
		{"collection prefix does not match", ptwRule, ptwPrefixColl, false},
		{"short collection prefix rule does not match longer collection", WorkerMockRule{Kind: "Mongo", Match: map[string]string{"db": "api-server", "command": "findAndModify", "collection": "ptw_endpoint"}}, ptwNoop, false},
		{"command prefix find does not match findAndModify", WorkerMockRule{Kind: "Mongo", Match: map[string]string{"db": "api-server", "command": "find", "collection": "ptw_endpoints"}}, ptwNoop, false},
		{"ptw OP_QUERY $cmd matches", WorkerMockRule{Kind: "Mongo", Match: map[string]string{"db": "api-server", "command": "findAndModify", "collection": "ptw_endpoints", "response": `"lastErrorObject":\{"n":\{"\$number(?:Int|Long)":"0"\}`}}, opQueryCmd, true},
		{"OP_QUERY $cmd body $db overrides namespace (rejects wrong $db)", WorkerMockRule{Kind: "Mongo", Match: map[string]string{"db": "api-server", "command": "findAndModify", "collection": "ptw_endpoints"}}, opQueryCmdBodyDBOverrides, false},
		{"OP_QUERY $cmd body $db overrides namespace (accepts matching $db)", WorkerMockRule{Kind: "Mongo", Match: map[string]string{"db": "api-server", "command": "findAndModify", "collection": "ptw_endpoints"}}, opQueryCmdBodyDBMatches, true},
		{"OP_QUERY $cmd embedded msg string does not spoof command", ptwRule, opQueryEmbeddedMsg, false},
		{"OP_QUERY direct collection uses query command", WorkerMockRule{Kind: "Mongo", Match: map[string]string{"db": "api-server", "command": "query", "collection": "ptw_endpoints"}}, opQueryDirect, true},
		{"OP_QUERY malformed $cmd fails closed", ptwRule, opQueryMalformed, false},
		{"OP_MSG empty first key fails closed", ptwRule, emptyFirstKey, false},
		{"OP_MSG truncated json fails closed", ptwRule, truncatedOpMsg, false},
		{"OP_MSG bare json without msg prefix fails closed", ptwRule, mongoOpMsgMock("mock-bare", "mongo:27017", `{"findAndModify":"ptw_endpoints","$db":"api-server"}`, `{ SectionSingle msg: {"lastErrorObject":{"n":{"$numberInt":"0"}},"ok":1} }`), false},
		{"OP_MSG getMore reads collection field and counts as read", WorkerMockRule{Kind: "Mongo", Match: map[string]string{"db": "api-server", "collection": "ptw_endpoints"}}, getMorePtw, true},
		{"OP_MSG db-level read-only aggregate matches commandless db rule", WorkerMockRule{Kind: "Mongo", Match: map[string]string{"db": "admin"}}, dbAggRead, true},
		{"OP_MSG db-level aggregate with $out rejected by commandless db rule", WorkerMockRule{Kind: "Mongo", Match: map[string]string{"db": "admin"}}, dbAggWrite, false},
		{"multi-request with unparsed second fails closed", ptwRule, multiReqUnparsedSecond, false},
		{"multi-request with mismatched second fails closed", ptwRule, multiReqMismatchSecond, false},
		{"ptw command case-insensitive", WorkerMockRule{Kind: "Mongo", Match: map[string]string{"db": "api-server", "command": "FINDANDMODIFY", "collection": "ptw_endpoints"}}, ptwNoop, true},
		{"load stale no-op matches canonical ExtJSON", loadRule, loadNoop, true},
		{"load stale multi-section OP_MSG matches body section", loadRule, loadMultiSec, true},
		{"load stale modified row rejected", loadRule, loadModified, false},
		{"legacy MongoOpUpdate value matches", WorkerMockRule{Kind: "Mongo", Match: map[string]string{"db": "api-server", "command": "update", "collection": "loadTestRun"}}, legacyUpdate, true},
		{"unrelated collection rejected", ptwRule, oauthInsert, false},
		{"empty MongoRequests rejected when protocol keys set", ptwRule, &Mock{Kind: Mongo, Spec: MockSpec{Metadata: map[string]string{"destAddr": "mongo:27017"}}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.rule.Matches(tc.mock); got != tc.want {
				t.Fatalf("Matches = %v, want %v", got, tc.want)
			}
		})
	}

	// SuggestWorkerMockRule derives narrow {db, command, collection} for real Mongo mocks,
	// omits db when absent, allows collectionless handshake/health commands (ping, hello,
	// saslStart), and refuses arbitrary collectionless or multi-op/malformed mocks.
	derived, err := SuggestWorkerMockRule(ptwNoop)
	if err != nil {
		t.Fatalf("SuggestWorkerMockRule(ptwNoop): %v", err)
	}
	wantDerived := map[string]string{"db": "api-server", "command": "findAndModify", "collection": "ptw_endpoints"}
	if len(derived.Match) != len(wantDerived) {
		t.Fatalf("derived.Match = %v, want %v", derived.Match, wantDerived)
	}
	for k, v := range wantDerived {
		if derived.Match[k] != v {
			t.Fatalf("derived.Match[%q] = %q, want %q", k, derived.Match[k], v)
		}
	}
	if derived.Matches(oauthInsert) {
		t.Fatalf("derived rule %+v matched unrelated oauth_authorization_requests", derived)
	}
	noDBMock := mongoOpMsgMock("mock-nodb", "mongo:27017", `{ SectionSingle msg: {"findAndModify":"ptw_endpoints"} }`, "")
	derivedNoDB, err := SuggestWorkerMockRule(noDBMock)
	if err != nil {
		t.Fatalf("SuggestWorkerMockRule(noDBMock): %v", err)
	}
	if _, hasDB := derivedNoDB.Match["db"]; hasDB || derivedNoDB.Match["collection"] != "ptw_endpoints" {
		t.Fatalf("derivedNoDB.Match = %v, want collection without db", derivedNoDB.Match)
	}
	for _, cmd := range []string{"ping", "hello", "saslStart"} {
		m := mongoOpMsgMock("mock-"+cmd, "mongo:27017", `{ SectionSingle msg: {"`+cmd+`":{"$numberInt":"1"},"$db":"admin"} }`, "")
		r, err := SuggestWorkerMockRule(m)
		if err != nil {
			t.Fatalf("SuggestWorkerMockRule(%s): %v", cmd, err)
		}
		if r.Match["command"] != cmd || r.Match["db"] != "admin" || r.Match["collection"] != "" {
			t.Fatalf("SuggestWorkerMockRule(%s) = %v, want {db:admin, command:%s}", cmd, r.Match, cmd)
		}
		if !r.Matches(m) || r.Matches(ptwNoop) {
			t.Fatalf("derived %s rule match mismatch", cmd)
		}
	}
	for name, bad := range map[string]*Mock{
		"arbitrary collectionless command": mongoOpMsgMock("mock-custom", "mongo:27017", `{ SectionSingle msg: {"customAdminCmd":1,"$db":"admin"} }`, ""),
		"db-level aggregate":               dbAggRead,
		"multi-op mismatch":                multiReqMismatchSecond,
		"malformed op":                     truncatedOpMsg,
	} {
		if r, err := SuggestWorkerMockRule(bad); err == nil {
			t.Fatalf("SuggestWorkerMockRule(%s) returned %+v, want error", name, r)
		}
	}

	// WorkerMockCallKey: identical static calls share one key EVEN when driver
	// metadata (lsid, $clusterTime, $readPreference, maxTimeMS, afterClusterTime)
	// varies; different destAddr, clock-varying cutoffs, or unhandled requests
	// keep distinct keys.
	ptwVolatileA := mongoOpMsgMock(
		"mock-ptw-vola", "mongo:27017",
		`{ SectionSingle msg: {"findAndModify":"ptw_endpoints","query":{"dirtyAt":{"$lte":{"$numberLong":"1764300000"}}},"lsid":{"id":{"$binary":{"base64":"AAAA","subType":"04"}}},"$clusterTime":{"clusterTime":{"$timestamp":{"t":1764300000,"i":1}}},"$readPreference":{"mode":"primary"},"readConcern":{"level":"majority","afterClusterTime":{"$timestamp":{"t":1764300000,"i":1}}},"maxTimeMS":5000,"$db":"api-server"} }`,
		`{ SectionSingle msg: {"lastErrorObject":{"n":{"$numberInt":"0"}},"value":null,"ok":1} }`,
	)
	ptwVolatileB := mongoOpMsgMock(
		"mock-ptw-volb", "mongo:27017",
		`{ SectionSingle msg: {"findAndModify":"ptw_endpoints","query":{"dirtyAt":{"$lte":{"$numberLong":"1764300000"}}},"lsid":{"id":{"$binary":{"base64":"BBBB","subType":"04"}}},"$clusterTime":{"clusterTime":{"$timestamp":{"t":1764300999,"i":9}}},"$readPreference":{"mode":"secondaryPreferred"},"readConcern":{"level":"majority","afterClusterTime":{"$timestamp":{"t":1764300999,"i":9}}},"maxTimeMS":9000,"$db":"api-server"} }`,
		`{ SectionSingle msg: {"lastErrorObject":{"n":{"$numberInt":"0"}},"value":null,"ok":1} }`,
	)
	ptwOtherDest := mongoOpMsgMock(
		"mock-ptw-otherdest", "mongo-secondary:27017",
		`{ SectionSingle msg: {"findAndModify":"ptw_endpoints","query":{"dirtyAt":{"$lte":{"$numberLong":"1764300000"}}},"$db":"api-server"} }`,
		`{ SectionSingle msg: {"lastErrorObject":{"n":{"$numberInt":"0"}},"value":null,"ok":1} }`,
	)
	ptwNextSec := mongoOpMsgMock(
		"mock-ptw-nextsec", "mongo:27017",
		`{ SectionSingle msg: {"findAndModify":"ptw_endpoints","query":{"dirtyAt":{"$lte":{"$numberLong":"1764300002"}}},"$db":"api-server"} }`,
		`{ SectionSingle msg: {"lastErrorObject":{"n":{"$numberInt":"0"}},"value":null,"ok":1} }`,
	)
	if WorkerMockCallKey(ptwVolatileA) != WorkerMockCallKey(ptwVolatileB) {
		t.Fatalf("identical static Mongo calls differing only in lsid/$clusterTime got different keys: %q vs %q", WorkerMockCallKey(ptwVolatileA), WorkerMockCallKey(ptwVolatileB))
	}
	if WorkerMockCallKey(ptwNoop) == WorkerMockCallKey(ptwOtherDest) {
		t.Fatalf("different destAddr Mongo calls collapsed to one key %q", WorkerMockCallKey(ptwNoop))
	}
	// The polls' own request times: an integer is read as the clock only near them.
	ptwNoop.Spec.ReqTimestampMock = time.Unix(1764300005, 0)
	ptwNextSec.Spec.ReqTimestampMock = time.Unix(1764300007, 0)
	kNoop, hasTimeNoop := WorkerMockCallKeyDetails(ptwNoop)
	kNext, hasTimeNext := WorkerMockCallKeyDetails(ptwNextSec)
	if kNoop != kNext || !hasTimeNoop || !hasTimeNext {
		t.Fatalf("clock-varying Mongo polls must share one time-normalized key with hasTime=true: (%q, %v) vs (%q, %v)", kNoop, hasTimeNoop, kNext, hasTimeNext)
	}
	// Value-type legacy opcode hashes cleanly; unhandled second request fails closed to mock-own key.
	legacyUpdateB := &Mock{
		Name: "mock-legacy-update-b",
		Kind: Mongo,
		Spec: MockSpec{
			Metadata:      map[string]string{"destAddr": "mongo:27017"},
			MongoRequests: []MongoRequest{{Message: MongoOpUpdate{FullCollectionName: "api-server.loadTestRun"}}},
		},
	}
	if WorkerMockCallKey(legacyUpdate) != WorkerMockCallKey(legacyUpdateB) {
		t.Fatalf("identical value-type MongoOpUpdate mocks got different keys")
	}
	multiUnparsedA := multiReqUnparsedSecond
	multiUnparsedB := &Mock{
		Name: "mock-multireq-unparsed-b",
		Kind: Mongo,
		Spec: multiReqUnparsedSecond.Spec,
	}
	if WorkerMockCallKey(multiUnparsedA) == WorkerMockCallKey(multiUnparsedB) {
		t.Fatalf("mocks with unhandled second MongoRequest collapsed to one key")
	}
}

// A retryable write carries its session's txnNumber, as the Go driver sends a
// findAndModify to a replica set: two claims of one poll that differ in it
// (and in lsid and $clusterTime) are one call. A txnNumber inside the filter
// is the app's and keeps calls apart.
func TestAMongoCallKeyDropsTheSessionsTxnNumber(t *testing.T) {
	claim := func(name, lsid, txn, ct, filterTxn string) *Mock {
		return mongoOpMsgMock(name, "mongo:27017",
			`{ SectionSingle msg: {"findAndModify":"ptw_endpoints","query":{"dirtyAt":{"$gt":{"$numberLong":"0"}},"txnNumber":{"$numberLong":"`+filterTxn+`"}},"update":{"$set":{"claimedAt":{"$numberLong":"5"}}},"lsid":{"id":{"$binary":{"base64":"`+lsid+`","subType":"04"}}},"txnNumber":{"$numberLong":"`+txn+`"},"$clusterTime":{"clusterTime":{"$timestamp":{"t":`+ct+`,"i":1}}},"$db":"api-server"} }`,
			`{ SectionSingle msg: {"lastErrorObject":{"n":{"$numberInt":"0"}},"value":null,"ok":1} }`)
	}
	a := claim("a", "AAAA", "1", "100", "0")
	if b := claim("b", "BBBB", "7", "200", "0"); WorkerMockCallKey(a) != WorkerMockCallKey(b) {
		t.Errorf("two claims of one poll that differ only in the session's txnNumber, lsid and $clusterTime got different keys")
	}
	if c := claim("c", "AAAA", "1", "100", "1"); WorkerMockCallKey(a) == WorkerMockCallKey(c) {
		t.Errorf("claims whose filters differ (a txnNumber field the app queries) share a key")
	}
}

// An integer with an epoch's digits that is an id or a counter, years from
// the call that sends it, keeps calls apart: only a value near its call's own
// request time is read as the clock.
func TestAMongoCallKeyKeepsIdsWithAnEpochsDigitsApart(t *testing.T) {
	at := time.Date(2026, 11, 28, 10, 0, 0, 0, time.UTC)
	find := func(filter string) *Mock {
		m := mongoOpMsgMock("f", "mongo:27017", `{ SectionSingle msg: {"find":"items","filter":`+filter+`,"$db":"app"} }`, `{ SectionSingle msg: {"ok":1} }`)
		m.Spec.ReqTimestampMock = at
		return m
	}
	for _, p := range [][3]string{
		{"a 10-digit order id", `{"orderId":{"$numberLong":"1234567890"}}`, `{"orderId":{"$numberLong":"1234567891"}}`},
		{"a 10-digit int32 counter", `{"seq":{"$numberInt":"1000000001"}}`, `{"seq":{"$numberInt":"1000000002"}}`},
		{"a phone number", `{"phone":{"$numberLong":"2125551234"}}`, `{"phone":{"$numberLong":"3105559876"}}`},
		{"an EAN-13 barcode", `{"ean":{"$numberLong":"4006381333931"}}`, `{"ean":{"$numberLong":"4006381333948"}}`},
		{"a Discord snowflake", `{"userId":{"$numberLong":"1444555666777888999"}}`, `{"userId":{"$numberLong":"1444555666777889000"}}`},
		{"a Twitter snowflake", `{"tweetId":{"$numberLong":"1864123456789012345"}}`, `{"tweetId":{"$numberLong":"1864123456789012346"}}`},
	} {
		ka, hta := WorkerMockCallKeyDetails(find(p[1]))
		kb, _ := WorkerMockCallKeyDetails(find(p[2]))
		if hta || ka == kb {
			t.Errorf("%s was read as a time (hasTime=%v, one key=%v)", p[0], hta, ka == kb)
		}
	}
	// The call's own clock, an hour back, a day back and a year back, is one.
	k0, ht0 := WorkerMockCallKeyDetails(find(`{"at":{"$lte":{"$numberLong":"1795859990"}}}`))
	for _, v := range []int64{at.Unix(), at.Add(-time.Hour).Unix(), at.Add(-24 * time.Hour).Unix(), at.AddDate(-1, 0, 0).Unix()} {
		if k, ht := WorkerMockCallKeyDetails(find(fmt.Sprintf(`{"at":{"$lte":{"$numberLong":"%d"}}}`, v))); !ht0 || !ht || k != k0 {
			t.Errorf("a clock read %d (of a call at %d) keeps its own key", v, at.Unix())
		}
	}
}

// Time-typed values in a Mongo worker-call key are normalized by type and
// format only so clock-varying polls share a call key while calls with
// different time formats or non-time integers (including 64-bit IDs past 2^53)
// stay distinct.
func TestAMongoCallKeyNormalizesTimeTypedValuesByTypeAndFormat(t *testing.T) {
	// Epoch seconds in $numberLong (ptw_endpoints poll shape).
	ptw1 := mongoOpMsgMock("ptw1", "mongo:27017",
		`{ SectionSingle msg: {"findAndModify":"ptw_endpoints","query":{"$and":[{"dirtyAt":{"$lte":{"$numberLong":"1795800000"}}}]},"update":{"$set":{"claimedAt":{"$numberLong":"1795800002"}}},"txnNumber":{"$numberLong":"1"},"$db":"api-server"} }`,
		`{ SectionSingle msg: {"lastErrorObject":{"n":{"$numberInt":"0"}},"value":null,"ok":1} }`)
	ptw2 := mongoOpMsgMock("ptw2", "mongo:27017",
		`{ SectionSingle msg: {"findAndModify":"ptw_endpoints","query":{"$and":[{"dirtyAt":{"$lte":{"$numberLong":"1795803600"}}}]},"update":{"$set":{"claimedAt":{"$numberLong":"1795803602"}}},"txnNumber":{"$numberLong":"99"},"$db":"api-server"} }`,
		`{ SectionSingle msg: {"lastErrorObject":{"n":{"$numberInt":"0"}},"value":null,"ok":1} }`)
	// An integer is read as the clock only near its call's own request time.
	clk := time.Unix(1795800002, 0).UTC()
	ptw1.Spec.ReqTimestampMock = clk
	ptw2.Spec.ReqTimestampMock = clk.Add(time.Hour)
	if k, ht := WorkerMockCallKeyDetails(mongoOpMsgMock("ptw0", "mongo:27017", ptw1.Spec.MongoRequests[0].Message.(*MongoOpMessage).Sections[0], "")); ht || k == WorkerMockCallKey(ptw2) {
		t.Fatalf("a call with no request time read an integer as its clock")
	}
	k1, ht1 := WorkerMockCallKeyDetails(ptw1)
	k2, ht2 := WorkerMockCallKeyDetails(ptw2)
	if !ht1 || !ht2 || k1 != k2 {
		t.Fatalf("ptw epoch_s polls did not normalize to one time-tagged key: ht=(%v,%v) eq=%v", ht1, ht2, k1 == k2)
	}

	// Different time format (epoch ms vs epoch s) MUST keep distinct keys.
	ptwMs := mongoOpMsgMock("ptwMs", "mongo:27017",
		`{ SectionSingle msg: {"findAndModify":"ptw_endpoints","query":{"$and":[{"dirtyAt":{"$lte":{"$numberLong":"1795800000000"}}}]},"update":{"$set":{"claimedAt":{"$numberLong":"1795800002000"}}},"txnNumber":{"$numberLong":"1"},"$db":"api-server"} }`,
		`{ SectionSingle msg: {"lastErrorObject":{"n":{"$numberInt":"0"}},"value":null,"ok":1} }`)
	ptwMs.Spec.ReqTimestampMock = clk
	if kMs, htMs := WorkerMockCallKeyDetails(ptwMs); !htMs || kMs == k1 {
		t.Fatalf("epoch_ms poll collapsed onto epoch_s key: ht=%v eq=%v", htMs, kMs == k1)
	}

	// BSON $date ISO vs $date $numberLong normalize by their own formats.
	dateIso1 := mongoOpMsgMock("di1", "mongo:27017", `{ SectionSingle msg: {"find":"jobs","filter":{"at":{"$date":"2026-11-28T10:00:00Z"}},"$db":"app"} }`, `{ SectionSingle msg: {"ok":1} }`)
	dateIso2 := mongoOpMsgMock("di2", "mongo:27017", `{ SectionSingle msg: {"find":"jobs","filter":{"at":{"$date":"2026-11-28T12:34:56Z"}},"$db":"app"} }`, `{ SectionSingle msg: {"ok":1} }`)
	dateMs1 := mongoOpMsgMock("dm1", "mongo:27017", `{ SectionSingle msg: {"find":"jobs","filter":{"at":{"$date":{"$numberLong":"1795800000000"}}},"$db":"app"} }`, `{ SectionSingle msg: {"ok":1} }`)
	if WorkerMockCallKey(dateIso1) != WorkerMockCallKey(dateIso2) {
		t.Fatalf("$date ISO strings failed to normalize to one key")
	}
	if WorkerMockCallKey(dateIso1) == WorkerMockCallKey(dateMs1) {
		t.Fatalf("$date ISO and $date $numberLong collapsed across formats")
	}

	// RFC3339 strings normalize by format (Z vs nano Z vs offset).
	rfc1 := mongoOpMsgMock("r1", "mongo:27017", `{ SectionSingle msg: {"find":"jobs","filter":{"since":"2026-11-28T10:00:00Z"},"$db":"app"} }`, `{ SectionSingle msg: {"ok":1} }`)
	rfc2 := mongoOpMsgMock("r2", "mongo:27017", `{ SectionSingle msg: {"find":"jobs","filter":{"since":"2026-11-28T11:22:33Z"},"$db":"app"} }`, `{ SectionSingle msg: {"ok":1} }`)
	rfcNano := mongoOpMsgMock("rn", "mongo:27017", `{ SectionSingle msg: {"find":"jobs","filter":{"since":"2026-11-28T11:22:33.123456Z"},"$db":"app"} }`, `{ SectionSingle msg: {"ok":1} }`)
	if WorkerMockCallKey(rfc1) != WorkerMockCallKey(rfc2) {
		t.Fatalf("RFC3339 Z timestamps failed to normalize to one key")
	}
	if WorkerMockCallKey(rfc1) == WorkerMockCallKey(rfcNano) {
		t.Fatalf("RFC3339 Z and RFC3339Nano Z collapsed across formats")
	}

	// Kind-1 document sequence (loadTestRun stale scan shape) normalizes epoch timestamps while keeping non-time filters distinct.
	seq1 := &Mock{
		Name: "seq1", Kind: Mongo,
		Spec: MockSpec{
			Metadata:         map[string]string{"destAddr": "mongo:27017"},
			ReqTimestampMock: clk,
			MongoRequests: []MongoRequest{{Message: &MongoOpMessage{Sections: []string{
				`{ SectionSingle identifier: updates , msgs: [ {"q":{"$or":[{"last_active":{"$lt":{"$numberLong":"1795800000"}}},{"status":"stale"}]}} ] }`,
				`{ SectionSingle msg: {"update":"loadTestRun","ordered":true,"txnNumber":{"$numberLong":"1"},"$db":"api-server"} }`,
			}}}},
		},
	}
	seq2 := &Mock{
		Name: "seq2", Kind: Mongo,
		Spec: MockSpec{
			Metadata:         map[string]string{"destAddr": "mongo:27017"},
			ReqTimestampMock: clk.Add(2 * time.Hour),
			MongoRequests: []MongoRequest{{Message: &MongoOpMessage{Sections: []string{
				`{ SectionSingle identifier: updates , msgs: [ {"q":{"$or":[{"last_active":{"$lt":{"$numberLong":"1795807200"}}},{"status":"stale"}]}} ] }`,
				`{ SectionSingle msg: {"update":"loadTestRun","ordered":true,"txnNumber":{"$numberLong":"42"},"$db":"api-server"} }`,
			}}}},
		},
	}
	seqDiff := &Mock{
		Name: "seqDiff", Kind: Mongo,
		Spec: MockSpec{
			Metadata:         map[string]string{"destAddr": "mongo:27017"},
			ReqTimestampMock: clk.Add(2 * time.Hour),
			MongoRequests: []MongoRequest{{Message: &MongoOpMessage{Sections: []string{
				`{ SectionSingle identifier: updates , msgs: [ {"q":{"$or":[{"last_active":{"$lt":{"$numberLong":"1795807200"}}},{"status":"running"}]}} ] }`,
				`{ SectionSingle msg: {"update":"loadTestRun","ordered":true,"txnNumber":{"$numberLong":"42"},"$db":"api-server"} }`,
			}}}},
		},
	}
	ks1, hts1 := WorkerMockCallKeyDetails(seq1)
	ks2, hts2 := WorkerMockCallKeyDetails(seq2)
	if !hts1 || !hts2 || ks1 != ks2 {
		t.Fatalf("kind-1 loadTestRun polls did not normalize to one time-tagged key: ht=(%v,%v) eq=%v", hts1, hts2, ks1 == ks2)
	}
	if WorkerMockCallKey(seqDiff) == ks1 {
		t.Fatalf("kind-1 updates differing in non-time filter collapsed to one key")
	}

	// Non-time integers (including 64-bit IDs past 2^53) MUST NOT normalize.
	idA := mongoOpMsgMock("idA", "mongo:27017", `{ SectionSingle msg: {"find":"items","filter":{"id":9007199254740993},"$db":"app"} }`, `{ SectionSingle msg: {"ok":1} }`)
	idB := mongoOpMsgMock("idB", "mongo:27017", `{ SectionSingle msg: {"find":"items","filter":{"id":9007199254740994},"$db":"app"} }`, `{ SectionSingle msg: {"ok":1} }`)
	kA, htA := WorkerMockCallKeyDetails(idA)
	kB, htB := WorkerMockCallKeyDetails(idB)
	if htA || htB || kA == kB {
		t.Fatalf("non-time 64-bit IDs were normalized: ht=(%v,%v) eq=%v", htA, htB, kA == kB)
	}

	// readConcern.afterClusterTime is stripped so a readConcern with afterClusterTime
	// matches an otherwise identical call without afterClusterTime (and does not taint hasTime).
	rcPlain := mongoOpMsgMock("rcPlain", "mongo:27017", `{ SectionSingle msg: {"find":"items","filter":{"id":42},"readConcern":{"level":"majority"},"$db":"app"} }`, `{ SectionSingle msg: {"ok":1} }`)
	rcAfter := mongoOpMsgMock("rcAfter", "mongo:27017", `{ SectionSingle msg: {"find":"items","filter":{"id":42},"readConcern":{"level":"majority","afterClusterTime":{"$timestamp":{"t":1764300001,"i":1}}},"$db":"app"} }`, `{ SectionSingle msg: {"ok":1} }`)
	kRCP, htRCP := WorkerMockCallKeyDetails(rcPlain)
	kRCA, htRCA := WorkerMockCallKeyDetails(rcAfter)
	if kRCP != kRCA || htRCP || htRCA {
		t.Fatalf("readConcern.afterClusterTime was not stripped cleanly: (%q,%v) vs (%q,%v)", kRCP, htRCP, kRCA, htRCA)
	}

	// Top-level BSON key order is preserved (not sorted) so distinct BSON field
	// orders do not falsely collapse to one key.
	ordA := mongoOpMsgMock("ordA", "mongo:27017", `{ SectionSingle msg: {"find":"items","filter":{"id":42},"$db":"app"} }`, `{ SectionSingle msg: {"ok":1} }`)
	ordB := mongoOpMsgMock("ordB", "mongo:27017", `{ SectionSingle msg: {"find":"items","$db":"app","filter":{"id":42}} }`, `{ SectionSingle msg: {"ok":1} }`)
	if WorkerMockCallKey(ordA) == WorkerMockCallKey(ordB) {
		t.Fatalf("distinct BSON top-level key order collapsed to one key: %q", WorkerMockCallKey(ordA))
	}

	// Unrecognized / non-JSON auxiliary sections stay in the key rather than being dropped.
	rawSecA := &Mock{
		Name: "rawSecA", Kind: Mongo,
		Spec: MockSpec{
			Metadata: map[string]string{"destAddr": "mongo:27017"},
			MongoRequests: []MongoRequest{{Message: &MongoOpMessage{Sections: []string{
				`{ SectionSingle msg: {"find":"items","filter":{"id":42},"$db":"app"} }`,
				`{ SectionRaw opaque: payload-A }`,
			}}}},
		},
	}
	rawSecB := &Mock{
		Name: "rawSecB", Kind: Mongo,
		Spec: MockSpec{
			Metadata: map[string]string{"destAddr": "mongo:27017"},
			MongoRequests: []MongoRequest{{Message: &MongoOpMessage{Sections: []string{
				`{ SectionSingle msg: {"find":"items","filter":{"id":42},"$db":"app"} }`,
				`{ SectionRaw opaque: payload-B }`,
			}}}},
		},
	}
	if WorkerMockCallKey(rawSecA) == WorkerMockCallKey(rawSecB) {
		t.Fatalf("distinct auxiliary OP_MSG sections collapsed to one key")
	}

	// Commandless rule rejects OP_QUERY $cmd write commands (e.g. insert / findAndModify).
	cmdless := WorkerMockRule{ID: "any-read", Kind: "Mongo", Match: map[string]string{"db": "api-server"}}
	opQueryWrite := &Mock{
		Name: "opquery-write", Kind: Mongo,
		Spec: MockSpec{
			Metadata: map[string]string{"destAddr": "mongo:27017"},
			MongoRequests: []MongoRequest{{Message: &MongoOpQuery{
				FullCollectionName: "api-server.$cmd",
				Query:              `{"findAndModify":"ptw_endpoints","update":{"$set":{"claimed":true}}}`,
			}}},
		},
	}
	if cmdless.Matches(opQueryWrite) {
		t.Fatalf("commandless Mongo rule matched OP_QUERY $cmd write command")
	}

	// Only getMore falls back to the "collection" field; another command whose
	// value is non-string (e.g. db-level aggregate: 1) with an auxiliary
	// "collection" field must NOT match a collection-scoped rule.
	collRule := WorkerMockRule{ID: "users-read", Kind: "Mongo", Match: map[string]string{"db": "api-server", "collection": "users"}}
	dbAggWithCollField := mongoOpMsgMock("db-agg-coll", "mongo:27017",
		`{ SectionSingle msg: {"aggregate":1,"collection":"users","pipeline":[],"$db":"api-server"} }`,
		`{ SectionSingle msg: {"ok":1} }`,
	)
	if collRule.Matches(dbAggWithCollField) {
		t.Fatalf("collection-scoped rule matched non-getMore command via auxiliary collection field")
	}
}

func TestAMongoCallKeyNormalizesBsonTimestampOnly(t *testing.T) {
	ts1 := mongoOpMsgMock("ts1", "mongo:27017",
		`{ SectionSingle msg: {"find":"oplog_cursors","filter":{"ts":{"$gt":{"$timestamp":{"t":1795800001,"i":1}}}},"$db":"app"} }`,
		`{ SectionSingle msg: {"ok":1} }`,
	)
	ts2 := mongoOpMsgMock("ts2", "mongo:27017",
		`{ SectionSingle msg: {"find":"oplog_cursors","filter":{"ts":{"$gt":{"$timestamp":{"t":1795803605,"i":7}}}},"$db":"app"} }`,
		`{ SectionSingle msg: {"ok":1} }`,
	)
	k1, ht1 := WorkerMockCallKeyDetails(ts1)
	k2, ht2 := WorkerMockCallKeyDetails(ts2)
	if !ht1 || !ht2 || k1 != k2 {
		t.Fatalf("BSON $timestamp-only request did not normalize with hasTime=true: ht=(%v,%v) eq=%v", ht1, ht2, k1 == k2)
	}
}

func TestMongoWorkerRuleDeclaredTimeFields(t *testing.T) {
	// Validation: valid Mongo timeFields pass; invalid or non-Mongo fail.
	validRule := WorkerMockRule{
		ID:         "retention-sweep",
		Kind:       "Mongo",
		Match:      map[string]string{"db": "app", "command": "update", "collection": "audit_logs"},
		TimeFields: []string{"last_active", "meta.cutoff"},
	}
	if probs := validRule.Validate(); len(probs) != 0 {
		t.Fatalf("valid Mongo rule with timeFields failed validation: %v", probs)
	}
	for _, tc := range []struct {
		name string
		rule WorkerMockRule
	}{
		{"non-Mongo kind", WorkerMockRule{ID: "http-tf", Kind: "Http", Match: map[string]string{"path": "/health"}, TimeFields: []string{"ts"}}},
		{"empty entry", WorkerMockRule{ID: "empty-tf", Kind: "Mongo", Match: map[string]string{"db": "app", "collection": "c"}, TimeFields: []string{""}}},
		{"whitespace", WorkerMockRule{ID: "ws-tf", Kind: "Mongo", Match: map[string]string{"db": "app", "collection": "c"}, TimeFields: []string{"last active"}}},
		{"operator segment", WorkerMockRule{ID: "op-tf", Kind: "Mongo", Match: map[string]string{"db": "app", "collection": "c"}, TimeFields: []string{"$lt"}}},
		{"empty dot segment", WorkerMockRule{ID: "dot-tf", Kind: "Mongo", Match: map[string]string{"db": "app", "collection": "c"}, TimeFields: []string{"meta..cutoff"}}},
	} {
		if probs := tc.rule.Validate(); len(probs) == 0 {
			t.Errorf("%s: expected validation error, got none", tc.name)
		}
	}

	// A 2-year retention cutoff (> 400 days from ReqTimestampMock) stays
	// distinct without declaration, and normalizes with hasTime=true when
	// declared on the rule (including dotted paths and kind-1 updates), while
	// undeclared sibling IDs stay distinct.
	at := time.Date(2026, 11, 28, 10, 0, 0, 0, time.UTC)
	c1 := at.AddDate(-2, 0, 0).Unix()
	c2 := at.AddDate(-2, 0, -5).Unix()
	sweep1 := &Mock{
		Name: "sweep1", Kind: Mongo,
		Spec: MockSpec{
			Metadata:         map[string]string{"destAddr": "mongo:27017"},
			ReqTimestampMock: at,
			MongoRequests: []MongoRequest{{Message: &MongoOpMessage{Sections: []string{
				fmt.Sprintf(`{ SectionSingle identifier: updates , msgs: [ {"q":{"last_active":{"$lt":{"$numberLong":"%d"}},"meta":{"cutoff":{"$numberLong":"%d"}},"orderId":{"$numberLong":"1234567890"}}} ] }`, c1, c1),
				`{ SectionSingle msg: {"update":"audit_logs","ordered":true,"$db":"app"} }`,
			}}}},
		},
	}
	sweep2 := &Mock{
		Name: "sweep2", Kind: Mongo,
		Spec: MockSpec{
			Metadata:         map[string]string{"destAddr": "mongo:27017"},
			ReqTimestampMock: at.Add(time.Hour),
			MongoRequests: []MongoRequest{{Message: &MongoOpMessage{Sections: []string{
				fmt.Sprintf(`{ SectionSingle identifier: updates , msgs: [ {"q":{"last_active":{"$lt":{"$numberLong":"%d"}},"meta":{"cutoff":{"$numberLong":"%d"}},"orderId":{"$numberLong":"1234567890"}}} ] }`, c2, c2),
				`{ SectionSingle msg: {"update":"audit_logs","ordered":true,"$db":"app"} }`,
			}}}},
		},
	}
	sweepOtherID := &Mock{
		Name: "sweepOtherID", Kind: Mongo,
		Spec: MockSpec{
			Metadata:         map[string]string{"destAddr": "mongo:27017"},
			ReqTimestampMock: at.Add(time.Hour),
			MongoRequests: []MongoRequest{{Message: &MongoOpMessage{Sections: []string{
				fmt.Sprintf(`{ SectionSingle identifier: updates , msgs: [ {"q":{"last_active":{"$lt":{"$numberLong":"%d"}},"meta":{"cutoff":{"$numberLong":"%d"}},"orderId":{"$numberLong":"1234567891"}}} ] }`, c2, c2),
				`{ SectionSingle msg: {"update":"audit_logs","ordered":true,"$db":"app"} }`,
			}}}},
		},
	}
	// Without declaration: > 400 days away -> distinct, hasTime=false.
	ku1, htu1 := WorkerMockCallKeyDetails(sweep1)
	ku2, htu2 := WorkerMockCallKeyDetails(sweep2)
	if htu1 || htu2 || ku1 == ku2 {
		t.Fatalf("undeclared 2-year cutoff was normalized: ht=(%v,%v) eq=%v", htu1, htu2, ku1 == ku2)
	}
	// With declaration: normalizes declared fields even > 400 days away, keeps undeclared orderId apart.
	kd1, htd1 := WorkerMockCallKeyForRule(sweep1, validRule)
	kd2, htd2 := WorkerMockCallKeyForRule(sweep2, validRule)
	kdOther, _ := WorkerMockCallKeyForRule(sweepOtherID, validRule)
	if !htd1 || !htd2 || kd1 != kd2 {
		t.Fatalf("declared timeFields 2-year cutoff did not normalize: ht=(%v,%v) eq=%v", htd1, htd2, kd1 == kd2)
	}
	if kdOther == kd1 {
		t.Fatalf("undeclared orderId collapsed under declared timeFields rule")
	}

	// Declared timeFields still preserve epoch resolution (seconds vs milliseconds).
	sweepMs := &Mock{
		Name: "sweepMs", Kind: Mongo,
		Spec: MockSpec{
			Metadata:         map[string]string{"destAddr": "mongo:27017"},
			ReqTimestampMock: at,
			MongoRequests: []MongoRequest{{Message: &MongoOpMessage{Sections: []string{
				fmt.Sprintf(`{ SectionSingle identifier: updates , msgs: [ {"q":{"last_active":{"$lt":{"$numberLong":"%d"}},"meta":{"cutoff":{"$numberLong":"%d"}},"orderId":{"$numberLong":"1234567890"}}} ] }`, c1*1000, c1*1000),
				`{ SectionSingle msg: {"update":"audit_logs","ordered":true,"$db":"app"} }`,
			}}}},
		},
	}
	kdMs, htdMs := WorkerMockCallKeyForRule(sweepMs, validRule)
	if !htdMs || kdMs == kd1 {
		t.Fatalf("declared timeFields collapsed epoch_ms onto epoch_s: ht=%v eq=%v", htdMs, kdMs == kd1)
	}

	// Declaring a parent field must not leak time normalization into undeclared
	// non-operator child fields inside that sub-document.
	parentRule := WorkerMockRule{
		ID:         "window-sweep",
		Kind:       "Mongo",
		Match:      map[string]string{"db": "app", "command": "find", "collection": "audit_logs"},
		TimeFields: []string{"window"},
	}
	childA := mongoOpMsgMock("childA", "mongo:27017",
		`{ SectionSingle msg: {"find":"audit_logs","filter":{"window":{"orderId":{"$numberLong":"1234567890"}}},"$db":"app"} }`,
		`{ SectionSingle msg: {"ok":1} }`,
	)
	childB := mongoOpMsgMock("childB", "mongo:27017",
		`{ SectionSingle msg: {"find":"audit_logs","filter":{"window":{"orderId":{"$numberLong":"1234567891"}}},"$db":"app"} }`,
		`{ SectionSingle msg: {"ok":1} }`,
	)
	kChildA, htChildA := WorkerMockCallKeyForRule(childA, parentRule)
	kChildB, _ := WorkerMockCallKeyForRule(childB, parentRule)
	if htChildA || kChildA == kChildB {
		t.Fatalf("declared parent timeField leaked into undeclared nested child field: ht=%v eq=%v", htChildA, kChildA == kChildB)
	}
}

// A declared time field is matched by its name or dot path in both of Mongo's
// forms: nested ({"run":{"cutoff":...}}) and dotted ({"run.cutoff":...}), as
// well as positional/indexed array paths ({"items.$.at":...}, {"items.0.at":...}).
func TestMongoWorkerRuleDeclaredTimeFieldsMatchDottedKeys(t *testing.T) {
	at := time.Date(2026, 11, 28, 10, 0, 0, 0, time.UTC)
	old := at.AddDate(-2, 0, 0).Unix()
	find := func(filter string) *Mock {
		m := mongoOpMsgMock("f", "mongo:27017", `{ SectionSingle msg: {"find":"jobs","filter":`+filter+`,"$db":"app"} }`, `{ SectionSingle msg: {"ok":1} }`)
		m.Spec.ReqTimestampMock = at
		return m
	}
	for _, c := range []struct{ decl, shape string }{
		{"cutoff", `{"run":{"cutoff":{"$lt":{"$numberLong":"%d"}}}}`},
		{"cutoff", `{"run.cutoff":{"$lt":{"$numberLong":"%d"}}}`},
		{"run.cutoff", `{"run.cutoff":{"$lt":{"$numberLong":"%d"}}}`},
		{"run.cutoff", `{"job.run.cutoff":{"$lt":{"$numberLong":"%d"}}}`},
		{"run.cutoff", `{"job":{"run":{"cutoff":{"$lt":{"$numberLong":"%d"}}}}}`},
		{"meta.cutoff", `{"meta":{"cutoff":{"$lt":{"$numberLong":"%d"}}}}`},
		{"items.at", `{"items.$.at":{"$lt":{"$numberLong":"%d"}}}`},
		{"items.at", `{"items.0.at":{"$lt":{"$numberLong":"%d"}}}`},
		{"items.at", `{"items.$[].at":{"$lt":{"$numberLong":"%d"}}}`},
		{"items.at", `{"items.$[elem].at":{"$lt":{"$numberLong":"%d"}}}`},
	} {
		rule := WorkerMockRule{ID: "sweep", Kind: "Mongo", Match: map[string]string{"db": "app", "collection": "jobs"}, TimeFields: []string{c.decl}}
		k1, ht1 := WorkerMockCallKeyForRule(find(fmt.Sprintf(c.shape, old)), rule)
		k2, ht2 := WorkerMockCallKeyForRule(find(fmt.Sprintf(c.shape, old+7)), rule)
		if !ht1 || !ht2 || k1 != k2 {
			t.Errorf("timeFields %q did not normalize %s: ht=(%v,%v) one key=%v", c.decl, c.shape, ht1, ht2, k1 == k2)
		}
	}
	// A field whose name only ends like a declared one (or a sibling under a
	// positional array) must not be normalized.
	rule := WorkerMockRule{ID: "sweep", Kind: "Mongo", Match: map[string]string{"db": "app", "collection": "jobs"}, TimeFields: []string{"cutoff", "items.at"}}
	for _, shape := range []string{
		`{"run.xcutoff":{"$numberLong":"%d"}}`,
		`{"items.$.other":{"$numberLong":"%d"}}`,
	} {
		k1, ht1 := WorkerMockCallKeyForRule(find(fmt.Sprintf(shape, int64(1444555666777888999))), rule)
		k2, ht2 := WorkerMockCallKeyForRule(find(fmt.Sprintf(shape, int64(1444555666777889000))), rule)
		if ht1 || ht2 || k1 == k2 {
			t.Errorf("timeFields unexpectedly normalized %s: ht=(%v,%v) eq=%v", shape, ht1, ht2, k1 == k2)
		}
	}
}

func TestMongoWorkerRuleDeclaredTimeFieldsEdgeCases(t *testing.T) {
	at := time.Date(2026, 11, 28, 10, 0, 0, 0, time.UTC)
	old := at.AddDate(-2, 0, 0).Unix()

	// Exact dotPath match at the root of a find filter (no leading prefix).
	exactRule := WorkerMockRule{
		ID:         "exact-path",
		Kind:       "Mongo",
		Match:      map[string]string{"db": "app", "collection": "jobs"},
		TimeFields: []string{"meta.cutoff"},
	}
	ex1 := mongoOpMsgMock("ex1", "mongo:27017", fmt.Sprintf(`{ SectionSingle msg: {"find":"jobs","filter":{"meta":{"cutoff":{"$lt":{"$numberLong":"%d"}}}},"$db":"app"} }`, old), `{ SectionSingle msg: {"ok":1} }`)
	ex2 := mongoOpMsgMock("ex2", "mongo:27017", fmt.Sprintf(`{ SectionSingle msg: {"find":"jobs","filter":{"meta":{"cutoff":{"$lt":{"$numberLong":"%d"}}}},"$db":"app"} }`, old+9), `{ SectionSingle msg: {"ok":1} }`)
	ex1.Spec.ReqTimestampMock, ex2.Spec.ReqTimestampMock = at, at
	kEx1, htEx1 := WorkerMockCallKeyForRule(ex1, exactRule)
	kEx2, htEx2 := WorkerMockCallKeyForRule(ex2, exactRule)
	if !htEx1 || !htEx2 || kEx1 != kEx2 {
		t.Fatalf("exact dotPath meta.cutoff in find filter did not normalize: ht=(%v,%v) eq=%v", htEx1, htEx2, kEx1 == kEx2)
	}

	// Non-RFC3339 string values inside a declared timeField normalize, while
	// undeclared strings stay distinct.
	strRule := WorkerMockRule{
		ID:         "str-time",
		Kind:       "Mongo",
		Match:      map[string]string{"db": "app", "collection": "jobs"},
		TimeFields: []string{"cutoff"},
	}
	s1 := mongoOpMsgMock("s1", "mongo:27017", `{ SectionSingle msg: {"find":"jobs","filter":{"cutoff":"2024-05-01 00:00:00"},"$db":"app"} }`, `{ SectionSingle msg: {"ok":1} }`)
	s2 := mongoOpMsgMock("s2", "mongo:27017", `{ SectionSingle msg: {"find":"jobs","filter":{"cutoff":"2024-05-02 00:00:00"},"$db":"app"} }`, `{ SectionSingle msg: {"ok":1} }`)
	s1.Spec.ReqTimestampMock, s2.Spec.ReqTimestampMock = at, at
	kS1, htS1 := WorkerMockCallKeyForRule(s1, strRule)
	kS2, htS2 := WorkerMockCallKeyForRule(s2, strRule)
	if !htS1 || !htS2 || kS1 != kS2 {
		t.Fatalf("declared non-RFC3339 string timeField did not normalize: ht=(%v,%v) eq=%v", htS1, htS2, kS1 == kS2)
	}
	if ku1, htu1 := WorkerMockCallKeyDetails(s1); htu1 || ku1 == func() string { k, _ := WorkerMockCallKeyDetails(s2); return k }() {
		t.Fatalf("undeclared non-RFC3339 string unexpectedly normalized: ht=%v", htu1)
	}

	// Array elements ($in) inside a declared timeField keep declared status.
	arr1 := mongoOpMsgMock("a1", "mongo:27017", fmt.Sprintf(`{ SectionSingle msg: {"find":"jobs","filter":{"cutoff":{"$in":[{"$numberLong":"%d"},{"$numberLong":"%d"}]}},"$db":"app"} }`, old, old+1), `{ SectionSingle msg: {"ok":1} }`)
	arr2 := mongoOpMsgMock("a2", "mongo:27017", fmt.Sprintf(`{ SectionSingle msg: {"find":"jobs","filter":{"cutoff":{"$in":[{"$numberLong":"%d"},{"$numberLong":"%d"}]}},"$db":"app"} }`, old+10, old+11), `{ SectionSingle msg: {"ok":1} }`)
	arr1.Spec.ReqTimestampMock, arr2.Spec.ReqTimestampMock = at, at
	kA1, htA1 := WorkerMockCallKeyForRule(arr1, strRule)
	kA2, htA2 := WorkerMockCallKeyForRule(arr2, strRule)
	if !htA1 || !htA2 || kA1 != kA2 {
		t.Fatalf("declared timeField array elements did not normalize: ht=(%v,%v) eq=%v", htA1, htA2, kA1 == kA2)
	}

	// Legacy OP_QUERY commands pass declared timeFields through to query normalization.
	oq1 := &Mock{
		Name: "oq1", Kind: Mongo,
		Spec: MockSpec{
			Metadata:         map[string]string{"destAddr": "mongo:27017"},
			ReqTimestampMock: at,
			MongoRequests: []MongoRequest{{Message: &MongoOpQuery{
				FullCollectionName: "app.jobs",
				Query:              fmt.Sprintf(`{"cutoff":{"$lt":{"$numberLong":"%d"}}}`, old),
			}}},
		},
	}
	oq2 := &Mock{
		Name: "oq2", Kind: Mongo,
		Spec: MockSpec{
			Metadata:         map[string]string{"destAddr": "mongo:27017"},
			ReqTimestampMock: at,
			MongoRequests: []MongoRequest{{Message: &MongoOpQuery{
				FullCollectionName: "app.jobs",
				Query:              fmt.Sprintf(`{"cutoff":{"$lt":{"$numberLong":"%d"}}}`, old+5),
			}}},
		},
	}
	kOQ1, htOQ1 := WorkerMockCallKeyForRule(oq1, strRule)
	kOQ2, htOQ2 := WorkerMockCallKeyForRule(oq2, strRule)
	if !htOQ1 || !htOQ2 || kOQ1 != kOQ2 {
		t.Fatalf("OP_QUERY declared timeField did not normalize: ht=(%v,%v) eq=%v", htOQ1, htOQ2, kOQ1 == kOQ2)
	}

	// Bare json.Number values: fractional epoch honors the 400-day near-call
	// window (near normalizes, 2 years away stays distinct), and declared
	// bare json.Number normalizes even 2 years from the call.
	nearSec := at.Unix() - 5
	fracNear1 := mongoOpMsgMock("fn1", "mongo:27017", fmt.Sprintf(`{ SectionSingle msg: {"find":"jobs","filter":{"ts":%d.123},"$db":"app"} }`, nearSec), `{ SectionSingle msg: {"ok":1} }`)
	fracNear2 := mongoOpMsgMock("fn2", "mongo:27017", fmt.Sprintf(`{ SectionSingle msg: {"find":"jobs","filter":{"ts":%d.456},"$db":"app"} }`, nearSec+2), `{ SectionSingle msg: {"ok":1} }`)
	fracNear1.Spec.ReqTimestampMock, fracNear2.Spec.ReqTimestampMock = at, at
	kFN1, htFN1 := WorkerMockCallKeyDetails(fracNear1)
	kFN2, htFN2 := WorkerMockCallKeyDetails(fracNear2)
	if !htFN1 || !htFN2 || kFN1 != kFN2 {
		t.Fatalf("near-call fractional epoch did not normalize: ht=(%v,%v) eq=%v", htFN1, htFN2, kFN1 == kFN2)
	}
	fracFar1 := mongoOpMsgMock("ff1", "mongo:27017", fmt.Sprintf(`{ SectionSingle msg: {"find":"jobs","filter":{"ts":%d.123},"$db":"app"} }`, old), `{ SectionSingle msg: {"ok":1} }`)
	fracFar2 := mongoOpMsgMock("ff2", "mongo:27017", fmt.Sprintf(`{ SectionSingle msg: {"find":"jobs","filter":{"ts":%d.456},"$db":"app"} }`, old+2), `{ SectionSingle msg: {"ok":1} }`)
	fracFar1.Spec.ReqTimestampMock, fracFar2.Spec.ReqTimestampMock = at, at
	kFF1, htFF1 := WorkerMockCallKeyDetails(fracFar1)
	kFF2, htFF2 := WorkerMockCallKeyDetails(fracFar2)
	if htFF1 || htFF2 || kFF1 == kFF2 {
		t.Fatalf("far-from-call fractional epoch unexpectedly normalized: ht=(%v,%v) eq=%v", htFF1, htFF2, kFF1 == kFF2)
	}
	bareDecl1 := mongoOpMsgMock("bd1", "mongo:27017", fmt.Sprintf(`{ SectionSingle msg: {"find":"jobs","filter":{"cutoff":%d},"$db":"app"} }`, old), `{ SectionSingle msg: {"ok":1} }`)
	bareDecl2 := mongoOpMsgMock("bd2", "mongo:27017", fmt.Sprintf(`{ SectionSingle msg: {"find":"jobs","filter":{"cutoff":%d},"$db":"app"} }`, old+3), `{ SectionSingle msg: {"ok":1} }`)
	bareDecl1.Spec.ReqTimestampMock, bareDecl2.Spec.ReqTimestampMock = at, at
	kBD1, htBD1 := WorkerMockCallKeyForRule(bareDecl1, strRule)
	kBD2, htBD2 := WorkerMockCallKeyForRule(bareDecl2, strRule)
	if !htBD1 || !htBD2 || kBD1 != kBD2 {
		t.Fatalf("declared bare numeric timeField did not normalize: ht=(%v,%v) eq=%v", htBD1, htBD2, kBD1 == kBD2)
	}

	// Validation enforces maxWorkerRuleTimeFields (32 passes, 33 fails).
	thirtyTwo := make([]string, maxWorkerRuleTimeFields)
	for i := range thirtyTwo {
		thirtyTwo[i] = fmt.Sprintf("f%d", i)
	}
	okCap := WorkerMockRule{ID: "cap-ok", Kind: "Mongo", Match: map[string]string{"db": "app", "collection": "jobs"}, TimeFields: thirtyTwo}
	if probs := okCap.Validate(); len(probs) != 0 {
		t.Fatalf("32 timeFields failed validation: %v", probs)
	}
	overCap := WorkerMockRule{ID: "cap-over", Kind: "Mongo", Match: map[string]string{"db": "app", "collection": "jobs"}, TimeFields: append(append([]string(nil), thirtyTwo...), "f32")}
	if probs := overCap.Validate(); len(probs) == 0 {
		t.Fatalf("33 timeFields passed validation; want max %d error", maxWorkerRuleTimeFields)
	}
}
