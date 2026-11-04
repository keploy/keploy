package http

import (
	"sort"
	"strconv"
	"strings"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/templates"
)

// repeatReadIgnored is the request headers the matcher ignores under the
// proven switch (models.ProvenFlakyHeaderNoise): a repeated read's call is
// compared without them.
var repeatReadIgnored = models.ProvenFlakyHeaderNoise()

// RepeatReadOf is m as one recording of a repeated read: recordings of one
// read are recordings of one call, every one with one answer, and a test that
// recorded such a read at least twice may make it once more or once fewer at
// replay (exact serving's models.TestModeInfo.RepeatRead). It is the one
// proof both exact serving and whatever names a test's repeated reads use,
// for any method: the proof is the recordings, not the method.
//
// call is the request as the matcher compares it under the proven switch:
// its method, URL and body, the name of each header, and the value of Host
// and of each header that says who calls (models.CredentialHeaders). keploy's
// own headers and those the matcher ignores (models.ProvenFlakyHeaderNoise)
// are left out.
//
// answer is the response as a test's response is compared: its status, body
// and binary body, and the name and value of each header but those a server
// mints fresh on every answer (models.IsVolatileResponseHeader). Of a
// Set-Cookie value, the Expires date of a cookie whose Max-Age is a number is
// left out: a client keeps the cookie by its Max-Age and does not read the
// date (RFC 6265 §5.3), and a server writes it from the time it answered.
//
// ok is false for a recording that is not HTTP, has no request or response,
// whose response keploy kept apart and cannot read back, or whose body was
// left out or streamed: keploy cannot say what it answered.
func RepeatReadOf(m *models.Mock) (call, answer string, ok bool) {
	if m == nil || m.Kind != models.HTTP {
		return "", "", false
	}
	c := m.ShallowCopy()
	if err := c.HydrateResponse(); err != nil {
		return "", "", false
	}
	q, a := c.Spec.HTTPReq, c.Spec.HTTPResp
	if q == nil || a == nil || a.BodySkipped || len(a.StreamBody) > 0 {
		return "", "", false
	}
	var qh []string
	for k, v := range q.Header {
		lk := strings.ToLower(k)
		if strings.HasPrefix(lk, "keploy") {
			continue
		}
		if _, ignored := repeatReadIgnored[lk]; ignored {
			continue
		}
		if lk == "host" || isCredentialHeader(lk) {
			qh = append(qh, lk+": "+v)
			continue
		}
		qh = append(qh, lk)
	}
	var ah []string
	for k, v := range a.Header {
		if models.IsVolatileResponseHeader(k) {
			continue
		}
		lk := strings.ToLower(k)
		if lk == "set-cookie" {
			v = withoutCookieDates(v)
		}
		ah = append(ah, lk+": "+v)
	}
	return keyOf(append([]string{string(q.Method), q.URL, q.Body}, sorted(qh)...)),
		keyOf(append([]string{strconv.Itoa(a.StatusCode), a.Body, a.Binary}, sorted(ah)...)), true
}

// isCredentialHeader reports whether lk (lowercased) is one of the headers
// that say who calls (models.CredentialHeaders).
func isCredentialHeader(lk string) bool {
	for _, h := range models.CredentialHeaders {
		if lk == h {
			return true
		}
	}
	return false
}

// withoutCookieDates is a Set-Cookie value without the Expires date of each
// cookie whose Max-Age is a number, every other byte kept: a cookie keploy
// cannot read keeps its date.
func withoutCookieDates(s string) string {
	var b strings.Builder
	from := 0
	for _, c := range templates.SplitSetCookie(s) {
		ma, ok := c.Attr("Max-Age")
		if !ok || !ma.HasValue {
			continue
		}
		if _, err := strconv.Atoi(s[ma.ValueStart:ma.ValueEnd]); err != nil {
			continue
		}
		for _, at := range c.Attrs {
			if strings.EqualFold(at.Name, "Expires") && at.HasValue && at.ValueStart >= from {
				b.WriteString(s[from:at.ValueStart])
				from = at.ValueEnd
			}
		}
	}
	if from == 0 {
		return s
	}
	b.WriteString(s[from:])
	return b.String()
}

// sorted is ss in order.
func sorted(ss []string) []string {
	sort.Strings(ss)
	return ss
}

// keyOf joins fields so that no two lists of fields join the same: each is
// its length, a colon, and itself.
func keyOf(fields []string) string {
	var b strings.Builder
	for _, f := range fields {
		b.WriteString(strconv.Itoa(len(f)))
		b.WriteByte(':')
		b.WriteString(f)
	}
	return b.String()
}
