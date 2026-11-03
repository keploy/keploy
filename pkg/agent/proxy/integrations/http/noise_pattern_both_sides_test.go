package http

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// keploy.yml's noise entry with patterns lets a field vary only within
// them: the learn step's comparison excuses a body leaf or a header only
// when its recorded and its live value each match one. A live value outside
// them, or a side that lacks the field, is a change.
func TestLearnNoisePatternHoldsTheRecordedAndTheLiveValue(t *testing.T) {
	h, _, _ := learnOn(t)
	opts := models.OutgoingOptions{Learn: true, NoiseConfig: map[string]map[string][]string{"header": {"X-Id": {`^a-\d$`}}}}
	n := h.newLearnNoise(opts, map[string][]string{"id": {`^1$`}}, nil)
	for _, c := range []struct {
		name, recBody, recID, body, id string
		same                           bool
	}{
		{"both values match", `{"id":1}`, "a-1", `{"id":1}`, "a-2", true},
		{"the live body value is outside", `{"id":1}`, "a-1", `{"id":2}`, "a-1", false},
		{"the recorded body value is outside", `{"id":2}`, "a-1", `{"id":1}`, "a-1", false},
		{"the live body lacks the field", `{"id":1}`, "a-1", `{}`, "a-1", false},
		{"the recorded body lacks the field", `{}`, "a-1", `{"id":1}`, "a-1", false},
		{"the live header value is outside", `{"id":1}`, "a-1", `{"id":1}`, "b-1", false},
		{"the recorded header value is outside", `{"id":1}`, "b-1", `{"id":1}`, "a-1", false},
		{"a null the pattern does not name", `{"id":null}`, "a-1", `{"id":1}`, "a-1", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := learnRecorded("m", 0, "POST", "/v1/x", c.recBody, map[string]string{"X-Id": c.recID}, `{}`)
			in := live("POST", "/v1/x", c.body, map[string]string{"X-Id": c.id})
			d := learn.Compare(n.shape(liveParts(context.Background(), in), ""), n.shape(recordedParts(m), ""), n.known(m))
			if same := d.Structural == "" && len(d.Drift) == 0; same != c.same {
				t.Fatalf("same = %v (%+v), want %v", same, d, c.same)
			}
		})
	}
	// A pattern that names null excuses a recorded null.
	nullable := h.newLearnNoise(opts, map[string][]string{"id": {`^(null|1)$`}}, nil)
	m := learnRecorded("m", 0, "POST", "/v1/x", `{"id":null}`, map[string]string{"X-Id": "a-1"}, `{}`)
	in := live("POST", "/v1/x", `{"id":1}`, map[string]string{"X-Id": "a-1"})
	if d := learn.Compare(nullable.shape(liveParts(context.Background(), in), ""), nullable.shape(recordedParts(m), ""), nullable.known(m)); d.Structural != "" || len(d.Drift) > 0 {
		t.Fatalf("a recorded null the pattern names was compared: %+v", d)
	}
}

// A field one side lacks is excused by keploy.yml only through an entry with
// no patterns, whichever side holds it: an entry with patterns, even one an
// empty value matches, leaves a header the recording or the call lacks a
// change. A variable's place excuses a field the recording lacks.
func TestLearnNoiseOnAFieldOneSideLacks(t *testing.T) {
	h, _, _ := learnOn(t)
	opts := models.OutgoingOptions{Learn: true, NoiseConfig: map[string]map[string][]string{"header": {"X-Trace": {`^(t-\d+)?$`}}}}
	n := h.newLearnNoise(opts, map[string][]string{"extra": {}}, nil)
	trace := map[string]string{"X-Trace": "t-1"}
	for _, c := range []struct {
		name, recBody, body string
		recHeader, header   map[string]string
		at                  string
		same                bool
	}{
		{"a body field only the call holds, an entry with no patterns", `{"id":1}`, `{"id":1,"extra":2}`, nil, nil, "", true},
		{"a body field only the recording holds, an entry with no patterns", `{"id":1,"extra":2}`, `{"id":1}`, nil, nil, "", true},
		{"a body field only the call holds, no entry", `{"id":1}`, `{"id":1,"page":2}`, nil, nil, "", false},
		{"a body field only the call holds, at a variable's place", `{"id":1}`, `{"id":1,"page":2}`, nil, nil, "body.page", true},
		{"a header only the call holds, a pattern an empty value matches", `{}`, `{}`, nil, trace, "", false},
		{"a header only the recording holds, a pattern an empty value matches", `{}`, `{}`, trace, nil, "", false},
		{"a header both hold, values the pattern matches", `{}`, `{}`, trace, map[string]string{"X-Trace": "t-2"}, "", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := learnRecorded("m", 0, "POST", "/v1/x", c.recBody, c.recHeader, `{}`)
			in := live("POST", "/v1/x", c.body, c.header)
			known := n.known(m)
			if c.at != "" {
				known = excusedAt{Known: known, at: map[string]bool{c.at: true}}
			}
			d := learn.Compare(n.shape(liveParts(context.Background(), in), ""), n.shape(recordedParts(m), ""), known)
			if same := d.Structural == "" && len(d.Drift) == 0; same != c.same {
				t.Fatalf("same = %v (%+v), want %v", same, d, c.same)
			}
		})
	}
}

