package models

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"go.keploy.io/server/v3/pkg/models/mysql"
)

// WorkerMockRule is a user's rule for a call the application makes on its own
// rather than for one test: typically once in each of its worker processes,
// on that process's first request (a JWKS fetch, an ORM's table schema read).
// At record, each mock a rule matches is added to the set's worker scope
// (Mapping.Worker) with Reason WorkerRuleReason(the rule's id), and a replay
// without the proven switch serves it to every test (ServeAsSession), so a
// test that reaches a cold worker process is answered whichever test
// recorded the call.
//
// Kind is the mock's kind (Http, Http2, MySQL, PostgresV3, Redis, or any other
// kind for a destination-only rule), and Match its keys, all of which must
// hold:
//
//   - every kind: "destination", a glob (path.Match) on the host:port the
//     call's connection reached (the mock's destAddr).
//   - Http, Http2: "method" (case-insensitive), "host" (a glob on the request's
//     host; without a port it matches any port), "path" (a glob) and
//     "pathRegex". Keys are compared without case.
//   - MySQL: "query", a regex every statement the mock records must match
//     (COM_QUERY and COM_STMT_PREPARE text, whitespace collapsed).
//   - PostgresV3: "query", a regex on the mock's normalized statement.
//   - Redis: "command" (case-insensitive) and "key", a regex on its first
//     argument; every command the mock records must match.
//   - Http, Http2, MySQL, Redis: "response", an optional regex on what the
//     call answered (the HTTP body; the MySQL result packets as JSON, column
//     names and values included, so a quote or backslash in a value reads
//     escaped, \" and \\, and bytes that are not UTF-8 read as U+FFFD; the
//     Redis reply), to name one of a call's copies (the one carrying a new
//     key, say). Only a user's rule holds it: a rule derived from a mock
//     (SuggestWorkerMockRule) never does.
//
// Regexes are RE2 (linear time) and unanchored: a rule anchors with ^ and $.
type WorkerMockRule struct {
	// ID names the rule in the entries it marks (WorkerRuleReason). Empty, it
	// is derived from the rule's kind and match (EffectiveID).
	ID    string            `json:"id,omitempty" yaml:"id,omitempty" mapstructure:"id"`
	Kind  string            `json:"kind" yaml:"kind" mapstructure:"kind"`
	Match map[string]string `json:"match" yaml:"match" mapstructure:"match"`
	// Note says why the rule exists, for whoever reads it next.
	Note string `json:"note,omitempty" yaml:"note,omitempty" mapstructure:"note"`
	// Source, CreatedBy and CreatedAt say who added the rule and how (ui, cli,
	// agent). They are kept with the rule and never matched on.
	Source    string `json:"source,omitempty" yaml:"source,omitempty" mapstructure:"source"`
	CreatedBy string `json:"createdBy,omitempty" yaml:"createdBy,omitempty" mapstructure:"createdBy"`
	CreatedAt int64  `json:"createdAt,omitempty" yaml:"createdAt,omitempty" mapstructure:"createdAt"`
}

// Limits on a rule set: rules per application, and a regex's length.
const (
	MaxWorkerMockRules     = 50
	maxWorkerRuleRegex     = 512
	workerRuleIDPattern    = `^[a-z0-9][a-z0-9-]{0,63}$`
	workerRuleDerivedIDLen = 12
)

var workerRuleID = regexp.MustCompile(workerRuleIDPattern)

// workerRuleKeys are the match keys each kind reads; "destination" is read for
// every kind.
var workerRuleKeys = map[Kind][]string{
	HTTP:       {"method", "host", "path", "pathregex", "response"},
	HTTP2:      {"method", "host", "path", "pathregex", "response"},
	MySQL:      {"query", "response"},
	PostgresV3: {"query"},
	REDIS:      {"command", "key", "response"},
}

// workerRuleRegexKeys are the keys whose values are regexes.
var workerRuleRegexKeys = map[string]bool{"pathregex": true, "query": true, "key": true, "response": true}

// workerRuleKinds are the kinds this tree records. A rule naming one of them
// with another case (http for Http) is refused, as the typo it is; a kind not
// here (one an enterprise parser records: Couchbase, Pulsar, ...) is taken as
// written, matched by its destination.
var workerRuleKinds = map[Kind]bool{
	HTTP: true, HTTP2: true, GENERIC: true, REDIS: true, KAFKA: true, MySQL: true,
	Postgres: true, PostgresV2: true, PostgresV3: true, Mongo: true, Aerospike: true,
	GRPC_EXPORT: true, DNS: true,
}

