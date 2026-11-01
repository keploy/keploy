package learn

import (
	"fmt"
	"runtime"
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

	sevenSent := []Earlier{{Mock: "mock-1", At: costAt.Add(-time.Second), Run: true, StartUp: true, Seen: []Seen{{Side: Request,
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
		return []Earlier{{Mock: "mock-1", At: costAt.Add(-time.Second), Seen: []Seen{
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
// not serve, served changed with no view, that holds both values, recorded
// after the call, or answered at the call's own instant (follows_test.go:
// 388-392); a value that took part of a longer word F gave (:318); a row
// holding both 50 and a/b (:401-405).
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
		{"the CREATE TABLE was recorded after the call (:391)", "7", startup(eHistory(eRecAt.Add(10*time.Millisecond), 6, true))},
		{"the CREATE TABLE was answered at the call's own instant (:392)", "7", startup(eHistory(eRecAt.Add(-time.Millisecond), 6, true))},
	} {
		costRefused(t, c.what, oWord(t, Decide(costF("50", c.live, nil, c.raw, nil)), "50", c.live), costUnknown, "mock-5")
	}
	d := oWord(t, Decide(costF("k-48d1", "k-6c7d", nil, append(eStartup(), eRow("48d1 aaaa", "x", "y").ServedAs(6).ServedLive(eRow("6c7daaaa", "x", "y"))), nil)), "48d1", "6c7d")
	costRefused(t, "the value took a part of a longer word F gave (follows_test.go:318)", d, costUnknown, "mock-6")
	src := ePg("mock-s", 150*time.Millisecond, "SELECT a, b FROM t", []models.PostgresV3Cells{{{Value: "50"}, {Value: "x a/b y"}}}, "SELECT 1").ServedAs(6).ServedAsRecorded()
	for _, live := range []string{"a/b", "a%2Fb"} {
		d := oWord(t, Decide(costF("50", live, nil, append(eStartup(), src), nil)), "50", live)
		costRefused(t, "50 in mock-s, which holds F's "+live+" too (follows_test.go:401-405)", d, []RefusalKind{"unknown", "changed-shape"}, "mock-s")
	}
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
