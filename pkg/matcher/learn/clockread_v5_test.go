package learn

import (
	"encoding/base64"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// A read of the clock learned inside a percent-encoded text, at its place
// in the text decoded once (models.ClockWordDecoded: a query string's
// createdAt%3E2026-10-26T11%3A14%3A17Z), is checked at replay in the call's
// text decoded the same way: the application's value passes at every phase
// of the second, and a stale value, one a minute ahead, and a text whose
// side of the word changed fail. A word learned in a text as it is (Form
// "") reads the raw text, as before: the application's value passes there,
// and the same value percent-encoded is a text that changed.
func TestAWordLearnedInItsTextDecodedOnceIsReadThereAtReplay(t *testing.T) {
	sec, ms := time.Second, time.Millisecond
	p := "url.query.q"
	stamp := func(at time.Time) string { return at.UTC().Format(time.RFC3339) }
	raw := func(at time.Time) string { return "q=createdAt>" + stamp(at) + "&limit=10" }
	encoded := func(at time.Time) string { return "q=" + url.QueryEscape("createdAt>"+stamp(at)) + "&limit=10" }
	recAt := time.Date(2026, 10, 26, 11, 14, 17, 0, time.UTC)
	word := func(form string) models.ClockOffset {
		d := raw(recAt)
		st := strings.Index(d, "2026")
		w := models.ClockWordOf(d, st, st+len(stamp(recAt)))
		w.Form = form
		return models.ClockOffset{Res: sec, Word: w, Layout: clockTextLayout(stamp(recAt))}
	}
	check := func(o models.ClockOffset, v string, s Span) (bool, string) {
		return CheckClockRead(p, o, Leaf{Path: p, Noise: p, Type: String, Value: v}, s, true)
	}
	base := recAt.Add(48 * time.Hour)
	for _, c := range []struct {
		form string
		text func(time.Time) string
	}{
		{models.ClockWordDecoded, encoded},
		{"", raw},
	} {
		o := word(c.form)
		for phase := time.Duration(0); phase < sec; phase += 50 * ms {
			s := Span{From: base.Add(phase), To: base.Add(phase + 61*ms)}
			read := s.To.Add(-20 * ms)
			for _, row := range []struct {
				name string
				v    string
				want bool
			}{
				{"the application's value", c.text(read), true},
				{"the recorded value, two days stale", c.text(recAt), false},
				{"a minute ahead", c.text(read.Add(time.Minute)), false},
				{"a side of the word changed", strings.Replace(c.text(read), "limit=10", "limit=11", 1), false},
			} {
				if got, why := check(o, row.v, s); got != row.want {
					t.Errorf("%s: form %q, %s into its second, %s %q: %v (%s)", map[bool]string{true: "FALSE RED", false: "FALSE GREEN"}[row.want], c.form, phase, row.name, row.v, got, why)
				}
			}
		}
	}
	s := Span{From: base, To: base.Add(61 * ms)}
	if got, why := check(word(""), encoded(base), s); got {
		t.Errorf("FALSE GREEN: a word learned in the raw text passed the same value percent-encoded: %s", why)
	}
}

// A read of the clock learned inside a base64 cursor (a page token whose
// JSON carries the instant it pages after, as the query's
// cursor=eyJhZnRlciI6...), at its place in the text decoded once (the run
// decoded where it stands: {"after":"2026-10-26T11:14:17Z","id":7}), is
// checked at replay in the call's cursor decoded the same way: the
// application's value passes at every phase of the second, and a stale
// value, one a minute ahead, and a cursor whose JSON beside the word
// changed fail. Read raw, the cursor holds no word at that place.
func TestAWordLearnedInABase64CursorIsReadInItsDecodedText(t *testing.T) {
	sec, ms := time.Second, time.Millisecond
	p := "url.query.cursor"
	stamp := func(at time.Time) string { return at.UTC().Format(time.RFC3339) }
	plain := func(at time.Time, id int) string {
		return `{"after":"` + stamp(at) + `","id":` + strconv.Itoa(id) + `}`
	}
	cursor := func(at time.Time, id int) string { return base64.RawURLEncoding.EncodeToString([]byte(plain(at, id))) }
	recAt := time.Date(2026, 10, 26, 11, 14, 17, 0, time.UTC)
	d := plain(recAt, 7)
	st := strings.Index(d, "2026")
	w := models.ClockWordOf(d, st, st+len(stamp(recAt)))
	w.Form = models.ClockWordDecoded
	o := models.ClockOffset{Res: sec, Word: w, Layout: clockTextLayout(stamp(recAt))}
	if got := decodedWordForm(cursor(recAt, 7)); got != d {
		t.Fatalf("the recorded cursor decodes to %q, want %q", got, d)
	}
	base := recAt.Add(48 * time.Hour)
	for phase := time.Duration(0); phase < sec; phase += 50 * ms {
		s := Span{From: base.Add(phase), To: base.Add(phase + 61*ms)}
		read := s.To.Add(-20 * ms)
		for _, row := range []struct {
			name string
			v    string
			want bool
		}{
			{"the application's cursor", cursor(read, 7), true},
			{"the recorded cursor, two days stale", cursor(recAt, 7), false},
			{"a minute ahead", cursor(read.Add(time.Minute), 7), false},
			{"the JSON beside the word changed", cursor(read, 8), false},
		} {
			got, why := CheckClockRead(p, o, Leaf{Path: p, Noise: p, Type: String, Value: row.v}, s, true)
			if got != row.want {
				t.Errorf("%s: %s into its second, %s %q: %v (%s)", map[bool]string{true: "FALSE RED", false: "FALSE GREEN"}[row.want], phase, row.name, row.v, got, why)
			}
		}
	}
}
