package models

import (
	"net"
	"net/url"
	"regexp"
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
// segment one of its field diffs names (url.path[<n>], a test variable's
// place) written {}: the miss says a value the app sent is there, whatever
// its look.
func MissShape(m UnmatchedCall) string {
	shape := CallShape(m.Protocol, m.ActualSummary)
	if !IsPathProtocol(m.Protocol) || len(m.FieldDiffs) == 0 {
		return shape
	}
	places := make([]string, len(m.FieldDiffs))
	for i, d := range m.FieldDiffs {
		places[i] = d.Path
	}
	return holePathPlaces(shape, places)
}

// CallShape is a miss's summary in its shape: for HTTP and gRPC the method
// and the target's shape ("GET /users/{}/orders", "/pkg.v1.Service/Method"),
// for SQL the statement with its literals written "?" (SQLShape), and "" for
// any other protocol, whose summary may hold the values it sent (a cache key,
// a document) and which keploy has no shape for. An HTTP or gRPC shape is its
// own shape; a SQL statement cut at its bound may shape again shorter.
func CallShape(protocol, summary string) string {
	switch {
	case IsPathProtocol(protocol):
		return methodTargetShape(summary)
	case isSQLCall(protocol):
		return SQLShape(summary)
	}
	return ""
}

// IsHTTPProtocol reports whether a miss's protocol is HTTP, whose summary is
// a method and a path: "HTTP", "HTTPS", "HTTP2" or "HTTP/2".
func IsHTTPProtocol(protocol string) bool {
	for _, p := range []string{"HTTP", "HTTPS", "HTTP2", "HTTP/2"} {
		if strings.EqualFold(protocol, p) {
			return true
		}
	}
	return false
}

// IsPathProtocol reports whether a miss's summary is a method and a URL, or
// a gRPC full method: HTTP's and gRPC's.
func IsPathProtocol(protocol string) bool {
	return IsHTTPProtocol(protocol) || strings.EqualFold(protocol, "gRPC")
}

// MockURLShape is a recorded HTTP mock's URL as evidence names it: its host
// and port and its target's shape (targetShape), with every path segment the
// mock declares as a test variable's place (url.path[<n>]) written {},
// whatever its look. "" for a mock with no HTTP request.
func MockURLShape(m *Mock) string {
	if m == nil || m.Spec.HTTPReq == nil {
		return ""
	}
	var places []string
	for _, v := range m.Spec.Variables {
		if v.Bind != "" {
			places = append(places, v.Bind)
		}
		places = append(places, v.Use...)
	}
	raw := strings.TrimSpace(m.Spec.HTTPReq.URL)
	shape := targetShape(raw)
	if u, err := url.Parse(raw); err == nil && u.Host != "" && absoluteURL.MatchString(raw) {
		shape = u.Host + shape
	}
	return holePathPlaces(shape, places)
}

// isSQLCall reports whether a miss's protocol speaks SQL, whose summary is
// the statement keploy saw.
func isSQLCall(protocol string) bool {
	p := strings.ToLower(protocol)
	return strings.Contains(p, "sql") || strings.Contains(p, "postgres")
}

// PathShape is a URL path as the proven model's evidence keeps it (design v14
// §2.1, §5.3): its static segments, each variable one written {}, and the
// query's names with every value written {}.
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
// Query names follow the segment rule; a component with no '=' is a name or
// a value, and follows it too. A shape is its own shape: an already-templated
// path ("/users/{}/orders") is kept as it is, and a named placeholder
// ("/users/{id}") is written {} like any other segment that holds a brace.
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
		k = pathSegmentShape(k)
		if hasValue {
			k += "=" + pathHole
		}
		params = append(params, k)
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

// apiVersionPart is an API version: v1, V2, v1beta1, v2alpha, v3rc2.
var apiVersionPart = regexp.MustCompile(`^[vV][0-9]{1,3}((alpha|beta|rc)[0-9]{0,3})?$`)

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

// hostPort reports whether h reads as a host, with an optional numeric port:
// an IPv4 address, a bracketed IPv6 address, or a name that is dotted or has
// a port (pay.internal, db:5432), whose every label holds a letter and is
// letters, digits and '-', and whose last label is letters only. A token
// with no dot and no port (12345, a UUID), credentials (user@host), and a
// dotted value (a1b2.c3d4, a JWT) are no host.
func hostPort(h string) bool {
	host, hasPort := h, false
	if i := strings.LastIndexByte(h, ':'); i >= 0 && !strings.HasSuffix(h, "]") {
		host, hasPort = h[:i], true
		port := h[i+1:]
		if port == "" || len(port) > 5 {
			return false
		}
		for j := 0; j < len(port); j++ {
			if !isDigitByte(port[j]) {
				return false
			}
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

// pathPlaceIndex is the segment a place "url.path[<n>]" names.
func pathPlaceIndex(place string) (int, bool) {
	const prefix = "url.path["
	if !strings.HasPrefix(place, prefix) || !strings.HasSuffix(place, "]") {
		return 0, false
	}
	digits := place[len(prefix) : len(place)-1]
	if digits == "" || len(digits) > 4 {
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
		out += " …"
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
