package templates

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestSplitSetCookie_SeveralCookiesAndAnExpiresDate(t *testing.T) {
	h := "a=1; Path=/; Expires=Thu, 23 Oct 2026 10:00:00 GMT, sid=S1; HttpOnly\nlang=en,b= 2 ;Max-Age=3"
	var got []string
	for _, c := range SplitSetCookie(h) {
		s := c.Name + "=" + h[c.ValueStart:c.ValueEnd] + " |"
		for _, a := range c.Attrs {
			s += " " + a.Name
			if a.HasValue {
				s += "=" + h[a.ValueStart:a.ValueEnd]
			}
		}
		got = append(got, s)
	}
	assert.Equal(t, []string{
		"a=1 | Path=/ Expires=Thu, 23 Oct 2026 10:00:00 GMT",
		"sid=S1 | HttpOnly",
		"lang=en |",
		"b=2 | Max-Age=3",
	}, got)
}

// RFC 6265 §5.3: a cookie is its name, Domain (any case, no leading dot) and
// Path; of one identity in one value the last stays, and the earlier ones
// are shadowed.
func TestCookieIdentity(t *testing.T) {
	v := "sid=a; Path=/,sid=b; Path=/admin,sid=c; Path=/; Domain=.Example.com,sid=d; path=/,sid=e; Path=/; Domain=example.COM"
	cs := SplitSetCookie(v)
	assert.Equal(t, []bool{true, false, true, false, false}, Shadowed(cs, v, nil))
	byID := cookiesByID(v)
	assert.Equal(t, "d", byID[CookieID{Name: "sid", Path: "/"}].Value(v))
	assert.Equal(t, "b", byID[CookieID{Name: "sid", Path: "/admin"}].Value(v))
	assert.Equal(t, "e", byID[CookieID{Name: "sid", Path: "/", Domain: "example.com"}].Value(v))
	assert.Equal(t, "sid (Path=/admin)", CookieID{Name: "sid", Path: "/admin"}.String())
	assert.Equal(t, "", SplitSetCookie(`a=""`)[0].Value(`a=""`), "two double quotes are an empty value")
	assert.Equal(t, `"x"`, SplitSetCookie(`a="x"`)[0].Value(`a="x"`))
}

func TestCookieLifetime(t *testing.T) {
	for _, v := range []string{"Thu, 01 Jan 1970 00:00:00 GMT", "Thu, 01-Jan-1970 00:00:00 GMT", "Thursday, 01-Jan-70 00:00:00 GMT", "Thu Jan  1 00:00:00 1970"} {
		got, ok := cookieTime(v)
		assert.True(t, ok, v)
		assert.Equal(t, int64(0), got.Unix(), v)
	}
	at := time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)
	del := func(v string) string { c := SplitSetCookie(v)[0]; return c.deletedAt(v, at) }
	assert.Equal(t, "Max-Age=0", del("a=1; Max-Age=0; Expires=Fri, 23 Oct 2026 10:00:00 GMT"))
	assert.Equal(t, "Max-Age=-1", del("a=1; Max-Age=-1"))
	assert.Equal(t, "", del("a=1; Max-Age=60; Expires=Thu, 01 Jan 1970 00:00:00 GMT"), "Max-Age takes precedence")
	assert.Equal(t, "Expires=Thu, 01 Jan 1970 00:00:00 GMT", del("a=1; Expires=Thu, 01 Jan 1970 00:00:00 GMT"))
	assert.Equal(t, "", del("a=1; HttpOnly"), "a session cookie")

	rec := "Mon, 05 Oct 2026 11:00:00 GMT" // an hour after at
	ansAt := at.Add(24 * time.Hour)
	assert.True(t, expiresHolds(rec, at, "Tue, 06 Oct 2026 11:00:04 GMT", ansAt, time.Time{}))
	assert.False(t, expiresHolds(rec, at, "Tue, 06 Oct 2026 11:00:06 GMT", ansAt, time.Time{}))
	assert.False(t, expiresHolds(rec, at, "Thu, 01 Jan 1970 00:00:00 GMT", ansAt, time.Time{}))
	assert.False(t, expiresHolds(rec, at, "later", ansAt, time.Time{}))
	assert.True(t, expiresHolds(rec, at, "Tue, 06 Oct 2026 12:00:00 GMT", time.Time{}, ansAt), "no Date: not before now")
	assert.False(t, expiresHolds(rec, at, "Mon, 05 Oct 2026 12:00:00 GMT", time.Time{}, ansAt), "no Date: before now")
}