// match is the rule's match keys lowercased: keploy.yml is read by viper,
// which lowercases map keys (pathRegex arrives as pathregex), so keys are
// compared without case everywhere.
func (r WorkerMockRule) match() map[string]string {
	out := make(map[string]string, len(r.Match))
	for k, v := range r.Match {
		out[strings.ToLower(k)] = v
	}
	return out
}

// EffectiveID is the rule's ID, or one derived from its kind and match when it
// names none: the same rule always derives the same ID.
func (r WorkerMockRule) EffectiveID() string {
	mt := r.match()
	if r.ID != "" {
		return r.ID
	}
	keys := make([]string, 0, len(mt))
	for k := range mt {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "%s\n", r.Kind)
	for _, k := range keys {
		_, _ = fmt.Fprintf(h, "%d:%s=%d:%s\n", len(k), k, len(mt[k]), mt[k])
	}
	return "r-" + hex.EncodeToString(h.Sum(nil))[:workerRuleDerivedIDLen]
}

// Validate is what is wrong with the rule, one message each; none for a
// rule a recording can apply.
func (r WorkerMockRule) Validate() []string {
	mt := r.match()
	var problems []string
	if r.ID != "" && !workerRuleID.MatchString(r.ID) {
		problems = append(problems, fmt.Sprintf("its id %q is not lower-case letters, digits and dashes, at most 64", r.ID))
	}
	if strings.TrimSpace(r.Kind) == "" {
		problems = append(problems, "it names no kind")
	} else if !workerRuleKinds[Kind(r.Kind)] {
		for k := range workerRuleKinds {
			if strings.EqualFold(string(k), r.Kind) {
				problems = append(problems, fmt.Sprintf("its kind %q is spelled %q in a mock", r.Kind, k))
				break
			}
		}
	}
	allowed := map[string]bool{"destination": true}
	for _, k := range workerRuleKeys[Kind(r.Kind)] {
		allowed[k] = true
	}
	set := 0
	for k, v := range mt {
		if !allowed[k] {
			problems = append(problems, fmt.Sprintf("its match key %q is not one a %s rule reads (%s)", k, r.Kind, strings.Join(sortedKeys(allowed), ", ")))
			continue
		}
		if strings.TrimSpace(v) == "" {
			problems = append(problems, fmt.Sprintf("its match key %q is empty", k))
			continue
		}
		set++
		switch {
		case workerRuleRegexKeys[k]:
			if len(v) > maxWorkerRuleRegex {
				problems = append(problems, fmt.Sprintf("its %s is longer than %d characters", k, maxWorkerRuleRegex))
			} else if _, err := compiledWorkerRegex(v); err != nil {
				problems = append(problems, fmt.Sprintf("its %s is not a regex: %v", k, err))
			} else if matchesEverything(v) {
				problems = append(problems, fmt.Sprintf("its %s matches everything", k))
			}
		case k == "destination" || k == "host" || k == "path":
			if _, err := path.Match(v, ""); err != nil {
				problems = append(problems, fmt.Sprintf("its %s is not a glob: %v", k, err))
			}
		}
	}
	if set == 0 && strings.TrimSpace(r.Kind) != "" {
		problems = append(problems, "it matches on nothing: set at least one match key")
	}
	// The request names the call; a response alone would name every call of
	// the kind that answered alike. A request key that matches everything
	// names none.
	// A method or a Redis command alone names no call either (every GET to
	// any host, every GET of any key), so a response rule needs more.
	naming, callNaming, regexAll := 0, 0, false
	for k, v := range mt {
		switch {
		case k == "response", strings.TrimSpace(v) == "":
		case workerRuleRegexKeys[k] && matchesEverything(v):
			regexAll = true // said above
		case (k == "host" || k == "destination") && globMatchesEverything(k, v):
		default:
			naming++
			if k != "method" && k != "command" {
				callNaming++
			}
		}
	}
	if response := strings.TrimSpace(mt["response"]) != ""; response && callNaming == 0 {
		problems = append(problems, "it matches on the response alone: name the call too (a request key, or destination)")
	} else if !response && naming == 0 && set > 0 && !regexAll {
		if set == 1 {
			problems = append(problems, "its only match key matches everything")
		} else {
			problems = append(problems, "every match key of it matches everything")
		}
	}
	return problems
}

