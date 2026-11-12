package models

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

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
	HTTP:       {"method", "host", "path", "pathregex"},
	HTTP2:      {"method", "host", "path", "pathregex"},
	MySQL:      {"query"},
	PostgresV3: {"query"},
	REDIS:      {"command", "key"},
}

// workerRuleRegexKeys are the keys whose values are regexes.
var workerRuleRegexKeys = map[string]bool{"pathregex": true, "query": true, "key": true}

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
	if set == 1 {
		for _, k := range []string{"host", "destination"} {
			if g, ok := mt[k]; ok && globMatchesEverything(k, g) {
				problems = append(problems, "its only match key matches everything")
			}
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
	switch m.Kind {
	case HTTP, HTTP2:
		return r.matchesHTTP(m)
	case MySQL:
		return r.matchesStatements(mysqlStatements(m))
	case PostgresV3:
		return r.matchesStatements(postgresV3Statements(m))
	case REDIS:
		return r.matchesRedis(m)
	}
	return true
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
			out = append(out, collapseSpace(p.Query))
		case *mysql.StmtPreparePacket:
			out = append(out, collapseSpace(p.Query))
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
	return []string{collapseSpace(s.Query.SQLNormalized)}
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
