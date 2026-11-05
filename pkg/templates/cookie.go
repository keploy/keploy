package templates

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// SetCookie is one cookie of a Set-Cookie header value, by byte span. keploy
// stores the Set-Cookie lines of one answer as one value joined by ","
// (pkg.ToYamlHTTPHeader), so a value can hold several cookies; the comma in
// an Expires date does not start one.
type SetCookie struct {
	Name string
	// Start and End span the whole cookie: name=value; attributes.
	Start, End int
	// ValueStart and ValueEnd span its value: after "name=", up to the first
	// ';', without the spaces around it.
	ValueStart, ValueEnd int
	Attrs                []CookieAttr
}

// CookieAttr is one attribute of a cookie (Path=/, Expires=..., HttpOnly).
type CookieAttr struct {
	// Name is as written: "Expires", "max-age".
	Name string
	// ValueStart and ValueEnd span its value after '=', trimmed; HasValue is
	// false for a flag such as HttpOnly.
	ValueStart, ValueEnd int
	HasValue             bool
}

// Attr is the cookie's attribute of that name, in any letter case. When the
// name repeats, the last one is the one a user agent keeps (RFC 6265 §5.3).
func (c SetCookie) Attr(name string) (CookieAttr, bool) {
	for i := len(c.Attrs) - 1; i >= 0; i-- {
		if strings.EqualFold(c.Attrs[i].Name, name) {
			return c.Attrs[i], true
		}
	}
	return CookieAttr{}, false
}

// attrValue is the value of the cookie's attribute of that name in s, and
// whether it has one.
func (c SetCookie) attrValue(s, name string) (string, bool) {
	a, ok := c.Attr(name)
	if !ok || !a.HasValue {
		return "", false
	}
	return s[a.ValueStart:a.ValueEnd], true
}

// Value is the cookie's value in s. A value of two double quotes and nothing
// between them ("") is empty, as the cookie it sets is.
func (c SetCookie) Value(s string) string {
	v := s[c.ValueStart:c.ValueEnd]
	if v == `""` {
		return ""
	}
	return v
}

// CookieID is what makes a cookie one cookie (RFC 6265 §5.3): its name, its
// Domain (in any letter case, without a leading dot) and its Path, each "" when
// the cookie sets none. A later cookie of the same identity in one answer
// replaces an earlier one; cookies of one name and another Path are two
// cookies.
type CookieID struct {
	Name, Domain, Path string
}

// String names the cookie as a person reads it: sid, or sid (Path=/admin).
func (id CookieID) String() string {
	var attrs []string
	if id.Path != "" {
		attrs = append(attrs, "Path="+id.Path)
	}
	if id.Domain != "" {
		attrs = append(attrs, "Domain="+id.Domain)
	}
	if len(attrs) == 0 {
		return id.Name
	}
	return id.Name + " (" + strings.Join(attrs, "; ") + ")"
}

// ID is the cookie's identity in s. render, when not nil, gives the cookie's
// name and an attribute value as they are sent (a placeholder at its value: a
// secret keploy sanitize wrote in a name is the name the recording held).
func (c SetCookie) ID(s string, render func(string) string) CookieID {
	id := CookieID{Name: c.Name}
	if render != nil {
		id.Name = render(c.Name)
	}
	if v, ok := c.attrValue(s, "Domain"); ok {
		if render != nil {
			v = render(v)
		}
		id.Domain = strings.TrimPrefix(strings.ToLower(v), ".")
	}
	if v, ok := c.attrValue(s, "Path"); ok {
		if render != nil {
			v = render(v)
		}
		id.Path = v
	}
	return id
}

// cookiesByID indexes a Set-Cookie value's cookies by identity; of a repeated
// identity the last one is the cookie a user agent keeps.
func cookiesByID(s string) map[CookieID]SetCookie {
	out := map[CookieID]SetCookie{}
	for _, c := range SplitSetCookie(s) {
		out[c.ID(s, nil)] = c
	}
	return out
}

// cookiesOfID groups a Set-Cookie value's cookies by identity, each group in
// the order the value sets them: its last is the one a user agent keeps.
func cookiesOfID(s string) map[CookieID][]SetCookie {
	out := map[CookieID][]SetCookie{}
	for _, c := range SplitSetCookie(s) {
		id := c.ID(s, nil)
		out[id] = append(out[id], c)
	}
	return out
}

// Shadowed reports, for each cookie of cookies (split from s), whether a later
// cookie of the value has its identity: the later one replaces it, so what it
// sets never reaches a user agent's jar.
func Shadowed(cookies []SetCookie, s string, render func(string) string) []bool {
	out := make([]bool, len(cookies))
	seen := map[CookieID]bool{}
	for i := len(cookies) - 1; i >= 0; i-- {
		id := cookies[i].ID(s, render)
		out[i] = seen[id]
		seen[id] = true
	}
	return out
}

// lifetimeSlack is how far an answer's cookie lifetime (Expires − Date) may be
// from the recorded one: the two dates are written a moment apart, and a
// second can tick between them.
const lifetimeSlack = 5 * time.Second

