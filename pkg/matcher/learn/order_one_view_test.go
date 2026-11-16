package learn

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// One recording round F served more than once, at serves 3 and 7 with
// serves between that no other place numbers, is the one candidate of a
// word that moved between two leaves of its answer: the recorded token at
// the first document, round F's at the second. Where every serve gave the
// application one view (told as recorded each time, or with the one view
// they gave), whichever serve an application copying by position took,
// it read the same text there: the serve order cannot matter, and the
// word is decided by its place as for one serve: refused as moved (check
// "moved"), naming the leaf round F gave it at, with the one view; with
// the recording's own text, which holds both runs' tokens, by the place's
// state (check "state-unknown"). Where the serves' views are not known alike (told
// both as recorded and with a view, views that differ, a view keploy could
// not read then told as recorded, or an adapter that states no view), the
// serve order is asked as before and refuses the word as order-unknown. The
// word is refused every way; only the check named changes. Both
// KEPLOY_PROVEN modes.
func TestOneViewOfARecordingServedMoreThanOnceAsksNoServeOrder(t *testing.T) {
	const p = "body.u.token"
	for _, mode := range []string{"", "1"} {
		t.Run("proven="+mode, func(t *testing.T) {
			t.Setenv(provenswitch.Env, mode)
			for _, c := range []struct {
				name string
				of   func(name, ask string, tokens ...string) *models.Mock
				at   string // where round F gave its word
			}{
				{"a Mongo find", sectionFind, ".msg.cursor.firstBatch[1].token"},
				{"an HTTP GET", sectionGet, "items[1].token"},
			} {
				rec := func() Raw {
					return RawOf(c.of("mock-31", "q-rec", sectionTokenRec, sectionTokenLive), true).ServedIn(3, 7)
				}
				view := func(ask string) Raw {
					return RawOf(c.of("mock-31", ask, sectionTokenRec, sectionTokenLive), true)
				}
				for _, row := range []struct {
					name   string
					raw    Raw
					noView bool // the adapter states no view (Caps.FViewStated false)
					check  string
				}{
					{"one view every serve", rec().ServedLive(view("q-live")), false, "moved"},
					{"as recorded every serve", rec().ServedAsRecorded(), false, "state-unknown"},
					{"told as recorded and with a view", rec().ServedLive(view("q-live")).ServedAsRecorded(), false, "order-unknown"},
					{"two views", rec().ServedChanged(view("q-live"), view("q-live-2")), false, "order-unknown"},
					{"a view keploy could not read, then told as recorded", rec().ServedChanged().ServedAsRecorded(), false, "order-unknown"},
					{"one view every serve, no view stated by the adapter", rec().ServedLive(view("q-live")), true, "order-unknown"},
				} {
					in := oInput(p, sectionTokenRec, sectionTokenLive, nil, []Raw{row.raw})
					in.Scope = func(string) Scope { return ScopeTest("test-2") }
					in.Caps.FViewStated = !row.noView
					what := c.name + ", " + row.name
					d := oWord(t, Decide(in), sectionTokenRec, sectionTokenLive)
					oRefused(t, what, d, RefusalUnknown, "mock-31")
					if d.Refusal != nil && d.Refusal.Check != row.check {
						t.Errorf("FALSE REASON: %s: %s (check %q); want refused by check %q", what, oDescribe([]Decision{d}), d.Refusal.Check, row.check)
					}
				}
			}
		})
	}
}
