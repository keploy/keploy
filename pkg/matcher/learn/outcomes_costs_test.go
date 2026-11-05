package learn

import (
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.mongodb.org/mongo-driver/v2/x/mongo/driver/wiremessage"
)

// Rows for what the one entry costs against main's learning: each row is a
// row of main's tests, read on the same inputs, cited by file:line. Some
// flip on purpose (an unchanged place holding one of the two words is
// chance; a start-up sibling's second is the call's own read); some stay as
// main pins them; some are refused where main binds (stricter, a red only).
// Rows main pins that the evidence, instant and word files already hold are
// not repeated: follows_test.go:165 (outcomes_evidence_test.go), :462-487
// and :605-665 (outcomes_word_test.go), client_bind_test.go:23-81
// (outcomes_instant_test.go).

// costAt is the instant of the request-case rows (given_test.go's at).
var costAt = time.Date(2026, 10, 22, 11, 13, 40, 0, time.UTC)

// costCall is main's ClassifyIn/ClassifyFollowing call at path (a string
// leaf), recorded rec and seen in F as live, as Decide's input.
func costCall(dc *DriftCache, path, rec, live string, at time.Time, earlier []Earlier, raw []Raw, resp []Leaf, w *Window) Input {
	leaf := func(v string) Shape {
		return Shape{Leaves: map[string]Leaf{path: {Path: path, Noise: path, Type: String, Value: v}}}
	}
	return Input{
		Drifts: dc, Drift: []string{path},
		Live: leaf(live), Rec: leaf(rec),
		At: at, Arrived: at, Earlier: earlier, Raw: raw, Resp: resp, Window: w,
		Test: "test-1", Mock: "mock-300",
		Caps: Caps{BindAtWord: true, AnswerAtWord: true, FViewStated: true},
	}
}

// costF is classifyF's call (follows_test.go:65) as Decide's input.
func costF(recorded, live string, earlier []Earlier, raw []Raw, resp []Leaf) Input {
	return costCall(clockedDrifts(), "bind.$8", recorded, live, eRecAt, earlier, raw, resp, nil)
}

// costReq is the test case's request whose body is body, sent as recorded.
func costReq(body string) []Earlier {
	return []Earlier{GivenRequest(&models.TestRequest{Test: "test-1", Parts: []models.TestRequestPart{jsonType, {Name: "body", Recorded: body, Sent: body}}})}
}

// costFresh checks d is a fresh Value.
func costFresh(t *testing.T, what string, d Decision) {
	t.Helper()
	if d.Outcome != OutcomeFresh {
		t.Errorf("%s: %s; want fresh, bound at its first call", what, oDescribe([]Decision{d}))
	}
}

// costRefused checks d is refused (as one of kinds, where any is named),
// its line naming every one of names.
func costRefused(t *testing.T, what string, d Decision, kinds []RefusalKind, names ...string) {
	t.Helper()
	if d.Outcome != OutcomeRefused || d.Refusal == nil {
		t.Errorf("FALSE GREEN? %s: %s; want refused", what, oDescribe([]Decision{d}))
		return
	}
	if len(kinds) > 0 {
		ok := false
		for _, k := range kinds {
			ok = ok || d.Refusal.Kind == k
		}
		if !ok {
			t.Errorf("%s: %s; want refused as one of %q", what, oDescribe([]Decision{d}), kinds)
			return
		}
	}
	oRefused(t, what, d, d.Refusal.Kind, names...)
}

var (
	costUnknown  = []RefusalKind{"unknown"}
	costConflict = []RefusalKind{"conflict"}
)

// An unchanged place that holds only one of the two words is chance where
// the word is short (under 32 bits): the word is made anew each run, fresh
// (on purpose, unlike main at given_test.go:195, :228, run_test.go:751).
// One of 32 bits or more there is a conflict, and a place that holds both
// words leaves it unknown (pinned: given_test.go:192, :193, :194, :223).
func TestOutcomeCostAnUnchangedPlaceHoldingOneShortWordIsChance(t *testing.T) {
	const p = "body.slot"
	for _, c := range []struct{ what, body, rec, live string }{
		{"open to closed beside keys open and close (given_test.go:195)", `{"open":"2026-10-22T09:00:00Z","close":"2026-10-22T17:00:00Z"}`, "open", "closed"},
	} {
		in := costCall(clockedDrifts(), p, c.rec, c.live, costAt, costReq(c.body), nil, nil, nil)
		costFresh(t, c.what, oWord(t, Decide(in), c.rec, c.live))
	}
	tenant := []Earlier{GivenRequest(&models.TestRequest{Test: "test-1", Parts: []models.TestRequestPart{{Name: "header.X-Tenant-acme7", Recorded: "1", Sent: "1"}}})}
	costFresh(t, "tenant-acme7 to tenant-beta2 beside a header named X-Tenant-acme7 (given_test.go:228)",
		oWord(t, Decide(costCall(clockedDrifts(), p, "tenant-acme7", "tenant-beta2", costAt, tenant, nil, nil, nil)), "acme7", "beta2"))

	sevenSent := []Earlier{{Mock: "mock-1", At: costAt.Add(-time.Second), Run: true, StartUp: true, Payload: oPayload("mock-1", costAt.Add(-time.Second)), Seen: []Seen{{Side: Request,
		Leaf: Leaf{Path: "body.retries", Noise: "body.retries", Type: String, Value: "7"}, Live: "7", Place: "body.retries"}}}}
	costFresh(t, "82 to 7 beside a 7 the start-up sent unchanged (run_test.go:751)",
		oWord(t, Decide(costCall(clockedDrifts(), "body.count", "82", "7", costAt, sevenSent, nil, nil, nil)), "82", "7"))

	// A seven-letter word counts by its text, so the request's unchanged
	// leaf that held it disproves it as a conflict; a short id is unknown.
	// Either way it stays refused.
	for _, c := range []struct {
		what, body, rec, live string
		kinds                 []RefusalKind
	}{
		{"morning to evening, the request lists both (given_test.go:192)", `{"shifts":["morning","evening"]}`, "morning", "evening", []RefusalKind{"unknown", "conflict"}},
		{"s17 to s18, the request lists both (given_test.go:193)", `{"slots":[{"id":"s17","at":"11:14"},{"id":"s18","at":"11:15"}]}`, "s17", "s18", costUnknown},
		{"17 to 18, the request lists both (given_test.go:194)", `{"slots":[{"id":17,"at":"11:14"},{"id":18,"at":"11:15"}]}`, "17", "18", costUnknown},
	} {
		in := costCall(clockedDrifts(), p, c.rec, c.live, costAt, costReq(c.body), nil, nil, nil)
		costRefused(t, c.what, oWord(t, Decide(in), c.rec, c.live), c.kinds, "test-1")
	}
	query := []Earlier{GivenRequest(&models.TestRequest{Test: "test-1", Parts: []models.TestRequestPart{{Name: "url", Recorded: "http://h/book?shift=morning", Sent: "http://h/book?shift=morning"}}})}
	costRefused(t, "morning to evening, the query sent morning unchanged: 33 bits (given_test.go:223)",
		oWord(t, Decide(costCall(clockedDrifts(), p, "morning", "evening", costAt, query, nil, nil, nil)), "morning", "evening"), costConflict, "url.query.shift")

	// Leaves an earlier exchange served unchanged, one holding the changed
	// word (unlike main on purpose, learn_test.go:1816 "served leaves"): four hex
	// letters there are chance, fresh; eight are refused, naming the leaf.
	served := func(a, b string) []Earlier {
		return []Earlier{{Mock: "mock-1", At: costAt.Add(-time.Second), Payload: oPayload("mock-1", costAt.Add(-time.Second)), Seen: []Seen{
			{Side: Response, Leaf: Leaf{Path: "body.a", Noise: "body.a", Type: String, Value: a}, Live: a},
			{Side: Response, Leaf: Leaf{Path: "body.b", Noise: "body.b", Type: String, Value: b}, Live: b},
		}}}
	}
	costFresh(t, "aaa1 to bbb2 beside a leaf served aaa1 unchanged (learn_test.go:1816)",
		oWord(t, Decide(costCall(clockedDrifts(), "body.ref", "x-aaa1-ccc3", "x-bbb2-ddd4", costAt, served("ccc3", "aaa1"), nil, nil, nil)), "aaa1", "bbb2"))
	costRefused(t, "aaaaaaa1 to bbbbbbb2 beside a leaf served aaaaaaa1 unchanged",
		oWord(t, Decide(costCall(clockedDrifts(), "body.ref", "x-aaaaaaa1-ccccccc3", "x-bbbbbbb2-ddddddd4", costAt, served("ccccccc3", "aaaaaaa1"), nil, nil, nil)), "aaaaaaa1", "bbbbbbb2"),
		costConflict, "mock-1", "body.b")
}

// Flyway's execution_time recorded 50 beside a CREATE TABLE F sent byte for
// byte (VARCHAR(50)), 7 in F, the call's recorded answer echoing 50: the
// unchanged place holds one short word, chance, so the value is fresh, its
// echo named where the adapter names its answer's cells (Input.PlaceOf);
// refused where it names none or cannot answer a word (unlike main on purpose,
// follows_test.go:407-411).
func TestOutcomeCostAValueItsAnswerEchoesBesideAChancePlaceIsFresh(t *testing.T) {
	raw := append(eStartup(), eHistory(eRecAt.Add(-150*time.Millisecond), 6, true))
	resp := []Leaf{{Path: "rows[0][0]", Type: String, Value: "50"}}
	cells := func(l Leaf, answer bool) (string, bool) {
		if !answer {
			return "", false
		}
		return VarPlace(Leaf{Path: "body." + l.Path}, true)
	}
	in := costF("50", "7", nil, raw, resp)
	in.PlaceOf = cells
	d := oWord(t, Decide(in), "50", "7")
	costFresh(t, "50 echoed in its answer beside VARCHAR(50) sent unchanged", d)
	if len(d.Echo) != 1 || d.Echo[0].Place.Part == 0 {
		t.Errorf("50 echoed in its answer: %s, Echo %v; want the answer's place named", oDescribe([]Decision{d}), d.Echo)
	} else if want := "answered at " + d.Echo[0].String() + ", where the call's answer holds \"50\" whole as recorded"; !strings.Contains(d.Line, want) {
		// The answer rewritten at replay is learned too: its line says where.
		t.Errorf("50 echoed in its answer: line %q; want it to name the answer place (%q)", d.Line, want)
	}
	costRefused(t, "50 echoed at an answer place its adapter names not", oWord(t, Decide(costF("50", "7", nil, raw, resp)), "50", "7"), []RefusalKind{"cannot-check"})
	in.Caps.AnswerAtWord = false
	costRefused(t, "50 echoed in its answer, an adapter that cannot answer a word", oWord(t, Decide(in), "50", "7"), []RefusalKind{"cannot-check"})
}