// cookieTime reads a cookie date as servers write it: RFC 1123 with "GMT"
// (Go, Node, Python, Java), with dashes (PHP, Symfony: Thu, 01-Jan-1970
// 00:00:01 GMT), RFC 850 and asctime.
func cookieTime(v string) (time.Time, bool) {
	v = strings.TrimSpace(v)
	if t, err := http.ParseTime(v); err == nil {
		return t, true
	}
	for _, layout := range []string{"Mon, 02-Jan-2006 15:04:05 MST", "Mon, 02-Jan-06 15:04:05 MST", time.RFC1123, time.RFC1123Z} {
		if t, err := time.Parse(layout, v); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// responseTime is a response's Date header, read; the zero time when it has
// none that reads.
func responseTime(h map[string]string) time.Time {
	if v, ok := headerLookup(h, "Date"); ok {
		if t, ok := cookieTime(v); ok {
			return t
		}
	}
	return time.Time{}
}

// deletedAt says how a cookie is deleted at time at, or "" when a user agent
// keeps it then (RFC 6265 §5.3): a Max-Age that is a number takes precedence
// and deletes it at 0 or below; otherwise an Expires date before at does. A
// cookie with neither lives for the session.
func (c SetCookie) deletedAt(s string, at time.Time) string {
	if v, ok := c.attrValue(s, "Max-Age"); ok {
		if n, err := strconv.Atoi(v); err == nil {
			if n <= 0 {
				return "Max-Age=" + v
			}
			return ""
		}
	}
	if v, ok := c.attrValue(s, "Expires"); ok {
		if t, ok := cookieTime(v); ok && t.Before(at) {
			return "Expires=" + v
		}
	}
	return ""
}

// expiresHolds reports whether an answer's Expires date gives its cookie the
// lifetime the recording gave it: the answer's (Expires − Date) within
// lifetimeSlack of the recorded (Expires − Date). Without a Date on both sides
// (recordedAt or answerAt zero) the answer's date must read and, when the
// recorded cookie was not expired, must not be before the answer's Date, or
// before now when the answer has no Date.
func expiresHolds(recorded string, recordedAt time.Time, answer string, answerAt, now time.Time) bool {
	ans, ok := cookieTime(answer)
	if !ok {
		return false
	}
	rec, recOK := cookieTime(recorded)
	if recOK && !recordedAt.IsZero() && !answerAt.IsZero() {
		d := ans.Sub(answerAt) - rec.Sub(recordedAt)
		return d <= lifetimeSlack && d >= -lifetimeSlack
	}
	recordedAlive := !recOK || recordedAt.IsZero() || !rec.Before(recordedAt)
	if !recordedAlive {
		return true
	}
	at := answerAt
	if at.IsZero() {
		at = now
	}
	return !ans.Before(at)
}

// SplitSetCookie finds the cookies of a Set-Cookie header value: one per
// line, or joined by ",", each a name=value pair with its attributes.
func SplitSetCookie(s string) []SetCookie {
	var out []SetCookie
	start := 0
	for start < len(s) {
		for start < len(s) && (s[start] == ' ' || s[start] == '\t' || s[start] == '\n' || s[start] == '\r' || s[start] == ',') {
			start++
		}
		if start >= len(s) {
			break
		}
		end := nextCookieStart(s, start)
		if c, ok := parseCookie(s, start, end); ok {
			out = append(out, c)
		}
		start = end
	}
	return out
}

// nextCookieStart is where the cookie starting at from ends: at a newline,
// or at a comma that starts another name=value.
func nextCookieStart(s string, from int) int {
	for i := from; i < len(s); i++ {
		switch s[i] {
		case '\n':
			return i
		case ',':
			rest := strings.TrimLeft(s[i+1:], " \t")
			head := rest
			if k := strings.IndexByte(head, ';'); k >= 0 {
				head = head[:k]
			}
			if eq := strings.IndexByte(head, '='); eq > 0 && cookieNameStart(rest) && !strings.ContainsAny(head[:eq], " ,\t") {
				return i
			}
		}
	}
	return len(s)
}

// cookieNameStart recognizes the first byte of an HTTP token (RFC 6265
// section 4.1.1). In particular, a leading dot is common in ASP.NET cookies.
func cookieNameStart(name string) bool {
	if name == "" {
		return false
	}
	c := name[0]
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' ||
		c >= '0' && c <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", rune(c))
}

// parseCookie reads the cookie in s[start:end].
func parseCookie(s string, start, end int) (SetCookie, bool) {
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t' || s[end-1] == '\r') {
		end--
	}
	headEnd := end
	if k := strings.IndexByte(s[start:end], ';'); k >= 0 {
		headEnd = start + k
	}
	eq := strings.IndexByte(s[start:headEnd], '=')
	if eq <= 0 {
		return SetCookie{}, false
	}
	name := strings.TrimSpace(s[start : start+eq])
	if name == "" {
		return SetCookie{}, false
	}
	vs, ve := trimSpan(s, start+eq+1, headEnd)
	c := SetCookie{Name: name, Start: start, End: end, ValueStart: vs, ValueEnd: ve}
	for pos := headEnd; pos < end; {
		next := end
		if k := strings.IndexByte(s[pos+1:end], ';'); k >= 0 {
			next = pos + 1 + k
		}
		as, ae := trimSpan(s, pos+1, next)
		if as < ae {
			a := CookieAttr{Name: s[as:ae]}
			if k := strings.IndexByte(s[as:ae], '='); k >= 0 {
				a.Name = strings.TrimSpace(s[as : as+k])
				a.ValueStart, a.ValueEnd = trimSpan(s, as+k+1, ae)
				a.HasValue = true
			}
			c.Attrs = append(c.Attrs, a)
		}
		pos = next
	}
	return c, true
}

func trimSpan(s string, start, end int) (int, int) {
	for start < end && (s[start] == ' ' || s[start] == '\t') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t') {
		end--
	}
	return start, end
}
