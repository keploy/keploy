package models

import (
	"encoding/json"
	"net"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// The shape of a call keploy could not answer, as the proven model's evidence
// names it (design v14 §2.1, §5.3): what the call was, without the values its
// URL or statement held, as far as their look, or the call itself, tells them
// apart (PathShape, SQLShape). The agent keeps a miss in its shape; so do the
// reason it answers a test with and the JUnit file's proven evidence lines.
// The report file and `keploy report` keep the full summary, as they keep
// full bodies.

// MissShape is a miss's summary in its shape (CallShape), with every URL path
// segment that a test variable's place names (url.path[<n>]) written {}: a
// place one of its field diffs names, or one in VariablePlaces (the place a
// variable miss names, one the closest mock or a mock on the call's route
// declares, or one holding a value the test's variables have). The miss says
// a value the app sent is there, whatever its look.
//
// A MongoDB miss's shape also names, from the request the matcher rendered
// (ReceivedReq), the database the command ran in and its filter's keys
// (mongoMissShape).
func MissShape(m UnmatchedCall) string {
	if isMongoCall(m.Protocol) {
		return mongoMissShape(m)
	}
	shape := CallShape(m.Protocol, m.ActualSummary)
	if !IsPathProtocol(m.Protocol) || (len(m.FieldDiffs) == 0 && len(m.VariablePlaces) == 0) {
		return shape
	}
	places := append([]string(nil), m.VariablePlaces...)
	for _, d := range m.FieldDiffs {
		places = append(places, d.Path)
	}
	return holePathPlaces(shape, places)
}

// CallShape is a miss's summary in its shape: for HTTP and gRPC the method
// and the target's shape ("GET /users/{}/orders", "/pkg.v1.Service/Method"),
// for SQL the statement with its literals written "?" (SQLShape), for
// MongoDB the command and the namespace it runs on, each name by KeyShape
// (mongoShape: "find on app_config_audits"), and "" for any other protocol,
// whose summary may hold the values it sent (a cache key, a document) and
// which keploy has no shape for. An HTTP, gRPC or MongoDB shape is its own
// shape; a SQL statement cut at its bound may shape again shorter.
func CallShape(protocol, summary string) string {
	switch {
	case IsPathProtocol(protocol):
		return methodTargetShape(summary)
	case isSQLCall(protocol):
		return SQLShape(summary)
	case isMongoCall(protocol):
		return mongoShape(summary)
	}
	return ""
}

// isHTTPProtocol reports whether a miss's protocol is HTTP, whose summary is
// a method and a path: "HTTP", "HTTPS", "HTTP2" or "HTTP/2".
func isHTTPProtocol(protocol string) bool {
	for _, p := range []string{"HTTP", "HTTPS", "HTTP2", "HTTP/2"} {
		if strings.EqualFold(protocol, p) {
			return true
		}
	}
	return false
}

// IsPathPlace reports whether a variable's place is a URL path segment
// (url.path[<n>]), the only places a call's shape names.
func IsPathPlace(place string) bool {
	_, ok := pathPlaceIndex(place)
	return ok
}

// IsPathProtocol reports whether a miss's summary is a method and a URL, or
// a gRPC full method: HTTP's and gRPC's.
func IsPathProtocol(protocol string) bool {
	return isHTTPProtocol(protocol) || strings.EqualFold(protocol, "gRPC")
}

// MockURLShape is a recorded HTTP mock's URL as evidence names it: its host
// and port, as a call's destination names them (no credentials), and its
// path's shape (PathShape), read as a variable's place reads it (the parsed
// URL's escaped path and query), with every path segment the
// mock declares for a test variable (DeclaredPathPlaces) written {}, whatever
// its look. "" for a mock with no HTTP request; {} for a URL that does not
// parse.
func MockURLShape(m *Mock) string {
	if m == nil || m.Spec.HTTPReq == nil {
		return ""
	}
	u, err := url.Parse(strings.TrimSpace(m.Spec.HTTPReq.URL))
	if err != nil {
		return pathHole
	}
	// A place counts segments after the path's leading slash; a relative
	// URL's path has none, so it is counted as if it had one.
	p := u.EscapedPath()
	relative := p != "" && p[0] != '/'
	if relative {
		p = "/" + p
	}
	if u.RawQuery != "" {
		p += "?" + u.RawQuery
	}
	shape := holePathPlaces(PathShape(p), DeclaredPathPlaces(m))
	if relative {
		shape = strings.TrimPrefix(shape, "/")
	}
	return u.Host + shape
}

// DeclaredPathPlaces are the URL path places (url.path[<n>]) a mock declares
// for test variables, as bind or use places, without repeats, in the order of
// the variables' names: the segments of a call to its route that hold the
// application's values.
func DeclaredPathPlaces(m *Mock) []string {
	if m == nil {
		return nil
	}
	var out []string
	add := func(p string) {
		if IsPathPlace(p) && !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	names := make([]string, 0, len(m.Spec.Variables))
	for name := range m.Spec.Variables {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		v := m.Spec.Variables[name]
		add(v.Bind)
		for _, p := range v.Use {
			add(p)
		}
	}
	return out
}

// FieldShapes are the names of the fields a call differs in (row S1's
// Differs), each in its shape (FieldShape), once, for a reason that names
// them.
func FieldShapes(fields []string) []string {
	if len(fields) == 0 {
		return nil
	}
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if shape := FieldShape(f); !slices.Contains(out, shape) {
			out = append(out, shape)
		}
	}
	return out
}

// FieldShape is a compared field's name as a reason names it: each
// dot-separated part by PathShape's segment rule, with its indexes ([], [0],
// up to four digits) kept, so a part that may be a value the app sent (a map
// key that is an ID, an email address, a token) is {}. By that rule any key
// that does not read as a name is {} too (line1, $set, iOS, café), and an
// API version (v1) is kept. A header's name is kept whole: it names the
// field (header.Authorization). A raw dotted key is shaped part by part
// (alice.smith@example.com keeps "alice"), so a producer shapes its keys
// first (KeyShape), as the strict comparison does. An SQL call's place name
// is kept whole too (sqlPlace: bind.$1, literal.3): a position keploy counts
// in the statement, not a value the app sent.
func FieldShape(f string) string {
	if name, ok := strings.CutPrefix(f, "header."); ok && name != "" && !strings.Contains(name, ".") {
		return f
	}
	if sqlPlace.MatchString(f) {
		return f
	}
	parts := strings.Split(f, ".")
	for i, p := range parts {
		base, index := p, ""
		if j := strings.IndexByte(p, '['); j >= 0 && fieldIndexes.MatchString(p[j:]) {
			base, index = p[:j], p[j:]
		}
		parts[i] = KeyShape(base) + index
	}
	return strings.Join(parts, ".")
}

// fieldIndexes are a field part's array indexes: [], [0], [3][].
var fieldIndexes = regexp.MustCompile(`^(\[[0-9]{0,4}\])+$`)

// sqlPlace is a whole field that is an SQL call's place name, as the
// Postgres parser names a statement's places: bind.$N, its Nth bound
// parameter, and literal.N, the Nth constant of its text, each counted from
// 1, up to five digits (a statement binds at most 65535 parameters).
var sqlPlace = regexp.MustCompile(`^(bind\.\$|literal\.)[1-9][0-9]{0,4}$`)

// KeyShape is an object key, a form key or a query name as a field's name
// holds it: itself when it reads as a name (PathShape's segment rule), {}
// when it may be a value the app sent.
func KeyShape(k string) string {
	return pathSegmentShape(k)
}

// isSQLCall reports whether a miss's protocol speaks SQL, whose summary is
// the statement keploy saw.
func isSQLCall(protocol string) bool {
	p := strings.ToLower(protocol)
	return strings.Contains(p, "sql") || strings.Contains(p, "postgres")
}

// isMongoCall reports whether a miss's protocol is MongoDB's, whose summary
// is its command and the collection it names: "find on users".
func isMongoCall(protocol string) bool {
	return strings.EqualFold(protocol, "MongoDB") || strings.EqualFold(protocol, "Mongo")
}

// mongoMaxKeys bounds the filter keys a MongoDB shape names; past it the
// shape says "…".
const mongoMaxKeys = 8

// mongoShape is a MongoDB miss's summary in its shape: "<command>", or
// "<command> on <namespace>", then "filter <key>,<key>" when the summary
// names its filter's keys, as a shape of one does (mongoMissShape). The
// command, each dot-separated part of the namespace ("api.app_config_audits"
// or a collection alone) and of each key is kept when it reads as a name
// (KeyShape), else {}: a tenant's database or collection named by a value
// (tenant_42), an ID used as a key. The legacy command pseudo-collection
// $cmd is a name. Anything else the summary holds is left out, so what the
// shape names is only ever a name.
func mongoShape(summary string) string {
	f := strings.Fields(summary)
	if len(f) == 0 {
		return ""
	}
	out := KeyShape(f[0])
	i := 1
	if i+1 < len(f) && f[i] == "on" {
		out += " on " + mongoNameShape(f[i+1])
		i += 2
	}
	if i+1 < len(f) && f[i] == "filter" {
		out += " filter " + strings.Join(mongoKeysShape(strings.Split(f[i+1], ",")), ",")
	}
	return out
}

// mongoMissShape is a MongoDB miss's shape (mongoShape) naming, when the
// request the matcher rendered for it (ReceivedReq, its OP_MSG command as
// "cmd") holds them, the database the command ran in ("$db": the namespace
// "<db>.<collection>") and the keys of its filter ("filter", or "query" for
// count, distinct and findAndModify), in their order by name: "find on
// api.app_config_audits filter appId,name". A miss with no rendered request
// (no closest mock), or one that does not read as a command, is shaped by
// its summary alone; so are a write's per-statement filters (delete's and
// update's q) and an aggregate's $match, which name no "filter".
func mongoMissShape(m UnmatchedCall) string {
	shape := mongoShape(m.ActualSummary)
	var body struct {
		Cmd map[string]json.RawMessage `json:"cmd"`
	}
	if m.ReceivedReq == "" || json.Unmarshal([]byte(m.ReceivedReq), &body) != nil || body.Cmd == nil {
		return shape
	}
	f := strings.Fields(m.ActualSummary)
	if len(f) == 0 {
		return shape
	}
	var command, coll, db string
	command = f[0]
	if json.Unmarshal(body.Cmd[command], &coll) != nil || coll == "" {
		return shape
	}
	_ = json.Unmarshal(body.Cmd["$db"], &db)
	ns := coll
	if db != "" {
		ns = db + "." + coll
	}
	out := KeyShape(command) + " on " + mongoNameShape(ns)
	var filter map[string]json.RawMessage
	for _, k := range []string{"filter", "query"} {
		if json.Unmarshal(body.Cmd[k], &filter) == nil && len(filter) > 0 {
			break
		}
		filter = nil
	}
	if len(filter) > 0 {
		keys := make([]string, 0, len(filter))
		for k := range filter {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		if len(keys) > mongoMaxKeys {
			keys = append(keys[:mongoMaxKeys], "…")
		}
		out += " filter " + strings.Join(mongoKeysShape(keys), ",")
	}
	return out
}

// mongoNameShape is a MongoDB name (a namespace, a collection, a dotted
// key) with each dot-separated part kept when it reads as a name (KeyShape),
// else {}, and $cmd kept.
func mongoNameShape(s string) string {
	parts := strings.Split(s, ".")
	for i, p := range parts {
		if p != "$cmd" {
			parts[i] = KeyShape(p)
		}
	}
	return strings.Join(parts, ".")
}

// mongoKeysShape are filter keys each in its shape (mongoNameShape), once,
// in their order, with a last "…" (keys left out) kept and an empty key {}.
func mongoKeysShape(keys []string) []string {
	out := make([]string, 0, len(keys))
	for i, k := range keys {
		shape := mongoNameShape(k)
		switch {
		case k == "…" && i == len(keys)-1:
			shape = k
		case k == "":
			shape = pathHole
		}
		if !slices.Contains(out, shape) {
			out = append(out, shape)
		}
	}
	return out
}

// PathShape is a URL path as the proven model's evidence keeps it (design v14
// §2.1, §5.3): its segments that read as names, every other one written {},
// and the query's names with every value written {}.
// "/users/alice@example.com/orders/12345?expand=items" is
// "/users/{}/orders/{}?expand={}". A request target carries no fragment, so
// a '#' is a decoded %23, part of its segment; a '?' starts the query, also
// one decoded from %3F.
//
// A segment is kept only when it reads as a name. It is split into parts at
// '.', '-', '_', '~' and ':' ("pkg.v1.Service", "user-profile",
// "projects:batchGet"), and each part must be an API version (v1, v2beta1),
// or letters only, not eight or more of the letters a-f alone, and either in
// one case and shorter than 32, or camelCase words (pathCamelWords:
// "AccessSecretVersion", "batchGet", "XMLHttpRequest"). Every other segment is
// {}: a number, a UUID, a hash, an email address, a token or ID with a digit
// in it (acct_42, a1b2), most random tokens in both cases
// (cus_NffrFeUfNVaHib), one holding any other character (base64's '+' and
// '=', '%', '@', '#', a space, non-ASCII), and one longer than 64 bytes. A
// name with a digit (oauth2) is {} too: by its look it cannot be told from
// an ID.
//
// The shape is what a segment's look says. A value of letters in one case (a
// user name, a slug, a lower-case token, a code like USD), or one that reads
// as camelCase words (about one in nine random tokens in both cases of 8
// letters does, one in thirty-five of 16), cannot be told from a name, and is
// kept, unless the call names its place as a value (holePathPlaces).
//
// Query names follow the segment rule; a component with no '=' (a flag or a
// bare value, as in ?1697040000) is {}. A shape is its own shape: an
// already-templated path ("/users/{}/orders") is kept as it is, and a named
// placeholder ("/users/{id}") is written {} like any other segment that
// holds a brace.
func PathShape(p string) string {
	p, query, hasQuery := strings.Cut(p, "?")
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = pathSegmentShape(s)
	}
	out := strings.Join(segs, "/")
	if !hasQuery {
		return out
	}
	var params []string
	for _, kv := range strings.Split(query, "&") {
		if kv == "" {
			continue
		}
		k, _, hasValue := strings.Cut(kv, "=")
		if !hasValue {
			params = append(params, pathHole)
			continue
		}
		params = append(params, pathSegmentShape(k)+"="+pathHole)
	}
	if len(params) == 0 {
		return out
	}
	return out + "?" + strings.Join(params, "&")
}

// Bounds of a segment PathShape keeps.
const (
	pathMaxSegment = 64
	pathMaxPart    = 32
	pathMinHex     = 8
)

// pathHole is what PathShape writes for a variable segment or value.
const pathHole = "{}"

// apiVersionPart is an API version: v1, V2, v10, v1beta1, v2alpha, v3rc2.
var apiVersionPart = regexp.MustCompile(`^[vV][0-9]{1,2}((alpha|beta|rc)[0-9]{0,2})?$`)

// pathSegmentShape is s when it reads as a name (PathShape), else {}.
func pathSegmentShape(s string) string {
	if s == "" || s == pathHole {
		return s
	}
	if len(s) > pathMaxSegment {
		return pathHole
	}
	for i := 0; i < len(s); i++ {
		if !isASCIILetter(s[i]) && !isDigitByte(s[i]) && !isPathNameSeparator(s[i]) {
			return pathHole
		}
	}
	for _, part := range strings.FieldsFunc(s, func(r rune) bool { return r < 0x80 && isPathNameSeparator(byte(r)) }) {
		if !pathNamePart(part) {
			return pathHole
		}
	}
	return s
}

// pathNamePart reports whether one part of a segment reads as a name: an API
// version, or letters only, not pathMinHex or more of the letters a-f alone,
// and either in one case and shorter than pathMaxPart, or camelCase words.
func pathNamePart(p string) bool {
	if apiVersionPart.MatchString(p) {
		return true
	}
	hex, upper, lower := true, false, false
	for i := 0; i < len(p); i++ {
		c := p[i]
		if !isASCIILetter(c) {
			return false
		}
		if !strings.ContainsRune("abcdefABCDEF", rune(c)) {
			hex = false
		}
		if isUpperByte(c) {
			upper = true
		} else {
			lower = true
		}
	}
	switch {
	case hex && len(p) >= pathMinHex:
		return false
	case upper && lower:
		return pathCamelWords(p)
	}
	return len(p) < pathMaxPart
}

// pathShortWords are the words of fewer than three letters a camelCase name
// is made of (GetUserById, ConvertToJson).
var pathShortWords = map[string]bool{"by": true, "to": true, "of": true, "on": true, "in": true, "at": true, "is": true, "or": true, "as": true, "if": true, "id": true, "up": true, "an": true}

// pathCamelWords reports whether p, letters in both cases, reads as words in
// camelCase or PascalCase: each word a capital and lower-case letters, or a
// run of capitals (an acronym: XMLHttpRequest is XML, Http, Request), and
// only the first word may start lower-case. A word with lower-case letters
// is three letters or more, or one of pathShortWords; an acronym is two
// capitals or more. Most random tokens in both cases (NffrFeUfNVaHib,
// XkTqPzRm) break into words of one or two letters, so they are no name.
func pathCamelWords(p string) bool {
	for i := 0; i < len(p); {
		start, capitals := i, 0
		for i < len(p) && isUpperByte(p[i]) {
			i++
			capitals++
		}
		if capitals > 1 && i < len(p) {
			// Capitals, then lower-case: the last capital starts the next
			// word, and the ones before it are an acronym.
			i--
			if i-start < 2 {
				return false
			}
			continue
		}
		for i < len(p) && !isUpperByte(p[i]) {
			i++
		}
		w := p[start:i]
		switch {
		case capitals == len(w):
			if len(w) < 2 {
				return false
			}
		case len(w) < 3 && !pathShortWords[strings.ToLower(w)]:
			return false
		}
	}
	return true
}

// absoluteURL is a target that starts with a scheme: "http://host/path".
var absoluteURL = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*://`)

// targetShape is an HTTP or gRPC call's target as evidence keeps it: a path
// (it starts with '/') is its shape (PathShape), query names included. An
// absolute URL keeps only its path and query, shaped; one that does not parse
// is {}. A target that names its host first, as a dependency row does
// ("pay.internal:8443/cards/42"), keeps the host and port, as a call's
// destination keeps them, when they read as a host (hostPort). Any other
// target is shaped as a path without its leading slash.
func targetShape(t string) string {
	if t == "" || t[0] == '/' {
		return PathShape(t)
	}
	if absoluteURL.MatchString(t) {
		if u, err := url.Parse(t); err == nil {
			p := u.EscapedPath()
			if u.RawQuery != "" {
				p += "?" + u.RawQuery
			}
			return PathShape(p)
		}
		return pathHole
	}
	host, rest := t, ""
	if i := strings.IndexByte(t, '/'); i >= 0 {
		host, rest = t[:i], t[i:]
	}
	if hostPort(host) {
		return host + PathShape(rest)
	}
	return PathShape(t)
}

// hostPort reports whether h reads as a host, with an optional port from 1 to
// 65535: an IPv4 address, a bracketed IPv6 address, or a name that is dotted
// or has a port (pay.internal, db:5432), whose every label holds a letter and
// is letters, digits and '-', and whose last label is letters only. A token
// with no dot and no port (12345, a UUID), credentials (user@host), and a
// dotted value whose last label has a digit or whose labels lack a letter
// (a1b2.c3d4, 4111.1111, most JWTs) are no host; one that passes those
// checks (acct42.example) is read as a host.
func hostPort(h string) bool {
	host, hasPort := h, false
	if i := strings.LastIndexByte(h, ':'); i >= 0 && !strings.HasSuffix(h, "]") {
		host, hasPort = h[:i], true
		port, n := h[i+1:], 0
		if port == "" || len(port) > 5 {
			return false
		}
		for j := 0; j < len(port); j++ {
			if !isDigitByte(port[j]) {
				return false
			}
			n = n*10 + int(port[j]-'0')
		}
		if n < 1 || n > 65535 {
			return false
		}
	}
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		ip := net.ParseIP(host[1 : len(host)-1])
		return ip != nil && ip.To4() == nil
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.To4() != nil
	}
	labels := strings.Split(host, ".")
	if len(labels) < 2 && !hasPort {
		return false
	}
	for i, label := range labels {
		if label == "" || len(label) > 63 {
			return false
		}
		letter := false
		for j := 0; j < len(label); j++ {
			c := label[j]
			switch {
			case isASCIILetter(c):
				letter = true
			case (isDigitByte(c) || c == '-') && i < len(labels)-1:
			default:
				return false
			}
		}
		if !letter {
			return false
		}
	}
	return true
}