// Pinned by the evidence table (no flip): a word both values hold at a place
// (follows_test.go:178), a word only F's view of a changed place holds
// (:300), a leaf F gave nothing new at (:324), and a candidate F did not
// serve between two it served in order (:479).
func TestOutcomeCostTheEvidenceTablesPinnedRowsStayRefused(t *testing.T) {
	before := eRecAt.Add(-50 * time.Millisecond)
	d := oWord(t, Decide(costF("82 1", "4 1 1", nil, eStartup(), nil)), "82", "4")
	costRefused(t, "a word both values hold (follows_test.go:178)", d, []RefusalKind{"unknown", "changed-shape"})
	d = oWord(t, Decide(costF("82", "6c7d", nil, jobsServed(chanceSrcRec, "5", eJobs(chanceSrcLive, "5")), nil)), "82", "6c7d")
	costRefused(t, "only F's view holds the word the value holds now (follows_test.go:300)", d, costUnknown, "mock-6")
	d = oWord(t, Decide(costF("took 82 ms", "took 5 ms", []Earlier{eLeaf(before, 6, "82 7", "7")}, eStartup(), nil)), "82", "5")
	costRefused(t, "a leaf F gave nothing new at (follows_test.go:324)", d, costUnknown, "mock-8")
	a := eAnswer("mock-a", 300*time.Millisecond, "7")
	b := eAnswer("mock-b", 200*time.Millisecond, "82")
	raw := []Raw{a.ServedAs(1).ServedAsRecorded(), eAnswer("mock-c", 250*time.Millisecond, "7"), b.ServedAs(2).ServedAsRecorded()}
	d = oWord(t, Decide(costF("slot 82", "slot 7", nil, raw, nil)), "82", "7")
	costRefused(t, "a 7 the recording gave between them, F did not serve it (follows_test.go:479)", d, costUnknown, "mock-c")
}

// Refused where main binds (stricter, a red only): a copy of a place F did
// not serve, served changed with no view, or that holds both values
// (follows_test.go:388-390); a value that took part of a longer word F gave
// (:318); a row holding both 50 and a/b (:401-405). A place recorded after
// the call (:391) gave R's application nothing before it, so R's 50 there
// is no source, and F's text there holds no 7: the value is made anew each
// run, where main binds it to that place. One asked a millisecond before
// the call and answered at its own instant (:392) holds 50 in its request,
// which the application sent before the call: it is placed by when it was
// sent, and round F sent it again as recorded while the value took 7, so
// the value did not follow it: made anew, as where it was answered before.
func TestOutcomeCostACopyOfAPlaceNotBeforeInBothRunsIsRefused(t *testing.T) {
	before := eRecAt.Add(-150 * time.Millisecond)
	startup := func(x Raw) []Raw { return append(eStartup(), x) }
	for _, c := range []struct {
		what, live string
		raw        []Raw
	}{
		{"F did not serve the CREATE TABLE (follows_test.go:388)", "7", startup(eHistory(before, 0, false))},
		{"F served the CREATE TABLE changed, no view (:389)", "7", startup(eHistory(before, 6, false))},
		{"the CREATE TABLE holds F's value too (:390)", "200", startup(eHistory(before, 6, true))},
	} {
		costRefused(t, c.what, oWord(t, Decide(costF("50", c.live, nil, c.raw, nil)), "50", c.live), costUnknown, "mock-5")
	}
	for _, c := range []struct {
		what string
		at   time.Time
	}{
		{"the CREATE TABLE was recorded after the call (:391)", eRecAt.Add(10 * time.Millisecond)},
		{"the CREATE TABLE was asked a millisecond before the call and answered at its own instant (:392)", eRecAt.Add(-time.Millisecond)},
	} {
		if d := oWord(t, Decide(costF("50", "7", nil, startup(eHistory(c.at, 6, true)), nil)), "50", "7"); d.Outcome != OutcomeFresh {
			t.Errorf("%s: %s; want a value made anew", c.what, oDescribe([]Decision{d}))
		}
	}
	d := oWord(t, Decide(costF("k-48d1", "k-6c7d", nil, append(eStartup(), eRow("48d1 aaaa", "x", "y").ServedAs(6).ServedLive(eRow("6c7daaaa", "x", "y"))), nil)), "48d1", "6c7d")
	costRefused(t, "the value took a part of a longer word F gave (follows_test.go:318)", d, costUnknown, "mock-6")
	src := ePg("mock-s", 150*time.Millisecond, "SELECT a, b FROM t", []models.PostgresV3Cells{{{Value: "50"}, {Value: "x a/b y"}}}, "SELECT 1").ServedAs(6).ServedAsRecorded()
	for _, live := range []string{"a/b", "a%2Fb"} {
		d := oWord(t, Decide(costF("50", live, nil, append(eStartup(), src), nil)), "50", live)
		costRefused(t, "50 in mock-s, which holds F's "+live+" too (follows_test.go:401-405)", d, []RefusalKind{"unknown", "changed-shape"}, "mock-s")
	}
}

// The value made anew at follows_test.go:392 (main binds it to the CREATE
// TABLE's VARCHAR(50)) is looser than main only at its first send: it is
// bound there, and every later use is checked against it. A later call of
// the test that sends the same value again is a Value of the first call, so
// a replay whose later value is not what its first call sent goes red; and
// where round F's later value is not what its first call sent, it is
// refused, never made anew.
func TestOutcomeCostAValueMadeAnewBesideALiteralIsCheckedAtItsLaterUse(t *testing.T) {
	first := eRecAt
	hist := func() []Raw { return append(eStartup(), eHistory(first.Add(-time.Millisecond), 6, true)) }
	costFresh(t, "50 beside VARCHAR(50), asked a millisecond before the call and answered at its own instant, 7 in F (follows_test.go:392)",
		oWord(t, Decide(costF("50", "7", nil, hist(), nil)), "50", "7"))
	// The first call, as a later call of the test sees it: it sent 50, and 7
	// in round F.
	sent := []Earlier{{Mock: "mock-300", At: first, Own: true,
		Payload: ePgAnswered("mock-300", 0, first.Add(time.Millisecond), "INSERT INTO flyway_schema_history (execution_time) VALUES ($8)", nil, "INSERT 0 1").ServedAs(7),
		Seen:    []Seen{{Side: Request, Leaf: Leaf{Path: "bind.$8", Noise: "bind.$8", Type: String, Value: "50"}, Live: "7", Place: "bind.$8"}}}}
	later := func(live string) Decision {
		in := costCall(clockedDrifts(), "bind.$3", "50", live, first.Add(time.Second), sent, hist(), nil, nil)
		in.Mock = "mock-301"
		return oWord(t, Decide(in), "50", live)
	}
	iValue(t, "a later call sends the value again", later("7"), "mock-300")
	costRefused(t, "round F's later value is not what its first call sent", later("9"), nil, "mock-300")
}

// costFind is the branch janitor's find on branches (startup_same_second_
// test.go:46-58): its request holds sent, its answer answer, recorded
// before before eRecAt and answered at eRecAt+answered.
func costFind(sent, answer string, before, answered time.Duration) Raw {
	return RawOf(&models.Mock{Name: "mock-17", Kind: models.Mongo, Spec: models.MockSpec{
		MongoRequests: []models.MongoRequest{{Header: &models.MongoHeader{Opcode: wiremessage.OpMsg}, Message: &models.MongoOpMessage{Sections: []string{
			`{ SectionSingle msg: {"find":"branches","filter":{"status":{"$in":["merged","closed"]},"expires_at":{"$lte":{"$numberLong":"` + sent + `"},"$exists":true}},"$db":"api-server"} }`,
		}}}},
		MongoResponses: []models.MongoResponse{{Header: &models.MongoHeader{Opcode: wiremessage.OpMsg}, Message: &models.MongoOpMessage{Sections: []string{
			`{ SectionSingle msg: {"cursor":{"firstBatch":[` + answer + `],"id":{"$numberLong":"0"},"ns":"api-server.branches"},"ok":{"$numberDouble":"1.0"}} }`,
		}}}},
		ReqTimestampMock: eRecAt.Add(-before), ResTimestampMock: eRecAt.Add(answered),
	}}, false)
}