// The strict comparison holds a header keploy.yml guards with patterns to
// both values, and its reason names the field, the patterns and the side
// outside them by its length, never the value.
func TestStrictHeaderNoisePatternHoldsBothSides(t *testing.T) {
	ct := map[string]string{"Content-Type": "application/json"}
	rec := recorded("m", "/x", map[string]string{"Content-Type": "application/json", "X-Id": "a-1"}, `{}`)
	noise := map[string][]string{"x-id": {`^a-\d$`}}
	with := func(id string) map[string]string {
		h := map[string]string{"X-Id": id}
		for k, v := range ct {
			h[k] = v
		}
		return h
	}
	if got, diag := strictMatch(t, sent("/x", with("a-2"), `{}`), noise, nil, nil, rec); got == nil {
		t.Fatalf("a header whose values both match was compared: %v", differs(diag))
	}
	got, diag := strictMatch(t, sent("/x", with("secret-b"), `{}`), noise, nil, nil, rec)
	if got != nil || !hasField(differs(diag), "header.x-id") {
		t.Fatalf("a live header value outside the pattern: served %v, differs %v", got != nil, differs(diag))
	}
	why := whyOf(diag, "header.x-id")
	for _, want := range []string{"header.x-id", `"^a-\\d$"`, "live value (a string of 8 bytes)"} {
		if !strings.Contains(why, want) {
			t.Fatalf("reason %q does not say %q", why, want)
		}
	}
	if strings.Contains(why, "secret-b") {
		t.Fatalf("reason %q says the value", why)
	}
	other := recorded("m", "/x", map[string]string{"Content-Type": "application/json", "X-Id": "z"}, `{}`)
	if got, diag := strictMatch(t, sent("/x", with("a-1"), `{}`), noise, nil, nil, other); got != nil || !strings.Contains(whyOf(diag, "header.x-id"), "recorded value (a string of 1 bytes)") {
		t.Fatalf("a recorded header value outside the pattern: served %v, reason %q", got != nil, whyOf(diag, "header.x-id"))
	}
	if got, diag := strictMatch(t, sent("/x", ct, `{}`), noise, nil, nil, rec); got != nil || !strings.Contains(whyOf(diag, "header.x-id"), "live value (absent)") {
		t.Fatalf("a header the call lacks: served %v, reason %q", got != nil, whyOf(diag, "header.x-id"))
	}
	if got, diag := strictMatch(t, sent("/x", with("a-1"), `{}`), noise, nil, nil, recorded("m", "/x", ct, `{}`)); got != nil || !strings.Contains(whyOf(diag, "header.x-id"), "recorded value (absent)") {
		t.Fatalf("a header the recording lacks: served %v, reason %q", got != nil, whyOf(diag, "header.x-id"))
	}
	// A header no pattern guards gives no such reason.
	if _, diag := strictMatch(t, sent("/x", with("a-1"), `{}`), noise, nil, nil, recorded("m", "/x", map[string]string{"Content-Type": "application/json", "X-Id": "a-1", "X-Other": "1"}, `{}`)); whyOf(diag, "header.x-other") != "" || !hasField(differs(diag), "header.x-other") {
		t.Fatalf("an unguarded header: differs %v, reason %q", differs(diag), whyOf(diag, "header.x-other"))
	}
	// A pattern that does not compile excuses no value, and the call is
	// still compared.
	if got, diag := strictMatch(t, sent("/x", with("a-2"), `{}`), map[string][]string{"x-id": {`(`}}, nil, nil, rec); got != nil || !hasField(differs(diag), "header.x-id") {
		t.Fatalf("a header whose pattern does not compile: served %v, differs %v", got != nil, differs(diag))
	}
	// A header entry with no patterns still excuses any value.
	if got, diag := strictMatch(t, sent("/x", with("anything"), `{}`), map[string][]string{"x-id": {}}, nil, nil, rec); got == nil {
		t.Fatalf("a header keploy.yml names with no patterns was compared: %v", differs(diag))
	}
}

