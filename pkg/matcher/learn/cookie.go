package learn

import (
	"strings"

	"go.keploy.io/server/v3/pkg/variables"
)

// HeldJWT is a JWT a header holds (HeaderJWTs), and where: Place is the
// token's place, which its claims' leaves are named under (JWT.Leaves:
// "<Place>{jwt}.<claim>"), Cookie the cookie whose value it is, "" for the
// header's whole value.
type HeldJWT struct {
	Place  string
	Cookie string
	JWT    JWT
}

// HeaderJWTs are the JWTs the header at place ("header.<name>") holds as
// value v: the whole value (ReadJWT), or, for a Cookie header
// (variables.CookieHeader), the value of each cookie that is one, at
// "header.cookie{<cookie>}". A cookie whose name two pairs hold has no one
// place, so its token is not read apart: the header is compared whole there.
// None for a Cookie header keploy cannot read as "<name>=<value>" pairs.
func HeaderJWTs(place, v string) []HeldJWT {
	name, ok := strings.CutPrefix(place, "header.")
	if !ok {
		return nil
	}
	if !strings.EqualFold(name, variables.CookieHeader) {
		if j, ok := ReadJWT(v); ok {
			return []HeldJWT{{Place: place, JWT: j}}
		}
		return nil
	}
	pairs, ok := cookiePairs(v)
	if !ok {
		return nil
	}
	var out []HeldJWT
	for _, p := range pairs {
		if p.twice {
			continue
		}
		if j, ok := ReadJWT(v[p.at:p.end]); ok && j.Scheme == "" {
			out = append(out, HeldJWT{Place: place + "{" + p.name + "}", Cookie: p.name, JWT: j})
		}
	}
	return out
}

// TokenHeader is the header place a token's place (HeldJWT.Place) is in:
// "header.cookie" of "header.cookie{session}", the place itself for a
// header's whole value. A header's name is a token, which holds no '{'.
func TokenHeader(token string) string {
	if i := strings.IndexByte(token, '{'); i >= 0 {
		return token[:i]
	}
	return token
}

// CookieValue is the value of the cookie named cookie the Cookie header's
// value v holds, and whether it holds it in one pair alone.
func CookieValue(v, cookie string) (string, bool) {
	p, ok := onePair(v, cookie)
	if !ok {
		return "", false
	}
	return v[p.at:p.end], true
}

// WithCookieValue is the Cookie header's value v with the value of the
// cookie named cookie (one pair alone) replaced by value, every other byte
// of v kept.
func WithCookieValue(v, cookie, value string) (string, bool) {
	p, ok := onePair(v, cookie)
	if !ok {
		return "", false
	}
	return v[:p.at] + value + v[p.end:], true
}

// CookieNames is what of a Cookie header's value v is compared whole beside
// the cookies read apart in it (HeaderJWTs, CookieLeaves): v with the value
// of each cookie one pair alone names replaced by a mark (JWTMark for a JWT,
// cookieMark for any other value), so only the names, their order and a
// cookie two pairs name are compared as recorded. ok is false where v holds
// no cookie whose value is a JWT: the header is compared whole there.
func CookieNames(v string) (string, bool) {
	pairs, ok := cookiePairs(v)
	if !ok {
		return "", false
	}
	var b strings.Builder
	last, found := 0, false
	for _, p := range pairs {
		if p.twice {
			continue
		}
		mark := cookieMark
		if j, ok := ReadJWT(v[p.at:p.end]); ok && j.Scheme == "" {
			mark, found = JWTMark, true
		}
		b.WriteString(v[last:p.at])
		b.WriteString(mark)
		last = p.end
	}
	if !found {
		return "", false
	}
	b.WriteString(v[last:])
	return b.String(), true
}

// cookieMark stands for a cookie's value that is no JWT in CookieNames: a
// leaf of its own (CookieLeaves).
const cookieMark = "{value}"

