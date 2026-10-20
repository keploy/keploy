package models

import (
	"strings"
	"unicode"
)

// URLShape is a URL, a request target, a path, or an HTTP or gRPC call's
// summary in its shape, as a log line names it (utils/logcontent/logurl).
// Its path and query are shaped as CallShape shapes an HTTP call's
// (PathShape): every path segment and query name that does not read as a
// name, and every query value, written {}. It differs from CallShape where a
// log line needs it to:
//
//   - An absolute URL keeps its scheme when it reads as one (urlScheme), and
//     its host and port when they read as a host (urlHost), so the line still
//     says which dependency the call went to:
//     "GET http://pay.internal:8443/cards/{}?cvv={}". A scheme or a host that
//     does not is {}. Its credentials, as url.Parse reads them, and its
//     fragment are dropped. A network-path reference ("//pay.internal/x")
//     names its authority after "//" as an absolute URL does after its
//     scheme, and a target that names its host first
//     ("pay.internal:8443/cards/42") is read as the URL with a scheme is.
//   - An '@' after the host ends credentials that url.Parse cannot delimit,
//     when their password holds a '/', a '?' or a '#', and it may end an
//     application's value, an email address or an npm scope: the shape
//     cannot tell them apart. So the host is {}, and so is everything before
//     the last '@' after it, path segment, query name or value alike:
//     "http://{}/{}/{}/orders" for "http://admin:2024/a/b@db/orders", and
//     "https://{}/{}?{}={}" for "https://api.example.com/users?email=a@b.com".
//     Every target that does not start with a path's single '/' is read so
//     when it holds an '@' after its first '/', as if it named its host
//     first: "{}/{}/app" for "root:secret/x@tcp(db:3306)/app". A path names
//     no host, so an '@' in it is the application's, and the segment or
//     value that holds it is {} as any other is ("/users/{}/orders"), unless
//     a "//" or a ':' comes before it, as a URL the path or its query holds
//     (a proxy's target, a redirect's ?next=), its "//" merged or not, or a
//     user:password does: that '@' may end credentials, so everything before
//     the last '@' is {} too, "/{}/{}//{}/{}/x" for
//     "/proxy/http://admin:hunter/pw@db/x". The shape cannot tell those from
//     another "//" or ':' ("/users?since=10:00&email=a@b.com" is
//     "/{}?{}={}&{}={}").
//   - A method is kept only when it is one of knownMethods; another word in
//     capitals, which can be a code or a key, is {}: "{} /x" for
//     "SECRETKEY /x".
//   - A target with no '/' before its query or fragment is read only as a
//     host and port (hostPort: "CONNECT pay.internal:443") or as "*", the
//     asterisk-form target; anything else is {}, whatever its look
//     ("hunterpassword", "user:hunter", "users?next=/home"), where CallShape
//     would read it as a relative path. A value that reads as a host and port
//     ("alice:4242", "user.secret") is kept as one: u is a URL.
//
// Whitespace around u is not its target's. It is "" for an empty u, and for
// a summary with a '/' before its query or fragment whose first word is
// neither a method nor a target, as CallShape is. A shape is its own shape.
func URLShape(u string) string {
	s := strings.TrimSpace(u)
	if s == "" {
		return ""
	}
	if urlMethodWord(s) {
		return methodShape(s)
	}
	method, target := "", s
	if i := strings.IndexFunc(s, unicode.IsSpace); i >= 0 && urlMethodWord(s[:i]) {
		method, target = methodShape(s[:i])+" ", strings.TrimLeftFunc(s[i:], unicode.IsSpace)
	}
	if loc := absoluteURL.FindStringIndex(target); loc != nil {
		return method + absoluteURLShape(target[:loc[1]-len("://")], target[loc[1]:])
	}
	if rest, ok := strings.CutPrefix(target, pathHole+"://"); ok {
		// A URL whose scheme is already {}.
		return method + absoluteURLShape(pathHole, rest)
	}
	if rest, ok := strings.CutPrefix(target, "//"); ok {
		return method + "//" + authorityShape(rest)
	}
	beforeQuery := target
	if i := strings.IndexAny(target, "?#"); i >= 0 {
		beforeQuery = target[:i]
	}
	slash := strings.IndexByte(beforeQuery, '/')
	switch {
	case slash < 0:
		if target == "*" || hostPort(target) {
			return method + target
		}
		return method + pathHole
	case slash > 0 && (hostPort(target[:slash]) || strings.Contains(target[slash:], "@")):
		return method + hostRestShape(target[:slash], target[slash:], hostPort)
	case slash == 0 && credentialsBeforeAt(target):
		return method + PathShape(holeThrough(target, strings.LastIndexByte(target, '@')))
	case method != "":
		return method + targetShape(target)
	}
	return methodTargetShape(s)
}