// methodTargetShape is an HTTP or gRPC call's summary in its shape: "<method>
// <target>" as its method and its target's shape (targetShape), a method
// alone as itself, and a target alone (a gRPC full method,
// "/pkg.Service/Method") as its shape. The target is all that follows the
// method, to the end: a decoded path can hold a space, even a last one, and
// the segment that holds it is {}. A summary whose first word is neither a
// method (isMethodWord) nor a target is "": keploy has no shape for it.
func methodTargetShape(summary string) string {
	s := strings.TrimLeftFunc(summary, unicode.IsSpace)
	if strings.TrimSpace(s) == "" {
		return ""
	}
	if s[0] == '/' {
		return targetShape(s)
	}
	i := strings.IndexFunc(s, unicode.IsSpace)
	if i < 0 {
		if isMethodWord(s) {
			return s
		}
		return targetShape(s)
	}
	if !isMethodWord(s[:i]) {
		return ""
	}
	if t := targetShape(strings.TrimLeftFunc(s[i:], unicode.IsSpace)); t != "" {
		return s[:i] + " " + t
	}
	return s[:i]
}

// isMethodWord reports whether s is a call's method: capital letters and '-'
// (GET, M-SEARCH), at most sixteen, or gRPC.
func isMethodWord(s string) bool {
	if s == "gRPC" {
		return true
	}
	if s == "" || len(s) > 16 || !isUpperByte(s[0]) {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !isUpperByte(s[i]) && s[i] != '-' {
			return false
		}
	}
	return true
}

