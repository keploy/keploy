package models

import (
	"encoding/base64"
	"strconv"
	"strings"
	"testing"

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