// CookieLeaves adds to out, for a Cookie header at place ("header.cookie")
// whose value v holds a cookie that is a JWT (CookieNames), each other cookie
// one pair alone names as a leaf of its own ("<place>{<cookie>}"), learned as
// any header value is (noise, a variable, a read of the clock), where the
// header's whole value would be learned only whole: a CSRF or load
// balancer's cookie set afresh on every response moves beside a session
// token whose claims are still compared. None where v holds no such token.
func CookieLeaves(place, v string, out map[string]Leaf) {
	if !strings.EqualFold(place, "header."+variables.CookieHeader) {
		return
	}
	if _, ok := CookieNames(v); !ok {
		return
	}
	pairs, _ := cookiePairs(v)
	for _, p := range pairs {
		if p.twice {
			continue
		}
		val := v[p.at:p.end]
		if j, ok := ReadJWT(val); ok && j.Scheme == "" {
			continue
		}
		path := place + "{" + p.name + "}"
		out[path] = Leaf{Path: path, Noise: path, Type: String, Value: val}
	}
}

// CookieOf is the cookie a leaf's path names one of a Cookie header's
// cookies by (CookieLeaves: "header.cookie{<cookie>}"), and whether it names
// one: a token's claim ("header.cookie{<cookie>}{jwt}.<claim>") names none.
func CookieOf(path string) (string, bool) {
	header, rest, ok := strings.Cut(path, "{")
	if !ok || !strings.EqualFold(header, "header."+variables.CookieHeader) || !strings.HasSuffix(rest, "}") {
		return "", false
	}
	cookie := rest[:len(rest)-1]
	if !cookieName(cookie) {
		return "", false
	}
	return cookie, true
}

// cookieName reports whether name can be a cookie's name keploy names a
// place by: a token (RFC 6265 §4.1.1), so no brace, which ends it in a
// place, and no separator or space ("{other cookies}" names none).
func cookieName(name string) bool {
	return name != "" && !strings.ContainsAny(name, "{}=;, \t\"")
}

// cookiePair is one "<name>=<value>" pair of a Cookie header's value: its
// name, the bytes of its value (v[at:end], the spaces around it left out),
// and whether another pair holds the same name.
type cookiePair struct {
	name    string
	at, end int
	twice   bool
}

// cookiePairs are the pairs of a Cookie header's value v (RFC 6265 §4.2.1:
// "<name>=<value>" joined by "; "), in order; ok is false where a non-empty
// part between ';' holds no '=' after a name, or a name is no token
// (cookieName: a brace, a space), which a place could not name.
func cookiePairs(v string) ([]cookiePair, bool) {
	var out []cookiePair
	seen := map[string]int{}
	for start := 0; start <= len(v); {
		end := strings.IndexByte(v[start:], ';')
		if end < 0 {
			end = len(v)
		} else {
			end += start
		}
		at, stop := start, end
		for at < stop && (v[at] == ' ' || v[at] == '\t') {
			at++
		}
		for stop > at && (v[stop-1] == ' ' || v[stop-1] == '\t') {
			stop--
		}
		if at < stop {
			eq := strings.IndexByte(v[at:stop], '=')
			if eq < 0 {
				return nil, false
			}
			name := strings.TrimRight(v[at:at+eq], " \t")
			if !cookieName(name) {
				return nil, false
			}
			val := at + eq + 1
			for val < stop && (v[val] == ' ' || v[val] == '\t') {
				val++
			}
			if i, ok := seen[name]; ok {
				out[i].twice = true
				out = append(out, cookiePair{name: name, at: val, end: stop, twice: true})
			} else {
				seen[name] = len(out)
				out = append(out, cookiePair{name: name, at: val, end: stop})
			}
		}
		start = end + 1
	}
	return out, true
}

// onePair is the pair named cookie of the Cookie header's value v, where one
// pair alone holds that name.
func onePair(v, cookie string) (cookiePair, bool) {
	pairs, ok := cookiePairs(v)
	if !ok {
		return cookiePair{}, false
	}
	for _, p := range pairs {
		if p.name == cookie {
			return p, !p.twice
		}
	}
	return cookiePair{}, false
}
