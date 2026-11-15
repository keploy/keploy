package learn

import (
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// A fresh word's line names what the place it set aside held of the word,
// and so does a conflict's: the recorded word where the place held it
// before the call and round F gave another text there, round F's word
// where the place held that one in both runs while the value went to it
// from the recorded one (a place round F gave as recorded, or changed only
// elsewhere), and both where it held both in both runs (a piece of an id).
// The line said the recorded word at every such place, so one that held
// round F's word in both runs read as a place round F had moved off the
// recorded word: an insert's duration_ms "3" went to "1" while an earlier
// aggregate held "8" in both runs, and the line said that aggregate held
// "3". In keploy test and keploy mock alike.
func TestOutcomeASetAsidePlaceIsNamedByTheWordItHeld(t *testing.T) {
	const ms = time.Millisecond
	own := func(mock string, run, startUp bool, path, rec, live string) []Earlier {
		return oServed(mock, run, startUp, Response, oText(path, rec), live, ms)
	}
	// later is the test's own answer at path, asked a second after own's.
	later := func(mock, path, rec, live string) []Earlier {
		m := &models.Mock{Name: mock, Spec: models.MockSpec{ReqTimestampMock: oAt.Add(-time.Second), ResTimestampMock: oAt.Add(-time.Second + ms)}}
		return []Earlier{{Mock: mock, At: oAt.Add(-time.Second), Own: true,
			Payload: RawOf(m, true).ServedAs(3), Seen: []Seen{{Side: Response, Leaf: oText(path, rec), Live: live}}}}
	}
	set := func(x Raw) func() ([]GivenPlaces, error) {
		return func() ([]GivenPlaces, error) { return []GivenPlaces{PlacesOf(x)}, nil }
	}
	type row struct {
		what      string
		earlier   []Earlier
		given     func() ([]GivenPlaces, error)
		rec, live string // the call's value in each run
		word      [2]string
		refused   bool
		says, not string
	}
	rows := []row{
		{what: "the test's place held the recorded word and round F moved it",
			earlier: own("mock-4", false, false, "body.n", "3", "2"), rec: "3", live: "8", word: [2]string{"3", "8"},
			says: `mock-4 body.n held "3" and round F gave "2" there: set aside`},
		{what: "the test's place held round F's word in both runs",
			earlier: own("mock-4", false, false, "body.n", "8", "8"), rec: "3", live: "8", word: [2]string{"3", "8"},
			says: `mock-4 body.n held "8" in both runs, while the value went to it from "3": set aside`, not: `held "3"`},
		{what: "the start-up's place held round F's word in both runs, changed elsewhere",
			earlier: own("mock-4", true, true, "body.n", "8 ready", "8 waiting"), rec: "3", live: "8", word: [2]string{"3", "8"},
			says: `mock-4 body.n held "8" in both runs, while the value went to it from "3": set aside`, not: `held "3"`},
		{what: "the first place set aside is the one named",
			earlier: append(own("mock-4", false, false, "body.n", "3", "2"), later("mock-5", "body.m", "8", "8")...), rec: "3", live: "8", word: [2]string{"3", "8"},
			says: `mock-4 body.n held "3" and round F gave "2" there: set aside`, not: `mock-5 body.m held`},
		{what: "the test's own place set aside comes before another test's recording",
			earlier: own("mock-4", false, false, "body.n", "3", "2"), given: set(oAnswer("mock-31", 2*time.Second, "8").ServedAs(3).ServedAsRecorded().OfAnotherTest()), rec: "3", live: "8", word: [2]string{"3", "8"},
			says: `mock-4 body.n held "3" and round F gave "2" there: set aside`, not: `mock-31 held`},
		{what: "another test's recording, served as recorded, held round F's word",
			given: set(oAnswer("mock-31", 2*time.Second, "8").ServedAs(3).ServedAsRecorded().OfAnotherTest()), rec: "3", live: "8", word: [2]string{"3", "8"},
			says: `mock-31 held "8" in both runs, while the value went to it from "3": set aside`, not: `held "3"`},
		{what: "another test's recording, served as recorded, held the recorded word",
			given: set(oAnswer("mock-31", 2*time.Second, "3").ServedAs(3).ServedAsRecorded().OfAnotherTest()), rec: "3", live: "8", word: [2]string{"3", "8"},
			says: `mock-31 held "3" and round F gave the same text there: set aside`},
		{what: "another test's recording held both words, and round F's view of it only the recorded one",
			given: set(oAnswer("mock-31", 2*time.Second, "3 8").ServedAs(3).ServedLive(oAnswer("mock-31", 2*time.Second, "3 7")).OfAnotherTest()), rec: "3", live: "8", word: [2]string{"3", "8"},
			says: `mock-31 held "3" and round F gave a value without "8" there: set aside`, not: `in both runs`},
		{what: "a 32-bit word the test's place held as round F's in both runs",
			earlier: own("mock-4", false, false, "body.id", "Q8M31WPN", "Q8M31WPN"), rec: "A7K29QXM", live: "Q8M31WPN", word: [2]string{"A7K29QXM", "Q8M31WPN"},
			refused: true, says: `"Q8M31WPN" stood whole at mock-4 body.id in both runs, while the value went to it from "A7K29QXM"`, not: `"A7K29QXM" stood whole`},
		{what: "a 32-bit word the test's place held as recorded, moved",
			earlier: own("mock-4", false, false, "body.id", "A7K29QXM", "B4N08ZRT"), rec: "A7K29QXM", live: "Q8M31WPN", word: [2]string{"A7K29QXM", "Q8M31WPN"},
			refused: true, says: `"A7K29QXM" stood whole at mock-4 body.id; round F gave "B4N08ZRT" there and the value did not follow`},
	}
	for _, mode := range []struct {
		what   string
		drifts func() *DriftCache
	}{{"keploy test", oDrifts}, {"keploy mock", oClientDrifts}} {
		for _, r := range rows {
			in := oInput("body.eventDetails.duration_ms", r.rec, r.live, r.earlier, nil)
			in.Drifts = mode.drifts()
			in.PlaceOf = func(l Leaf, answer bool) (string, bool) { return VarPlace(l, true) }
			if r.given != nil {
				in.Given = r.given
			}
			d := oWord(t, Decide(in), r.word[0], r.word[1])
			what := mode.what + ", " + r.what
			if r.refused {
				oRefused(t, what, d, RefusalConflict, r.says)
				if d.Refusal != nil && r.not != "" && strings.Contains(d.Refusal.Why, r.not) {
					t.Errorf("%s: refused %q; it names a word the place never held (%s)", what, d.Refusal.Why, r.not)
				}
				continue
			}
			if d.Outcome != OutcomeFresh || !strings.Contains(d.Line, r.says) || r.not != "" && strings.Contains(d.Line, r.not) {
				t.Errorf("%s: %s, line %q; want fresh, its line saying %q and not %q", what, oDescribe([]Decision{d}), d.Line, r.says, r.not)
			}
		}
	}
}

// A piece of a minted id that a place held beside round F's piece, both in
// fields the two runs gave alike, is named with both pieces, at the call's
// set's recording and at another test's alike: a line naming
// only the recorded piece, with what round F gave there, would say round F
// gave a value without its own piece.
func TestOutcomeAPieceSetAsideBesideRoundFsIsNamedWithBoth(t *testing.T) {
	const (
		path      = "body.docs.msgs.0._id"
		rec, live = "294e693b-0963-42b8-a8ba-8cdf7fb6d3f1", "22ee59fa-7c10-412d-b6e2-90ad3f5c7e18"
		withR     = "d4927bdf-978e-42b8-86de-eeb6c761151c"
		withF     = "35cd9af2-dc5d-412d-a1c4-3e21b56315a6"
	)
	list := func(session string, ids ...string) *models.Mock {
		rows := []models.PostgresV3Cells{{{Value: session}}}
		for _, id := range ids {
			rows = append(rows, models.PostgresV3Cells{{Value: id}})
		}
		return oPgMock("mock-45", 97*time.Second, "SELECT id FROM suites", rows, "SELECT 3")
	}
	for _, c := range []struct {
		what  string
		raw   Raw
		other bool // another test's recording (Input.Given), not the call's set
	}{
		{"served as recorded", RawOf(list("s-1", withR, withF), true).ServedAs(1).ServedAsRecorded(), false},
		{"changed elsewhere (its session)", RawOf(list("s-1", withR, withF), true).ServedAs(1).ServedLive(RawOf(list("s-2", withR, withF), true)), false},
		{"another test's, served as recorded", RawOf(list("s-1", withR, withF), true).ServedAs(1).ServedAsRecorded().OfAnotherTest(), true},
	} {
		in := oInput(path, rec, live, nil, []Raw{c.raw})
		if c.other {
			x := c.raw
			in = oInput(path, rec, live, nil, nil)
			in.Given = func() ([]GivenPlaces, error) { return []GivenPlaces{PlacesOf(x)}, nil }
		}
		d := oWord(t, Decide(in), "42b8", "412d")
		if d.Outcome != OutcomeFresh || !strings.Contains(d.Line, `mock-45 held "42b8" and "412d" in both runs: set aside`) {
			t.Errorf("%s: %s, line %q; want fresh, naming both pieces mock-45 held", c.what, oDescribe([]Decision{d}), d.Line)
		}
	}
}
