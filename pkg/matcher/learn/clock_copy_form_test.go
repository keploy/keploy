package learn

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// A value the test set's start-up sent at a place its learn step learned as
// a plain read of the clock, which a test's call sends a minute later,
// outside its own call's window (api-server's PAT last-use touch, stamped in
// an earlier call's second): keploy test (T1, no client tests) refuses it as
// before, naming the earlier call and place. keploy mock's client tests (T2)
// learn it as a time checked by its form alone (clockCopyByForm, the
// founder's 2026-11-11 time default), never bound, the refusal kept beside
// it: keploy.yml's times: strict gets the refusal back (OffsetReadUnder),
// and a clock rule keploy.yml declares at the field decides the refusal, not
// the read (DeclaredClockLearned). A time of day copied so is a time too. At
// a key (a URL path segment, a leaf under an _id) the refusal stands in T2 as
// well: a time there names what the call fetches or writes, and a text that holds the copy as one of its
// words is no time to check by form, and stays refused.
func TestACopyOfAnEarlierCallsClockReadIsCheckedByItsFormInKeployMockAlone(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	rec := time.Date(2026, 10, 30, 12, 26, 2, 0, time.UTC)
	epoch := func(at time.Time) string { return strconv.FormatInt(at.Unix(), 10) }
	tod := func(at time.Time) string { return at.Format("15:04:05") }
	words := func(at time.Time) string { return "touched " + epoch(at) + " by ci" }
	boot := rec.Add(50 * time.Millisecond)
	call := boot.Add(time.Minute)
	fCall := call.Add(FullRoundLead)
	w := &Window{Rec: Span{From: call.Add(-5 * time.Millisecond), To: call}, F: Span{From: fCall.Add(-5 * time.Millisecond), To: fCall.Add(time.Millisecond)}}
	// classify is the call's leaf at path, the value sent (v of the
	// start-up's moment) at the start-up's place body.registeredAt, which
	// its learn step learned as a read of the clock (Seen.Clock).
	classify := func(dc *DriftCache, path string, sent, v func(time.Time) string) Learned {
		tr := NewTracker()
		tr.Served(0, Earlier{Mock: "mock-s", At: boot, Seen: []Seen{{Side: Request, Leaf: Leaf{Path: "body.registeredAt", Type: String, Value: sent(boot)}, Live: sent(boot.Add(FullRoundLead)), Place: "body.registeredAt", Clock: true}}})
		tr.Begin()
		_, earlier, _, _ := tr.Test()
		return ClassifyIn(dc, []string{path}, strLeaf(path, v(boot.Add(FullRoundLead))), strLeaf(path, v(boot)), call, earlier, nil, nil, w)[0]
	}
	const refusal = "its recorded value was sent in mock-s at body.registeredAt, the test set's start-up's call, where keploy learned it as a read of the application's clock, and this call does not hold it as its own read of the clock"
	refused := func(what string, ln Learned) {
		t.Helper()
		if ln.Kind != KindUnaccounted || ln.From != (Place{}) || !strings.Contains(ln.Why, refusal) || strings.Contains(ln.Why, epoch(boot)) {
			t.Errorf("%s: learned %s from %+v (%s); want refused: %s, never the value, no place to bind at", what, ln.Kind, ln.From, ln.Why, refusal)
		}
	}
	byForm := func(what string, ln Learned, form string) {
		t.Helper()
		o, ok := ln.Clock()
		switch {
		case ln.Kind != KindClockRead || !ok || ln.From != (Place{}):
			t.Errorf("%s: learned %s from %+v (%s); want a time checked by its form, nothing bound", what, ln.Kind, ln.From, ln.Why)
		case o.Form != form || o.FormF != "" || o.Res != 0 || o.Min != 0 || o.Max != 0 || o.Since != "":
			t.Errorf("%s: checked as %+v; want its form %q alone, no range", what, o, form)
		case !strings.Contains(o.Why, "checked by its form alone") || !strings.Contains(o.Why, "mock-s at body.registeredAt, the test set's start-up's call,") || strings.Contains(o.Why, epoch(boot)):
			t.Errorf("%s: the learned line says %q; want the form check and the start-up's place named, never the value", what, o.Why)
		}
	}

	// keploy test (T1): refused as before.
	refused("keploy test", classify(clockedDrifts(), "body.lastUsedAt", epoch, epoch))

	// keploy mock (T2): a time checked by its form.
	ln := classify(clientTestCache(), "body.lastUsedAt", epoch, epoch)
	byForm("keploy mock", ln, TimeForm(Leaf{Type: String, Value: epoch(boot)}))
	if got := OffsetReadUnder(ln, true); got.Kind != KindClockRead {
		t.Errorf("under the time default: %s (%s); want the read by form kept", got.Kind, got.Why)
	}
	refused("keploy mock, times: strict", OffsetReadUnder(ln, false))
	// A clock rule at the field decides the refusal, not the read.
	rule := Source{Kind: SourceClock, Rule: "pat-last-used-at", Clock: models.ClockOffset{Min: -time.Second, Res: time.Second}}
	if got := DeclaredClockLearned(ln, rule, w, true); got.Kind != KindClockRead || got.Read == nil || !strings.Contains(got.Read.Why, "keploy.yml's rule pat-last-used-at") {
		t.Errorf("a clock rule at the field: %s (%+v); want the rule to decide it", got.Kind, got.Read)
	}

	// A time of day copied so is a time of its form.
	byForm("keploy mock, a time of day", classify(clientTestCache(), "body.lastUsedAt", tod, tod), TimeForm(Leaf{Type: String, Value: tod(boot)}))

	// At a key the refusal stands: a time there is an identity, and another
	// value would select another recorded call or document.
	refused("keploy mock, at a URL path segment", classify(clientTestCache(), "url.path.3", epoch, epoch))
	refused("keploy mock, under an _id key", classify(clientTestCache(), "filter._id", epoch, epoch))

	// A text that holds the copy as one of its words is no time.
	if ln := classify(clientTestCache(), "body.note", epoch, words); ln.Kind == KindClockRead {
		t.Errorf("keploy mock, the copy inside a text: learned a time %+v (%s); want no check by form", ln.Read, ln.Why)
	}
}