// api-server's start-up janitors read time.Now().Unix() on two connections
// in one second. The notifications janitor's second, beside the branch
// janitor's find of the same second: a Clock its own window holds, never a
// Value of the find (pinned: startup_same_second_test.go:33-102, whether F
// had served the find or not, the find in flight or answered before; the
// flips at :134, :136 (on purpose) read F's served find the same way, one cut
// looser than main). Where the find's answer gave the instant in flight
// (:135), step 3 does not take it: a Clock or refused. A client test whose
// window is an hour from F's second (:159-162): the begin's Clock or
// refused, never a Value.
func TestOutcomeCostAStartUpSiblingsSecondIsTheCallsOwnRead(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	const path = "body.cmd.query.createdAt.$lt.$numberLong"
	recT := strconv.FormatInt(eRecAt.Unix(), 10)
	fAt := eRecAt.Add(FullRoundLead)
	liveT := strconv.FormatInt(fAt.Unix(), 10)
	w := &Window{Rec: Span{From: eRecAt.Add(-time.Millisecond), To: eRecAt}, F: Span{From: fAt.Add(-time.Millisecond), To: fAt.Add(time.Millisecond)}}
	call := func(dc *DriftCache, raw []Raw, w *Window) Decision {
		in := costCall(dc, path, recT, liveT, eRecAt, nil, raw, nil, w)
		in.Arrived = fAt
		return oWord(t, Decide(in), recT, liveT)
	}
	notValue := func(what string, d Decision) {
		t.Helper()
		if d.Outcome == OutcomeValue || d.Outcome == OutcomeFresh || d.From.Mock != "" {
			t.Errorf("FALSE GREEN? %s: %s; want a Clock or refused, never bound", what, oDescribe([]Decision{d}))
		}
	}
	for _, mode := range []struct {
		name string
		dc   func() *DriftCache
	}{{"client test", clientTestCache}, {"keploy test", clockedDrifts}} {
		for _, shape := range []struct {
			what     string
			answered time.Duration
		}{{"in flight together", 400 * time.Microsecond}, {"answered before the call", -100 * time.Microsecond}} {
			d := call(mode.dc(), []Raw{costFind(recT, "", 600*time.Microsecond, shape.answered)}, w)
			iClock(t, mode.name+", "+shape.what+": beside a find F had not served (startup_same_second_test.go:33-102)", d)
			notValue(mode.name+", "+shape.what, d)
		}
		iClock(t, mode.name+": F served the find, no view stated (:134)", call(mode.dc(), []Raw{costFind(recT, "", 600*time.Microsecond, 400*time.Microsecond).ServedAs(1)}, w))
		// Answered 100us before the call, the find's request is still a send
		// of the same second, never what the find gave: the timing of its
		// answer moves nothing.
		iClock(t, mode.name+": F served the find, answered before the call", call(mode.dc(), []Raw{costFind(recT, "", 600*time.Microsecond, -100*time.Microsecond).ServedAs(1)}, w))
		iClock(t, mode.name+": the find's text holds F's second too (:136)", call(mode.dc(), []Raw{costFind(recT, `{"n":{"$numberLong":"`+liveT+`"}}`, 600*time.Microsecond, 400*time.Microsecond)}, w))
		notValue(mode.name+": the find's answer gave the instant in flight (:135)", call(mode.dc(), []Raw{costFind(recT, `{"expires_at":{"$numberLong":"`+recT+`"}}`, 600*time.Microsecond, 400*time.Microsecond)}, w))
	}
	far := &Window{Rec: w.Rec, F: Span{From: fAt.Add(time.Hour), To: fAt.Add(time.Hour + time.Millisecond)}}
	notValue("client test, the call's window an hour from F's second (:159-162)", call(clientTestCache(), []Raw{costFind(recT, "", 600*time.Microsecond, 400*time.Microsecond)}, far))
}

