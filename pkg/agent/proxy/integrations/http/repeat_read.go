package http

import (
	"regexp"
	"sort"
	"strconv"
	"strings"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/templates"
	"go.uber.org/zap"
)

// RepeatReadNoise is the request header noise the strict matcher excuses
// under the proven switch for a set's calls (NewRepeatReadNoise): a repeated
// read's proof (RepeatReadOf) excuses those headers and no other. The zero
// value excuses none, so a proof made without the set's noise is never wider
// than the matcher.
type RepeatReadNoise struct {
	// whole are the header names the matcher never compares; pats, by
	// lowercased name, the headers it compares unless the recorded and the
	// live value each match one (splitHeaderNoise).
	whole map[string]bool
	pats  map[string][]*regexp.Regexp
}

// NewRepeatReadNoise is the header noise the strict matcher compares a set's
// calls with under opts, which a replay hands its agent for the set
// (NewRequestNoise): keploy.yml's header noise, and the static list
// (models.ProvenFlakyHeaderNoise) unless the run turns it off
// (test.disableAutoHeaderNoise). Only opts' NoiseConfig and
// DisableAutoHeaderNoise are read. A pattern that does not compile excuses no
// value, as in the matcher.
func NewRepeatReadNoise(opts models.OutgoingOptions) RepeatReadNoise {
	whole, pats := splitHeaderNoise(zap.NewNop(), requestHeaderNoise(opts, models.ProvenFlakyHeaderNoise()))
	return RepeatReadNoise{whole: whole, pats: pats}
}

// RepeatReadOf is m as one recording of a repeated read: recordings of one
// read are recordings of one call, every one with one answer, and a test that
// recorded such a read at least twice may make it once more or once fewer at
// replay (exact serving's models.TestModeInfo.RepeatRead). It is the one
// proof both exact serving and whatever names a test's repeated reads use,
// for any method: the proof is the recordings, not the method.
//
// call is the request with every part the strict matcher compares under the
// proven switch, byte for byte: its method, URL and body, and each header
// with its value. Left out are only the headers the matcher does not compare
// (keploy's own, and those noise names whole: n's keploy.yml header noise
// and static list). Of a header the matcher excuses within keploy.yml's
// patterns, a recorded value that matches one is kept as matching, not by
// its bytes; of one the recording learned as noise (header.<name>), that it
// was learned. Two recordings are one call only when they agree on each:
// the matcher then takes a call for either one exactly when it takes it for
// the other. A header the matcher reads more loosely than by its bytes
// (Content-Type, User-Agent, a SigV4 Authorization, a value the obfuscator
// redacted) is compared by its bytes here, which is narrower, never wider.
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
func RepeatReadOf(m *models.Mock, n RepeatReadNoise) (call, answer string, ok bool) {
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
	learned := strictKnown{learned: c.Spec.ReqBodyNoise}
	var qh []string
	for k, v := range q.Header {
		lk := strings.ToLower(k)
		if strings.HasPrefix(lk, "keploy") || n.whole[lk] {
			continue
		}
		if pats, guarded := n.pats[lk]; guarded {
			if matchesAny(pats, v) {
				qh = append(qh, lk+" (within keploy.yml's patterns)")
				continue
			}
		} else if learned.learnedNoiseAt("header."+lk, "") {
			qh = append(qh, lk+" (learned)")
			continue
		}
		qh = append(qh, lk+": "+v)
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

// withoutCookieDates is a Set-Cookie value without the Expires date of each
// cookie whose Max-Age is a number, every other byte kept: a cookie keploy
// cannot read keeps its date.
func withoutCookieDates(s string) string {
	var b strings.Builder
	from := 0
	for _, c := range templates.SplitSetCookie(s) {
		// A Max-Age absent or with no value spans nothing, which is no
		// number.
		ma, _ := c.Attr("Max-Age")
		if _, err := strconv.Atoi(s[ma.ValueStart:ma.ValueEnd]); err != nil {
			continue
		}
		for _, at := range c.Attrs {
			if at.HasValue && strings.EqualFold(at.Name, "Expires") {
				b.WriteString(s[from:at.ValueStart])
				from = at.ValueEnd
			}
		}
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