// ValidateWorkerMockRules is what is wrong with rules as a set: each rule's
// problems (Validate), named by its position, plus too many rules or two with
// one id.
func ValidateWorkerMockRules(rules []WorkerMockRule) []string {
	var problems []string
	if len(rules) > MaxWorkerMockRules {
		problems = append(problems, fmt.Sprintf("there are %d worker-mock rules; at most %d are allowed", len(rules), MaxWorkerMockRules))
	}
	ids := map[string]int{}
	for i, r := range rules {
		for _, p := range r.Validate() {
			problems = append(problems, fmt.Sprintf("worker-mock rule %d: %s", i+1, p))
		}
		id := r.EffectiveID()
		if j, dup := ids[id]; dup {
			problems = append(problems, fmt.Sprintf("worker-mock rules %d and %d have one id, %q", j+1, i+1, id))
			continue
		}
		ids[id] = i
	}
	return problems
}

// matchesEverything reports whether the regex v matches every probe below,
// strings that share nothing (a letter, SQL, a key, a path): a rule on it
// would mark every call. A regex such as "." or "(?s)."
// that matches any non-empty value is caught as well as ".*".
func matchesEverything(v string) bool {
	re, err := compiledWorkerRegex(v)
	if err != nil {
		return false
	}
	for _, probe := range []string{"x", "SELECT 1", "Q7:zz", "/a/b?c=d"} {
		if !re.MatchString(probe) {
			return false
		}
	}
	return true
}

// globMatchesEverything reports whether the host or destination glob g
// matches every probe: "*", "**", "*:*" and the like.
func globMatchesEverything(key, g string) bool {
	probes := []string{"x", "auth.example.com", "10.0.0.1:3306"}
	if key == "destination" {
		// A destination is always a host and port.
		probes = []string{"x:1", "auth.example.com:443", "10.0.0.1:3306"}
	}
	for _, probe := range probes {
		if ok, _ := path.Match(g, probe); !ok {
			return false
		}
	}
	return true
}

// MatchWorkerMockRules is the id of the first rule in rules that matches m,
// and whether one did. Rules are validated where they are read
// (ValidateWorkerMockRules); one that reads a key its kind does not, or whose
// regex does not compile, matches nothing here.
func MatchWorkerMockRules(rules []WorkerMockRule, m *Mock) (string, bool) {
	if m == nil {
		return "", false
	}
	for _, r := range rules {
		if r.Matches(m) {
			return r.EffectiveID(), true
		}
	}
	return "", false
}

// WorkerMockCallKey is the call m records: an HTTP request's method, host,
// path and query, with a digest of its body when it has one; the statements
// of a SQL mock and the commands of a Redis one, with the destination they
// went to. Two mocks with one key record the same call (another worker's copy
// of it), and one of them, served to every test for the session, answers it
// for all. A mock whose call keploy cannot name (another kind, or one that
// records no statement or command it reads) is a call of its own: its key
// names the mock, so it is never taken for another's copy.
func WorkerMockCallKey(m *Mock) string {
	if m == nil {
		return ""
	}
	kind := string(m.Kind)
	dest := strings.TrimSpace(m.Spec.Metadata["destAddr"])
	own := kind + "\x00mock\x00" + m.Name
	switch m.Kind {
	case HTTP, HTTP2:
		method, host, p, ok := httpCallOf(m)
		if !ok {
			return own
		}
		query, body := httpQueryAndBodyOf(m)
		parts := []string{kind, strings.ToUpper(method), strings.ToLower(host), p, query}
		if body != "" {
			sum := sha256.Sum256([]byte(body))
			parts = append(parts, hex.EncodeToString(sum[:]))
		}
		return strings.Join(parts, "\x00")
	case MySQL:
		if st := mysqlStatements(m); len(st) > 0 {
			return strings.Join(append([]string{kind, dest}, st...), "\x00")
		}
	case PostgresV3:
		if st := postgresV3Statements(m); len(st) > 0 {
			parts := append([]string{kind, dest}, st...)
			// The normalized statement has its constants as $N: the
			// values (each table's schema read names its table there)
			// are the call too.
			if q := m.Spec.PostgresV3.Query; len(q.BindValues) > 0 || len(q.Literals) > 0 {
				raw, err := json.Marshal(struct{ B, L PostgresV3Cells }{q.BindValues, q.Literals})
				if err != nil {
					return own
				}
				sum := sha256.Sum256(raw)
				parts = append(parts, hex.EncodeToString(sum[:]))
			}
			return strings.Join(parts, "\x00")
		}
	case REDIS:
		if cmds := redisCommands(m); len(cmds) > 0 {
			parts := []string{kind, dest}
			for _, c := range cmds {
				parts = append(parts, strings.Join(c, " "))
			}
			return strings.Join(parts, "\x00")
		}
	}
	return own
}

