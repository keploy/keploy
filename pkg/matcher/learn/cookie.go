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

// CookiesBesideJWTs is what of a Cookie header's value v is compared whole
// beside the tokens HeaderJWTs reads apart in it: v with each such token
// replaced by variables.JWTMark, so every other cookie, the names and their
// order are compared as recorded. ok is false where v holds no such token.
func CookiesBesideJWTs(v string) (string, bool) {
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
		if j, ok := ReadJWT(v[p.at:p.end]); ok && j.Scheme == "" {
			b.WriteString(v[last:p.at])
			b.WriteString(JWTMark)
			last, found = p.end, true
		}
	}
	if !found {
		return "", false
	}
	b.WriteString(v[last:])
	return b.String(), true
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
// part between ';' holds no '=' after a name, or a name holds a brace,
// which a place could not name.
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
			if name == "" || strings.ContainsAny(name, "{}") {
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