// credentialsBeforeAt reports whether p, a path and its query, may hold
// credentials that its last '@' ends (URLShape): a "//" or a ':' comes
// before that '@', as a URL's authority or a user:password does, its "//"
// merged by a proxy or not.
func credentialsBeforeAt(p string) bool {
	at := strings.LastIndexByte(p, '@')
	if at < 0 {
		return false
	}
	return strings.Contains(p[:at], "//") || strings.IndexByte(p[:at], ':') >= 0
}

// urlMethodWord reports whether w is a call's method to URLShape, one it
// keeps or one it writes {}: a method word (isMethodWord), one of
// knownMethods, or {}.
func urlMethodWord(w string) bool {
	return isMethodWord(w) || knownMethods[w] || w == pathHole
}

// methodShape is a call's method as URLShape keeps it: itself when it is one
// of knownMethods, and {} otherwise.
func methodShape(m string) string {
	if knownMethods[m] {
		return m
	}
	return pathHole
}

// absoluteURLShape is URLShape's shape of an absolute URL whose scheme is
// scheme and whose authority, path, query and fragment are rest.
func absoluteURLShape(scheme, rest string) string {
	if !urlScheme(scheme) {
		scheme = pathHole
	}
	return scheme + "://" + authorityShape(rest)
}

// authorityShape is rest, a URL's authority and the path, query and fragment
// that follow it, in its shape (hostRestShape). The authority runs to the
// path, the query or the fragment, and its host follows its last '@', as
// url.Parse reads them.
func authorityShape(rest string) string {
	host := rest
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		host, rest = rest[:i], rest[i:]
	} else {
		rest = ""
	}
	if i := strings.LastIndexByte(host, '@'); i >= 0 {
		host = host[i+1:]
	}
	return hostRestShape(host, rest, urlHost)
}

// hostRestShape is a host, and the path, query and fragment that follow it,
// in their shape (URLShape): the host when isHost reads it as one, else {},
// and the rest as PathShape shapes it, without its fragment. An '@' in the
// rest makes the host {}, and everything before the last one (holeThrough).
func hostRestShape(host, rest string, isHost func(string) bool) string {
	if at := strings.LastIndexByte(rest, '@'); at >= 0 {
		host = pathHole
		rest = holeThrough(rest, at)
	}
	if i := strings.IndexByte(rest, '#'); i >= 0 {
		rest = rest[:i]
	}
	if host != "" && !isHost(host) {
		host = pathHole
	}
	return host + PathShape(rest)
}

// urlDelimiters are what ends a path segment, a query name or a query value.
const urlDelimiters = "/?&=#"

// holeThrough is s, a URL's path, query and fragment, with every path
// segment, query name and query value up to the one that holds s[at] written
// {}, and what follows that one kept.
func holeThrough(s string, at int) string {
	end := len(s)
	if i := strings.IndexAny(s[at:], urlDelimiters); i >= 0 {
		end = at + i
	}
	var b strings.Builder
	inPart := false
	for i := 0; i < end; i++ {
		if strings.IndexByte(urlDelimiters, s[i]) >= 0 {
			b.WriteByte(s[i])
			inPart = false
			continue
		}
		if !inPart {
			b.WriteString(pathHole)
			inPart = true
		}
	}
	return b.String() + s[end:]
}