// httpQueryAndBodyOf is the query string and body of m's HTTP request.
func httpQueryAndBodyOf(m *Mock) (query, body string) {
	var raw string
	switch {
	case m.Kind == HTTP && m.Spec.HTTPReq != nil:
		raw, body = m.Spec.HTTPReq.URL, m.Spec.HTTPReq.Body
	case m.Kind == HTTP2 && m.Spec.HTTP2Req != nil:
		raw, body = m.Spec.HTTP2Req.URL, m.Spec.HTTP2Req.Body
	}
	if u, err := ParseRequestURL(raw); err == nil {
		query = u.RawQuery
	}
	return query, body
}

// Matches reports whether m is a call the rule names: its kind is the rule's
// and every match key holds.
func (r WorkerMockRule) Matches(m *Mock) bool {
	mt := r.match()
	if m == nil || string(m.Kind) != r.Kind || !r.applicable() {
		return false
	}
	if g, ok := mt["destination"]; ok {
		dest := strings.TrimSpace(m.Spec.Metadata["destAddr"])
		if ok, _ := path.Match(g, dest); dest == "" || !ok {
			return false
		}
	}
	request := true
	switch m.Kind {
	case HTTP, HTTP2:
		request = r.matchesHTTP(m)
	case MySQL:
		request = r.matchesStatements(mysqlStatements(m))
	case PostgresV3:
		request = r.matchesStatements(postgresV3Statements(m))
	case REDIS:
		request = r.matchesRedis(m)
	}
	if !request {
		return false
	}
	// The response last: reading it (a MySQL result as JSON) costs more than
	// any request key.
	if v, set := mt["response"]; set && !workerRegexMatches(v, workerMockResponseText(m)) {
		return false
	}
	return true
}

// workerMockResponseText is what m's call answered, as a rule's "response"
// reads it: the HTTP body, the MySQL result packets as JSON (one per line),
// the Redis reply. "" for a kind whose answer it does not read.
func workerMockResponseText(m *Mock) string {
	switch m.Kind {
	case HTTP:
		if m.Spec.HTTPResp != nil {
			return m.Spec.HTTPResp.Body
		}
	case HTTP2:
		if m.Spec.HTTP2Resp != nil {
			return m.Spec.HTTP2Resp.Body
		}
	case MySQL:
		// JSON as written, < > & included, so a regex reads the values
		// as the rows hold them.
		var b strings.Builder
		enc := json.NewEncoder(&b)
		enc.SetEscapeHTML(false)
		for _, resp := range m.Spec.MySQLResponses {
			_ = enc.Encode(resp.Message) // one JSON value and a newline each
		}
		return b.String()
	case REDIS:
		var b strings.Builder
		for _, p := range m.Spec.GenericResponses {
			for _, msg := range p.Message {
				data := msg.Data
				if msg.Type == "binary" {
					if raw, err := base64.StdEncoding.DecodeString(msg.Data); err == nil {
						data = string(raw)
					}
				}
				b.WriteString(data)
			}
		}
		return b.String()
	}
	return ""
}

// applicable is the part of Validate a match needs, cheap enough to run for
// every mock a recording writes: the rule sets a match key, and reads only
// keys its kind reads. A regex that does not compile fails its key's match
// (workerRegexMatches), and a glob that does not parse fails path.Match.
func (r WorkerMockRule) applicable() bool {
	mt := r.match()
	if len(mt) == 0 {
		return false
	}
	reads := workerRuleKeys[Kind(r.Kind)]
	for k, v := range mt {
		if strings.TrimSpace(v) == "" {
			return false
		}
		if k == "destination" {
			continue
		}
		known := false
		for _, rk := range reads {
			known = known || rk == k
		}
		if !known {
			return false
		}
	}
	return true
}

func (r WorkerMockRule) matchesHTTP(m *Mock) bool {
	mt := r.match()
	method, host, p, ok := httpCallOf(m)
	if !ok {
		return false
	}
	if v, set := mt["method"]; set && !strings.EqualFold(v, method) {
		return false
	}
	if v, set := mt["host"]; set && !workerRuleHostMatches(v, host, dialledPortOf(m)) {
		return false
	}
	if v, set := mt["path"]; set {
		if ok, _ := path.Match(v, p); !ok {
			return false
		}
	}
	if v, set := mt["pathregex"]; set && !workerRegexMatches(v, p) {
		return false
	}
	return true
}