// The strict comparison's reason for a body field keploy.yml guards with
// patterns: the field, the patterns and the outside side's shape.
func TestStrictBodyNoisePatternSaysWhy(t *testing.T) {
	ct := map[string]string{"Content-Type": "application/json"}
	noise := map[string][]string{"id": {`^1$`}}
	for _, c := range []struct{ name, rec, live, want string }{
		{"the live value", `{"id":1}`, `{"id":"tok-123"}`, "live value (a string of 7 bytes)"},
		{"the recorded value", `{"id":5}`, `{"id":1}`, "recorded value (a number of 1 characters)"},
		{"the live side lacks it", `{"id":1}`, `{}`, "live value (absent)"},
		{"the recorded side lacks it", `{}`, `{"id":1}`, "recorded value (absent)"},
		{"a value of another kind", `{"id":1}`, `{"id":{"a":1}}`, "live value (an object)"},
		{"an array of another length", `{"id":[1]}`, `{"id":[1,2]}`, "live value (an array)"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, diag := strictMatch(t, sent("/x", ct, c.live), nil, noise, nil, recorded("m", "/x", ct, c.rec))
			why := whyOf(diag, "body.id")
			if got != nil || !strings.Contains(why, `body.id vary only within "^1$"`) || !strings.Contains(why, c.want) {
				t.Fatalf("served %v, reason %q; want %q", got != nil, why, c.want)
			}
			if strings.Contains(why, "tok-123") {
				t.Fatalf("reason %q says the value", why)
			}
		})
	}
	// A field no pattern guards gives no such reason.
	if _, diag := strictMatch(t, sent("/x", ct, `{"n":2}`), nil, noise, nil, recorded("m", "/x", ct, `{"n":1}`)); whyOf(diag, "body.n") != "" {
		t.Fatalf("an unguarded field has reason %q", whyOf(diag, "body.n"))
	}
}

// A place learned as a read of the application's clock that keploy.yml's
// pattern reaches is excused only when its recorded and its live value
// each match it; otherwise its clock read is checked.
func TestAClockReadKeployYmlPatternHoldsBothSides(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	base := time.Date(2026, 10, 26, 15, 50, 35, 0, time.UTC)
	old := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC).Format(time.RFC3339)
	stamp := []string{`^\d{4}-\d{2}-\d{2}T`}
	for _, c := range []struct {
		name, recHeader, recBody string
		header, body             map[string][]string
		place                    string
		differs                  bool
	}{
		{"a body value both match", old, `{"at":"` + old + `"}`, nil, map[string][]string{"at": stamp}, "body.at", false},
		{"a body value only the live one matches", old, `{"at":"later"}`, nil, map[string][]string{"at": stamp}, "body.at", true},
		{"a header both match", old, `{}`, map[string][]string{"x-sent-at": stamp}, nil, "header.x-sent-at", false},
		{"a header only the live one matches", "later", `{}`, map[string][]string{"x-sent-at": stamp}, nil, "header.x-sent-at", true},
		{"a body value only the recorded one matches", old, `{"at":"` + old + `"}`, nil, map[string][]string{"at": stamp}, "body.at", true},
		{"a header only the recorded one matches", old, `{}`, map[string][]string{"x-sent-at": stamp}, nil, "header.x-sent-at", true},
		{"a body value recorded as null, a pattern that admits null", old, `{"at":null}`, nil, map[string][]string{"at": {`^(null|\d{4}-\d{2}-\d{2}T.*)$`}}, "body.at", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := recorded("m", "/v1/u", map[string]string{"Content-Type": "application/json", "X-Sent-At": c.recHeader}, c.recBody)
			m.Spec.Clocks = map[string]models.ClockOffset{c.place: {Res: time.Second}}
			liveAt := old
			if strings.Contains(c.name, "only the recorded") {
				liveAt = "later"
			}
			in := sent("/v1/u", map[string]string{"Content-Type": "application/json", "X-Sent-At": liveAt}, `{"at":"`+liveAt+`"}`)
			if c.recBody == `{}` {
				in = sent("/v1/u", map[string]string{"Content-Type": "application/json", "X-Sent-At": liveAt}, `{}`)
			}
			in.arrived = base.Add(2 * time.Second)
			got, diag := strictMatchIn(t, clockConn(true, base), in, c.header, c.body, nil, m)
			if refused := got == nil; refused != c.differs || c.differs && !hasField(differs(diag), c.place) {
				t.Fatalf("refused %v, differs %v; want refused %v naming %s", refused, differs(diag), c.differs, c.place)
			}
		})
	}
}

// whyOf is the reason the field at path was compared, "" when none.
func whyOf(diag *matchDiag, path string) string {
	if diag == nil {
		return ""
	}
	for _, f := range diag.differs {
		if f.diff.Path == path {
			return f.why
		}
	}
	return ""
}