// knownMethods are the methods URLShape keeps: those of IANA's HTTP method
// registry, and gRPC.
var knownMethods = map[string]bool{
	"ACL": true, "BASELINE-CONTROL": true, "BIND": true, "CHECKIN": true,
	"CHECKOUT": true, "CONNECT": true, "COPY": true, "DELETE": true, "GET": true,
	"HEAD": true, "LABEL": true, "LINK": true, "LOCK": true, "MERGE": true,
	"MKACTIVITY": true, "MKCALENDAR": true, "MKCOL": true, "MKREDIRECTREF": true,
	"MKWORKSPACE": true, "MOVE": true, "OPTIONS": true, "ORDERPATCH": true,
	"PATCH": true, "POST": true, "PRI": true, "PROPFIND": true, "PROPPATCH": true,
	"PUT": true, "QUERY": true, "REBIND": true, "REPORT": true, "SEARCH": true,
	"TRACE": true, "UNBIND": true, "UNCHECKOUT": true, "UNLINK": true,
	"UNLOCK": true, "UPDATE": true, "UPDATEREDIRECTREF": true,
	"VERSION-CONTROL": true, "gRPC": true,
}

// digitSchemes are the URL schemes with a digit in them, which do not read
// as a name (pathSegmentShape) and are kept anyway.
var digitSchemes = map[string]bool{"s3": true, "s3a": true, "s3n": true, "h2": true, "h2c": true}

// urlScheme reports whether s, an absolute URL's scheme, reads as one: each
// of its '+'-separated parts reads as a name, as a path segment does
// (pathSegmentShape: http, wss, mongodb+srv, postgresql), or it is one of
// digitSchemes, or {}.
func urlScheme(s string) bool {
	if s == pathHole || digitSchemes[strings.ToLower(s)] {
		return true
	}
	for _, part := range strings.Split(s, "+") {
		if part == "" || pathSegmentShape(part) != part {
			return false
		}
	}
	return true
}

// urlHost reports whether h, an absolute URL's host and port, reads as one:
// as hostPort reads a host, or as a name with an optional port from 1 to
// 65535, whose dot-separated labels are letters, digits, '-' and '_', and
// whose every part between them reads as a name as a path segment's does
// (pathNamePart: letters, not a long run of hex digits, or an API version):
// localhost, payment-svc, payment_svc, payments.my-ns, api.v2. The "://"
// before it says it is a host, so a name needs no dot or port. A token with
// a digit in it, or a UUID, for a host is no host, unless hostPort reads it
// as one (a label with digits before a last label of letters:
// cluster0.example.net) or it is an API version (v2beta1); one of letters
// alone reads as a name, as it does in a path, and is kept. {} reads as a
// host too.
func urlHost(h string) bool {
	if h == pathHole || hostPort(h) {
		return true
	}
	host := h
	if i := strings.LastIndexByte(h, ':'); i >= 0 {
		host = h[:i]
		if !portNumber(h[i+1:]) {
			return false
		}
	}
	if host == "" {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) > 63 {
			return false
		}
		parts := strings.FieldsFunc(label, func(r rune) bool { return r == '-' || r == '_' })
		if len(parts) == 0 {
			return false
		}
		for _, part := range parts {
			if !pathNamePart(part) {
				return false
			}
		}
	}
	return true
}

// portNumber reports whether p is a port from 1 to 65535, in decimal digits.
func portNumber(p string) bool {
	if p == "" || len(p) > 5 {
		return false
	}
	n := 0
	for i := 0; i < len(p); i++ {
		if !isDigitByte(p[i]) {
			return false
		}
		n = n*10 + int(p[i]-'0')
	}
	return n >= 1 && n <= 65535
}