// httpSchemeOf is the scheme of m's request, lower case: "" when it does not
// say.
func httpSchemeOf(m *Mock) string {
	switch {
	case m.Kind == HTTP && m.Spec.HTTPReq != nil:
		if u, err := url.Parse(m.Spec.HTTPReq.URL); err == nil {
			return strings.ToLower(u.Scheme)
		}
	case m.Kind == HTTP2 && m.Spec.HTTP2Req != nil:
		if m.Spec.HTTP2Req.Scheme != "" {
			return strings.ToLower(m.Spec.HTTP2Req.Scheme)
		}
		if u, err := url.Parse(m.Spec.HTTP2Req.URL); err == nil {
			return strings.ToLower(u.Scheme)
		}
	}
	return ""
}

// dialledPortOf is the port m's call was made to: the port of the address its
// connection reached (destAddr, which keploy stamps on every mock it records),
// or, without one, the default port of its request's scheme. "" when neither
// says. An HTTP/1 request names its host as its Host header does, without the
// default port, and its URL as its request line does, with no scheme, so the
// port is told by the connection, not the request.
func dialledPortOf(m *Mock) string {
	if d := strings.TrimSpace(m.Spec.Metadata["destAddr"]); d != "" {
		if _, p, err := net.SplitHostPort(d); err == nil {
			return p
		}
	}
	return map[string]string{"https": "443", "http": "80"}[httpSchemeOf(m)]
}

// workerRuleHostMatches matches the glob rule against host. A rule without a
// port matches the host on any port. One with a port matches a host that names
// that port, or a host that names none when the call was dialled on it
// (dialledPort): a Host header leaves out the default port.
func workerRuleHostMatches(rule, host, dialledPort string) bool {
	host = strings.ToLower(host)
	rule = strings.ToLower(rule)
	if ok, _ := path.Match(rule, host); ok {
		return true
	}
	if rh, rp, err := net.SplitHostPort(rule); err == nil {
		if _, _, herr := net.SplitHostPort(host); herr != nil && dialledPort != "" {
			if portOK, _ := path.Match(rp, dialledPort); !portOK {
				return false
			}
			ok, _ := path.Match(rh, host)
			return ok
		}
		return false
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		ok, _ := path.Match(rule, h)
		return ok
	}
	return false
}

// matchesStatements: the rule's query must match every statement, and there
// must be one. A mock with no statement (a handshake, a ping) is not one a
// query rule names.
func (r WorkerMockRule) matchesStatements(statements []string) bool {
	mt := r.match()
	q, set := mt["query"]
	if !set {
		return true
	}
	if len(statements) == 0 {
		return false
	}
	for _, s := range statements {
		if !workerRegexMatches(q, s) {
			return false
		}
	}
	return true
}

func (r WorkerMockRule) matchesRedis(m *Mock) bool {
	mt := r.match()
	cmd, cmdSet := mt["command"]
	key, keySet := mt["key"]
	if !cmdSet && !keySet {
		return true
	}
	commands := redisCommands(m)
	if len(commands) == 0 {
		return false
	}
	for _, c := range commands {
		if cmdSet && (len(c) == 0 || !strings.EqualFold(c[0], cmd)) {
			return false
		}
		if keySet && (len(c) < 2 || !workerRegexMatches(key, c[1])) {
			return false
		}
	}
	return true
}

// mysqlStatements is the statement text of each of m's MySQL requests that
// carries one (COM_QUERY, COM_STMT_PREPARE), whitespace collapsed.
func mysqlStatements(m *Mock) []string {
	var out []string
	for _, req := range m.Spec.MySQLRequests {
		switch p := req.Message.(type) {
		case *mysql.QueryPacket:
			out = append(out, collapseSpace(stripSQLComments(p.Query)))
		case *mysql.StmtPreparePacket:
			out = append(out, collapseSpace(stripSQLComments(p.Query)))
		}
	}
	return out
}

// postgresV3Statements is m's normalized statement, for a query mock.
func postgresV3Statements(m *Mock) []string {
	s := m.Spec.PostgresV3
	if s == nil || s.Query == nil || strings.TrimSpace(s.Query.SQLNormalized) == "" {
		return nil
	}
	return []string{collapseSpace(stripPostgresComments(s.Query.SQLNormalized))}
}

// redisCommands is each command m's Redis requests send, as its arguments
// (the command first), read from the RESP the recorder kept. A request that
// is not a RESP array of bulk strings adds nothing.
func redisCommands(m *Mock) [][]string {
	var out [][]string
	for _, p := range m.Spec.GenericRequests {
		for _, msg := range p.Message {
			raw := msg.Data
			if msg.Type == "binary" {
				b, err := base64.StdEncoding.DecodeString(msg.Data)
				if err != nil {
					continue
				}
				raw = string(b)
			}
			out = append(out, parseRESPCommands(raw)...)
		}
	}
	return out
}