// Pinned: an instant read in its own window whose recorded text stands
// whole in a connection's start-up keploy may swap (a server parameter set
// per pool) is refused, naming the start-up (copy_or_read_test.go:352): the
// moment stands at an answer whose state in F keploy cannot tell.
func TestOutcomeCostAReadWholeInASwappableStartUpIsRefused(t *testing.T) {
	const p = "body.auditedAt"
	read := eRecAt.Add(-100 * time.Millisecond)
	fAt := eRecAt.Add(69700 * time.Millisecond)
	secs := func(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05Z") }
	a, b := eLaneSession("mock-hs-81", 2*time.Minute, 81), eLaneSession("mock-hs-82", 110*time.Second, 82)
	a.Spec.PostgresV3.Session.ParameterStatus["application_name"] = secs(read)
	b.Spec.PostgresV3.Session.ParameterStatus["application_name"] = "hikari"
	raw := []Raw{RawOf(a, false).ServedAs(1).ServedAsRecorded(), RawOf(b, false).ServedAs(2).ServedAsRecorded()}
	in := costCall(clientTestCache(), p, secs(read), secs(fAt.Add(-100*time.Millisecond)), eRecAt, nil, raw, nil, iWin(eRecAt, fAt, 200*time.Millisecond))
	in.Arrived = fAt
	d := oWord(t, Decide(in), secs(read), secs(fAt.Add(-100*time.Millisecond)))
	costRefused(t, "a read whole in a start-up keploy may swap", d, []RefusalKind{"pick"}, "mock-hs-81")
}

// Pinned: one slot names both moments by two rules (given_test.go:701):
// a minute read in its window beside it is refused, naming the slot.
func TestOutcomeCostASlotNamingBothMomentsIsAPick(t *testing.T) {
	w := &Window{Rec: Span{From: costAt.Add(-time.Minute), To: costAt.Add(time.Minute)}, F: Span{From: costAt.Add(-time.Minute), To: costAt.Add(3 * time.Minute)}}
	in := costCall(clockedDrifts(), "body.slot", "11:14", "11:15", costAt, []Earlier{GivenRequest(slotRequest("11:14:40.120"))}, nil, nil, w)
	costRefused(t, "a minute read beside a slot of both moments", oWord(t, Decide(in), "11:14", "11:15"), []RefusalKind{"pick"}, "body.slots[0]")
}

// A time whose day stands in the test case's request as recorded, where round
// F's time is a day on: the request names the recorded moment by a coarse
// cut and none of round F's, so it may be a pick the request no longer
// lists. Refused naming the slot, never a read of the clock its window
// holds. It is a pick with no drift cache too: the order is one rule,
// whatever the caller passes.
func TestOutcomeCostAPickFromTheRequestIsRefusedWithOrWithoutACache(t *testing.T) {
	const rec, live = "2026-10-22T11:14:40Z", "2026-10-23T09:00:10Z"
	fAt := time.Date(2026, 10, 23, 9, 0, 20, 0, time.UTC)
	w := &Window{Rec: Span{From: costAt.Add(-time.Minute), To: costAt.Add(2 * time.Minute)}, F: Span{From: fAt.Add(-time.Minute), To: fAt}}
	for name, dc := range map[string]*DriftCache{"a cache": clockedDrifts(), "no cache": nil} {
		in := costCall(dc, "body.at", rec, live, costAt, []Earlier{GivenRequest(slotRequest("2026-10-22"))}, nil, nil, w)
		costRefused(t, name, oWord(t, Decide(in), rec, live), []RefusalKind{"pick"}, "body.slots[0]")
	}
}

// Pinned: where the test case's request could not be read (none
// sent, or of a kind keploy reads no request of), every change is refused
// as unread, an instant or not, echoed in its answer or not
// (given_test.go:203-207, :240-260; typed_test.go:2088-2091). A clock plus
// an offset the call's recorded answer holds is refused (typed_test.go:
// 2083-2087): keploy would serve the answer's recorded moment.
func TestOutcomeCostAnUnreadRequestAndAnAnswersOffsetAreRefused(t *testing.T) {
	const p = "body.slot"
	unread := map[string][]Earlier{
		"none sent": {GivenRequest(nil)},
		"unread":    {GivenRequest(&models.TestRequest{Test: "test-1", Unread: "keploy reads no request of a test case of kind X"})},
	}
	for name, earlier := range unread {
		for _, f := range [][2]string{{"11:14", "11:15"}, {"2026-10-22T11:14:00Z", "2026-10-22T11:15:01Z"}, {"at 1792667640", "at 1792667701"}, {"req-a7c4e2d10b3f", "req-3f2b8c1e9a4d"}} {
			for _, echoed := range []bool{false, true} {
				var resp []Leaf
				if echoed {
					resp = []Leaf{{Path: p, Noise: p, Type: String, Value: f[0]}}
				}
				ds := Decide(costCall(clockedDrifts(), p, f[0], f[1], costAt, earlier, nil, resp, nil))
				if len(ds) == 0 {
					t.Errorf("FALSE GREEN? %s, %q -> %q: no decision; want refused as unread", name, f[0], f[1])
				}
				for _, d := range ds {
					costRefused(t, name+", "+f[0]+" -> "+f[1], d, []RefusalKind{"unread"}, "test case")
				}
			}
		}
	}
	ms := time.Millisecond
	rAt, fAt := costAt.Add(-100*ms), costAt.Add(FullRoundLead-100*ms)
	rec, live := iMs(rAt.Add(30*time.Minute)), iMs(fAt.Add(30*time.Minute))
	in := costCall(clockedDrifts(), "body.expiresAt", rec, live, costAt, nil, nil, []Leaf{{Path: "reply.expiresAt", Noise: "reply.expiresAt", Type: String, Value: rec}}, iWin(costAt, costAt.Add(FullRoundLead), 200*ms))
	in.Arrived = costAt.Add(FullRoundLead)
	costRefused(t, "clock + 30m the recorded answer holds", oWord(t, Decide(in), rec, live), nil)
}

// Pinned (#5016): a given is read by its text, whole or as a part, never by
// a moment. A value made from a given nonce (a 19-digit number, whole or
// inside a longer value) is a Value bound at the given; a typed instant
// whose text is the given whole too (test_given_test.go:84-109). The
// application's own clock read beside a given nonce stays its Clock, as
// with no given (:120-150).
func TestOutcomeCostAGivenIsReadByItsText(t *testing.T) {
	const rec, full = "1761609600123456789", "1761700000000000001"
	gave := []Earlier{TestGiven("test o::TestApps", "given:nonce", rec, full)}
	for _, f := range [][2]string{{"app-" + rec, "app-" + full}, {"app-" + rec + "-x", "app-" + full + "-x"}, {rec, full}} {
		in := costCall(NewDriftCache(), "body.name", f[0], f[1], costAt, gave, nil, nil, nil)
		iValue(t, f[0]+" -> "+f[1]+", made from the given", oWord(t, Decide(in), rec, full), "test o::TestApps")
	}
	recAt := time.Date(2026, 10, 22, 11, 14, 17, 152e6, time.UTC)
	stamp, fstamp := iMs(recAt), iMs(recAt.Add(FullRoundLead))
	given := []Earlier{TestGiven("test o::TestApps", "given:at", stamp, fstamp)}
	iValue(t, "a value that is the given's instant text", oWord(t, Decide(costCall(NewDriftCache(), "body.at", stamp, fstamp, costAt, given, nil, nil, nil)), stamp, fstamp), "test o::TestApps")

	at := time.Date(2026, 10, 22, 11, 14, 17, 300e6, time.UTC)
	created, fcreated := iMs(at.Add(-100*time.Millisecond)), iMs(at.Add(FullRoundLead-100*time.Millisecond))
	nonceRec := strconv.FormatInt(at.Add(-140*time.Millisecond).UnixNano(), 10)
	nonceFull := strconv.FormatInt(at.Add(FullRoundLead-135*time.Millisecond).UnixNano(), 10)
	w := iWin(at, at.Add(FullRoundLead), 200*time.Millisecond)
	alone := costCall(oDrifts(), "body.createdAt", created, fcreated, at, nil, nil, nil, w)
	alone.Arrived = at.Add(FullRoundLead)
	iClock(t, "the app's clock read, no given", oWord(t, Decide(alone), created, fcreated))
	beside := alone
	beside.Earlier = []Earlier{TestGiven("test o::TestApps", "given:nonce", nonceRec, nonceFull)}
	iClock(t, "the app's clock read beside a given nonce", oWord(t, Decide(beside), created, fcreated))
}

// Pinned (#5016, test_given_forms_test.go:219, :279): a given's text glued
// inside a word that changed with it (its recorded text in the recorded
// word, its new text in round F's) is a part of the word: refused, naming
// the given, never learned fresh. The chance rule does not apply: the test
// gave it, so whether the word changed with it is always read, at any
// length. A word that holds it on one side only learns as with no given.
func TestOutcomeCostAGivenGluedInsideAWordIsAPart(t *testing.T) {
	const rec, full = "1761131656960000000", "1761225360000000001"
	for _, c := range []struct{ what, gr, gf, r, f string }{
		{"app<nonce>", rec, full, "app" + rec, "app" + full},
		{"<nonce>x9", rec, full, rec + "x9", full + "x9"},
		{"slot14 -> slot15, given 14", "14", "15", "slot14", "slot15"},
		{"seedAb12, given Ab12", "Ab12", "Cd34", "seedAb12", "seedCd34"},
		{"app<10 digits>, 9 in round F", rec[:10], full[:9], "app" + rec[:10], "app" + full[:9]},
	} {
		gave := []Earlier{TestGiven("test o::TestApps", "given:name", c.gr, c.gf)}
		in := costCall(NewDriftCache(), "body.name", c.r, c.f, costAt, gave, nil, nil, nil)
		d := oWord(t, Decide(in), c.r, c.f)
		oRefused(t, c.what+", made from the given", d, RefusalUnknown, "test o::TestApps")
		if d.Refusal != nil && !strings.Contains(d.Refusal.Why, "the given stands inside the word in both runs") {
			t.Errorf("%s, made from the given: %q; want it to say the given stands inside the word", c.what, d.Refusal.Why)
		}
	}
	gave := []Earlier{TestGiven("test o::TestApps", "given:name", "14", "15")}
	in := costCall(NewDriftCache(), "body.name", "slot14", "slot99", costAt, gave, nil, nil, nil)
	if d := oWord(t, Decide(in), "slot14", "slot99"); d.Outcome != OutcomeFresh {
		t.Errorf("slot14 -> slot99, given 14 -> 15: %s; want fresh, as with no given", oDescribe([]Decision{d}))
	}
}

// The places a call reads keep each earlier recording, never its text: a
// text is read only when a question needs it, so a call that asks about
// many large recordings holds one at a time. Kept as strings, 64
// recordings of 1 MiB held 64 MiB for the whole call, past the set's
// bounded store of payloads.
func TestOutcomeCostThePlacesKeepNoRecordingsText(t *testing.T) {
	defer func(b int64) { bodyBudget = b }(bodyBudget)
	bodyBudget = 1 << 20
	const n, size = 64, 1 << 20
	mocks := make([]*models.Mock, n)
	for i := range mocks {
		mocks[i] = getAnswer(fmt.Sprintf("mock-%d", i), "http://api/items/"+strconv.Itoa(i), `{"note":"`+fmt.Sprintf("%07d ", i)+strings.Repeat("a", size)+`"}`, eRecAt.Add(-time.Duration(n-i)*time.Second))
	}
	tr := NewTracker()
	tr.NewSet()
	tr.Begin()
	defer tr.End()
	dc := tr.Drifts()
	in := costCall(dc, "bind.$8", "0000001", "0000002", eRecAt, nil, lazyOf(dc, mocks), nil, nil)
	heap := func() uint64 {
		runtime.GC()
		var st runtime.MemStats
		runtime.ReadMemStats(&st)
		return st.HeapAlloc
	}
	before := heap()
	ps, unread := placesOf(in)
	ps = withTexts(newReaderIndex(dc), ps)
	grew := int64(heap()) - int64(before)
	runtime.KeepAlive(ps)
	if unread != "" || len(ps) != n {
		t.Fatalf("%d places, unread %q: want %d read places", len(ps), unread, n)
	}
	if grew > 8<<20 {
		t.Errorf("the call's places hold %d MiB after %d recordings of 1 MiB were read: they keep the texts", grew>>20, n)
	}
	if got, _ := ps[1].t.rec.textNow(); !strings.Contains(got, "0000001 ") {
		t.Errorf("a place's text, read when asked, is not its recording's (%d bytes)", len(got))
	}
}

// A short word of a value at a place the start-up or an earlier test sent,
// which round F moved to a word that shares nothing with F's new one, is
// chance: the place shows the value did not follow it, so the word is made
// anew each run, fresh (http learn_test.go:2829, :2853 on main refuse it;
// a changed place disproves it). A word of 32 bits or
// more there is a conflict, and so is the start-up's place that held the
// value's whole recorded token; an earlier test's is out of the call's
// scope, so that one is chance too.
func TestOutcomeCostAShortWordAtAMovedEarlierPlaceIsChance(t *testing.T) {
	const long, longF = "A7K29QXMtq", "Zq81Lm0Xwe"
	for _, c := range []struct {
		what                 string
		startUp              bool
		rec, live, sent, inF string
		kinds                []RefusalKind
	}{
		{"k-1 to k-3 beside the start-up's n-1, n-2 in F (http learn_test.go:2829)", true, "k-1", "k-3", "n-1", "n-2", nil},
		{"k-1 to k-3 beside an earlier test's n-1, n-2 in F (http learn_test.go:2853)", false, "k-1", "k-3", "n-1", "n-2", nil},
		{"a ten-letter word beside the start-up's", true, "k-" + long, "k-" + longF, "n-" + long, "n-Pp00Kk11Jj", costConflict},
		{"a ten-letter word beside an earlier test's", false, "k-" + long, "k-" + longF, "n-" + long, "n-Pp00Kk11Jj", costConflict},
		{"k-1 to k-3, the start-up sent k-1 whole, k-2 in F", true, "k-1", "k-3", "k-1", "k-2", costConflict},
		{"k-1 to k-3, an earlier test sent k-1 whole, k-2 in F", false, "k-1", "k-3", "k-1", "k-2", nil},
	} {
		in := oInput("body.key", c.rec, c.live, oServed("pool-1", true, c.startUp, Request, oText("body.node", c.sent), c.inF, time.Millisecond), nil)
		r, f := "1", "3"
		if c.rec != "k-1" {
			r, f = long, longF
		}
		d := oWord(t, Decide(in), r, f)
		if c.kinds == nil {
			costFresh(t, c.what, d)
			continue
		}
		costRefused(t, c.what, d, c.kinds, "pool-1", "body.node")
	}
}

// A word round F's view of an earlier recording gave at another field than
// the one whose recorded text holds the recorded word (follows_test.go:326
// and its subcases; main refuses each): a recording is read field by field,
// so a place shows a word in both runs only where one field holds the
// recorded word as recorded and round F's word in F's view. One that moved
// column, stood at two, or moved to a row F's view grew is refused as
// unknown, naming the field F gave it at; one that stayed is bound there,
// and its line says the place shows it in both runs.
func TestOutcomeAWordFsViewMovedToAnotherFieldIsRefused(t *testing.T) {
	for _, c := range []struct {
		what  string
		r, f  Raw
		moved string
	}{
		{"moved to another column", eRow("48d1", "6c7d", "aaaa"), eRow("9998", "9999", "6c7d"), "rows[0][2]"},
		{"given at two columns, neither the recorded one", eRow("48d1", "6c7d", "aaaa"), eRow("9999", "6c7d", "6c7d"), "rows[0][1]"},
		{"moved to a row F's view grew", eRows([]models.PostgresV3Cells{{{Value: "48d1"}, {Value: "6c7d"}}}),
			eRows([]models.PostgresV3Cells{{{Value: "9999"}, {Value: "6c7d"}}, {{Value: "6c7d"}, {Value: "1"}}}), "rows[0][1]"},
		{"stayed at its column", eRow("48d1", "x", "y"), eRow("6c7d", "x", "y"), ""},
	} {
		raw := append(eStartup(), c.r.ServedAs(6).ServedLive(c.f))
		d := oWord(t, Decide(costF("k-48d1", "k-6c7d", nil, raw, nil)), "48d1", "6c7d")
		if c.moved == "" {
			if d.Outcome != OutcomeValue || d.From.Mock != "mock-6" || !strings.Contains(d.Line, "shows it in both runs") {
				t.Errorf("k-48d1 to k-6c7d, %s: %s; want a Value from mock-6 that shows it in both runs", c.what, oDescribe([]Decision{d}))
			}
			continue
		}
		costRefused(t, "k-48d1 to k-6c7d, F's view "+c.what, d, costUnknown, "mock-6")
		if d.Refusal != nil && !strings.Contains(d.Refusal.Why, c.moved) {
			t.Errorf("k-48d1 to k-6c7d, %s: why %q does not name the field F gave it at (%s)", c.what, d.Refusal.Why, c.moved)
		}
	}
}

// Beside an exchange keploy holds no payload of, a client test's read of the
// clock made since its test began (in seconds, before its call's window) is
// refused, naming the exchange, even where a send of its test before the
// call holds the same second (a PUT's lastUsedAt its call learned as its own
// read of the clock): main binds such a read as a copy of that send or, in
// its audit-read row (copy_or_read_test.go:2035), learns it as a read of the
// clock; this refusal is stricter than main, a red only. The same send with
// its payload read leaves the read a Clock since the test began.
func TestOutcomeCostAReadSinceItsTestBeganBesideAnExchangeWithNoPayloadIsRefused(t *testing.T) {
	const p = "body.docs.msgs.0.app.createdAt"
	secs := func(t time.Time) string { return strconv.FormatInt(t.Unix(), 10) }
	frac := func(t time.Time) string { return secs(t) + "." + fmt.Sprintf("%06d", t.Nanosecond()/1000) }
	r0, f0 := oAt.Add(6300*time.Millisecond), oAt.Add(61*time.Second+14800*time.Millisecond)
	rAt, fAt := r0.Add(900*time.Millisecond), f0.Add(1400*time.Millisecond)
	w := iWin(rAt, fAt, 100*time.Millisecond)
	w.Rec.Began, w.F.Began = r0, f0
	rec, live := secs(r0.Add(100*time.Millisecond)), secs(f0.Add(150*time.Millisecond))
	if secs(r0.Add(120*time.Millisecond)) != rec || secs(f0.Add(170*time.Millisecond)) != live {
		t.Fatal("fixture: the send must hold the read's second in both runs")
	}
	put := iSent("mock-204", rAt.Add(-500*time.Millisecond), "body.data.lastUsedAt", Number, frac(r0.Add(120*time.Millisecond)), frac(f0.Add(170*time.Millisecond)))
	put.Seen[0].Clock = true
	call := func(put Earlier) Decision {
		in := iCall(p, Number, rec, live, rAt, fAt, w, []Earlier{put}, nil)
		in.Drifts = oClientDrifts()
		return oWord(t, Decide(in), rec, live)
	}
	d := call(put)
	iClock(t, "a read since its test began beside a send of the same second, its payload read", d)
	if d.Clock != nil && d.Clock.Since != models.SinceTestBegin {
		t.Errorf("a read since its test began, its payload read: %+v; want a read since the test began", *d.Clock)
	}
	put.Payload = Raw{}
	costRefused(t, "a read since its test began beside a send of the same second, no payload given", call(put), []RefusalKind{RefusalUnread}, "mock-204", "keploy was given no payload of mock-204")
	put.Payload = Raw{mock: "mock-204", unread: "its kind is unknown"}
	costRefused(t, "a read since its test began beside a send of the same second, its payload unread", call(put), []RefusalKind{RefusalUnread}, "mock-204", "keploy could not read mock-204")
}

// A call's word questions read each earlier recording once and check only
// the words the call asks about: no list of a recording's every word is
// built, so the call's work does not grow with the words of the run, and a
// later test of the set reads each recording's other forms as the set keeps
// them, derived once for the set, not again per test. A list of every word
// of every recording, built again on every call because no cache held a
// run's ~3,000 recordings, was half of the agent's CPU and doubled the
// round's time. Counted, not timed; the verdicts are the same at both sizes
// and in both tests.
func TestOutcomeCostACallChecksEachRecordingOnlyForTheWordsItAsks(t *testing.T) {
	const rec, live = "ids a1b2c3d4 b2c3d4e5 c3d4e5f6 d4e5f6a7 e5f6a7b8", "ids f0e9d8c7 e9d8c7b6 d8c7b6a5 c7b6a5f4 b6a5f4e3"
	lists, read := 0, tokensOfForms
	t.Cleanup(func() { tokensOfForms = read })
	tokensOfForms = func(f []string) []map[string]bool { lists++; return read(f) }
	extra := readTally{tierWords, partExtra}
	type result struct {
		lists, extras [2]int
		got           [2]string
	}
	call := func(n int) (r result) {
		mocks := make([]*models.Mock, n)
		for i := range mocks {
			name, body, at := fmt.Sprintf("mock-%d", i), fmt.Sprintf(`{"note":"Item %d of the run","ref":"Ref-%05d"}`, i, i), eRecAt.Add(-time.Duration(n-i)*time.Millisecond)
			if i == n/2 {
				name, body, at = "mock-held", `{"note":"Kept A1B2C3D4","ref":"a1b2c3d4"}`, eRecAt.Add(-time.Second)
			}
			mocks[i] = getAnswer(name, fmt.Sprintf("http://api/items/%d", i), body, at)
		}
		tr := NewTracker()
		tr.NewSet()
		for test := range 2 {
			tr.Begin()
			before := tallyOf(tr.derived)
			lists = 0
			dc := tr.Drifts()
			r.got[test] = oDescribe(Decide(costCall(dc, "body.ids", rec, live, eRecAt, nil, lazyOf(dc, mocks), nil, nil)))
			r.lists[test], r.extras[test] = lists, readSince(before, tallyOf(tr.derived))[extra]
			tr.End()
		}
		return r
	}
	small, large := call(300), call(3000)
	t.Logf("decided: %s; word lists built %v over 300 recordings, %v over 3,000", small.got[0], small.lists, large.lists)
	for _, r := range []result{small, large} {
		if r.got[1] != r.got[0] {
			t.Errorf("a later test's call decides %s; the first decided %s", r.got[1], r.got[0])
		}
	}
	if large.got[0] != small.got[0] {
		t.Errorf("over 3,000 recordings the call decides %s; over 300 it decided %s", large.got[0], small.got[0])
	}
	if large.lists != small.lists {
		t.Errorf("the call built %v lists of every word of a text over 3,000 recordings, %v over 300: it lists each recording's words, not only the words it asks", large.lists, small.lists)
	}
	for _, c := range []struct {
		n int
		r result
	}{{300, small}, {3000, large}} {
		// The set derives a recording's other forms (payloadExtra) only where
		// a scan reads it, past its filter (setFilter): the recording that
		// holds the word and a filter's rare false hit, once for the set, in
		// the first test, and none in the later.
		if c.r.extras[0] < 1 || c.r.extras[0] > 1+c.n/100 || c.r.extras[1] != 0 {
			t.Errorf("over %d recordings the two tests derived the recordings' other forms %v times; want once for each recording a scan reads (1 and a few false hits), in the first test, and none in the later", c.n, c.r.extras)
		}
	}
}

// What a call's question finds of a word in a text, read as a recording
// the set keeps, as one read whole, or as a text the caller hands, is what
// main's lists of the text's words in every declared form hold, in each
// reading, within the call's budget and past it, and no such list is built.
func TestOutcomeCostAWordIsFoundWhereMainsListsHoldIt(t *testing.T) {
	defer func(b int64) { readerBudget = b }(readerBudget)
	bodies := []string{
		`{"id":"order-17","path":"uploads/17/a.png","q":"Order+17"}`,
		`{"note":"cart_42 and 43, café é9 tok_a-b_c --x__ ABC Def"}`,
		`{"jwt":"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.sig_-x"}`,
		`{"u":"a%2Fb%20c%3D17 dGVzdC12YWx1ZQ== x"}`,
		`{"note":"{\"inner\":\"V-9\"}"}`,
	}
	lists, read := 0, tokensOfForms
	t.Cleanup(func() { tokensOfForms = read })
	tokensOfForms = func(f []string) []map[string]bool { lists++; return read(f) }
	mocks := make([]*models.Mock, len(bodies))
	for i, b := range bodies {
		mocks[i] = getAnswer(fmt.Sprintf("mock-%d", i), fmt.Sprintf("http://api/items/%d", i), b, eRecAt.Add(-time.Second))
	}
	tr := NewTracker()
	tr.NewSet()
	tr.Begin()
	defer tr.End()
	dc := tr.Drifts()
	lazy := lazyOf(dc, mocks)
	for _, budget := range []int64{0, 1} {
		readerBudget = budget
		for i, m := range mocks {
			whole := RawOf(m, true)
			text := whole.payload().text
			want := read(textForms(text))
			words := map[string]bool{"": true, "zz": true, "17/a": true, "-": true, "a-b": true, "order-17": true}
			for _, set := range want {
				for w := range set {
					words[w], words[w[:len(w)/2]], words[strings.ToUpper(w)] = true, true, true
				}
			}
			x := newReaderIndex(dc)
			for _, h := range []struct {
				what string
				t    *readerText
			}{{"the set's recording", x.textOf(lazy[i])}, {"a recording read whole", x.textOf(whole)}, {"a handed text", x.text(text)}} {
				for w := range words {
					for r := range readings {
						if held, ok := h.t.wordIn(w, r); held != want[r][w] || !ok {
							t.Errorf("budget %d, %s of %s: %q in reading %d held %v read %v; main's lists hold it: %v", budget, h.what, m.Name, w, r, held, ok, want[r][w])
						}
					}
					if held, _ := h.t.holdsWord(w); held != (want[0][w] || want[1][w] || want[2][w]) {
						t.Errorf("budget %d, %s of %s: %q held %v; main's lists hold it in a reading: %v", budget, h.what, m.Name, w, held, !held)
					}
				}
			}
			x.release()
		}
	}
	if lists != 0 {
		t.Errorf("the questions built %d lists of every word of a text; want each word checked alone", lists)
	}
}

// A call asks each place's texts once for each word it decides, in one scan
// of each text for all three readings: the scans grow with the places times
// the words asked, at a fixed few per place and word, and the verdicts stay.
func TestOutcomeCostACallScansEachPlaceOnceForEachWordItAsks(t *testing.T) {
	const rec, live = "ids a1b2c3d4 b2c3d4e5 c3d4e5f6 d4e5f6a7 e5f6a7b8", "ids f0e9d8c7 e9d8c7b6 d8c7b6a5 c7b6a5f4 b6a5f4e3"
	const words = 5
	call := func(n int) (string, int64) {
		mocks := make([]*models.Mock, n)
		for i := range mocks {
			name, body, at := fmt.Sprintf("mock-%d", i), fmt.Sprintf(`{"note":"Item %d of the run","ref":"Ref-%05d"}`, i, i), eRecAt.Add(-time.Duration(n-i)*time.Millisecond)
			if i == n/2 {
				name, body, at = "mock-held", `{"note":"Kept A1B2C3D4","ref":"a1b2c3d4"}`, eRecAt.Add(-time.Second)
			}
			mocks[i] = getAnswer(name, fmt.Sprintf("http://api/items/%d", i), body, at)
		}
		tr := NewTracker()
		tr.NewSet()
		tr.Begin()
		defer tr.End()
		dc := tr.Drifts()
		before := heldScans.Load()
		got := oDescribe(Decide(costCall(dc, "body.ids", rec, live, eRecAt, nil, lazyOf(dc, mocks), nil, nil)))
		return got, heldScans.Load() - before
	}
	small, sScans := call(300)
	large, lScans := call(3000)
	t.Logf("decided: %s; scans %d over 300 recordings, %d over 3,000", small, sScans, lScans)
	if large != small {
		t.Errorf("over 3,000 recordings the call decides %s; over 300 it decided %s", large, small)
	}
	// Each place has two texts (as recorded and as round F gave it), each
	// read with its other declared forms: four scans for each word.
	const perPlaceWord = 4
	for _, c := range []struct {
		n     int
		scans int64
	}{{300, sScans}, {3000, lScans}} {
		if limit := int64(perPlaceWord*words*c.n + 64); c.scans > limit {
			t.Errorf("over %d recordings the call made %d scans of a text for a word; want at most %d: a place asked twice for a word, or once for each reading", c.n, c.scans, limit)
		}
	}
}

// costSet decides the call of costCall's row in two tests of one set over
// n recordings, one of which holds the recorded word: what each test
// decided, and the filters it built (setFilters.builds), the scans of a text
// it made for a word (heldScans) and the words it asked of filters
// (filterLookups).
func costSet(n int) (got [2]string, builds [2]int, scans, lookups [2]int64) {
	const rec, live = "ids a1b2c3d4 b2c3d4e5 c3d4e5f6 d4e5f6a7 e5f6a7b8", "ids f0e9d8c7 e9d8c7b6 d8c7b6a5 c7b6a5f4 b6a5f4e3"
	mocks := make([]*models.Mock, n)
	for i := range mocks {
		name, body, at := fmt.Sprintf("mock-%d", i), fmt.Sprintf(`{"note":"Item %d of the run","ref":"Ref-%05d"}`, i, i), eRecAt.Add(-time.Duration(n-i)*time.Millisecond)
		if i == n/2 {
			name, body, at = "mock-held", `{"note":"Kept A1B2C3D4","ref":"a1b2c3d4"}`, eRecAt.Add(-time.Second)
		}
		mocks[i] = getAnswer(name, fmt.Sprintf("http://api/items/%d", i), body, at)
	}
	tr := NewTracker()
	tr.NewSet()
	built := func(dc *DriftCache) int {
		fs := filtersOf(dc.set)
		fs.mu.Lock()
		defer fs.mu.Unlock()
		return fs.builds
	}
	for c := range got {
		tr.Begin()
		dc := tr.Drifts()
		b, sc, l := built(dc), heldScans.Load(), filterLookups.Load()
		got[c] = oDescribe(Decide(costCall(dc, "body.ids", rec, live, eRecAt, nil, lazyOf(dc, mocks), nil, nil)))
		builds[c], scans[c], lookups[c] = built(dc)-b, heldScans.Load()-sc, filterLookups.Load()-l
		tr.End()
	}
	return got, builds, scans, lookups
}

// A set builds the filter of each recording's words once, at the first call
// that asks it a word, and every later call of the set asks the filters:
// a few lookups per place and word asked, and a scan of a text only where
// its filter may hold the word (the recording that holds it, and the
// filter's rare false hits), however many recordings the set holds. The
// verdicts stay.
func TestOutcomeCostASetFiltersEachRecordingsWordsOnceAndScansOnlyWhereOneMayHoldAWord(t *testing.T) {
	const words = 5
	var first string
	for _, n := range []int{300, 3000} {
		got, builds, scans, lookups := costSet(n)
		t.Logf("%d recordings: decided %s; filters built %v, scans %v, lookups %v (first test, second)", n, got[1], builds, scans, lookups)
		if first == "" {
			first = got[0]
		}
		if got[0] != first || got[1] != first {
			t.Errorf("over %d recordings the tests decide %q; want %q in both", n, got, first)
		}
		if builds[0] != n || builds[1] != 0 {
			t.Errorf("over %d recordings the tests built %v filters; want each recording's once, in the first test: [%d 0]", n, builds, n)
		}
		// The recording that holds the word, asked for the recorded and round
		// F's word in a few places, and a false hit in a hundred lookups at most.
		if limit := int64(4*words + n/100); scans[1] > limit {
			t.Errorf("over %d recordings the second test scanned a text %d times for a word; want at most %d", n, scans[1], limit)
		}
		if lookups[1] < int64(words*n) || lookups[1] > int64(4*words*n) {
			t.Errorf("over %d recordings the second test asked filters %d words; want a few for each of %d places and %d words", n, lookups[1], n, words)
		}
	}
}

// costViews decides costSet's call in two tests of one set over n
// recordings of the running test, each served changed with round F's view
// of it (ServedLive) of pad bytes past its recording, as the Mongo adapter
// hands each call a view it reads anew: one recording holds R's word and its
// view F's. It returns what each test decided, the filters of texts it built
// (setFilters.textBuilds) and the scans of a text it made for a word
// (heldScans).
func costViews(n, pad int) (got [2]string, builds [2]int, scans [2]int64) {
	const rec, live = "ids a1b2c3d4 b2c3d4e5 c3d4e5f6 d4e5f6a7 e5f6a7b8", "ids f0e9d8c7 e9d8c7b6 d8c7b6a5 c7b6a5f4 b6a5f4e3"
	mocks, views := make([]*models.Mock, n), make([]*models.Mock, n)
	for i := range mocks {
		name, url, at := fmt.Sprintf("mock-%d", i), fmt.Sprintf("http://api/items/%d", i), eRecAt.Add(-time.Duration(n-i)*time.Millisecond)
		body, view := fmt.Sprintf(`{"note":"Item %d of the run","ref":"Ref-%05d"}`, i, i), fmt.Sprintf(`{"note":"Item %d of the run","ref":"Ref-%05d","seen":"%s"}`, i, i, strings.Repeat("as served ", pad/10))
		if i == n/2 {
			body, view = `{"note":"Kept A1B2C3D4","ref":"a1b2c3d4"}`, fmt.Sprintf(`{"note":"Kept F0E9D8C7","ref":"f0e9d8c7","seen":"%s"}`, strings.Repeat("as served ", pad/10))
		}
		mocks[i], views[i] = getAnswer(name, url, body, at), getAnswer(name, url, view, at)
	}
	tr := NewTracker()
	tr.NewSet()
	for c := range got {
		tr.Begin()
		dc := tr.Drifts()
		raw := make([]Raw, n)
		for i, m := range mocks {
			raw[i] = dc.LazyRawOf(m, true, nil).ServedAs(i + 1).ServedLive(RawOf(views[i], true))
		}
		b, sc := textsBuilt(dc), heldScans.Load()
		got[c] = oDescribe(Decide(costCall(dc, "body.ids", rec, live, eRecAt, nil, raw, nil, nil)))
		builds[c], scans[c] = textsBuilt(dc)-b, heldScans.Load()-sc
		tr.End()
	}
	return got, builds, scans
}

// textsBuilt is how many filters of texts dc's set has built (textEntry).
func textsBuilt(dc *DriftCache) int {
	fs := filtersOf(dc.set)
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.textBuilds
}

// Round F's view of a recording is read anew in each call, with no
// recording's identity, yet a set builds the filter of each view's words
// once, by its text, at the first call that asks it a word, and every later
// call that hands the same text asks the filter: a scan of a view only where
// its filter may hold the word, however many views the set holds. The
// verdicts stay, and are those of a set past its budget, which keeps no
// filter.
func TestOutcomeCostASetFiltersEachViewsWordsOnceByItsText(t *testing.T) {
	const words = 5
	for _, n := range []int{300, 3000} {
		got, builds, scans := costViews(n, filterTextMin)
		t.Logf("%d views: decided %s; filters built %v, scans %v (first test, second)", n, got[1], builds, scans)
		if got[0] != got[1] {
			t.Errorf("over %d views the tests decide %q; want one verdict", n, got)
		}
		if builds != [2]int{n, 0} {
			t.Errorf("over %d views the tests built %v filters of texts; want each view's once, in the first test: [%d 0]", n, builds, n)
		}
		// The view that holds F's word, asked in a few places, and a false
		// hit in a hundred lookups at most.
		if limit := int64(4*words + n/100); scans[1] > limit {
			t.Errorf("over %d views the second test scanned a text %d times for a word; want at most %d", n, scans[1], limit)
		}
		func() {
			defer func(b int64) { learnBudget = b }(learnBudget)
			learnBudget = 1
			past, _, pastScans := costViews(n, filterTextMin)
			if past != got {
				t.Errorf("over %d views past the budget the tests decide %q; want %q", n, past, got)
			}
			if pastScans[1] < int64(words*n) {
				t.Errorf("over %d views past the budget the second test scanned a text %d times; want each view scanned for each word, as with no filter", n, pastScans[1])
			}
		}()
	}
}

// A text with no recording's identity has the set's filter of its own words:
// a handed text and a view of one text share one, built once for the set,
// and two texts of one length that differ in a word keep two, so neither
// rules out the word only the other lacks. A view keploy could not read is
// filtered by no text, and stays unread. A text shorter than filterTextMin
// keeps no filter, and is scanned for each word in each test. Each filter
// counts its words and its key on the set's budget.
func TestOutcomeCostATextsFilterIsKeptByItsWholeText(t *testing.T) {
	tr := NewTracker()
	tr.NewSet()
	pad := strings.Repeat("as served ", filterTextMin/10)
	one, other := `{"ref":"a1b2c3d4","seen":"`+pad+`"}`, `{"ref":"a1b2c3d5","seen":"`+pad+`"}`
	for c := 0; c < 2; c++ {
		tr.Begin()
		dc := tr.Drifts()
		x := newReaderIndex(dc)
		used := func() int64 { dc.set.mu.Lock(); defer dc.set.mu.Unlock(); return dc.set.budget.used }
		b, u := textsBuilt(dc), used()
		handed, view := x.text(one), x.textOf(Raw{mock: "mock-view", rText: one, rRead: true})
		if !handed.ruledOut("f0e9d8c7") || !view.ruledOut("f0e9d8c7") {
			t.Errorf("test %d: the filter of %s does not rule out f0e9d8c7, which it lacks", c, one)
		}
		if n := textsBuilt(dc) - b; n != 1-c {
			t.Errorf("test %d: the handed text and the view of one text built %d filters; want %d (one for the set, in the first test)", c, n, 1-c)
		}
		if c == 0 {
			var f wordFilter
			if handed.filter != nil {
				f = handed.filter.f
			}
			if want, got := headerBytes+8*int64(len(f))+filterEntryBytes+filterTextKeyBytes, used()-u; f == nil || got != want {
				t.Errorf("the text's filter counted %d bytes on the set's budget; want %d: its words and its key (a filter kept: %v)", got, want, f != nil)
			}
		}
		for _, v := range []struct{ text, word string }{{other, "a1b2c3d5"}, {one, "a1b2c3d4"}} {
			if held, _ := x.textOf(Raw{mock: "mock-view", rText: v.text, rRead: true}).holdsWord(v.word); !held {
				t.Errorf("test %d: the view %s does not hold %q", c, v.text, v.word)
			}
			if held, _ := x.text(v.text).holdsWord(v.word); !held {
				t.Errorf("test %d: the handed text %s does not hold %q", c, v.text, v.word)
			}
		}
		unread := x.textOf(Raw{mock: "mock-view", rText: one, rRead: false})
		if held, read := unread.holdsWord("f0e9d8c7"); held || read {
			t.Errorf("test %d: a view keploy could not read holds f0e9d8c7 %v, read %v; want unread", c, held, read)
		}
		head := `{"ref":"a1b2c3d4","seen":"`
		short := head + pad[:filterTextMin-1-len(head)-2] + `"}`
		b, sc := textsBuilt(dc), heldScans.Load()
		for _, h := range []*readerText{x.text(short), x.textOf(Raw{mock: "mock-view", rText: short, rRead: true})} {
			if h.ruledOut("f0e9d8c7") {
				t.Errorf("test %d: the %d-byte text %s is ruled out of f0e9d8c7 by a filter; want it scanned", c, len(short), short)
			}
			if held, _ := h.holdsWord("f0e9d8c7"); held {
				t.Errorf("test %d: the text %s holds f0e9d8c7", c, short)
			}
		}
		if n, scans := textsBuilt(dc)-b, heldScans.Load()-sc; len(short) != filterTextMin-1 || n != 0 || scans != 4 {
			t.Errorf("test %d: the %d-byte text, handed and as a view, built %d filters and was scanned %d times; want a %d-byte text, none built, each scanned (with its forms) for the word: 4", c, len(short), n, scans, filterTextMin-1)
		}
		x.release()
		tr.End()
	}
}

// Past the set's budget a recording keeps no filter: each call scans it as
// before, and none reads it again to build one. The verdict stays.
func TestOutcomeCostARecordingsFilterPastTheSetsBudgetIsNotKept(t *testing.T) {
	const words = 5
	_, _, wantScans, _ := costSet(300)
	defer func(b int64) { learnBudget = b }(learnBudget)
	learnBudget = 1
	want, _, _, _ := costSet(300)
	got, builds, scans, lookups := costSet(300)
	t.Logf("past the budget: decided %s; filters built %v, scans %v, lookups %v (with room: scans %v)", got[1], builds, scans, lookups, wantScans)
	if got != want {
		t.Errorf("past the budget the tests decide %q; want %q", got, want)
	}
	if builds[0] != 300 || builds[1] != 0 || lookups != [2]int64{} {
		t.Errorf("past the budget the tests built %v filters and asked them %v words; want each built once, kept by none, asked none", builds, lookups)
	}
	if scans[1] < int64(words*300) {
		t.Errorf("past the budget the second test scanned a text %d times; want each place scanned for each word, as with no filter", scans[1])
	}
}

// A recording keploy could not read when a call first asked it a word
// keeps no filter of no words: the set marks it unread (the store's read
// bit), so every later test of the set refuses it as unread, as a set whose
// reads all succeed decides it, never fresh.
func TestOutcomeCostARecordingUnreadAtItsFirstAskKeepsNoFilter(t *testing.T) {
	defer func(r, b int64) { readerBudget, bodyBudget = r, b }(readerBudget, bodyBudget)
	readerBudget, bodyBudget = 1, 1
	const rec, live = "ids a1b2c3d4", "ids f0e9d8c7"
	m := getAnswer("mock-held", "http://api/items/1", `{"note":"Kept a1b2c3d4"}`, eRecAt.Add(-time.Second))
	decide := func(fail int) (got [2]string) {
		tr := NewTracker()
		tr.NewSet()
		for c := range got {
			tr.Begin()
			dc := tr.Drifts()
			loads := 0
			raw := dc.LazyRawOf(m, true, func() (*models.Mock, error) {
				if loads++; c == 0 && loads == fail {
					return nil, fmt.Errorf("load %d of the recording failed", loads)
				}
				return m, nil
			}).ServedAs(1).ServedAsRecorded()
			got[c] = oDescribe(Decide(costCall(dc, "body.ids", rec, live, eRecAt, nil, []Raw{raw}, nil, nil)))
			tr.End()
		}
		return got
	}
	want := decide(0)
	for k := 1; k <= 12; k++ {
		if got := decide(k); got[1] != want[1] && !strings.Contains(got[1], " refused unread: ") || strings.Contains(got[1], " fresh]") {
			t.Errorf("the first test's load %d failed; the second decides %s; want it refused as unread, or %s as where every read succeeds", k, got[1], want[1])
		}
	}
}

// A recording's filter rules out only a word no form of its text holds in
// any reading: never one a reading holds, a word of letters glued to '_',
// '-' or a byte past ASCII included; and it rules out most words its text
// does not hold. A short view (a text with no recording's identity, below
// filterTextMin) is never filtered: it holds its own words.
func TestOutcomeCostARecordingsFilterRulesOutOnlyWordsNoFormHolds(t *testing.T) {
	tr := NewTracker()
	tr.NewSet()
	tr.Begin()
	defer tr.End()
	dc := tr.Drifts()
	x := newReaderIndex(dc)
	defer x.release()
	texts := []string{
		`{"note":"cart_42 a b, x%y","id":"order-17","path":"up_17/a.png"}`,
		`{"k":"tok_a-b_c --x__ v1.2","u":"na\u00efve42\u00e9 café-9"}`,
		`{"cursor":"eyJpZCI6ImFiYzEyMyIsIm4iOjd9","q":"a%20b42%2Fc"}`,
	}
	out, absent := 0, 0
	for i, text := range texts {
		r := dc.LazyRawOf(getAnswer(fmt.Sprintf("mock-%d", i), "http://api/items", text, eRecAt), true, nil)
		h := x.textOf(r)
		forms := textForms(text)
		joined := strings.Join(forms, "\n")
		for a := 0; a < len(joined); a++ {
			for b := a + 1; b <= len(joined) && b <= a+16; b++ {
				w := joined[a:b]
				held := heldIn(forms, w, 0, 1, 2)
				if h.ruledOut(w) {
					out++
					if held {
						t.Errorf("the filter of %s rules out %q, which a reading holds as a word of it", text, w)
					}
				}
				if !held && slices.ContainsFunc(wordsBy(w, isWord), func(run string) bool { return !slices.Contains(wordsBy(joined, isWord), run) }) {
					absent++
				}
			}
		}
	}
	if out*10 < absent*9 {
		t.Errorf("the filters ruled out %d words of %d with a run of letters and digits their texts lack; want nearly all", out, absent)
	}
	for _, v := range []struct{ text, word string }{{"alpha 17", "17"}, {"beta 42", "42"}} {
		if held, _ := x.textOf(Raw{mock: "mock-view", rText: v.text, rRead: true}).holdsWord(v.word); !held {
			t.Errorf("the view %q does not hold %q", v.text, v.word)
		}
	}
}

// A word some byte of which no reading reads as a word byte is a word of no
// text, and no text is scanned for it; one only some readings read is held
// where those readings hold it.
func TestOutcomeCostAWordNoReadingReadsScansNoText(t *testing.T) {
	const s = `{"note":"cart_42 a b, x%y"}`
	before := heldScans.Load()
	for _, w := range []string{"a b", "x%y", "\""} {
		if heldIn(textForms(s), w, 0, 1, 2) {
			t.Errorf("%q is held as a word of %s; no reading reads it as one", w, s)
		}
	}
	if n := heldScans.Load() - before; n != 0 {
		t.Errorf("asking words no reading reads scanned a text %d times; want none", n)
	}
	// Asked in every reading at once, a word is held where any one reading's
	// list of the text's words holds it, though another reading that reads
	// each of its bytes holds it nowhere in the text.
	for _, text := range []string{s, `{"id":"order-17","path":"up_17/a.png","k":"tok_a-b_c --x__ v1.2"}`} {
		for r := range readings {
			for w := range tokensBy(text, readings[r]) {
				want := slices.ContainsFunc(readings, func(word func(byte) bool) bool { return tokensBy(text, word)[w] })
				if got := heldIn([]string{text}, w, 0, 1, 2); got != want {
					t.Errorf("%q in %s, asked in every reading: held %v; the readings' lists hold it: %v", w, text, got, want)
				}
			}
		}
	}
	// Whatever order the readings are asked in: "42" is no word of
	// "cart_42" where '_' continues a word, and is one where it does not.
	if !heldIn([]string{s}, "42", 2, 0) || heldIn([]string{s}, "42", 2) {
		t.Errorf("42 in %s: held asked in readings 2 and 0 %v, in reading 2 alone %v; want held only where a reading ends a word at '_'", s, heldIn([]string{s}, "42", 2, 0), heldIn([]string{s}, "42", 2))
	}
	if !heldIn([]string{s}, "cart_42", 0, 1, 2) || heldIn([]string{s}, "cart_42", 0) {
		t.Errorf("cart_42 in %s: held in any reading %v, in reading 0 alone %v; want held only where a reading reads '_' as a word byte", s, heldIn([]string{s}, "cart_42", 0, 1, 2), heldIn([]string{s}, "cart_42", 0))
	}
}

// A place keploy read for the call but cannot read again where the call
// asks it for the word (past the reader's budget, its load failing) leaves
// the word unknown: refused, never fresh by a place taken to hold nothing.
func TestOutcomeCostAPlaceThatCannotBeReadAgainLeavesTheWordUnknown(t *testing.T) {
	defer func(r, b, d, x int64) { readerBudget, bodyBudget, derivedBudget, textBudget = r, b, d, x }(readerBudget, bodyBudget, derivedBudget, textBudget)
	readerBudget, bodyBudget, derivedBudget, textBudget = 1, 1, 1, 1
	m := getAnswer("mock-held", "http://api/items/1", `{"note":"Kept a1b2c3d4"}`, eRecAt.Add(-time.Second))
	seen := map[string]bool{}
	for k := 1; k <= 12; k++ {
		loads := 0
		tr := NewTracker()
		tr.NewSet()
		tr.Begin()
		dc := tr.Drifts()
		raw := dc.LazyRawOf(m, true, func() (*models.Mock, error) {
			if loads++; loads > k {
				return nil, fmt.Errorf("load %d of the recording failed", loads)
			}
			return m, nil
		}).ServedAs(1).ServedAsRecorded()
		got := oDescribe(Decide(costCall(dc, "body.ids", "ids a1b2c3d4", "ids f0e9d8c7", eRecAt, nil, []Raw{raw}, nil, nil)))
		tr.End()
		seen[got] = true
		if strings.Contains(got, " fresh]") {
			t.Errorf("the recording's load %d failed: %s; want the word refused, as unread or past the budget", k+1, got)
		}
	}
	t.Logf("decisions: %v", seen)
}

// wordsRead is how many outside recordings' words dc's set has read
// (outsideWords).
func wordsRead(dc *DriftCache) int {
	fs := filtersOf(dc.set)
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.wordsBuilds
}

// costOutside is n recordings of other tests (Raw.OfAnotherTest), each
// served changed with round F's view of it (ServedLive), as the Mongo
// adapter hands them to each call, handed to two calls of one set, one
// recording holding R's word where F's view holds F's: each call's verdict
// and how many recordings' words it read.
func costOutside(n int) (got [2]string, builds [2]int) {
	const rec, live = "ids a1b2c3d4", "ids f0e9d8c7"
	mocks, views := make([]*models.Mock, n), make([]*models.Mock, n)
	for i := range mocks {
		name, url, at := fmt.Sprintf("mock-%d", i), fmt.Sprintf("http://api/items/%d", i), eRecAt.Add(-time.Duration(n-i)*time.Millisecond)
		body, view := fmt.Sprintf(`{"note":"Item %d","ref":"Ref-%05d"}`, i, i), fmt.Sprintf(`{"note":"Item %d","ref":"Ref-%05d","seen":"yes"}`, i, i)
		if i == n/2 {
			body, view = `{"ref":"a1b2c3d4"}`, `{"ref":"f0e9d8c7"}`
		}
		mocks[i], views[i] = getAnswer(name, url, body, at), getAnswer(name, url, view, at)
	}
	tr := NewTracker()
	tr.NewSet()
	for c := range got {
		tr.Begin()
		dc := tr.Drifts()
		raw := make([]Raw, n)
		for i, m := range mocks {
			raw[i] = dc.LazyRawOf(m, true, nil).ServedAs(i + 1).ServedLive(RawOf(views[i], true)).OfAnotherTest()
		}
		b := wordsRead(dc)
		got[c] = oDescribe(Decide(costCall(dc, "body.ids", rec, live, eRecAt, nil, raw, nil, nil)))
		builds[c] = wordsRead(dc) - b
		tr.End()
	}
	return got, builds
}

// A set reads the words of each other test's recording handed to its calls
// once, at the first call, and a later call handed it as before asks the
// kept words, not its texts and its view's: no recording's words read again,
// however many the set holds. Past the set's budget none is kept, each call
// reads them as before, and the verdicts stay.
func TestOutcomeCostASetReadsEachOtherTestsRecordingsWordsOnce(t *testing.T) {
	for _, n := range []int{50, 500} {
		got, builds := costOutside(n)
		t.Logf("%d recordings: decided %s; words read %v (first test, second)", n, got[1], builds)
		if got[0] != got[1] {
			t.Errorf("over %d recordings the tests decide %q; want one verdict", n, got)
		}
		if builds != [2]int{n, 0} {
			t.Errorf("over %d recordings the tests read %v recordings' words; want each once, in the first test: [%d 0]", n, builds, n)
		}
		func() {
			defer func(b int64) { learnBudget = b }(learnBudget)
			learnBudget = 1
			past, pastBuilds := costOutside(n)
			if past != got {
				t.Errorf("over %d recordings past the budget the tests decide %q; want %q", n, past, got)
			}
			if pastBuilds != [2]int{n, n} {
				t.Errorf("over %d recordings past the budget the tests read %v recordings' words; want each in each test, kept by none: [%d %d]", n, pastBuilds, n, n)
			}
		}()
	}
}

// The words a set keeps of another test's recording are the words wordsOf
// reads of it as each call hands it: its own serves and test in that call,
// and one entry for each way F served it and each view F gave, which a
// later call handed it another way reads apart.
func TestOutcomeCostAnOtherTestsRecordingsKeptWordsAreWhatItsCallHandsIt(t *testing.T) {
	at := eRecAt.Add(-time.Second)
	m := getAnswer("mock-x", "http://api/items/1", `{"ref":"a1b2c3d4","note":"Kept","at":"2026-10-24T09:55:50Z"}`, at)
	view := func(body string) Raw { return RawOf(getAnswer("mock-x", "http://api/items/1", body, at), true) }
	v1, v2, v3 := view(`{"ref":"f0e9d8c7"}`), view(`{"ref":"e9d8c7b6","note":"Kept"}`), view(`{"ref":"e9d8c7b6"}`)
	gone := getAnswer("mock-gone", "http://api/items/2", `{"ref":"b2c3d4e5"}`, at)
	// One view three more ways, each alike in all but one part of its
	// payload wordsOf reads: a clock read of the instant its body holds, a
	// stamp over that instant, its body's field dropped.
	const instant = "2026-10-24T09:55:50Z"
	vt := view(`{"ref":"e9d8c7b6","at":"` + instant + `"}`)
	// Its payload read now (a lazy Raw reads it from its loader), so each
	// copy changes one part of it.
	b := vt.payload()
	vt.src, vt.rText, vt.rClock, vt.rFields, vt.rStamps, vt.rRead = nil, b.text, b.answerClock, b.fields, b.stamps, b.read
	vClock, vStamp, vField := vt, vt, vt
	in, err := time.Parse(time.RFC3339, instant)
	if err != nil {
		t.Fatal(err)
	}
	vClock.rClock = []time.Time{in}
	start := strings.Index(vt.rText, instant)
	vStamp.rStamps = []rawField{{start: start, end: start + len(instant), path: "Res.body"}}
	body := slices.IndexFunc(vt.rFields, func(f rawField) bool { return f.path == "Res.body" && !f.key })
	if start < 0 || body < 0 || len(vt.rClock)+len(vt.rStamps) > 0 {
		t.Fatalf("fixture: the view's body (%d), its instant (%d), and no clock read or stamp (%v, %v)", body, start, vt.rClock, vt.rStamps)
	}
	vField.rFields = slices.Delete(slices.Clone(vt.rFields), body, body+1)
	type shape struct {
		name     string
		of       func(Raw, int) Raw
		unserved bool
	}
	shapes := []shape{
		{name: "served as recorded", of: func(x Raw, n int) Raw { return x.ServedAs(n).ServedAsRecorded() }},
		{name: "with F's view", of: func(x Raw, n int) Raw { return x.ServedAs(n).ServedLive(v1) }},
		{name: "with another view", of: func(x Raw, n int) Raw { return x.ServedAs(n).ServedLive(v2) }},
		{name: "with other words at the view's fields", of: func(x Raw, n int) Raw { return x.ServedAs(n).ServedLive(v3) }},
		{name: "with a view holding an instant", of: func(x Raw, n int) Raw { return x.ServedAs(n).ServedLive(vt) }},
		{name: "with that view's clock read of its instant", of: func(x Raw, n int) Raw { return x.ServedAs(n).ServedLive(vClock) }},
		{name: "with that view's instant a stamp", of: func(x Raw, n int) Raw { return x.ServedAs(n).ServedLive(vStamp) }},
		{name: "with that view's body field dropped", of: func(x Raw, n int) Raw { return x.ServedAs(n).ServedLive(vField) }},
		{name: "with two views", of: func(x Raw, n int) Raw { return x.ServedAs(n).ServedChanged(v1, v2) }},
		{name: "with the two views the other way", of: func(x Raw, n int) Raw { return x.ServedAs(n).ServedChanged(v2, v1) }},
		{name: "served changed, no view read", of: func(x Raw, n int) Raw { return x.ServedIn(n, n+2).ServedChanged() }},
		{name: "served, nothing stated", of: func(x Raw, n int) Raw { return x.ServedIn(n, n+1) }},
		{name: "not served", of: func(x Raw, _ int) Raw { return x }, unserved: true},
	}
	tr := NewTracker()
	tr.NewSet()
	for c := range 2 {
		tr.Begin()
		dc := tr.Drifts()
		d := &decider{dr: dc}
		b := wordsRead(dc)
		read := map[string][]any{}
		// The call's own earlier place, and a recording of another test keploy
		// cannot read: offsetPlacesOf reads neither's words.
		raw := []Raw{dc.LazyRawOf(m, true, nil).ServedAs(1).ServedAsRecorded(),
			dc.LazyRawOf(gone, true, func() (*models.Mock, error) { return nil, errors.New("gone") }).ServedAs(2).OfAnotherTest()}
		for i, s := range shapes {
			x := s.of(dc.LazyRawOf(m, !s.unserved, nil), 1+i+10*c).OfAnotherTest()
			x.test, x.prev, x.late = 2+c, c == 1, c == 1
			got, want := d.outsideWords(x), dc.wordsOf(x)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("test %d, %s: the set's words are %+v; want wordsOf's %+v", c+1, s.name, got, want)
			}
			read[s.name] = []any{want.same, want.gave, want.view}
			raw = append(raw, x)
		}
		for _, n := range []string{"with that view's clock read of its instant", "with that view's instant a stamp", "with that view's body field dropped"} {
			if reflect.DeepEqual(read[n], read["with a view holding an instant"]) {
				t.Fatalf("fixture: %s reads the words the view does without it: %+v", n, read[n])
			}
		}
		if got, want := wordsRead(dc)-b, []int{len(shapes), 0}[c]; got != want {
			t.Errorf("test %d read the recording's words %d times; want %d (one entry each way it was handed)", c+1, got, want)
		}
		// The call's places are offsetPlacesOf's, in its order: raw's, then the
		// set's.
		set := []GivenPlaces{PlacesOf(RawOf(getAnswer("mock-set", "http://api/items/3", `{"ref":"c3d4e5f6","at":"2026-10-24T09:55:51Z"}`, at), true).ServedAs(3))}
		got, want := (&decider{dr: dc, in: Input{At: eRecAt, Raw: raw}, set: set}).offsetPlaces(), dc.offsetPlacesOf(eRecAt, nil, raw, set)
		if len(got.words) != len(shapes)+1 || len(got.answers) == 0 || !reflect.DeepEqual(got.words, want.words) || !reflect.DeepEqual(got.answers, want.answers) {
			t.Errorf("test %d: the call reads %d outside recordings' words %+v and instants %+v; want offsetPlacesOf's %d: %+v and %+v", c+1, len(got.words), got.words, got.answers, len(want.words), want.words, want.answers)
		}
		tr.End()
	}
}