// holePathPlaces is a call's shape with every URL path segment one of places
// names (url.path[<n>], counted from 0 after the leading slash, as a test
// variable's place is) written {}: the call says a value the app sent is
// there, whatever its look. Every other place is no path segment. A place
// counts the segments of the escaped path, so the shape must be of the
// escaped path too, as a miss at a variable's place names it: decoded, a %2F
// would shift the count.
func holePathPlaces(shape string, places []string) string {
	start := strings.IndexByte(shape, '/')
	if start < 0 {
		return shape
	}
	end := len(shape)
	if q := strings.IndexByte(shape[start:], '?'); q >= 0 {
		end = start + q
	}
	segs := strings.Split(shape[start+1:end], "/")
	holed := false
	for _, p := range places {
		n, ok := pathPlaceIndex(p)
		if ok && n < len(segs) && segs[n] != pathHole {
			segs[n], holed = pathHole, true
		}
	}
	if !holed {
		return shape
	}
	return shape[:start+1] + strings.Join(segs, "/") + shape[end:]
}

// pathPlaceIndex is the segment a place "url.path[<n>]" names, read as a
// variable's place is read (a number from 0, without leading zeros), or a
// word of it ("url.path[<n>]#<w>", after any "|<step>": a word address,
// variables.ParseAddress); a place past url.path[9999] is ignored.
func pathPlaceIndex(place string) (int, bool) {
	const prefix = "url.path["
	if i := strings.IndexByte(place, ']'); i >= 0 && i+1 < len(place) && (place[i+1] == '#' || place[i+1] == '|') {
		place = place[:i+1]
	}
	if !strings.HasPrefix(place, prefix) || !strings.HasSuffix(place, "]") {
		return 0, false
	}
	digits := place[len(prefix) : len(place)-1]
	if digits == "" || len(digits) > 4 || (len(digits) > 1 && digits[0] == '0') {
		return 0, false
	}
	n := 0
	for i := 0; i < len(digits); i++ {
		if !isDigitByte(digits[i]) {
			return 0, false
		}
		n = n*10 + int(digits[i]-'0')
	}
	return n, true
}