// parseRESPCommands reads the RESP arrays of bulk strings in s (a client's
// commands, pipelined or not).
func parseRESPCommands(s string) [][]string {
	var out [][]string
	for len(s) > 0 && s[0] == '*' {
		end := strings.Index(s, "\r\n")
		if end < 0 {
			break
		}
		n, err := strconv.Atoi(s[1:end])
		if err != nil || n < 0 || n > 1<<16 {
			break
		}
		s = s[end+2:]
		args := make([]string, 0, n)
		for i := 0; i < n; i++ {
			if len(s) == 0 || s[0] != '$' {
				return out
			}
			end = strings.Index(s, "\r\n")
			if end < 0 {
				return out
			}
			l, err := strconv.Atoi(s[1:end])
			// Compared without adding to l first: a length near the int
			// limit would overflow the sum and pass.
			if err != nil || l < 0 || l > maxRESPBulk || l > len(s)-end-4 {
				return out
			}
			args = append(args, s[end+2:end+2+l])
			s = s[end+2+l+2:]
		}
		out = append(out, args)
	}
	return out
}

// maxRESPBulk bounds a bulk string the rule matcher reads: a key or a command
// name, never a value worth matching past it.
const maxRESPBulk = 1 << 20

func collapseSpace(s string) string { return strings.Join(strings.Fields(s), " ") }

// stripSQLComments drops q's plain block comments, the /*...*/ a tracer or a
// query tagger adds to every statement (dd-trace's sqlcommenter carries a
// fresh traceparent in each, Rails' query log tags the caller): the same call
// is then one statement however it was tagged, so a rule derived from one
// copy names the others and the next recording's, and the recorder marks one
// copy of it. An optimizer hint (/*+ ... */) and an executable comment
// (MySQL's /*! ... */, MariaDB's /*M! ... */) are part of the statement and
// stay, as does anything inside a quoted string or identifier, or in a line
// comment (-- or #, to the line's end, kept as it is). A removed comment
// leaves a space, so the words around it stay apart. An unterminated comment
// is kept as it is.
func stripSQLComments(q string) string { return stripComments(q, false) }

// stripPostgresComments is stripSQLComments in PostgreSQL's dialect: any --
// starts a line comment (MySQL's needs a space after it), # is an operator,
// block comments nest, a backslash in a string is a character except in an
// E'...' string, where it escapes the next byte as in MySQL, and a
// $tag$...$tag$ string is kept whole. A hint (/*+ ... */, pg_hint_plan's)
// stays; MySQL's executable comments are comments here.
func stripPostgresComments(q string) string { return stripComments(q, true) }

