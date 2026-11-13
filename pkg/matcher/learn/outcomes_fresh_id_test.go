package learn

import (
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// Rows in the shapes api-server's T2 records left set aside: inserts of ids
// the application mints each run (app_name_reservations' appId, an
// entity_versions entity_id, testSuiteEvents' and app_config_audits' _id, a
// v4 UUID each) and a cluster's access key (32 random bytes in standard
// base64).

// A fresh UUID's 16-bit group stands, as a piece of another value, at an
// earlier place keploy cannot read whole for it: the recording's text holds
// it inside another id, and round F's holds round F's group glued inside a
// longer word. The application copies an id whole, and the place shows
// nothing of this id's 32-bit and 48-bit groups in either run, so it cannot
// be where the id came from: the group is chance there, and the id fresh,
// its line naming the place. The id's long words still judge the place: one
// that holds the recorded id's 32-bit group too stays unknown. A short word
// that is no piece of one id (its long neighbour stands in another token),
// and a place that shows the piece itself (the recorded group in the
// recording and round F's in round F, at one field), read as before.
func TestOutcomeAPieceOfAFreshIdAtAPlaceShowingNothingOfItsLongWordsIsChance(t *testing.T) {
	const (
		rec, live = "1111aaaa-2222-48d1-9444-444455556666", "9999cccc-7777-4a2e-8888-000011112222"
		r, f      = "48d1", "4a2e"
	)
	changed := func(mock, recText, liveText string) Earlier {
		return oCarried(mock, "body.note", recText, liveText, 2*time.Second, time.Second, 1)
	}
	chance := changed("mock-45", "owner bb7ed9ee-670e-48d1-982f-b54b2b73982f", "owner 0c1d2e3f-4a5b-4c7d-8e9f-4a2e0b1c2d3e")
	ds := Decide(oInput("body.docs.msgs.0.appId", rec, live, []Earlier{chance}, nil))
	for _, d := range ds {
		if d.Outcome != OutcomeFresh {
			t.Errorf("a fresh UUID beside a place holding a group of it inside another id: %s; want every word fresh", oDescribe([]Decision{d}))
		}
	}
	d := oWord(t, ds, r, f)
	costFresh(t, "a 16-bit group at a place that shows nothing of the id's long words", d)
	if !strings.Contains(d.Line, "mock-45") || !strings.Contains(d.Line, "piece of an id") {
		t.Errorf("the line must name mock-45 and say why it is chance: %q", d.Line)
	}

	again := changed("mock-44", "lead 70e1c2d3-48d1-4f00-8abc-0123456789ab", "lead 1a2b3c4d-4c7d-4e8f-9a0b-4a2e00000000")
	d = oWord(t, Decide(oInput("body.docs.msgs.0.appId", rec, live, []Earlier{again, chance}, nil)), r, f)
	costFresh(t, "a 16-bit group at two places that show nothing of the id's long words", d)
	if !strings.Contains(d.Line, "mock-44") || !strings.Contains(d.Line, "mock-45") {
		t.Errorf("the line must name both places the group is chance at: %q", d.Line)
	}

	long := changed("mock-46", "owner 1111aaaa and bb7ed9ee-670e-48d1-982f-b54b2b73982f", "owner 0c1d2e3f-4a5b-4c7d-8e9f-4a2e0b1c2d3e")
	ds = Decide(oInput("body.docs.msgs.0.appId", rec, live, []Earlier{long}, nil))
	if d := oWord(t, ds, "1111aaaa", "9999cccc"); d.Outcome != OutcomeRefused {
		t.Errorf("the recorded id's 32-bit group at the place: %s; want refused", oDescribe([]Decision{d}))
	}
	oRefused(t, "a 16-bit group where the place holds the id's 32-bit group too", oWord(t, ds, r, f), RefusalUnknown, "mock-46")

	other := changed("mock-47", "qty 48d1 ok", "qty 4a2e0b1c ok")
	ds = Decide(oInput("body.note", "qty 48d1 ref 1111aaaa2222", "qty 4a2e ref 9999cccc7777", []Earlier{other}, nil))
	oRefused(t, "a short word whose long neighbour stands in another token", oWord(t, ds, r, f), RefusalUnknown, "mock-47")

	shown := oCarried("mock-48", "body.code", r, f, 2*time.Second, time.Second, 1)
	ds = Decide(oInput("body.docs.msgs.0.appId", rec, live, []Earlier{shown}, nil))
	if d := oWord(t, ds, r, f); d.Outcome != OutcomeValue || d.From.Mock != "mock-48" {
		t.Errorf("a place that shows the group at one field in both runs: %s; want a Value bound at mock-48", oDescribe([]Decision{d}))
	}
}

// A cluster's access key: 32 random bytes in standard base64, 44 characters
// with one '=' of padding. Its '+' and '/' are letters of that alphabet, and
// stand elsewhere in round F, so the runs' words do not pair and word by
// word replay could never write it: the key is one value, fresh at its first
// send and bound where an earlier place shows it whole. Two alphabets mixed
// (in one run, or one in each), a length no whole group of four, another
// padding in each run or more '=' than base64 pads with, a run that decodes
// to text, and a change whose bits stand in its '+' or '/' alone (its
// letters too few for a random value) are no such key.
func TestOutcomeAStandardBase64KeyIsOneValue(t *testing.T) {
	const (
		rec  = "q3+Zb7Lr0Tf/9XcVw2Kd8Hn+Pa4Me6Gs1Jy5Ru0Io7U="
		live = "Xk2Nw9/Ft4Qz+Lm8Hc3Vb6Dp1Sg7Ja0Ey5Ro2Ui9Tn+="
	)
	if len(rec) != 44 || len(live) != 44 {
		t.Fatalf("the keys are %d and %d characters; want 44", len(rec), len(live))
	}
	ds := Decide(oInput("body.docs.msgs.0.access_key", rec, live, nil, nil))
	if len(ds) != 1 || ds[0].Outcome != OutcomeFresh || ds[0].At.Word != -1 || ds[0].Recorded != rec || ds[0].Full != live {
		t.Errorf("an access key whose '+' and '/' moved: %s; want one fresh value, the field whole", oDescribe(ds))
	}
	ds = Decide(oInput("body.cmd.filter.access_key", rec, live, oSent(rec, live), nil))
	if len(ds) != 1 || ds[0].Outcome != OutcomeValue || ds[0].From.Mock != "mock-0" {
		t.Errorf("a later send of the key: %s; want a Value bound at the first send", oDescribe(ds))
	}
	text := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	const plain = "q3Zb7Lr0Tf9XcVw2Kd8HnPa4Me6Gs1Jy"
	for _, tc := range []struct{ name, r, f string }{
		{"two alphabets mixed", rec, strings.Replace(live, "/", "_", 1)},
		{"two alphabets mixed in the recording", strings.Replace(rec, "/", "_", 1), live},
		{"the URL-safe alphabet in the recording, the standard in round F, whole groups of four", strings.NewReplacer("+", "-", "/", "_").Replace(rec), live},
		{"no whole group of four", rec[:43], live[:43]},
		{"another padding in each run", rec[:42] + "==", live},
		{"a text in standard base64", text(`{"name":"Иван Петров"}+`), text(`{"name":"Пётр Сидоров"}/`)},
		{"the URL-safe alphabet in one run, the standard in the other", strings.NewReplacer("+", "-", "/", "_").Replace(rec[:43]), live[:43]},
		{"a change whose bits stand in its '+' alone", plain + "a+++++++++b=", plain + "c++++d++++e="},
		{"a change whose bits stand in its '/' alone", plain + "a/////////b=", plain + "c////d////e="},
		{"a change whose bits stand in its '+' and '/' alone in round F", plain + "aK7mQ2xZ9pW=", plain + "c++/+d+/++e="},
		{"a change whose bits stand in its '+' and '/' alone in the recording", plain + "c++/+d+/++e=", plain + "aK7mQ2xZ9pW="},
		// The other run's letters and digits carry the bits a token needs:
		// only the one run's '+' and '/' are short of them.
		{"a change whose bits stand in its '+' and '/' alone in round F, the recording's letters enough", plain[:30] + "aK7mQ2xZ9pWr4=", plain[:30] + "c++/+d+/++eAB="},
		{"a change whose bits stand in its '+' and '/' alone in the recording, round F's letters enough", plain[:30] + "c++/+d+/++eAB=", plain[:30] + "aK7mQ2xZ9pWr4="},
	} {
		ds := Decide(oInput("body.docs.msgs.0.access_key", tc.r, tc.f, nil, nil))
		if slices.ContainsFunc(ds, func(d Decision) bool { return d.Outcome == OutcomeFresh && d.At.Word < 0 }) {
			t.Errorf("FALSE GREEN? %s: %s; want no one fresh value", tc.name, oDescribe(ds))
		}
	}
	ds = Decide(oInput("body.docs.msgs.0.access_key", rec[:41]+"===", live[:41]+"===", nil, nil))
	if len(ds) != 1 || ds[0].Refusal == nil || ds[0].Refusal.Kind != RefusalChangedShape {
		t.Errorf("three '=' of padding, which no base64 encoder writes: %s; want it read word by word, refused as changed in shape", oDescribe(ds))
	}
}

// The id is the token the piece stands in, and its long words the changed
// words of 32 bits or more in both runs inside that token in both runs. A
// place that holds a 16-bit group inside another id, beside nothing of
// those, is chance only for such a piece; anywhere else it stays unknown,
// refused naming it: a short word that stands in no id token (a '/' ends
// one), a long word outside its token (before or after it), a long word
// inside it in one run only (the separator between them changed, '-' to
// '/', so round F's id is not round R's), a neighbour or a word of fewer
// than 32 bits in one run, and a place that holds the id's one long word.
func TestOutcomeOnlyAPieceOfOneIdIsChanceBesideNothingOfItsLongWords(t *testing.T) {
	changed := func(mock, recText, liveText string) Earlier {
		return oCarried(mock, "body.note", recText, liveText, 2*time.Second, time.Second, 1)
	}
	inside := changed("mock-45", "owner bb7ed9ee-670e-48d1-982f-b54b2b73982f", "owner 0c1d2e3f-4a5b-4c7d-8e9f-4a2e0b1c2d3e")
	for _, tc := range []struct {
		what, rec, live, r, f string
		place                 Earlier
	}{
		{"a piece in no id token", "48d1/1111aaaa2222", "4a2e/9999cccc7777", "48d1", "4a2e", inside},
		{"the long word after the piece's token", "ab-48d1 ref 1111aaaa2222", "ab-4a2e ref 9999cccc7777", "48d1", "4a2e", inside},
		{"the long word before the piece's token", "1111aaaa2222 ref ab-48d1", "9999cccc7777 ref ab-4a2e", "48d1", "4a2e", inside},
		{"a long word inside the piece's token in round R only", "ab12cd34-48d1", "ef56ab78/4a2e", "48d1", "4a2e", inside},
		{"a long word inside the piece's token in round F only", "ab12cd34/48d1", "ef56ab78-4a2e", "48d1", "4a2e", inside},
		{"a neighbour of 32 bits in round F only", "11112222-48d1", "9999cccc-4a2e", "48d1", "4a2e", inside},
		{"a word of 32 bits in round F only", "11112222-aaaabbbbcccc", "9999cccc-ddddeeeeffff", "11112222", "9999cccc",
			changed("mock-45", "owner 0000ffff-11112222-abcd", "owner 9999cccc0b1c2d3e")},
		{"a place that holds the id's one long word", "ab12cd34-48d1", "ef56ab78-4a2e", "48d1", "4a2e",
			changed("mock-45", "owner 0000ffff-ab12cd34-48d1-0000", "owner 0c1d2e3f-4a5b-4c7d-8e9f-4a2e0b1c2d3e")},
	} {
		d := oWord(t, Decide(oInput("body.ref", tc.rec, tc.live, []Earlier{tc.place}, nil)), tc.r, tc.f)
		oRefused(t, tc.what, d, RefusalUnknown, "mock-45")
	}
}

// A piece is chance only in an id the call minted: every long word of the
// id learned fresh, no place before the call showing it. Where an earlier
// place shows the id's long words, or one of them (the application built
// the id from values it took, and may have taken the piece from another
// place too), a place that holds the piece inside another id stays unknown,
// and the piece is refused naming it, while the long words it shows are
// bound there. So it is where an earlier place refuses one long word (round
// F's text there holds another value, or round F's word glued inside a
// longer one): a refused word is no word the call made. The same id with
// nothing shown of its long words is fresh.
func TestOutcomeAPieceIsChanceOnlyInAnIdTheCallMinted(t *testing.T) {
	const (
		path      = "body.docs.msgs.0.appId"
		rec, live = "1111aaaa-2222-48d1-9444-444455556666", "9999cccc-7777-4a2e-8888-000011112222"
		r, f      = "48d1", "4a2e"
	)
	inside := oCarried("mock-45", "body.note", "owner bb7ed9ee-670e-48d1-982f-b54b2b73982f", "owner 0c1d2e3f-4a5b-4c7d-8e9f-4a2e0b1c2d3e", 2*time.Second, time.Second, 1)
	d := oWord(t, Decide(oInput(path, rec, live, []Earlier{inside}, nil)), r, f)
	costFresh(t, "the piece of an id whose long words no place shows", d)
	for _, tc := range []struct {
		what, rec, live string
		bound           [][2]string
	}{
		{"both long words", "lead 1111aaaa tail 444455556666", "lead 9999cccc tail 000011112222", [][2]string{{"1111aaaa", "9999cccc"}, {"444455556666", "000011112222"}}},
		{"the 32-bit word", "lead 1111aaaa", "lead 9999cccc", [][2]string{{"1111aaaa", "9999cccc"}}},
		{"the 48-bit word", "tail 444455556666", "tail 000011112222", [][2]string{{"444455556666", "000011112222"}}},
	} {
		took := oCarried("mock-50", "body.ref", tc.rec, tc.live, 3*time.Second, time.Second, 1)
		ds := Decide(oInput(path, rec, live, []Earlier{took, inside}, nil))
		for _, b := range tc.bound {
			if d := oWord(t, ds, b[0], b[1]); d.Outcome != OutcomeValue || d.From.Mock != "mock-50" {
				t.Errorf("%s shown at mock-50: %s; want %q bound at mock-50", tc.what, oDescribe([]Decision{d}), b[0])
			}
		}
		oRefused(t, tc.what+" shown at an earlier place, the piece inside another id at mock-45", oWord(t, ds, r, f), RefusalUnknown, "mock-45")
	}
	for _, tc := range []struct {
		what, rec, live, wr, wf string
	}{
		{"the 32-bit word, round F's text holding another value", "lead 1111aaaa", "lead 0c0c0c0c", "1111aaaa", "9999cccc"},
		{"the 32-bit word, round F's glued inside a longer word", "lead 1111aaaa", "lead 9999cccc0b1c", "1111aaaa", "9999cccc"},
		{"the 48-bit word, round F's text holding another value", "tail 444455556666", "tail 777788889999", "444455556666", "000011112222"},
	} {
		took := oCarried("mock-50", "body.ref", tc.rec, tc.live, 3*time.Second, time.Second, 1)
		ds := Decide(oInput(path, rec, live, []Earlier{took, inside}, nil))
		if d := oWord(t, ds, tc.wr, tc.wf); d.Outcome != OutcomeRefused {
			t.Errorf("%s at mock-50: %s; want it refused", tc.what, oDescribe([]Decision{d}))
		}
		oRefused(t, tc.what+" refused at an earlier place, the piece inside another id at mock-45", oWord(t, ds, r, f), RefusalUnknown, "mock-45")
	}
}

// An instant is the run clock's, read whole by its moment and never
// learned fresh: an id that holds one ("1730361600-48d1", or beside a
// 32-bit word the call made, "1730361600-1111aaaa-48d1") is no id the call
// minted, and a place holding its piece inside another id stays unknown,
// refused naming it.
func TestOutcomeAnInstantIsNoLongWordOfAnId(t *testing.T) {
	inside := oCarried("mock-45", "body.note", "owner bb7ed9ee-670e-48d1-982f-b54b2b73982f", "owner 0c1d2e3f-4a5b-4c7d-8e9f-4a2e0b1c2d3e", 2*time.Second, time.Second, 1)
	ds := Decide(oInput("body.ref", "1730361600-48d1", "1730361660-4a2e", []Earlier{inside}, nil))
	oRefused(t, "a piece whose id's only long word is an instant", oWord(t, ds, "48d1", "4a2e"), RefusalUnknown, "mock-45")
	ds = Decide(oInput("body.ref", "1730361600-1111aaaa-48d1", "1730361660-9999cccc-4a2e", []Earlier{inside}, nil))
	if d := oWord(t, ds, "1111aaaa", "9999cccc"); d.Outcome != OutcomeFresh {
		t.Errorf("the 32-bit word beside the instant: %s; want fresh", oDescribe([]Decision{d}))
	}
	oRefused(t, "a piece of an id that holds an instant beside a word the call made", oWord(t, ds, "48d1", "4a2e"), RefusalUnknown, "mock-45")
}

// A word one run lacks refuses the text whole, after the words before it:
// a minted id's piece before it is still decided, in its place, and the
// refusal ends the list.
func TestOutcomeAPieceBeforeAWordOneRunLacksKeepsTheWholeRefusal(t *testing.T) {
	inside := oCarried("mock-45", "body.note", "owner bb7ed9ee-670e-48d1-982f-b54b2b73982f", "owner 0c1d2e3f-4a5b-4c7d-8e9f-4a2e0b1c2d3e", 2*time.Second, time.Second, 1)
	ds := Decide(oInput("body.ref", "id 1111aaaa-2222-48d1-9444-444455556666 at x y", "id 9999cccc-7777-4a2e-8888-000011112222 at z", []Earlier{inside}, nil))
	if len(ds) == 0 || ds[len(ds)-1].Refusal == nil || ds[len(ds)-1].Refusal.Word != -1 {
		t.Fatalf("a text where a word was added or dropped: %s; want its last decision a refusal of the text whole", oDescribe(ds))
	}
	d := oWord(t, ds, "48d1", "4a2e")
	costFresh(t, "a minted id's piece before the word one run lacks", d)
}

// A place holds a piece by chance only where none of its texts holds the
// piece as a whole id token, nor anything of the id's long words, in
// either run: the recording's answer, round F's view of it, and each view
// round F's serves gave. A set recording keploy cannot place before or
// after the call that holds round F's group as a value of its own, or round
// F's 32-bit group beside the recorded group, or whose round F view or one
// of whose round F serves holds round F's group whole, stays unknown,
// either view holding it whole where the answer holds nothing of the piece
// too; so does an earlier recording one of whose round F serves holds it
// whole.
// One whose texts hold the group only inside other ids is chance.
func TestOutcomeAPieceAnyViewHoldsWholeIsNoChance(t *testing.T) {
	const p = "body.appId"
	before := r1Insert.Add(-time.Minute)
	mock := func(held string, res time.Time) Raw {
		m := receiveOf("mock-31", held, before)
		m.Spec.ResTimestampMock = res
		return clockedDrifts().RawOf(m, true)
	}
	const inside = "bb7ed9ee-670e-48d1-982f-b54b2b73982f"
	for _, res := range []struct {
		what string
		at   time.Time
	}{{"no answer time", time.Time{}}, {"answered at the call's own instant", r1Insert}} {
		for _, c := range []struct {
			what string
			x    Raw
		}{
			{"round F's group as its own value", mock("4c7d", res.at).ServedAs(3).ServedAsRecorded()},
			{"round F's 32-bit group beside the recorded group", mock("0c1d2e3f "+inside, res.at).ServedAs(3).ServedAsRecorded()},
			{"round F's view holds round F's group whole", mock(inside, res.at).ServedAs(3).ServedLive(mock("4c7d", res.at))},
			{"round F's view alone holds round F's group whole, the answer nothing of the piece", mock("0f1e2d3c-5b6a-4978-8a9b-cdef01234567", res.at).ServedAs(3).ServedLive(mock("4c7d", res.at))},
			{"a view round F's serves gave holds round F's group whole", mock(inside, res.at).ServedAs(3).ServedChanged(mock("4c7d", res.at), mock(inside, res.at))},
			{"a view round F's serves gave alone holds round F's group whole, the answer nothing of the piece", mock("0f1e2d3c-5b6a-4978-8a9b-cdef01234567", res.at).ServedAs(3).ServedChanged(mock("4c7d", res.at), mock("0f1e2d3c-5b6a-4978-8a9b-cdef01234567", res.at))},
		} {
			in := oInput(p, "5c0e9a7b-1f2d-48d1-9a3b-c4d5e6f70812", "0c1d2e3f-4a5b-4c7d-8e9f-a0b1c2d3e4f5", nil, nil)
			in.At, in.Arrived = r1Insert, r1Insert
			x := c.x
			in.Given = func() ([]GivenPlaces, error) { return []GivenPlaces{PlacesOf(x)}, nil }
			oRefused(t, res.what+", "+c.what, oWord(t, Decide(in), "48d1", "4c7d"), RefusalUnknown, "mock-31")
		}
	}

	const recSID, liveSID = "5Xa0KmL0RxqLFfFyLtYhPw", "w6Hg1H8rSmm44V0Mn3o7Jg"
	const other, longer = "0f1e2d3c-5b6a-4978-8a9b-cdef01234567", "c0de0000-1111-48d1-8222-333344445555"
	ans := func(sid string, ids ...string) Raw {
		rows := make([]models.PostgresV3Cells, 0, len(ids))
		for _, id := range ids {
			rows = append(rows, models.PostgresV3Cells{{Value: id}})
		}
		return oPg("mock-0", 2*time.Second, "SELECT _id FROM suites /* session "+sid+" */", rows, "SELECT 2")
	}
	call := func(boot Raw) Decision {
		return oWord(t, Decide(oInput("body.cmd.documents.0._id", "1111aaaa-2222-4333-9444-444455556666", "9999cccc-7777-48d1-8888-000011112222", nil, []Raw{boot})), "4333", "48d1")
	}
	oRefused(t, "a view round F's serves gave holds round F's group whole", call(ans(recSID, other).ServedAs(1).ServedChanged(ans(liveSID, other, longer), ans(liveSID, other, "48d1"))), RefusalUnknown, "mock-0")
	if d := call(ans(recSID, other).ServedAs(1).ServedChanged(ans(liveSID, other, longer), ans(liveSID, other))); d.Outcome != OutcomeFresh {
		t.Errorf("the views round F's serves gave hold round F's group only inside another id: %s; want fresh", oDescribe([]Decision{d}))
	}
}

// A place that holds the piece in both runs, the recorded group in the
// recording and round F's in round F, may be where the application cut it
// out of another id in each run, or hold both by two coincidences, which
// keploy cannot tell apart. So it is no chance, though it shows nothing of
// the id's long words, and the piece is refused as unknown there, naming
// it. Round F holds its group in its text, or in a view one of its serves
// gave. A set recording whose answer holds the recorded group is no chance
// where round F's view of it holds round F's group. Where round F gave the
// place before the call and keploy cannot tell what (a served recording
// with no view stated, or a changed view it could not read), the id's long
// words are unknown there too: no chance either. A place that holds the
// piece in one run only is still chance: the recording alone (round F's
// text changed), or round F alone, even where round F's view holds the
// recorded group too: the recording's answer, which R's application was
// given, holds none.
func TestOutcomeAPieceAPlaceHoldsInBothRunsIsNoChance(t *testing.T) {
	const (
		path      = "body.docs.msgs.0._id"
		rec, live = "ed35a78f-905c-48d1-bc3d-a07311678c73", "3f0c9a12-7e6d-4b8a-9e21-5c6d7e8f9a0b"
		r, f      = "48d1", "4b8a"
		field     = "body.cursor.firstBatch.0._id"
	)
	for _, at := range []struct {
		what     string
		answered time.Duration
	}{{"no answer time", 0}, {"answered at the call", 2 * time.Second}} {
		for _, tc := range []struct {
			what, held, gave string
			chance           bool
		}{
			{"both runs", "b0fd3b58-3ff5-48d1-a1b2-c3d4e5f60718", "c1ae4c69-77aa-4b8a-b2c3-d4e5f6071829", false},
			{"the recording alone, round F's text changed", "b0fd3b58-3ff5-48d1-a1b2-c3d4e5f60718", "c1ae4c69-77aa-4c00-b2c3-d4e5f6071829", true},
			{"round F alone", "b0fd3b58-3ff5-4c00-a1b2-c3d4e5f60718", "c1ae4c69-77aa-4b8a-b2c3-d4e5f6071829", true},
		} {
			what := "the group inside another id in " + tc.what + ", " + at.what
			d := oWord(t, Decide(oInput(path, rec, live, oServed("mock-231", true, false, Response, oText(field, tc.held), tc.gave, at.answered), nil)), r, f)
			if tc.chance {
				costFresh(t, what, d)
				continue
			}
			oRefused(t, what, d, RefusalUnknown, "mock-231")
		}
	}

	const recSID, liveSID = "5Xa0KmL0RxqLFfFyLtYhPw", "w6Hg1H8rSmm44V0Mn3o7Jg"
	const other, holdsF, holdsR = "0f1e2d3c-5b6a-4978-8a9b-cdef01234567", "c0de0000-1111-48d1-8222-333344445555", "abcd0123-0000-4333-8000-000000000001"
	ans := func(sid string, ids ...string) Raw {
		rows := make([]models.PostgresV3Cells, 0, len(ids))
		for _, id := range ids {
			rows = append(rows, models.PostgresV3Cells{{Value: id}})
		}
		return oPg("mock-0", 2*time.Second, "SELECT _id FROM suites /* session "+sid+" */", rows, "SELECT 2")
	}
	call := func(boot Raw) Decision {
		return oWord(t, Decide(oInput("body.cmd.documents.0._id", "1111aaaa-2222-4333-9444-444455556666", "9999cccc-7777-48d1-8888-000011112222", nil, []Raw{boot})), "4333", "48d1")
	}
	oRefused(t, "the recording holds the recorded group, a view round F's serves gave round F's", call(ans(recSID, other, holdsR).ServedAs(1).ServedChanged(ans(liveSID, other, holdsF), ans(liveSID, other))), RefusalUnknown, "mock-0")
	oRefused(t, "the recording holds the recorded group, the second view round F's serves gave round F's", call(ans(recSID, other, holdsR).ServedAs(1).ServedChanged(ans(liveSID, other), ans(liveSID, other, holdsF))), RefusalUnknown, "mock-0")
	oRefused(t, "the recording holds the recorded group, round F served it with no view stated", call(ans(recSID, other, holdsR).ServedAs(1)), RefusalUnknown, "mock-0")
	// Serves that gave different views leave keploy no one view of the
	// place (ServedChanged): every rule but the one that refuses a word a
	// view holds reads that round F gave nothing there. So that no view holds
	// round F's group shows nothing of whether round F's application cut it
	// there, and the recording's group there is no chance (main and
	// integrations' served-view rows refuse it too).
	d := call(ans(recSID, other, holdsR).ServedAs(1).ServedChanged(ans(liveSID, other), ans(liveSID, other, "c0de0000-1111-4c00-8222-333344445555")))
	oRefused(t, "the recording holds the recorded group, no view round F's serves gave holds round F's", d, RefusalUnknown, "mock-0")
	oOneSideLine(t, d, "the recording's own piece of the id stands at mock-0, which round F gave the application with no view keploy reads: round F's making its own piece shows nothing of whether the application cut it from there")

	const (
		setRec, setLive = "5c0e9a7b-1f2d-48d1-9a3b-c4d5e6f70812", "0c1d2e3f-4a5b-4c7d-8e9f-a0b1c2d3e4f5"
		heldR, heldF    = "bb7ed9ee-670e-48d1-982f-b54b2b73982f", "d4c3b2a1-6f5e-4c7d-9b8a-5a4f3e2d1c0b"
	)
	// A recording answered a second after it was asked, a minute before the
	// call; untimed, one whose answer time keploy cannot read.
	timed := func(held string, answered bool) *models.Mock {
		m := receiveOf("mock-31", held, r1Insert.Add(-time.Minute))
		m.Spec.ResTimestampMock = time.Time{}
		if answered {
			m.Spec.ResTimestampMock = r1Insert.Add(-time.Minute + time.Second)
		}
		return m
	}
	mock := func(held string) *models.Mock { return timed(held, true) }
	untimed := func(held string) *models.Mock { return timed(held, false) }
	decideSet := func(g GivenPlaces) Decision {
		in := oInput("body.appId", setRec, setLive, nil, nil)
		in.At, in.Arrived = r1Insert, r1Insert
		in.Given = func() ([]GivenPlaces, error) { return []GivenPlaces{g}, nil }
		return oWord(t, Decide(in), "48d1", "4c7d")
	}
	for _, c := range []struct {
		what   string
		m      *models.Mock
		tells  []tell
		chance bool
	}{
		{"round F's view holds round F's group", untimed(heldR), []tell{toldView("v1", untimed(heldF))}, false},
		{"round F served it with no view stated", untimed(heldR), []tell{toldNothing}, false},
		{"round F served it changed with a view keploy could not read", untimed(heldR), []tell{toldUnkept}, false},
		{"round F's view alone holds round F's group", mock(other), []tell{toldView("v1", mock(heldF))}, true},
		{"round F's view holds both runs' groups, the recording's answer none", mock(other), []tell{toldView("v1", mock(heldR+" "+heldF))}, true},
		{"the recording alone holds the recorded group, round F's view none, its answer time unread", untimed(heldR), []tell{toldView("v1", untimed(other))}, true},
		{"the recording alone holds the recorded group, served as recorded, its answer time unread", untimed(heldR), []tell{toldAsRecorded}, true},
		// The recording made its group, but keploy cannot read when it
		// gave the application this recording: it may not have had it to
		// cut from, so round F's group in round F's view is no chance.
		{"round F's view alone holds round F's group, the recording's answer time unread", untimed(other), []tell{toldView("v1", mock(heldF))}, false},
		{"round F's view holds both runs' groups, the recording's answer none and its time unread", untimed(other), []tell{toldView("v1", mock(heldR+" "+heldF))}, false},
		// Round F made its group, but its serves gave two views: keploy
		// reads that it gave nothing there (ServedChanged), so the
		// recording's group in its answer is no chance.
		{"the recording alone holds the recorded group, round F's serves gave two views, neither round F's group", untimed(heldR), []tell{toldView("v1", untimed(other)), toldView("v2", untimed(heldR))}, false},
	} {
		for _, g := range givenBoth(t, c.m, c.tells...) {
			d := decideSet(g.g)
			if c.chance {
				costFresh(t, "a set recording ("+g.how+"): "+c.what, d)
				continue
			}
			oRefused(t, "a set recording ("+g.how+"): "+c.what, d, RefusalUnknown, "mock-31")
		}
	}
	for _, g := range givenBoth(t, untimed(other), toldView("v1", mock(heldF))) {
		oOneSideLine(t, decideSet(g.g), "round F's own piece of the id stands at mock-31, which the recording did not give the application before the call, by a time keploy reads: the recording's making its own piece shows nothing of whether the application cut it from there")
	}
}

// oNoChanceLine checks d's line says, in a sentence of its own, why the
// place named first is no chance, and nothing of what round F gave there,
// which is not the reason.
func oNoChanceLine(t *testing.T, d Decision, first string) {
	t.Helper()
	want := "each run's own piece of the id stands at a place that run had, where the application may have cut it from (" + first + " among them): keploy cannot tell a cut from chance"
	if d.Refusal == nil || d.Refusal.Why != want || !strings.Contains(d.Line, "refused (unknown): "+want+". Places: ") {
		t.Errorf("the line must say why %s is no chance, and only that: %q", first, d.Line)
	}
}

// oOneSideLine checks d's line says why, and only that: one run made its
// piece, and the other's stands at a place the first was not given before
// the call (want, which names it).
func oOneSideLine(t *testing.T, d Decision, want string) {
	t.Helper()
	if d.Refusal == nil || d.Refusal.Why != want || !strings.Contains(d.Line, "refused (unknown): "+want+". Places: ") {
		t.Errorf("the line must say why the piece is no chance, and only that: %q; want %q", d.Line, want)
	}
}

// A piece of an id the call minted is chance only where one run's own
// piece stands at no place that run had. The application runs the same
// code in each run: one that cut the piece out of a value it was given
// finds its own piece at a place in each run, at one place or at another
// in each. So where the recording holds the recorded group at one place
// and round F holds its group at another, each place holding it in one run
// only, keploy cannot tell a cut from chance, and the piece is refused as
// unknown, naming the places it left: two places left unknown for it (no
// answer time, or answered at the call's own instant); a place answered
// before the call that held the recorded group where round F's text lacks
// its own (set aside) beside one left unknown that holds round F's; and a
// place beside a set recording, either run's piece at either. Where one
// run's piece stands at no place it had, that run's application made it
// with the id, and the id is fresh: the recording's alone, round F's alone
// at two places, and the recorded group at a place answered after the
// call, or in a set recording's answer recorded after it, which R's
// application was not given before the call (one with no answer time may
// have been: refused).
func TestOutcomeAPieceEachRunHoldsAtAnotherPlaceIsNoChance(t *testing.T) {
	const (
		path           = "body.docs.msgs.0._id"
		rec, live      = "ed35a78f-905c-48d1-bc3d-a07311678c73", "3f0c9a12-7e6d-4b8a-9e21-5c6d7e8f9a0b"
		r, f           = "48d1", "4b8a"
		field          = "body.cursor.firstBatch.0._id"
		holdsR, holdsF = "b0fd3b58-3ff5-48d1-a1b2-c3d4e5f60718", "9e9e9e9e-77aa-4b8a-b2c3-d4e5f6071829"
		noneR, noneF   = "aaaa1111-2222-4c00-8333-444455556666", "c1ae4c69-77aa-4c00-b2c3-d4e5f6071829"
	)
	at := func(mock, recText, liveText string, answered time.Duration) []Earlier {
		return oServed(mock, true, false, Response, oText(field, recText), liveText, answered)
	}
	// own is a place of the call's own test: an earlier test's is given
	// before the call, whatever its answer's time says.
	own := func(mock, recText, liveText string, answered time.Duration) []Earlier {
		return oServed(mock, false, false, Response, oText(field, recText), liveText, answered)
	}
	set := func(held string, answered time.Duration, served func(Raw) Raw) func() ([]GivenPlaces, error) {
		m := receiveOf("mock-31", held, oAt.Add(-time.Minute))
		m.Spec.ResTimestampMock = time.Time{}
		if answered != 0 {
			m.Spec.ResTimestampMock = oAt.Add(-time.Minute + answered)
		}
		x := served(clockedDrifts().RawOf(m, true))
		return func() ([]GivenPlaces, error) { return []GivenPlaces{PlacesOf(x)}, nil }
	}
	view := func(held string) func(Raw) Raw {
		return func(x Raw) Raw {
			v := receiveOf("mock-31", held, oAt.Add(-time.Minute))
			v.Spec.ResTimestampMock = time.Time{}
			return x.ServedAs(3).ServedLive(clockedDrifts().RawOf(v, true))
		}
	}
	unserved := func(x Raw) Raw { return x }
	asRecorded := func(x Raw) Raw { return x.ServedAs(3).ServedAsRecorded() }
	decide := func(e []Earlier, given func() ([]GivenPlaces, error)) Decision {
		in := oInput(path, rec, live, e, nil)
		in.Given = given
		return oWord(t, Decide(in), r, f)
	}
	for _, a := range []struct {
		what     string
		answered time.Duration
	}{{"no answer time", 0}, {"answered at the call", 2 * time.Second}} {
		d := decide(append(at("mock-231", holdsR, noneF, a.answered), at("mock-232", noneR, holdsF, a.answered)...), nil)
		oRefused(t, "the recorded group at one place, round F's at another, "+a.what, d, RefusalUnknown, "mock-231", "mock-232")
		oNoChanceLine(t, d, "mock-231 body.cursor.firstBatch.0._id")
		oRefused(t, "the recorded group at a place, round F's in a set recording's view, "+a.what,
			decide(at("mock-231", holdsR, noneF, a.answered), set(noneR, 0, view(holdsF))), RefusalUnknown, "mock-231", "mock-31")
		oRefused(t, "round F's group at a place, the recorded group in a set recording's answer, "+a.what,
			decide(at("mock-232", noneR, holdsF, a.answered), set(holdsR, 0, view(noneF))), RefusalUnknown, "mock-232", "mock-31")
		oRefused(t, "round F's group at a place, the recorded group in the answer of a set recording round F never served, "+a.what,
			decide(at("mock-232", noneR, holdsF, a.answered), set(holdsR, 0, unserved)), RefusalUnknown, "mock-232")

		costFresh(t, "the recorded group at one place, nothing of either at another, "+a.what,
			decide(append(at("mock-231", holdsR, noneF, a.answered), at("mock-232", noneR, noneF, a.answered)...), nil))
		costFresh(t, "round F's group at two places, the recording's at none, "+a.what,
			decide(append(at("mock-231", noneR, holdsF, a.answered), at("mock-232", noneR, holdsF, a.answered)...), nil))
		costFresh(t, "round F's group at a place, the recorded group at a place answered after the call, "+a.what,
			decide(append(own("mock-231", holdsR, noneF, 3*time.Second), at("mock-232", noneR, holdsF, a.answered)...), nil))
		costFresh(t, "round F's group at a place, the recorded group in the answer of a set recording answered after the call, "+a.what,
			decide(at("mock-232", noneR, holdsF, a.answered), set(holdsR, 2*time.Minute, asRecorded)))
		oRefused(t, "round F's group at a place, the recorded group in the answer of a set recording with no answer time, served as recorded, "+a.what,
			decide(at("mock-232", noneR, holdsF, a.answered), set(holdsR, 0, asRecorded)), RefusalUnknown, "mock-232")
	}
	d := decide(append(at("mock-231", holdsR, noneF, time.Second), at("mock-232", noneR, holdsF, 0)...), nil)
	oRefused(t, "the recorded group at a place answered before the call, round F's text lacking its own, round F's group at another", d, RefusalUnknown, "mock-232")

	d = decide(nil, set(holdsR, 0, view(holdsF)))
	oRefused(t, "the recorded group in a set recording's answer, round F's in its view", d, RefusalUnknown, "mock-31")
	oNoChanceLine(t, d, "mock-31")

	// A place no chance by this rule is a candidate of serve order, as any
	// place left unknown is: given in another order than mock-50, which
	// held the recorded group whole and was set aside, it leaves mock-50
	// unknown too, and the line names both. mock-45, answered at the call's
	// own instant, holds the recorded group in the recording and round F's
	// in round F.
	aside := oCarried("mock-50", "body.ref", "ref 48d1 x", "ref zzzz x", 2*time.Second, time.Second, 2)
	m := &models.Mock{Name: "mock-45", Spec: models.MockSpec{ReqTimestampMock: oAt.Add(-900 * time.Millisecond), ResTimestampMock: oAt}}
	both := Earlier{Mock: "mock-45", At: oAt.Add(-900 * time.Millisecond), Own: true, Payload: RawOf(m, true).ServedAs(1),
		Seen: []Seen{{Side: Response, Leaf: oText("body.note", "owner "+holdsR), Live: "owner " + holdsF}}}
	d = decide([]Earlier{aside, both}, nil)
	oRefused(t, "the recorded group and round F's at mock-45, given in another order than mock-50", d, RefusalUnknown, "mock-50", "mock-45")
	if !strings.Contains(d.Line, "another order") {
		t.Errorf("the line must say the runs gave the places in another order: %q", d.Line)
	}

	// So is a place left unknown that is no chance of the piece at all,
	// however few runs hold their own piece: mock-45 holds the recorded
	// group as a whole token of its own, and round F nothing of its group
	// anywhere. Given in another order than mock-50, it leaves mock-50
	// unknown too, as on main.
	whole := both
	whole.Seen = []Seen{{Side: Response, Leaf: oText("body.note", "owner 48d1"), Live: "owner none"}}
	d = decide([]Earlier{aside, whole}, nil)
	oRefused(t, "the recorded group whole at mock-45, round F's nowhere, given in another order than mock-50", d, RefusalUnknown, "mock-50", "mock-45")
	if !strings.Contains(d.Line, "another order") {
		t.Errorf("the line must say the runs gave the places in another order: %q", d.Line)
	}
}

// Each run's holders of its own piece count by that run's rule, beside the
// other run's piece at an earlier test's place, and then no place is
// chance: the piece is refused as unknown (fail closed). The recording's
// rule is timed: a recording whose request holds the recorded group is
// placed by its request, sent a second before the call, though it was
// answered two after it; a set recording whose recorded time keploy cannot
// read counts, whatever its answer's time says. Round F's rule counts every
// place at any time: the text of a place of the call's own test answered
// after the call in R's time, a view of a recording round F served only
// after the call, and the recorded answer of a set recording round F
// served as recorded.
func TestOutcomeAPieceEachRunsHoldersCountByThatRunsRule(t *testing.T) {
	const (
		path           = "body.docs.msgs.0._id"
		rec, live      = "ed35a78f-905c-48d1-bc3d-a07311678c73", "3f0c9a12-7e6d-4b8a-9e21-5c6d7e8f9a0b"
		r, f           = "48d1", "4b8a"
		holdsR, holdsF = "b0fd3b58-3ff5-48d1-a1b2-c3d4e5f60718", "9e9e9e9e-77aa-4b8a-b2c3-d4e5f6071829"
		noneR, noneF   = "aaaa1111-2222-4c00-8333-444455556666", "c1ae4c69-77aa-4c00-b2c3-d4e5f6071829"
	)
	at := func(mock, field, recText, liveText string, run bool, answered time.Duration) []Earlier {
		return oServed(mock, run, false, Response, oText(field, recText), liveText, answered)
	}
	setOf := func(x Raw) func() ([]GivenPlaces, error) {
		return func() ([]GivenPlaces, error) { return []GivenPlaces{PlacesOf(x)}, nil }
	}
	decide := func(e []Earlier, raw []Raw, given func() ([]GivenPlaces, error)) Decision {
		in := oInput(path, rec, live, e, raw)
		in.Given = given
		return oWord(t, Decide(in), r, f)
	}
	rAt := at("mock-231", "body.cursor.firstBatch.0._id", holdsR, noneF, true, 0)
	fAt := at("mock-232", "body.cursor.firstBatch.0.parent", noneR, holdsF, true, 0)

	sent := RawOf(tokenMock("mock-5", holdsR, oAt.Add(-time.Second), oAt.Add(2*time.Second)), true).ServedAs(6).ServedAsRecorded()
	oRefused(t, "the recorded group in the request of a recording answered after the call, round F's at an earlier test's place",
		decide(fAt, []Raw{sent}, nil), RefusalUnknown, "mock-232")

	m := receiveOf("mock-31", holdsR, oAt.Add(-time.Minute))
	m.Spec.ResTimestampMock = oAt.Add(time.Minute)
	untimed := clockedDrifts().RawOf(m, true).ServedAs(3).ServedAsRecorded()
	untimed.at, untimed.noTime = oAt, true
	oRefused(t, "the recorded group in the answer of a set recording recorded at a time keploy cannot read, answered after the call, round F's at an earlier test's place",
		decide(fAt, nil, setOf(untimed)), RefusalUnknown, "mock-232", "mock-31")

	late := at("mock-233", "body.cursor.firstBatch.0.parent", noneR, holdsF, false, 3*time.Second)
	oRefused(t, "the recorded group at an earlier test's place, round F's at a place of the call's own test answered after the call",
		decide(append(slices.Clone(rAt), late...), nil, nil), RefusalUnknown, "mock-231")

	view := func(held string) Raw { return RawOf(receiveOf("mock-33", held, oAt.Add(-time.Minute)), true) }
	after := view(noneR).ServedAs(3).ServedChanged(view(noneF), view(holdsF))
	after.servedAfter = true
	oRefused(t, "the recorded group at an earlier test's place, round F's in a view of a recording round F served only after the call",
		decide(rAt, []Raw{after}, nil), RefusalUnknown, "mock-231")

	// A value the test gave keploy counts for the recording too. It is no
	// chance itself (left unknown where round F's gave nothing new), and the
	// place round F's group stands at is named beside it.
	gave := TestGiven("test test-2", "given:code", "code "+holdsR, "code")
	oRefused(t, "the recorded group in a value the test gave, round F's at an earlier test's place",
		decide(append([]Earlier{gave}, at("mock-232", "body.cursor.firstBatch.0.parent", noneR, holdsF, true, time.Second)...), nil, nil), RefusalUnknown, "given:code", "mock-232")

	s := receiveOf("mock-31", holdsF, oAt.Add(-time.Minute))
	s.Spec.ResTimestampMock = time.Time{}
	oRefused(t, "the recorded group at an earlier test's place, round F's in the recorded answer of a set recording served as recorded",
		decide(rAt, nil, setOf(clockedDrifts().RawOf(s, true).ServedAs(3).ServedAsRecorded())), RefusalUnknown, "mock-231")

	// The recording's holders count by the recording's own time, whatever
	// round F did with the place: a recording answered a minute before the
	// call that round F served only after it, and an earlier test's place
	// round F served only after the call, each hold the recorded group.
	// Round F's count at any time: its group in the text of an earlier
	// test's place that round F served only after the call.
	early := RawOf(receiveOf("mock-41", holdsR, oAt.Add(-time.Minute)), true).ServedAs(3).ServedAsRecorded()
	early.servedAfter = true
	oRefused(t, "the recorded group in a recording answered before the call that round F served only after it, round F's at an earlier test's place",
		decide(fAt, []Raw{early}, nil), RefusalUnknown, "mock-232")
	rLate := slices.Clone(rAt)
	rLate[0].Payload.servedAfter = true
	oRefused(t, "the recorded group at an earlier test's place round F served only after the call, round F's at another earlier test's place",
		decide(append(rLate, fAt...), nil, nil), RefusalUnknown, "mock-231", "mock-232")
	fLate := slices.Clone(fAt)
	fLate[0].Payload.servedAfter = true
	oRefused(t, "the recorded group at an earlier test's place, round F's in the text of another that round F served only after the call",
		decide(append(slices.Clone(rAt), fLate...), nil, nil), RefusalUnknown, "mock-231", "mock-232")

	// Each control holds only round F's own piece at that place, and the
	// recording's at none: the piece is chance, and the id fresh.
	costFresh(t, "round F's group at an earlier test's place, the recording's at none, beside a recording round F served only after the call",
		decide(fAt, []Raw{func() Raw { x := view(noneR).ServedAs(3).ServedChanged(view(noneF)); x.servedAfter = true; return x }()}, nil))
	answered := *s
	answered.Spec.ResTimestampMock = oAt.Add(-time.Minute + time.Second)
	costFresh(t, "round F's group at an earlier test's place and in a set recording's recorded answer, the recording's at none",
		decide(fAt, nil, setOf(clockedDrifts().RawOf(&answered, true).ServedAs(3).ServedAsRecorded())))
	// Where keploy cannot read when the recording was given that set
	// recording, the recording's application may not have had it to cut
	// its group from: round F's group there is no chance.
	oRefused(t, "round F's group at an earlier test's place and in a set recording's recorded answer with no answer time, the recording's at none",
		decide(fAt, nil, setOf(clockedDrifts().RawOf(s, true).ServedAs(3).ServedAsRecorded())), RefusalUnknown, "mock-31")
}

// One run's own piece stands at no place it had, so that run's application
// made it with the id. That shows nothing of a place it was not given
// before the call: where the other run's piece stands at one, the
// application may have cut it from there, and the piece is refused as
// unknown, naming the place (fail closed), in the shapes integrations'
// Mongo rows check. The recording's group at a place of the call's own test
// answered before the call that round F served only after it, or whose
// answer round F wrote after the call arrived; round F's group at one the
// recording answered after the call or with no answer time, sent at the
// call's own instant, or at a recorded time keploy cannot read (an
// exchange's, a recording's). Each control is chance, the id fresh:
// round F served the place before the call (its text holds none of its
// group); round F served it only after the call too, so neither run's
// application had it; the recording answered it before the call; round F's
// group in a request the recording sent before the call, answered after it.
func TestOutcomeAPieceOneRunMadeIsChanceOnlyAtPlacesThatRunHad(t *testing.T) {
	const (
		path           = "body.docs.msgs.0._id"
		rec, live      = "ed35a78f-905c-48d1-bc3d-a07311678c73", "3f0c9a12-7e6d-4b8a-9e21-5c6d7e8f9a0b"
		r, f           = "48d1", "4b8a"
		field          = "body.cursor.firstBatch.0._id"
		holdsR, holdsF = "b0fd3b58-3ff5-48d1-a1b2-c3d4e5f60718", "9e9e9e9e-77aa-4b8a-b2c3-d4e5f6071829"
		noneR, noneF   = "aaaa1111-2222-4c00-8333-444455556666", "c1ae4c69-77aa-4c00-b2c3-d4e5f6071829"
		fNotGiven      = ", which round F did not give the application before the call: round F's making its own piece shows nothing of whether the application cut it from there"
		rNotGiven      = ", which the recording did not give the application before the call, by a time keploy reads: the recording's making its own piece shows nothing of whether the application cut it from there"
	)
	// own is a place of the call's own test, sent two seconds before the
	// call and answered answered after that.
	own := func(mock string, side Side, recText, liveText string, answered time.Duration) []Earlier {
		return oServed(mock, false, false, side, oText(field, recText), liveText, answered)
	}
	decide := func(e []Earlier, raw []Raw) Decision {
		return oWord(t, Decide(oInput(path, rec, live, e, raw)), r, f)
	}
	fLate := func(e []Earlier) []Earlier { e[0].Payload.servedAfter = true; return e }

	d := decide(fLate(own("mock-51", Response, holdsR, noneF, time.Second)), nil)
	oRefused(t, "the recorded group at a place answered before the call that round F served only after it", d, RefusalUnknown, "mock-51")
	oOneSideLine(t, d, "the recording's own piece of the id stands at mock-51 "+field+fNotGiven)
	costFresh(t, "the recorded group at a place answered before the call that round F served before it too", decide(own("mock-51", Response, holdsR, noneF, time.Second), nil))

	view := func(mock, held string) Raw { return RawOf(receiveOf(mock, held, oAt.Add(-time.Minute)), true) }
	written := view("mock-55", holdsR).ServedAs(3).ServedLive(view("mock-55", noneF))
	written.servedAfter = true
	oRefused(t, "the recorded group in a recording answered before the call whose answer round F wrote after the call arrived", decide(nil, []Raw{written}), RefusalUnknown, "mock-55")
	costFresh(t, "the recorded group in a recording answered before the call that round F answered before it", decide(nil, []Raw{view("mock-55", holdsR).ServedAs(3).ServedLive(view("mock-55", noneF))}))

	d = decide(own("mock-52", Response, noneR, holdsF, 3*time.Second), nil)
	oRefused(t, "round F's group at a place the recording answered after the call", d, RefusalUnknown, "mock-52")
	oOneSideLine(t, d, "round F's own piece of the id stands at mock-52 "+field+rNotGiven)
	costFresh(t, "round F's group at a place the recording answered after the call, round F served only after it", decide(fLate(own("mock-52", Response, noneR, holdsF, 3*time.Second)), nil))
	costFresh(t, "round F's group at a place the recording answered before the call", decide(own("mock-52", Response, noneR, holdsF, time.Second), nil))
	costFresh(t, "round F's group in a request the recording sent before the call, answered after it", decide(own("mock-56", Request, noneR, holdsF, 3*time.Second), nil))

	untimed := own("mock-54", Response, noneR, holdsF, time.Second)
	untimed[0].noTime = true
	oRefused(t, "round F's group at an exchange whose recorded time keploy cannot read", decide(untimed, nil), RefusalUnknown, "mock-54")
	noTime := view("mock-53", noneR).ServedAs(3).ServedLive(view("mock-53", holdsF))
	noTime.noTime = true
	oRefused(t, "round F's group in the view of a recording whose recorded time keploy cannot read", decide(nil, []Raw{noTime}), RefusalUnknown, "mock-53")
	costFresh(t, "round F's group in the view of a recording answered before the call", decide(nil, []Raw{view("mock-53", noneR).ServedAs(3).ServedLive(view("mock-53", holdsF))}))
	answeredAt := func(mock string, answered time.Time) Raw {
		m := receiveOf(mock, noneR, oAt.Add(-time.Second))
		m.Spec.ResTimestampMock = answered
		return RawOf(m, true).ServedAs(3).ServedLive(view(mock, holdsF))
	}
	oRefused(t, "round F's group in the view of a recording asked before the call, answered after it", decide(nil, []Raw{answeredAt("mock-57", oAt.Add(time.Second))}), RefusalUnknown, "mock-57")
	oRefused(t, "round F's group in the view of a recording asked before the call, with no answer time", decide(nil, []Raw{answeredAt("mock-57", time.Time{})}), RefusalUnknown, "mock-57")
	atCall := own("mock-58", Request, noneR, holdsF, 0)
	atCall[0].At = oAt
	d = decide(atCall, nil)
	oRefused(t, "round F's group in a request the recording sent at the call's own instant", d, RefusalUnknown, "mock-58")
	oOneSideLine(t, d, "round F's own piece of the id stands at mock-58 "+field+rNotGiven)
	// An earlier test's place is given before the call in both runs,
	// whatever its times say (earlierState).
	sentLate := oServed("mock-59", true, false, Request, oText(field, noneR), holdsF, 0)
	sentLate[0].At = oAt.Add(time.Second)
	costFresh(t, "round F's group in a request of an earlier test's, recorded after the call", decide(sentLate, nil))
	costFresh(t, "round F's group in an answer of an earlier test's, answered after the call", decide(oServed("mock-60", true, false, Response, oText(field, noneR), holdsF, 3*time.Second), nil))
}

// A set recording holding the recorded group, beside the call's own places,
// is chance only where round F gave it with a view keploy reads: round F
// served it as recorded, or changed with one view. Not where round F's
// serves gave two views, it served it with no view stated, or did not serve
// it: keploy reads that round F gave nothing there, so round F's making its
// own group shows nothing of whether the application cut it there. Round
// F's group in a set recording is chance only where the recording gave it
// the application before the call, by a time keploy reads: not one
// answered after the call, asked after it, or with no answer time.
func TestOutcomeASetRecordingCountsForTheRunThatHadIt(t *testing.T) {
	const (
		path           = "body.docs.msgs.0._id"
		rec, live      = "ed35a78f-905c-48d1-bc3d-a07311678c73", "3f0c9a12-7e6d-4b8a-9e21-5c6d7e8f9a0b"
		r, f           = "48d1", "4b8a"
		holdsR, holdsF = "b0fd3b58-3ff5-48d1-a1b2-c3d4e5f60718", "9e9e9e9e-77aa-4b8a-b2c3-d4e5f6071829"
		noneR, noneF   = "aaaa1111-2222-4c00-8333-444455556666", "c1ae4c69-77aa-4c00-b2c3-d4e5f6071829"
	)
	// set is a set recording asked asked and answered answered from the
	// call (answered 0: no answer time), held in its answer.
	set := func(held string, asked, answered time.Duration) *models.Mock {
		m := receiveOf("mock-31", held, oAt.Add(asked))
		m.Spec.ResTimestampMock = time.Time{}
		if answered != 0 {
			m.Spec.ResTimestampMock = oAt.Add(answered)
		}
		return m
	}
	decide := func(g GivenPlaces) Decision {
		in := oInput(path, rec, live, nil, nil)
		in.Given = func() ([]GivenPlaces, error) { return []GivenPlaces{g}, nil }
		return oWord(t, Decide(in), r, f)
	}
	before := func(held string) *models.Mock { return set(held, -time.Minute, -time.Minute+time.Second) }
	for _, c := range []struct {
		what   string
		m      *models.Mock
		tells  []tell
		chance bool
	}{
		{"the recorded group, round F's view none of its group", before(holdsR), []tell{toldView("v1", before(noneF))}, true},
		{"the recorded group, round F's serves gave one view twice, none of its group", before(holdsR), []tell{toldView("v1", before(noneF)), toldView("v1", before(noneF))}, true},
		{"the recorded group, served as recorded", before(holdsR), []tell{toldAsRecorded}, true},
		{"the recorded group, served as recorded twice", before(holdsR), []tell{toldAsRecorded, toldAsRecorded}, true},
		{"the recorded group with no answer time, round F's view none of its group", set(holdsR, -time.Minute, 0), []tell{toldView("v1", before(noneF))}, true},
		{"the recorded group, round F's serves gave two views, neither its group", before(holdsR), []tell{toldView("v1", before(noneF)), toldView("v2", before(noneR))}, false},
		{"the recorded group, served as recorded once and with a view none of its group once", before(holdsR), []tell{toldAsRecorded, toldView("v1", before(noneF))}, false},
		{"the recorded group, served with no view stated", before(holdsR), []tell{toldNothing}, false},
		{"the recorded group, served as recorded once and with no view stated once", before(holdsR), []tell{toldAsRecorded, toldNothing}, false},
		{"the recorded group, served with a view keploy could not keep", before(holdsR), []tell{toldUnkept}, false},
		{"the recorded group, round F did not serve it", before(holdsR), nil, false},
		{"round F's group in its view, answered before the call", before(noneR), []tell{toldView("v1", before(holdsF))}, true},
		{"round F's group in its view, answered after the call", set(noneR, -time.Second, time.Second), []tell{toldView("v1", before(holdsF))}, false},
		{"round F's group in its view, asked after the call", set(noneR, time.Second, 2*time.Second), []tell{toldView("v1", before(holdsF))}, false},
		{"round F's group in its view, no answer time", set(noneR, -time.Minute, 0), []tell{toldView("v1", before(holdsF))}, false},
	} {
		for _, g := range givenBoth(t, c.m, c.tells...) {
			d := decide(g.g)
			if c.chance {
				costFresh(t, "a set recording ("+g.how+"): "+c.what, d)
				continue
			}
			oRefused(t, "a set recording ("+g.how+"): "+c.what, d, RefusalUnknown, "mock-31")
		}
	}
	noTime := clockedDrifts().RawOf(before(noneR), true).ServedAs(3).ServedLive(clockedDrifts().RawOf(before(holdsF), true))
	noTime.noTime = true
	oRefused(t, "a set recording: round F's group in its view, its recorded time unread", decide(PlacesOf(noTime)), RefusalUnknown, "mock-31")
	for _, g := range givenBoth(t, set(noneR, time.Second, 2*time.Second), toldView("v1", before(holdsF))) {
		oOneSideLine(t, decide(g.g), "round F's own piece of the id stands at mock-31, which the recording did not give the application before the call, by a time keploy reads: the recording's making its own piece shows nothing of whether the application cut it from there")
	}
	for _, g := range givenBoth(t, before(holdsR), toldView("v1", before(noneF)), toldView("v2", before(noneR))) {
		oOneSideLine(t, decide(g.g), "the recording's own piece of the id stands at mock-31, which round F gave the application with no view keploy reads: round F's making its own piece shows nothing of whether the application cut it from there")
	}
}

// tell is how one of round F's serves of a set recording was told: as
// recorded (toldAsRecorded), with a view named key (toldView), with a view
// keploy cannot keep (toldUnkept), or not at all (toldNothing).
type tell struct {
	key  string
	view *models.Mock
}

var (
	toldAsRecorded = tell{key: "\x00recorded"}
	toldNothing    = tell{key: "\x00untold"}
	toldUnkept     = tell{}
)

func toldView(key string, view *models.Mock) tell { return tell{key: key, view: view} }

// givenThrough is m as production hands a set recording to a call: served
// in a tracker's set once for each of tells, each serve told so, and read
// by Order.GivenPlaces, as integrations' Mongo and Postgres learn steps
// read it. No tells: round F did not serve it.
func givenThrough(t *testing.T, m *models.Mock, tells ...tell) GivenPlaces {
	t.Helper()
	tr := NewTracker()
	tr.Begin()
	for _, c := range tells {
		tr.ServedMock(m)
		switch c.key {
		case toldAsRecorded.key:
			tr.ServedMockAsRecorded(m)
		case toldNothing.key:
		default:
			var load func() (*models.Mock, error)
			if view := c.view; view != nil {
				load = func() (*models.Mock, error) { return view, nil }
			}
			tr.ServedMockLive(m, c.key, load)
		}
	}
	g, err := tr.Placed().Order.GivenPlaces(m)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

// givenRaw is m's Raw marked as Order.ServedView marks a recording of
// another test whose serves were told as tells say (statedView), read as
// a set recording: PlacesOf of it.
func givenRaw(m *models.Mock, tells ...tell) GivenPlaces {
	x := clockedDrifts().RawOf(m, true)
	if len(tells) == 0 {
		return PlacesOf(x)
	}
	x = x.ServedAs(3)
	recorded, untold, unkept := false, false, false
	var views []Raw
	seen := map[string]bool{}
	for _, c := range tells {
		switch {
		case c.key == toldAsRecorded.key:
			recorded = true
		case c.key == toldNothing.key:
			untold = true
		case c.key == "" || c.view == nil:
			unkept = true
		case !seen[c.key]:
			seen[c.key] = true
			views = append(views, clockedDrifts().RawOf(c.view, true))
		}
	}
	switch {
	case untold:
		return PlacesOf(x)
	case unkept:
		return PlacesOf(x.ServedChanged())
	case len(views) == 0:
		return PlacesOf(x.ServedAsRecorded())
	case !recorded && len(views) == 1:
		return PlacesOf(x.ServedLive(views[0]))
	case recorded:
		views = append(views, x)
	}
	return PlacesOf(x.ServedChanged(views...))
}

// givenShape is a set recording read one way (how).
type givenShape struct {
	how string
	g   GivenPlaces
}

// givenBoth is m as a set recording whose serves were told as tells say,
// read both ways: as a marked Raw (givenRaw) and as production reads one
// (givenThrough).
func givenBoth(t *testing.T, m *models.Mock, tells ...tell) []givenShape {
	t.Helper()
	return []givenShape{{"a marked Raw", givenRaw(m, tells...)}, {"through the tracker", givenThrough(t, m, tells...)}}
}

// A set recording was given R's application before the call only where it
// was recorded and answered at or before it (givenBefore): one asked after
// the call was not, even with no answer time, nor one answered after it. One
// whose recorded time keploy cannot read may have been, whatever its other
// times say (fail closed).
func TestOutcomeASetRecordingIsGivenBeforeTheCallByBothItsTimes(t *testing.T) {
	for _, c := range []struct {
		what                  string
		asked, answer         time.Duration
		untimed, noTime, want bool
	}{
		{"asked and answered before the call", -2 * time.Second, -time.Second, false, false, true},
		{"asked before the call, answered at it", -time.Second, 0, false, false, true},
		{"asked before the call, answered after it", -time.Second, time.Second, false, false, false},
		{"asked after the call, with no answer time", time.Second, 0, true, false, false},
		{"asked after the call, answered after it", time.Second, 2 * time.Second, false, false, false},
		{"recorded at a time keploy cannot read, answered after the call", time.Second, 2 * time.Second, false, true, true},
	} {
		var g givenWords
		g.at, g.noTime = oAt.Add(c.asked), c.noTime
		if !c.untimed {
			g.answered = oAt.Add(c.answer)
		}
		if got := g.givenBefore(oAt); got != c.want {
			t.Errorf("%s: given before the call %v; want %v", c.what, got, c.want)
		}
	}
}

// A call asks a minted id's long words of the set's lists once, not once
// per set recording that holds the id's piece by chance (each with no
// answer time, so it leaves the piece unknown and is asked whether it shows
// the id): past the set's budget, where the index keeps no list and a call
// searches each, sixteen such recordings search about four times the lists
// four do, every one of them chance and the id fresh.
func TestOutcomeCostAnIdsLongWordsAreAskedOncePerCall(t *testing.T) {
	defer func(b int64) { learnBudget = b }(learnBudget)
	learnBudget = 1
	const rec, live = "ed35a78f-905c-48d1-bc3d-a07311678c73", "3f0c9a12-7e6d-4b8a-9e21-5c6d7e8f9a0b"
	searched := func(n int) int64 {
		gs := make([]GivenPlaces, n)
		for i := range gs {
			m := receiveOf(fmt.Sprintf("mock-%d", 31+i), fmt.Sprintf("b0fd3b%02x-3ff5-48d1-a1b2-c3d4e5f607%02x", i, i), oAt.Add(-time.Minute))
			m.Spec.ResTimestampMock = time.Time{}
			gs[i] = PlacesOf(clockedDrifts().RawOf(m, true).ServedAs(3).ServedAsRecorded())
		}
		in := oInput("body.docs.msgs.0._id", rec, live, nil, nil)
		in.Given = func() ([]GivenPlaces, error) { return gs, nil }
		l := listsAsks.Load()
		d := oWord(t, Decide(in), "48d1", "4b8a")
		asked := listsAsks.Load() - l
		costFresh(t, fmt.Sprintf("the recorded group inside another id in %d set recordings", n), d)
		return asked
	}
	four, sixteen := searched(4), searched(16)
	t.Logf("lists searched: %d over four recordings, %d over sixteen", four, sixteen)
	if four == 0 || sixteen > 5*four {
		t.Errorf("the calls searched %d lists over four recordings and %d over sixteen; want some, growing with the lists (at most five times)", four, sixteen)
	}
}

// A piece of one id is chance only at a place left unknown for it: a place
// that disproves the piece (it held the recorded group and round F's text
// lacks F's) is read as for any word, a candidate whose serve order counts,
// while a place unknown for it that shows nothing of the id's long words is
// no candidate, and its order counts for nothing. mock-50 holds the
// recorded group whole and round F gave another word there, set aside; the
// recording gave it before mock-45 (or mock-51) and round F after it. A
// value the test gave keploy that stands inside the piece in both runs, and
// a body that declares no media type where the piece stands glued to the
// bytes around it, are no chance either: the test made the one, and keploy
// cannot tell the words of the other from its framing.
func TestOutcomeAPieceOfAnIdIsChanceOnlyAtAPlaceLeftUnknownForIt(t *testing.T) {
	const (
		rec, live = "1111aaaa-2222-48d1-9444-444455556666", "9999cccc-7777-4a2e-8888-000011112222"
		r, f      = "48d1", "4a2e"
		path      = "body.docs.msgs.0.appId"
	)
	aside := oCarried("mock-50", "body.ref", "ref 48d1 x", "ref zzzz x", 2*time.Second, time.Second, 2)
	chance := oCarried("mock-45", "body.note", "owner bb7ed9ee-670e-48d1-982f-b54b2b73982f", "owner 0c1d2e3f-4a5b-4c7d-8e9f-4a2e0b1c2d3e", 900*time.Millisecond, 100*time.Millisecond, 1)
	d := oWord(t, Decide(oInput(path, rec, live, []Earlier{aside, chance}, nil)), r, f)
	costFresh(t, "a piece set aside at mock-50, chance at mock-45, which the runs gave in another order", d)
	if !strings.Contains(d.Line, "mock-50") || !strings.Contains(d.Line, "mock-45") {
		t.Errorf("the line must name mock-50 set aside and mock-45 as chance: %q", d.Line)
	}

	disproves := oCarried("mock-51", "body.note", "ref x-48d1-y", "ref x-zzzz-y", 900*time.Millisecond, 100*time.Millisecond, 1)
	d = oWord(t, Decide(oInput(path, rec, live, []Earlier{aside, disproves}, nil)), r, f)
	oRefused(t, "a place that disproves the piece inside another id, given in another order than mock-50", d, RefusalUnknown, "mock-50")
	if !strings.Contains(d.Line, "another order: the 1st earlier answer was mock-50 in the recording and mock-51") {
		t.Errorf("the line must say the runs gave the places in another order: %q", d.Line)
	}
	inOrder := oCarried("mock-51", "body.note", "ref x-48d1-y", "ref x-zzzz-y", 900*time.Millisecond, 100*time.Millisecond, 3)
	costFresh(t, "the same places, given in one order", oWord(t, Decide(oInput(path, rec, live, []Earlier{aside, inOrder}, nil)), r, f))

	gave := TestGiven("test 4", "given:code", "48", "4a")
	oRefused(t, "a value the test gave, standing inside the piece in both runs", oWord(t, Decide(oInput(path, rec, live, []Earlier{gave}, nil)), r, f), RefusalUnknown)

	undeclared := Earlier{Mock: "test 4", Given: true, Seen: []Seen{{Side: Request, Leaf: oText("body", "BA48d1zz"), Live: "BA4a2ezz", Place: "body", Undeclared: true}}}
	oRefused(t, "a body that declares no media type, the piece glued to the bytes around it", oWord(t, Decide(oInput(path, rec, live, []Earlier{undeclared}, nil)), r, f), RefusalUnknown)
}

// A place keploy cannot read again for an id (its recording's answer gone
// from the store it is read from lazily) is no chance: whether it holds the
// piece as a whole id token, or one of the id's long words, is unknown, so
// it stays unknown for the piece (fail closed). Read, the place holds the
// recorded group only inside another id, and the id's 32-bit group beside
// it, which it shows (a word the set's filter rules out is answered without
// a read, so only one the text may hold asks the store). Through Decide a place's text is read for the piece just before, so the
// branch is pinned here, at the question, on a recording read lazily.
func TestOutcomeAPlaceNotReadAgainForAnIdIsNoChance(t *testing.T) {
	defer func(b, r int64) { bodyBudget, readerBudget = b, r }(bodyBudget, readerBudget)
	bodyBudget, readerBudget = 0, 1
	tr := NewTracker()
	tr.NewSet()
	tr.Begin()
	defer tr.End()
	dr := tr.Drifts()
	m := getAnswer("mock-7", "http://api/owners/1", `{"owner":"bb7ed9ee-670e-48d1-982f-b54b2b73982f","lead":"1111aaaa"}`, oAt.Add(-time.Second))
	gone := false
	raw := dr.LazyRawOf(m, true, func() (*models.Mock, error) {
		if gone {
			return nil, errors.New("the parked answer is gone")
		}
		return m, nil
	})
	long := []readWord{{Rec: "1111aaaa", Live: "9999cccc"}, {Rec: "444455556666", Live: "000011112222"}}
	for _, tc := range []struct {
		what  string
		gone  bool
		whole bool
		shown bool
	}{
		{"read again", false, false, true},
		{"not read again", true, true, true},
	} {
		x := newReaderIndex(dr)
		p := evPlace{name: "mock-7", t: &placeText{rec: x.textOf(raw), live: x.text("owner 0c1d2e3f-4a5b-4c7d-8e9f-4a2e0b1c2d3e")}}
		gone = tc.gone
		if got := pieceWholeAt(p, "48d1", "4a2e"); got != tc.whole {
			t.Errorf("%s: the place holds the piece as a whole id token: %v; want %v", tc.what, got, tc.whole)
		}
		if got := idShownAt(p, long); got != tc.shown {
			t.Errorf("%s: the place shows the id's long words: %v; want %v", tc.what, got, tc.shown)
		}
	}
}

// Through Decide, every long word of an id the call minted is fresh, and a
// word of 32 bits or more that a place or a set recording shows is bound
// or refused there instead: so no place a piece of such an id is asked at
// shows one of its long words. The long words still judge a place and a
// set recording where the piece is asked, a second guard that keeps them
// no chance should the call's own reading of a long word ever learn one
// fresh beside a place that shows it. Pinned here, at the question: a
// place that shows the id's one long word (the recorded 32-bit group in
// the recording, round F's word nowhere) is no chance, while one that
// shows nothing of it is; a set recording whose answer holds the recorded
// long word, or whose round F view holds round F's, is no chance, while
// one that holds neither is.
func TestOutcomeALongWordShownJudgesAPlaceAtTheQuestion(t *testing.T) {
	rd := textReading{Words: []readWord{{Rec: "1111aaaa", Live: "9999cccc"}, {Rec: "48d1", Live: "4a2e"}, {Rec: "2222bbbb3333", Live: "4444dddd5555"}}}
	if got := mintedLong(rd, map[int]bool{0: true, 2: true}, []int{0, 2}); len(got) != 2 || got[0].Rec != rd.Words[0].Rec || got[1].Rec != rd.Words[2].Rec {
		t.Errorf("an id the call minted: its long words %v; want both, in order", got)
	}
	if got := mintedLong(rd, map[int]bool{0: true}, []int{0, 2}); got != nil {
		t.Errorf("an id whose second long word was not learned fresh: %v; want none", got)
	}
	x := newReaderIndex(NewDriftCache())
	p := evPlace{name: "mock-45", t: &placeText{
		rec:  x.text("owner bb7ed9ee-670e-48d1-982f-b54b2b73982f lead 1111aaaa"),
		live: x.text("owner 0c1d2e3f-4a5b-4c7d-8e9f-4a2e0b1c2d3e"),
	}}
	w := readWord{Rec: "48d1", Live: "4a2e"}
	shown, none := readWord{Rec: "1111aaaa", Live: "9999cccc"}, readWord{Rec: "2222bbbb", Live: "3333dddd"}
	for _, tc := range []struct {
		what   string
		long   []readWord
		chance bool
	}{
		{"the place holds the id's one long word", []readWord{shown}, false},
		{"the place holds the second of the id's two long words", []readWord{none, shown}, false},
		{"the place holds the first of the id's two long words", []readWord{shown, none}, false},
		{"the place holds nothing of the id's one long word", []readWord{none}, true},
	} {
		dc := &decider{ps: []evPlace{p}}
		chance := dc.idChance(newWordPlaces(dc.ps, w.Rec), w, tc.long)
		if got := chance(0); got != tc.chance {
			t.Errorf("%s: the place holds the piece by chance: %v; want %v", tc.what, got, tc.chance)
		}
	}

	const (
		setRec, setLive = "5c0e9a7b-1f2d-48d1-9a3b-c4d5e6f70812", "0c1d2e3f-4a5b-4c7d-8e9f-a0b1c2d3e4f5"
		other           = "0f1e2d3c-5b6a-4978-8a9b-cdef01234567"
	)
	mock := func(held string) Raw {
		m := receiveOf("mock-31", held, r1Insert.Add(-time.Minute))
		m.Spec.ResTimestampMock = time.Time{}
		return clockedDrifts().RawOf(m, true)
	}
	long := []readWord{{Rec: "5c0e9a7b", Live: "0c1d2e3f"}, {Rec: "c4d5e6f70812", Live: "a0b1c2d3e4f5"}}
	piece := readWord{Rec: "48d1", Live: "4c7d"}
	for _, tc := range []struct {
		what  string
		x     Raw
		shows bool
	}{
		{"the answer holds the recorded 32-bit group, round F's view nothing of the id", mock("lead 5c0e9a7b " + other).ServedAs(3).ServedLive(mock(other)), true},
		{"round F's view holds round F's 48-bit group, the answer nothing of the id", mock(other).ServedAs(3).ServedLive(mock("tail a0b1c2d3e4f5 " + other)), true},
		{"neither holds anything of the id", mock(other).ServedAs(3).ServedLive(mock(other)), false},
	} {
		in := oInput("body.appId", setRec, setLive, nil, nil)
		in.At, in.Arrived = r1Insert, r1Insert
		x := tc.x
		in.Given = func() ([]GivenPlaces, error) { return []GivenPlaces{PlacesOf(x)}, nil }
		dc := newDecider(in, false, newReaderIndex(in.Drifts))
		oh := dc.held()
		shows := dc.idOutside(oh, piece, long)
		gs := dc.outside().words
		if len(gs) != 1 {
			t.Fatalf("%s: %d set recordings; want 1", tc.what, len(gs))
		}
		if got := shows(0); got != tc.shows {
			t.Errorf("%s: the set recording may be where the application took the id: %v; want %v", tc.what, got, tc.shows)
		}
	}
}

// A timed set recording holding the recorded group that round F served as
// recorded, or with one view none of its group, beside a place of the
// call's own test answered at the call's own instant that holds it too
// (unknown there; round F served it before the call): round F's group
// stands nowhere, so round F made it, and it gave the application both
// before the call with a text keploy reads. Chance: the id fresh. Not where
// round F's serves of the set recording gave two views, one with no view
// stated, or one keploy could not keep, which keploy reads as round F
// having given nothing there.
func TestOutcomeASetRecordingRoundFReadCountsBesideAnUnknownOwnPlace(t *testing.T) {
	const (
		path          = "body.docs.msgs.0._id"
		rec, live     = "ed35a78f-905c-48d1-bc3d-a07311678c73", "3f0c9a12-7e6d-4b8a-9e21-5c6d7e8f9a0b"
		r, f          = "48d1", "4b8a"
		field         = "body.cursor.firstBatch.0._id"
		holdsR        = "b0fd3b58-3ff5-48d1-a1b2-c3d4e5f60718"
		noneR, noneF  = "aaaa1111-2222-4c00-8333-444455556666", "c1ae4c69-77aa-4c00-b2c3-d4e5f6071829"
		answeredAtTwo = 2 * time.Second
	)
	set := func(held string) *models.Mock {
		m := receiveOf("mock-31", held, oAt.Add(-time.Minute))
		m.Spec.ResTimestampMock = oAt.Add(-time.Minute + time.Second)
		return m
	}
	decide := func(g *GivenPlaces) Decision {
		in := oInput(path, rec, live, oServed("mock-51", false, false, Response, oText(field, holdsR), noneF, answeredAtTwo), nil)
		if g != nil {
			in.Given = func() ([]GivenPlaces, error) { return []GivenPlaces{*g}, nil }
		}
		return oWord(t, Decide(in), r, f)
	}
	costFresh(t, "the own place alone", decide(nil))
	for _, c := range []struct {
		what   string
		tells  []tell
		chance bool
	}{
		{"served as recorded", []tell{toldAsRecorded}, true},
		{"served with one view none of round F's group", []tell{toldView("v1", set(noneF))}, true},
		{"served with two views, neither round F's group", []tell{toldView("v1", set(noneF)), toldView("v2", set(noneR))}, false},
		{"served with no view stated", []tell{toldNothing}, false},
		{"served as recorded once and with no view stated once", []tell{toldAsRecorded, toldNothing}, false},
		{"served with a view keploy could not keep", []tell{toldUnkept}, false},
	} {
		for _, g := range givenBoth(t, set(holdsR), c.tells...) {
			d := decide(&g.g)
			if c.chance {
				costFresh(t, "beside a set recording ("+g.how+") "+c.what, d)
				continue
			}
			oRefused(t, "beside a set recording ("+g.how+") "+c.what, d, RefusalUnknown, "mock-31")
			if len(c.tells) < 2 || c.tells[1].view == nil {
				continue
			}
			// Two views: the own place asks the rule first, and the set
			// recording is the holder round F was not given with a text
			// keploy reads.
			oOneSideLine(t, d, "the recording's own piece of the id stands at mock-31, which round F gave the application with no view keploy reads: round F's making its own piece shows nothing of whether the application cut it from there")
		}
	}
}

// A view a set recording's words were read from that cannot be read again
// for its instants is one keploy does not read: the recording counts for
// no run's making its piece (givenWords.fRead), as for a view never read.
func TestGivenPlacesAViewReadOnceOnlyIsNotRead(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	m := receiveOf("mock-31", "b0fd3b58-3ff5-48d1-a1b2-c3d4e5f60718", oAt.Add(-time.Minute))
	m.Spec.ResTimestampMock = oAt.Add(-time.Minute + time.Second)
	view := receiveOf("mock-31", "c1ae4c69-77aa-4c00-b2c3-d4e5f6071829", oAt.Add(-time.Minute))
	view.Spec.ResTimestampMock = m.Spec.ResTimestampMock
	for _, c := range []struct {
		what  string
		reads int
		want  bool
	}{{"read each time", 1 << 30, true}, {"read once only", 1, false}} {
		tr := NewTracker()
		tr.Begin()
		tr.ServedMock(m)
		n := 0
		tr.ServedMockLive(m, "v1", func() (*models.Mock, error) {
			if n++; n > c.reads {
				return nil, errors.New("the view is gone")
			}
			return view, nil
		})
		o := tr.Placed().Order
		// No set store of views: each reader reads the view again.
		o.places = nil
		g, err := o.GivenPlaces(m)
		if err != nil {
			t.Fatal(err)
		}
		if g.words.fRead != c.want || g.words.unread == c.want {
			t.Errorf("a view %s: fRead %v, unread %v; want fRead %v", c.what, g.words.fRead, g.words.unread, c.want)
		}
	}
}

// The recording made its group (it holds it at no place it had), and
// round F's group stands in one of the two views round F's serves gave of
// a place the recording did not give the application before the call
// (answered after it): the recording's making its group shows nothing of
// that place. Refused, naming it, for a recording handed to the call and
// for a set recording.
func TestOutcomeRoundFsGroupInOneOfTwoViewsOfAPlaceTheRecordingHadNot(t *testing.T) {
	const (
		path         = "body.docs.msgs.0._id"
		rec, live    = "ed35a78f-905c-48d1-bc3d-a07311678c73", "3f0c9a12-7e6d-4b8a-9e21-5c6d7e8f9a0b"
		r, f         = "48d1", "4b8a"
		holdsF       = "9e9e9e9e-77aa-4b8a-b2c3-d4e5f6071829"
		noneR, noneF = "aaaa1111-2222-4c00-8333-444455556666", "c1ae4c69-77aa-4c00-b2c3-d4e5f6071829"
	)
	// at is a recording of mock asked a second before the call and
	// answered answered from the call, holding held.
	at := func(mock, held string, answered time.Duration) *models.Mock {
		m := receiveOf(mock, held, oAt.Add(-time.Second))
		m.Spec.ResTimestampMock = oAt.Add(answered)
		return m
	}
	raw := func(mock, held string) Raw { return clockedDrifts().RawOf(at(mock, held, time.Second), true) }
	x := raw("mock-61", noneR).ServedAs(3).ServedChanged(raw("mock-61", noneF), raw("mock-61", holdsF))
	oRefused(t, "round F's group in one of two views of a recording the recording answered after the call", oWord(t, Decide(oInput(path, rec, live, nil, []Raw{x})), r, f), RefusalUnknown, "mock-61")
	for _, g := range givenBoth(t, at("mock-31", noneR, time.Second), toldView("v1", at("mock-31", noneF, time.Second)), toldView("v2", at("mock-31", holdsF, time.Second))) {
		in := oInput(path, rec, live, nil, nil)
		in.Given = func() ([]GivenPlaces, error) { return []GivenPlaces{g.g}, nil }
		oRefused(t, "a set recording ("+g.how+"): round F's group in one of two views, the recording answered it after the call", oWord(t, Decide(in), r, f), RefusalUnknown, "mock-31")
	}
}

// Where several set recordings hold the recorded group and round F gave
// none of them with a text keploy reads, the line names the first of them
// in the set's order, the same place on every decision; so too where
// several hold round F's group and the recording gave none of them before
// the call.
func TestOutcomeTheHolderTheLineNamesIsTheSetsFirst(t *testing.T) {
	const (
		path            = "body.docs.msgs.0._id"
		rec, live       = "ed35a78f-905c-48d1-bc3d-a07311678c73", "3f0c9a12-7e6d-4b8a-9e21-5c6d7e8f9a0b"
		r, f            = "48d1", "4b8a"
		holdsR, holdsR2 = "b0fd3b58-3ff5-48d1-a1b2-c3d4e5f60718", "0b0b0b0b-1c1c-48d1-a2a2-c3c3c3c3c3c3"
		noneR, noneF    = "aaaa1111-2222-4c00-8333-444455556666", "c1ae4c69-77aa-4c00-b2c3-d4e5f6071829"
	)
	set := func(name, held string) *models.Mock {
		m := receiveOf(name, held, oAt.Add(-time.Minute))
		m.Spec.ResTimestampMock = oAt.Add(-time.Minute + time.Second)
		return m
	}
	two := func(name, held string) GivenPlaces {
		return givenThrough(t, set(name, held), toldView("v1", set(name, noneF)), toldView("v2", set(name, noneR)))
	}
	gs := []GivenPlaces{two("mock-31", holdsR), two("mock-32", holdsR2), two("mock-33", holdsR), two("mock-34", holdsR2)}
	want := "the recording's own piece of the id stands at mock-31, which round F gave the application with no view keploy reads: round F's making its own piece shows nothing of whether the application cut it from there"
	for i := 0; i < 40; i++ {
		in := oInput(path, rec, live, nil, nil)
		in.Given = func() ([]GivenPlaces, error) { return gs, nil }
		d := oWord(t, Decide(in), r, f)
		if d.Refusal == nil || d.Refusal.Why != want {
			t.Fatalf("decision %d names another place: %s; want %q", i, oDescribe([]Decision{d}), want)
		}
	}
	// Round F's group in a view of each of several set recordings asked
	// after the call, which the recording did not give the application
	// before it: the line names the first of them too.
	holdsF, holdsF2 := "9e9e9e9e-77aa-4b8a-b2c3-d4e5f6071829", "1d1d1d1d-2e2e-4b8a-a3a3-d4d4d4d4d4d4"
	after := func(name, held string) GivenPlaces {
		m := receiveOf(name, noneR, oAt.Add(time.Second))
		m.Spec.ResTimestampMock = oAt.Add(2 * time.Second)
		return givenThrough(t, m, toldView("v1", set(name, held)))
	}
	gs = []GivenPlaces{after("mock-31", holdsF), after("mock-32", holdsF2), after("mock-33", holdsF), after("mock-34", holdsF2)}
	want = "round F's own piece of the id stands at mock-31, which the recording did not give the application before the call, by a time keploy reads: the recording's making its own piece shows nothing of whether the application cut it from there"
	for i := 0; i < 40; i++ {
		in := oInput(path, rec, live, nil, nil)
		in.Given = func() ([]GivenPlaces, error) { return gs, nil }
		d := oWord(t, Decide(in), r, f)
		if d.Refusal == nil || d.Refusal.Why != want {
			t.Fatalf("round F's holders, decision %d names another place: %s; want %q", i, oDescribe([]Decision{d}), want)
		}
	}
}

// Where each run holds its own piece at a place it had, the line says only
// that (bothRunsWhy), even where one of the recording's holders is a place
// round F was not given before the call: that one run made its piece is no
// longer the reason.
func TestOutcomeBothRunsHoldingSaysSoOverOneRunsHolder(t *testing.T) {
	const (
		path           = "body.docs.msgs.0._id"
		rec, live      = "ed35a78f-905c-48d1-bc3d-a07311678c73", "3f0c9a12-7e6d-4b8a-9e21-5c6d7e8f9a0b"
		r, f           = "48d1", "4b8a"
		field          = "body.cursor.firstBatch.0._id"
		holdsR, holdsF = "b0fd3b58-3ff5-48d1-a1b2-c3d4e5f60718", "9e9e9e9e-77aa-4b8a-b2c3-d4e5f6071829"
		noneR, noneF   = "aaaa1111-2222-4c00-8333-444455556666", "c1ae4c69-77aa-4c00-b2c3-d4e5f6071829"
	)
	late := oServed("mock-51", false, false, Response, oText(field, holdsR), noneF, time.Second)
	late[0].Payload.servedAfter = true
	e := append(late, oServed("mock-52", false, false, Response, oText(field, noneR), holdsF, time.Second)...)
	d := oWord(t, Decide(oInput(path, rec, live, e, nil)), r, f)
	oRefused(t, "the recorded group at a place round F served only after the call, round F's at a place answered before it", d, RefusalUnknown, "mock-51", "mock-52")
	oNoChanceLine(t, d, "mock-51 "+field)
}

// Where the place a refusal names first is unknown for a reason of its own
// (here, a request sent at the call's own instant that holds the recorded
// group whole), the line says that reason of it as of any place, not the
// reason a later place took from oneRun.
func TestOutcomeTheFirstPlacesOwnReasonLeadsTheLine(t *testing.T) {
	const (
		path      = "body.docs.msgs.0._id"
		rec, live = "ed35a78f-905c-48d1-bc3d-a07311678c73", "3f0c9a12-7e6d-4b8a-9e21-5c6d7e8f9a0b"
		r, f      = "48d1", "4b8a"
		field     = "body.cursor.firstBatch.0._id"
		holdsR    = "b0fd3b58-3ff5-48d1-a1b2-c3d4e5f60718"
		noneF     = "c1ae4c69-77aa-4c00-b2c3-d4e5f6071829"
		fNotGiven = ", which round F did not give the application before the call: round F's making its own piece shows nothing of whether the application cut it from there"
	)
	whole := oServed("mock-50", false, false, Request, oText(field, r), "c0de", 0)
	whole[0].At = oAt
	late := oServed("mock-51", false, false, Response, oText(field, holdsR), noneF, time.Second)
	late[0].Payload.servedAfter = true
	d := oWord(t, Decide(oInput(path, rec, live, append(whole, late...), nil)), r, f)
	oRefused(t, "the recorded group whole in a request sent at the call's instant, and in an id at a place round F served only after it", d, RefusalUnknown, "mock-50", "mock-51")
	if d.Refusal == nil || !strings.HasPrefix(d.Refusal.Why, "keploy cannot tell what round F gave at mock-50 "+field+" before this call (") || strings.Contains(d.Line, fNotGiven) {
		t.Errorf("the line must lead with mock-50's own reason: %q", d.Line)
	}
	alone := oWord(t, Decide(oInput(path, rec, live, late, nil)), r, f)
	oOneSideLine(t, alone, "the recording's own piece of the id stands at mock-51 "+field+fNotGiven)
}

// A place unknown for a piece of one id the call minted that the per-run
// rule (oneRun) leaves no chance, for one run's reason, stays a candidate
// whose serve order counts, as any place left unknown does: mock-50 holds
// the recorded group whole and round F gave another word there (set aside
// alone), and mock-45 holds the recorded group inside another id at a
// place round F served only after the call. The recording gave mock-50
// first and round F mock-45 first, so keploy cannot tell which the
// application took the word from: mock-50 is named unknown, not set aside.
func TestOutcomeAOneRunPlaceStaysAServeOrderCandidate(t *testing.T) {
	const (
		rec, live = "1111aaaa-2222-48d1-9444-444455556666", "9999cccc-7777-4a2e-8888-000011112222"
		r, f      = "48d1", "4a2e"
		path      = "body.docs.msgs.0.appId"
	)
	aside := oCarried("mock-50", "body.ref", "ref 48d1 x", "ref zzzz x", 2*time.Second, time.Second, 2)
	late := oCarried("mock-45", "body.note", "owner bb7ed9ee-670e-48d1-982f-b54b2b73982f", "owner 0c1d2e3f-4a5b-4c7d-8e9f-0b1c2d3e4f50", 900*time.Millisecond, 100*time.Millisecond, 1)
	late.Payload.servedAfter = true
	d := oWord(t, Decide(oInput(path, rec, live, []Earlier{aside, late}, nil)), r, f)
	oRefused(t, "the recorded group at a place round F served only after the call, given in another order than mock-50", d, RefusalUnknown, "mock-50", "mock-45")
	if d.Refusal == nil || !strings.HasPrefix(d.Refusal.Why, "keploy cannot tell what round F gave at mock-50 body.ref before this call (another order: the 1st earlier answer was mock-50 in the recording and mock-45") {
		t.Errorf("the line must lead with mock-50, which the runs gave in another order than mock-45: %q", d.Line)
	}
	// Alone, mock-45 is refused for the per-run rule's reason.
	alone := oWord(t, Decide(oInput(path, rec, live, []Earlier{late}, nil)), r, f)
	oOneSideLine(t, alone, "the recording's own piece of the id stands at mock-45 body.note, which round F did not give the application before the call: round F's making its own piece shows nothing of whether the application cut it from there")
}

// A cluster's access key whose words happen to pair in the two runs (runs
// m1c-x2, m1p-p1 and fsm2a-a2: the record bound access_key#0 and #1 at
// mock-1236, 1511 and 1567, and the judges then missed the clusters insert
// in exactly those runs, "with its variables' recorded values put back, the
// call is not the recording"): a key with no '+' or '/' in either run, or
// one '+' in each at another spot, is still one random value, never two
// words learned apart, since the next run's '+' and '/' fall elsewhere. The
// keys are made up in the runs' shape (44 characters of standard base64,
// one '='), no run's key copied. Two alphabets mixed, another length and a
// run that decodes to text, words paired, stay no such key.
func TestOutcomeAStandardBase64KeyWhoseWordsPairIsOneValue(t *testing.T) {
	for _, tc := range []struct{ name, r, f string }{
		{"no '+' or '/' in either run (x2 mock-1511's shape)", "aK7mQ2xZ9pWr4Tn8Vb3Lc6Hd1Sg5Jf0Ey2Ro9Ui7Mq4=", "Zq81Lm0XpVb4Nc7Hd2Sg6Jf3Ey9Ro5Ui1Tn8Kw0Pa2B="},
		{"one '+' in each run, at another spot (x2 mock-1567's shape)", "aK7mQ2xZ9pWr4Tn8Vb3Lc6Hd1Sg5Jf0Ey2Ro9U+7Mq4=", "Zq81Lm0+pVb4Nc7Hd2Sg6Jf3Ey9Ro5Ui1Tn8Kw0Pa2B="},
		{"a '+' and a '/' in each run, at the same spots", "aK7mQ2xZ9p+r4Tn8Vb3Lc6/d1Sg5Jf0Ey2Ro9Ui7Mq4=", "Zq81Lm0Xp+b4Nc7Hd2Sg6J/3Ey9Ro5Ui1Tn8Kw0Pa2B="},
	} {
		if len(tc.r) != 44 || len(tc.f) != 44 {
			t.Fatalf("%s: keys of %d and %d characters; want 44", tc.name, len(tc.r), len(tc.f))
		}
		ds := Decide(oInput("body.docs.msgs.0.access_key", tc.r, tc.f, nil, nil))
		if len(ds) != 1 || ds[0].Outcome != OutcomeFresh || ds[0].At.Word != -1 || ds[0].Recorded != tc.r || ds[0].Full != tc.f {
			t.Errorf("%s: %s; want one fresh value, the field whole", tc.name, oDescribe(ds))
		}
		ds = Decide(oInput("body.cmd.filter.access_key", tc.r, tc.f, oSent(tc.r, tc.f), nil))
		if len(ds) != 1 || ds[0].Outcome != OutcomeValue || ds[0].From.Mock != "mock-0" || ds[0].At.Word != -1 {
			t.Errorf("%s, a later send of the key: %s; want a Value bound at the first send, the field whole", tc.name, oDescribe(ds))
		}
	}
	text := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	for _, tc := range []struct{ name, r, f string }{
		{"two alphabets mixed, words paired", "aK7mQ2xZ9p+r4Tn8Vb3Lc6_d1Sg5Jf0Ey2Ro9Ui7Mq4=", "Zq81Lm0Xp+b4Nc7Hd2Sg6J_3Ey9Ro5Ui1Tn8Kw0Pa2B="},
		{"another length, no '+' or '/'", "aK7mQ2xZ9pWr4Tn8Vb3Lc6Hd1Sg5Jf0Ey2Ro9Ui7Mq4=", "Zq81Lm0XpVb4Nc7Hd2Sg6Jf3Ey9Ro5Ui1Tn8Kw0Pa2BxYz4="},
		{"a text in standard base64, no '+' or '/'", text(`{"name":"Ivan Petrov","n":1}`), text(`{"name":"Petr Sidrov","n":2}`)},
	} {
		if strings.ContainsAny(tc.r+tc.f, "+/") && !strings.Contains(tc.name, "mixed") {
			t.Fatalf("%s: the row's values hold '+' or '/'", tc.name)
		}
		ds := Decide(oInput("body.docs.msgs.0.access_key", tc.r, tc.f, nil, nil))
		if slices.ContainsFunc(ds, func(d Decision) bool { return d.Outcome == OutcomeFresh && d.At.Word < 0 }) {
			t.Errorf("FALSE GREEN? %s: %s; want no one fresh value", tc.name, oDescribe(ds))
		}
	}
}