func isASCIILetter(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }

func isUpperByte(c byte) bool { return c >= 'A' && c <= 'Z' }

func isPathNameSeparator(c byte) bool {
	return c == '.' || c == '-' || c == '_' || c == '~' || c == ':'
}

// maxSQLText bounds a statement's shape.
const maxSQLText = 160

// SQLShape is a SQL statement with every quoted string and number written
// "?". It fails closed: what it cannot read for certain is withheld, the rest
// of the statement written "…": a literal holding a backslash (MySQL reads
// \' as a quote by default, so where the literal ends depends on the
// server's sql_mode), one never closed, a dollar quote ($tag$), and a
// comment. MySQL's bound parameters ("params=[...]", which the replayer adds
// to a prepared statement's summary) are dropped whole, and a backtick
// identifier is kept only when it holds no quote. A quoted identifier
// (Postgres "table") is written "?" too: hiding a name beats keeping a value.
func SQLShape(q string) string {
	if i := strings.Index(q, "params=["); i >= 0 {
		q = q[:i]
	}
	var b strings.Builder
	withheld := false
	firstLiteral := -1
scan:
	for i := 0; i < len(q); {
		c := q[i]
		switch {
		case c == '\'' || c == '"':
			if firstLiteral < 0 {
				firstLiteral = b.Len()
			}
			end, ok := closeQuote(q, i)
			if !ok {
				// A literal never closed leaves no quote of the statement
				// paired for certain: everything from the first is withheld.
				if end < 0 {
					s := b.String()[:firstLiteral]
					b.Reset()
					b.WriteString(s)
				}
				b.WriteString("?")
				withheld = true
				break scan
			}
			b.WriteString("?")
			i = end
		case c == '`':
			// A MySQL identifier: kept, as a name. One never closed, or
			// holding a quote (which a reader would take for a literal's
			// start), is withheld with the rest; so is a backslash, and
			// Postgres's operator character.
			end, ok := closeQuote(q, i)
			if !ok || strings.ContainsAny(q[i+1:end-1], `'"`) {
				if !ok && end < 0 && firstLiteral >= 0 {
					s := b.String()[:firstLiteral]
					b.Reset()
					b.WriteString(s)
				}
				withheld = true
				break scan
			}
			// A name keeps its letters; a run of digits in it is written
			// "?", in case the span was misread (a Postgres operator, a
			// multi-byte character set's trail byte).
			b.WriteString(digitRuns.ReplaceAllString(q[i:end], "?"))
			i = end
		case c == '$' && i+1 < len(q) && isDigitByte(q[i+1]):
			// A placeholder ($1): kept.
			j := i + 1
			for j < len(q) && isDigitByte(q[j]) {
				j++
			}
			b.WriteString(q[i:j])
			i = j
		case c == '$', c == '#', strings.HasPrefix(q[i:], "--"), strings.HasPrefix(q[i:], "/*"):
			// A dollar quote or a comment: withheld.
			withheld = true
			break scan
		case isDigitByte(c) && (i == 0 || !isIdentByte(q[i-1])):
			j := i
			for j < len(q) && (isIdentByte(q[j]) || q[j] == '.') {
				j++
			}
			b.WriteString("?")
			i = j
		default:
			b.WriteByte(c)
			i++
		}
	}
	out := strings.TrimSpace(sqlSpace.ReplaceAllString(b.String(), " "))
	if withheld {
		// A statement withheld from its first byte (a leading comment) is
		// "…" alone, not " …".
		out = strings.TrimSpace(out + " …")
	}
	if len(out) > maxSQLText {
		i := maxSQLText
		for i > 0 && !utf8.RuneStart(out[i]) {
			i--
		}
		out = out[:i] + "…"
	}
	return out
}

// closeQuote is the index past the quote that closes the literal opening at
// q[i] (a doubled quote inside it is one quote), and false for a literal with
// a backslash in it (index 0), or one never closed (index -1).
func closeQuote(q string, i int) (int, bool) {
	quote := q[i]
	for j := i + 1; j < len(q); j++ {
		switch q[j] {
		case '\\':
			return 0, false
		case quote:
			if j+1 < len(q) && q[j+1] == quote {
				j++
				continue
			}
			return j + 1, true
		}
	}
	return -1, false
}

var (
	sqlSpace  = regexp.MustCompile(`\s+`)
	digitRuns = regexp.MustCompile(`[0-9]+`)
)

func isDigitByte(c byte) bool { return c >= '0' && c <= '9' }

func isIdentByte(c byte) bool {
	return c == '_' || isDigitByte(c) || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c >= 0x80
}