func stripComments(q string, postgres bool) string {
	if !strings.Contains(q, "/*") {
		return q
	}
	var b strings.Builder
	b.Grow(len(q))
	for i := 0; i < len(q); {
		c := q[i]
		switch {
		case c == '\'' || c == '"' || (c == '`' && !postgres):
			// A quoted run, to its closing quote: a doubled quote stays
			// inside, and in a MySQL string, or a PostgreSQL E'...' one, a
			// backslash escapes the next byte.
			escapes := !postgres && c != '`' || postgres && c == '\'' && escapeString(q, i)
			j := i + 1
			for j < len(q) {
				if escapes && q[j] == '\\' && j+1 < len(q) {
					j += 2
					continue
				}
				if q[j] == c {
					if j+1 < len(q) && q[j+1] == c {
						j += 2
						continue
					}
					break
				}
				j++
			}
			if j < len(q) {
				j++
			}
			b.WriteString(q[i:j])
			i = j
		case postgres && c == '$' && (i == 0 || !identByte(q[i-1])):
			// A dollar-quoted string, $tag$ to the same $tag$, kept whole.
			if tag := dollarTag(q[i:]); tag != "" {
				end := strings.Index(q[i+len(tag):], tag)
				if end < 0 {
					b.WriteString(q[i:])
					return b.String()
				}
				j := i + len(tag) + end + len(tag)
				b.WriteString(q[i:j])
				i = j
				continue
			}
			b.WriteByte(c)
			i++
		case (c == '#' && !postgres) || (c == '-' && i+1 < len(q) && q[i+1] == '-' && (postgres || i+2 == len(q) || q[i+2] <= ' ')):
			// A line comment, to its line's end: a quote in it opens nothing.
			j := strings.IndexByte(q[i:], '\n')
			if j < 0 {
				b.WriteString(q[i:])
				return b.String()
			}
			b.WriteString(q[i : i+j+1])
			i += j + 1
		case c == '/' && i+2 < len(q) && q[i+1] == '*' && q[i+2] != '+' &&
			(postgres || (q[i+2] != '!' && !strings.HasPrefix(q[i+2:], "M!"))):
			end := blockCommentEnd(q, i, postgres)
			if end < 0 {
				b.WriteString(q[i:])
				return b.String()
			}
			b.WriteByte(' ')
			i = end
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String()
}

// blockCommentEnd is the index just past the block comment opening at q[i],
// or -1 when it is not closed. PostgreSQL's nest; MySQL's end at the first */.
func blockCommentEnd(q string, i int, nested bool) int {
	depth := 0
	for j := i; j+1 < len(q); {
		switch {
		case q[j] == '/' && q[j+1] == '*':
			depth++
			j += 2
		case q[j] == '*' && q[j+1] == '/':
			depth--
			j += 2
			if depth == 0 || !nested {
				return j
			}
		default:
			j++
		}
	}
	return -1
}

// escapeString reports whether the quote at q[i] opens a PostgreSQL E'...'
// string: an E (or e) before it that is not the end of a longer name.
func escapeString(q string, i int) bool {
	return i > 0 && (q[i-1] == 'E' || q[i-1] == 'e') && (i == 1 || !identByte(q[i-2]))
}

// identByte reports whether b can be part of a PostgreSQL name ($ included).
func identByte(b byte) bool {
	return b == '_' || b == '$' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b >= 0x80
}

// dollarTag is the $tag$ opening a PostgreSQL dollar-quoted string at the
// start of s ($$ included), or "" when s does not start with one.
func dollarTag(s string) string {
	for j := 1; j < len(s); j++ {
		c := s[j]
		if c == '$' {
			return s[:j+1]
		}
		letter := c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
		digit := j > 1 && c >= '0' && c <= '9'
		if !letter && !digit {
			return ""
		}
	}
	return ""
}

// workerRegexes caches the compiled regexes of rules: a recording matches
// every mock against them.
var workerRegexes sync.Map // string -> *regexp.Regexp

func compiledWorkerRegex(v string) (*regexp.Regexp, error) {
	if re, ok := workerRegexes.Load(v); ok {
		return re.(*regexp.Regexp), nil
	}
	re, err := regexp.Compile(v)
	if err != nil {
		return nil, err
	}
	workerRegexes.Store(v, re)
	return re, nil
}

func workerRegexMatches(v, s string) bool {
	re, err := compiledWorkerRegex(v)
	return err == nil && re.MatchString(s)
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// SuggestWorkerMockRule is the narrowest rule that names the call m records,
// for a user who points at a recorded mock and says "serve this to every
// test": an HTTP call by its method, host and path (no query); a MySQL or
// Postgres (PostgresV3) call by its statements, each whole; a Redis call by
// its command and key. A mock none of that can be read from is named by its
// destination, the broadest rule (DestinationOnly in api-server), which the
// caller says is broad. The rule is read by the same extractors Matches
// reads, and is returned only when it validates and matches m.
func SuggestWorkerMockRule(m *Mock) (WorkerMockRule, error) {
	if m == nil {
		return WorkerMockRule{}, fmt.Errorf("no mock")
	}
	rule := WorkerMockRule{Kind: string(m.Kind)}
	// A kind keploy reads the call of is named by that call, or not at all:
	// widening a statement it cannot name to the mock's destination would
	// mark every call to that database or cache, served to every test.
	var err error
	switch m.Kind {
	case HTTP, HTTP2:
		method, host, p, ok := httpCallOf(m)
		if !ok || method == "" || !strings.HasPrefix(p, "/") {
			return WorkerMockRule{}, fmt.Errorf("mock %s records no HTTP request keploy can name", m.Name)
		}
		rule.Match = map[string]string{"method": strings.ToUpper(method), "path": globLiteral(p)}
		// Lower case: the host matches case-insensitively, and one call
		// must give one rule (and one rule id) however it was recorded.
		if host = strings.ToLower(strings.TrimSpace(host)); host != "" {
			rule.Match["host"] = globLiteral(host)
		}
	case MySQL:
		rule.Match, err = statementRule(m.Name, mysqlStatements(m))
	case PostgresV3:
		rule.Match, err = statementRule(m.Name, postgresV3Statements(m))
	case REDIS:
		rule.Match, err = redisRule(m.Name, redisCommands(m))
	default:
		// A kind whose call keploy does not read: its destination is all
		// a rule can name.
		dest := strings.TrimSpace(m.Spec.Metadata["destAddr"])
		if dest == "" {
			return WorkerMockRule{}, fmt.Errorf("mock %s records no call keploy can name, and no destination", m.Name)
		}
		rule.Match = map[string]string{"destination": globLiteral(dest)}
	}
	if err != nil {
		return WorkerMockRule{}, err
	}
	if problems := rule.Validate(); len(problems) > 0 {
		return WorkerMockRule{}, fmt.Errorf("the rule derived from mock %s is refused: %s", m.Name, strings.Join(problems, "; "))
	}
	if !rule.Matches(m) {
		return WorkerMockRule{}, fmt.Errorf("the rule derived from mock %s does not match it", m.Name)
	}
	return rule, nil
}

// statementRule matches the one statement the mock records: anchored, or,
// when the whole statement would make a regex longer than a rule may hold,
// its longest prefix that fits (a 30-column SELECT is told apart by its
// columns). A mock of several different statements is refused: the matcher
// requires every statement of a mock to match, so the only rule it would
// take is an alternation, which also matches a mock of any one of them
// alone (a lone SET NAMES: every connection's setup).
func statementRule(name string, statements []string) (map[string]string, error) {
	distinct := map[string]bool{}
	for _, s := range statements {
		if s = strings.TrimSpace(s); s != "" {
			distinct[s] = true
		}
	}
	switch len(distinct) {
	case 0:
		return nil, fmt.Errorf("mock %s records no statement keploy can name", name)
	case 1:
	default:
		return nil, fmt.Errorf("mock %s records %d different statements, and a rule naming them all would also match a mock of any one alone; mark it with a rule of your own (a query regex) instead", name, len(distinct))
	}
	return map[string]string{"query": anchoredLiteral(sortedKeys(distinct)[0])}, nil
}

// redisRule names the commands given by their command and the key or keys
// they read (an alternation of the distinct keys), or the command alone for
// AUTH and HELLO, whose arguments are credentials. Refused when they are not
// all one command, or when the keys do not fit in a rule: a command alone
// would match every call of it.
func redisRule(name string, commands [][]string) (map[string]string, error) {
	if len(commands) == 0 || len(commands[0]) == 0 {
		return nil, fmt.Errorf("mock %s records no Redis command keploy can name", name)
	}
	cmd := strings.ToUpper(commands[0][0])
	keys := map[string]bool{}
	for _, c := range commands {
		if len(c) == 0 || !strings.EqualFold(c[0], cmd) {
			return nil, fmt.Errorf("mock %s records more than one Redis command; mark it with a rule of your own instead", name)
		}
		if len(c) > 1 {
			keys[c[1]] = true
		}
	}
	out := map[string]string{"command": cmd}
	if cmd == "AUTH" || cmd == "HELLO" || len(keys) == 0 {
		return out, nil
	}
	sorted := sortedKeys(keys)
	if len(sorted) == 1 {
		out["key"] = anchoredLiteral(sorted[0])
		return out, nil
	}
	quoted := make([]string, 0, len(sorted))
	for _, k := range sorted {
		quoted = append(quoted, quoteLiteral(k))
	}
	q := "^(?:" + strings.Join(quoted, "|") + ")$"
	if len(q) > maxWorkerRuleRegex {
		return nil, fmt.Errorf("mock %s reads %d keys, more than a rule can name; mark it with a rule of your own instead", name, len(sorted))
	}
	out["key"] = q
	return out, nil
}

// anchoredLiteral is a regex matching s alone, or, when that would be longer
// than a rule may hold, every string starting with s's longest prefix that
// fits.
func anchoredLiteral(s string) string {
	if q := "^" + quoteLiteral(s) + "$"; len(q) <= maxWorkerRuleRegex {
		return q
	}
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(s); {
		_, size := utf8.DecodeRuneInString(s[i:])
		piece := quoteLiteral(s[i : i+size])
		if b.Len()+len(piece) > maxWorkerRuleRegex {
			break
		}
		b.WriteString(piece)
		i += size
	}
	return b.String()
}

// quoteLiteral is regexp.QuoteMeta for any bytes. U+FFFD is written
// \x{FFFD} whether s holds the character or a byte that is not UTF-8 (a
// _binary literal, a binary key), which is how the regexp engine reads such a
// byte: the regex compiles and matches s, and a mock whose invalid bytes were
// turned into U+FFFD on the way (JSON does) gives the same rule, and the same
// rule id, as the mock as recorded.
func quoteLiteral(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError {
			b.WriteString(`\x{FFFD}`)
		} else {
			b.WriteString(regexp.QuoteMeta(s[i : i+size]))
		}
		i += size
	}
	return b.String()
}

// globLiteral is a path.Match glob that matches s alone.
func globLiteral(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '*', '?', '[', ']', '\\':
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}