// A miss on a field a keploy.yml pattern guards warns with its reason, as
// such, never as a read of the application's clock.
func TestStrictNoisePatternMissWarnsItsReason(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	rec := recorded("m", "/x", map[string]string{"Content-Type": "application/json"}, `{"id":1}`)
	in := sent("/x", map[string]string{"Content-Type": "application/json"}, `{"id":2}`)
	core, logs := observer.New(zap.WarnLevel)
	h := &HTTP{Logger: zap.New(core)}
	ok, _, _, err := h.match(context.Background(), in, &mockMemDb{mocks: []*models.Mock{rec}, updateUnFilteredReturn: true}, models.ProvenFlakyHeaderNoise(), map[string][]string{"id": {`^1$`}}, nil, true, false, false, true, true)
	if err != nil || ok {
		t.Fatalf("match ok=%v err=%v; want the live id outside the pattern refused", ok, err)
	}
	if n := logs.FilterMessage("a learned read of the application's clock does not hold").Len(); n != 0 {
		t.Fatalf("a pattern's reason was said as a clock read %d times", n)
	}
	said := logs.FilterMessage("a keploy.yml noise pattern does not excuse a field").All()
	if len(said) != 1 || !strings.Contains(said[0].ContextMap()["reason"].(string), `"^1$"`) {
		t.Fatalf("warnings %+v; want one naming the pattern", said)
	}
}

// rangeDb is a disk store that says which request keys it was asked by.
type rangeDb struct {
	*mockMemDb
	asked *[]string
}

func (d rangeDb) RangeStagedRecordings(_ models.Kind, keys []string, _ func(*models.Mock) bool) error {
	*d.asked = append(*d.asked, keys...)
	return nil
}

// The learn step's check that a call is exactly another recording asks the
// disk store by the shape key alone when keploy.yml's noise reaches
// X-Amz-Target, with patterns or without: a recording of another target the
// patterns let vary is filed under another exact key.
func TestExactlyElsewhereAsksByTheShapeKeyWhenAPatternGuardsTheTarget(t *testing.T) {
	h, _, _ := learnOn(t)
	in := live("POST", "/", `{}`, map[string]string{"X-Amz-Target": "AmazonSQS.SendMessage"})
	for _, c := range []struct {
		name  string
		noise map[string][]string
		loose bool
	}{
		{"no noise on the target", nil, false},
		{"an entry with no patterns", map[string][]string{"X-Amz-Target": {}}, true},
		{"an entry with patterns", map[string][]string{"X-Amz-Target": {`^AmazonSQS\.`}}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			n := h.newLearnNoise(models.OutgoingOptions{Learn: true, NoiseConfig: map[string]map[string][]string{"header": c.noise}}, nil, nil)
			var asked []string
			if _, err := exactlyElsewhere(in, rangeDb{mockMemDb: &mockMemDb{}, asked: &asked}, scope{}, httpParts{}, learn.Shape{}, n, func(*models.Mock) bool { return true }); err != nil {
				t.Fatal(err)
			}
			if want := RequestKeysOf("POST", "/", "AmazonSQS.SendMessage", c.loose); !slices.Equal(asked, want) {
				t.Fatalf("asked by %q, want %q", asked, want)
			}
		})
	}
}

// A keploy.yml url pattern reaches a learned read of the application's
// clock in a query value only when it matches the recorded and the live
// value: when it matches one, the read is still checked, and the call is
// refused with the clock's reason at that field.
func TestAClockReadInTheURLKeployYmlPatternHoldsBothSides(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	base := time.Date(2026, 10, 26, 15, 50, 35, 0, time.UTC)
	old := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC).Format(time.RFC3339)
	pattern := []string{`\d{4}-\d{2}-\d{2}T[0-9:]+Z`}
	ct := map[string]string{"Content-Type": "application/json"}
	for _, c := range []struct {
		name, rec, live string
		refused         bool
	}{
		{"both values match", old, old, false},
		{"only the live value matches", "later", old, true},
		{"only the recorded value matches", old, "later", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := recorded("m", "/v1/u?at="+c.rec, ct, `{}`)
			m.Spec.Clocks = map[string]models.ClockOffset{"url.query.at": {Res: time.Second}}
			in := sent("/v1/u?at="+c.live, ct, `{}`)
			in.arrived = base.Add(2 * time.Second)
			got, diag := strictMatchIn(t, clockConn(true, base), in, nil, nil, pattern, m)
			if refused := got == nil; refused != c.refused {
				t.Fatalf("refused %v, differs %v; want %v", refused, differs(diag), c.refused)
			}
			if !c.refused {
				return
			}
			for _, f := range diag.differs {
				if f.clock && f.shape == "query.at" {
					return
				}
			}
			t.Fatalf("no read of the clock refused at query.at: %+v", diag.differs)
		})
	}
}
