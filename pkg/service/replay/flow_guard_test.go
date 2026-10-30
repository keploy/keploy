package replay

import (
	"context"
	"fmt"
	"net/http"
	"runtime"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
)

// The dataflow guard (flow_guard.go): a call made outside every test's
// window that no mock answered is the next test's miss only when the word
// rule keploy learn reads a value's source by says a word of what changed in
// that test this run may have come from the call's recorded answer.

// guardMock is the recording the background call's closest mock names.
func guardMock(answer string) *models.Mock {
	return &models.Mock{Name: "mock-12", Kind: models.HTTP, Spec: models.MockSpec{
		HTTPReq:  &models.HTTPReq{Method: models.Method("GET"), URL: "http://config:8080/boot"},
		HTTPResp: &models.HTTPResp{StatusCode: 200, Body: answer}}}
}

// guardCase is a test whose request carried reqBody and whose recorded
// response was respBody.
func guardCase(name, reqBody, respBody string) *models.TestCase {
	return &models.TestCase{Name: name,
		HTTPReq:  models.HTTPReq{Method: models.Method("POST"), URL: "http://app:8080/orders", Body: reqBody},
		HTTPResp: models.HTTPResp{StatusCode: 200, Body: respBody}}
}

// startUpCall is the call the application made before the set's first test,
// which no mock answered, mock-12 its closest recording.
func startUpCall() models.UnmatchedCall {
	return models.UnmatchedCall{Protocol: "HTTP", ActualSummary: "GET /boot", Kind: models.MissOutsideTest,
		BeforeSet: true, ClosestMock: "mock-12"}
}

func live(body string) *models.HTTPResp { return &models.HTTPResp{StatusCode: 200, Body: body} }

// A before-first call whose recorded answer gave the word the first test's
// response lost this run is that test's miss, the word and its place named.
func TestFlowGuard_ABeforeFirstCallWhoseAnswerTheTestsResponseLostIsCharged(t *testing.T) {
	var s setCalls
	s.reset(newFlowGuard([]*models.TestCase{guardCase("test-1", `{"item":"pen"}`, `{"tenant":"tenantA7K29Q"}`)}, nil,
		[]*models.Mock{guardMock(`{"tenant":"tenantA7K29Q"}`)}))
	rest := s.take(context.Background(), []models.UnmatchedCall{startUpCall()}, "test-1", live(`{"tenant":""}`))
	if len(rest) != 1 || !strings.Contains(rest[0].ReachesTest, "tenantA7K29Q") || !strings.Contains(rest[0].ReachesTest, "this test's response") ||
		!strings.Contains(rest[0].ReachesTest, "made before the set's first test") {
		t.Fatalf("the test keeps %+v; want the start-up call, naming the word and its place", rest)
	}
	if got := s.backgroundCalls(); len(got) != 0 {
		t.Fatalf("the set lists %+v; a call that reached the test is no background work", got)
	}
}

// The same through a request the test sent that no mock answered.
func TestFlowGuard_ABeforeFirstCallWhoseAnswerTheTestSentOnIsCharged(t *testing.T) {
	var s setCalls
	s.reset(newFlowGuard([]*models.TestCase{guardCase("test-1", `{"item":"pen"}`, `{"ok":true}`)}, nil,
		[]*models.Mock{guardMock(`{"tenant":"tenantA7K29Q"}`)}))
	own := models.UnmatchedCall{Protocol: "HTTP", ActualSummary: "POST /charge", ClosestMock: "mock-30",
		ClosestMockReq: `{"tenant":"tenantA7K29Q","amount":5}`, ReceivedReq: `{"tenant":"","amount":5}`}
	rest := s.take(context.Background(), []models.UnmatchedCall{startUpCall(), own}, "test-1", live(`{"ok":true}`))
	if len(rest) != 2 || !strings.Contains(rest[0].ReachesTest, "the request this test sent that mock-30 recorded") {
		t.Fatalf("the test keeps %+v; want the start-up call, charged through the request it sent, then its own", rest)
	}
}

// The words of api-server's start-up createIndexes answer ("ok", "1",
// "false") stand in the first test's response, but at their own places, as
// recorded: no word of what changed, so they reach nothing. A word the test
// changed that the answer holds ("2") is one keploy cannot tell came from
// elsewhere, as keploy learn refuses it as noise: charged (a stated limit;
// the test fails on its own change anyway).
func TestFlowGuard_AStartUpCreateIndexesReachesATestOnlyThroughAChangedWord(t *testing.T) {
	answer := `{"ok":1,"numIndexesBefore":1,"numIndexesAfter":2,"createdCollectionAutomatically":false}`
	recorded := `{"ok":true,"total":1,"archived":false,"id":"r1"}`
	for _, w := range []string{"ok", "1", "false"} {
		if !learn.ContainsToken(answer, w) || !learn.ContainsToken(recorded, w) {
			t.Fatalf("the row's premise: %q stands in both the answer and the response", w)
		}
	}
	for _, c := range []struct {
		live    string
		charged bool
	}{
		{recorded, false},
		{`{"ok":true,"total":1,"archived":false,"id":"r9"}`, false},
		{`{"ok":true,"total":2,"archived":false,"id":"r1"}`, true},
	} {
		var s setCalls
		s.reset(newFlowGuard([]*models.TestCase{guardCase("test-1", `{"item":"pen"}`, recorded)}, nil, []*models.Mock{guardMock(answer)}))
		rest := s.take(context.Background(), []models.UnmatchedCall{startUpCall()}, "test-1", live(c.live))
		if got := len(rest) == 1; got != c.charged {
			t.Fatalf("live %s: charged %v (kept %+v, listed %+v); want %v", c.live, got, rest, s.backgroundCalls(), c.charged)
		}
		if !c.charged && (len(s.backgroundCalls()) != 1 || s.backgroundCalls()[0].BackgroundWhy != madeBeforeFirst+", and its recorded answer reaches no test") {
			t.Fatalf("live %s: listed %+v; want the call as background work", c.live, s.backgroundCalls())
		}
	}
}

// A word the test was given otherwise (its request) did not come from the
// call.
func TestFlowGuard_AWordTheTestWasGivenReachesNothing(t *testing.T) {
	var s setCalls
	s.reset(newFlowGuard([]*models.TestCase{guardCase("test-1", `{"tenant":"tenantA7K29Q"}`, `{"tenant":"tenantA7K29Q"}`)}, nil,
		[]*models.Mock{guardMock(`{"tenant":"tenantA7K29Q"}`)}))
	if rest := s.take(context.Background(), []models.UnmatchedCall{startUpCall()}, "test-1", live(`{"tenant":""}`)); len(rest) != 0 {
		t.Fatalf("the test keeps %+v; a word its own request gave it is not the start-up call's", rest)
	}
}

// A call made between two tests whose recorded answer gave a word the next
// test changed is that test's miss; one whose answer did not is background
// work, saying where it was made.
func TestFlowGuard_ABetweenTestsCallIsTheNextTestsWhenItsAnswerReachesIt(t *testing.T) {
	for _, c := range []struct {
		live    string
		charged bool
	}{{`{"tenant":""}`, true}, {`{"tenant":"tenantA7K29Q"}`, false}} {
		var s setCalls
		s.reset(newFlowGuard([]*models.TestCase{guardCase("test-1", `{}`, `{}`), guardCase("test-2", `{"item":"pen"}`, `{"tenant":"tenantA7K29Q"}`)}, nil,
			[]*models.Mock{guardMock(`{"tenant":"tenantA7K29Q"}`)}))
		s.take(context.Background(), nil, "test-1", live(`{}`))
		bg := betweenTests("GET /boot")
		rest := s.take(context.Background(), []models.UnmatchedCall{bg}, "test-2", live(c.live))
		if c.charged {
			if len(rest) != 1 || rest[0].Background || !strings.Contains(rest[0].ReachesTest, "after test-1 was read and before test-2 began") {
				t.Fatalf("the test keeps %+v; want the between-tests call, charged and no longer background", rest)
			}
			continue
		}
		if len(rest) != 0 || len(s.backgroundCalls()) != 1 || !strings.Contains(s.backgroundCalls()[0].BackgroundWhy, "reaches no test") {
			t.Fatalf("kept %+v, listed %+v; want the call as background work", rest, s.backgroundCalls())
		}
	}
}

// A call made after the set's last test reaches no test, whatever its answer.
func TestFlowGuard_AnAfterLastCallReachesNoTest(t *testing.T) {
	var s setCalls
	s.reset(newFlowGuard([]*models.TestCase{guardCase("test-1", `{}`, `{"tenant":"tenantA7K29Q"}`)}, nil,
		[]*models.Mock{guardMock(`{"tenant":"tenantA7K29Q"}`)}))
	s.take(context.Background(), nil, "test-1", live(`{"tenant":""}`))
	calls, _ := afterLastBackground()
	if rest := s.take(context.Background(), calls, "", nil); len(rest) != 0 {
		t.Fatalf("the set end keeps %+v; want none", rest)
	}
	if got := s.backgroundCalls(); len(got) != 1 || got[0].BackgroundWhy != madeAfterLast {
		t.Fatalf("the set lists %+v; want the call, made after the last test", got)
	}
}

// A marked background series' call made inside a test's window is guarded
// against that test, as a call made between two tests is against the next:
// one whose recorded answer gave a word the test changed is the test's miss,
// named as made inside its window; one whose answer did not is background
// work.
func TestFlowGuard_AMarkedSeriesCallInsideATestsWindowIsTheTestsWhenItsAnswerReachesIt(t *testing.T) {
	for _, c := range []struct {
		live    string
		charged bool
	}{{`{"tenant":""}`, true}, {`{"tenant":"tenantA7K29Q"}`, false}} {
		var s setCalls
		s.reset(newFlowGuard([]*models.TestCase{guardCase("test-1", `{"item":"pen"}`, `{"tenant":"tenantA7K29Q"}`)}, nil,
			[]*models.Mock{guardMock(`{"tenant":"tenantA7K29Q"}`)}))
		rest := s.take(context.Background(), []models.UnmatchedCall{inTestSeries("GET /boot")}, "test-1", live(c.live))
		if c.charged {
			if len(rest) != 1 || rest[0].Background || !strings.Contains(rest[0].ReachesTest, madeInside("test-1")) {
				t.Fatalf("the test keeps %+v; want the series' call, charged, named as made inside test-1's window", rest)
			}
			continue
		}
		if len(rest) != 0 || len(s.backgroundCalls()) != 1 || s.backgroundCalls()[0].BackgroundWhy != madeInside("test-1")+", and its recorded answer reaches no test" {
			t.Fatalf("kept %+v, listed %+v; want the series' call as background work made inside test-1's window", rest, s.backgroundCalls())
		}
	}
}

// A word one of the test's own recordings gave it did not come from the
// call, though the call's closest recording holds it too.
func TestFlowGuard_AWordTheTestsOwnRecordingGaveReachesNothing(t *testing.T) {
	own := guardMock(`{"tenant":"tenantA7K29Q"}`)
	own.Name = "mock-30"
	var s setCalls
	s.reset(newFlowGuard([]*models.TestCase{guardCase("test-1", `{"item":"pen"}`, `{"tenant":"tenantA7K29Q"}`)},
		map[string][]models.MockEntry{"test-1": {{Name: "mock-30"}}}, []*models.Mock{guardMock(`{"tenant":"tenantA7K29Q"}`), own}))
	if rest := s.take(context.Background(), []models.UnmatchedCall{startUpCall()}, "test-1", live(`{"tenant":""}`)); len(rest) != 0 {
		t.Fatalf("the test keeps %+v; a word its own recording mock-30 gave it is not the start-up call's", rest)
	}
	if got := s.backgroundCalls(); len(got) != 1 || got[0].BackgroundWhy != madeBeforeFirst+", and its recorded answer reaches no test" {
		t.Fatalf("the set lists %+v; want the start-up call as background work", got)
	}
}

// The call's closest recording is no word the test was given, even when the
// test owns it: a test whose recording the application took before the test
// began lost that recording's word to the call, so the call is the test's.
func TestFlowGuard_TheCallsClosestRecordingGivesTheTestNothingThoughItOwnsIt(t *testing.T) {
	var s setCalls
	s.reset(newFlowGuard([]*models.TestCase{guardCase("test-1", `{"item":"pen"}`, `{"tenant":"tenantA7K29Q"}`)},
		map[string][]models.MockEntry{"test-1": {{Name: "mock-12"}}}, []*models.Mock{guardMock(`{"tenant":"tenantA7K29Q"}`)}))
	rest := s.take(context.Background(), []models.UnmatchedCall{startUpCall()}, "test-1", live(`{"tenant":""}`))
	if len(rest) != 1 || !strings.Contains(rest[0].ReachesTest, `mock-12 holds "tenantA7K29Q"`) {
		t.Fatalf("the test keeps %+v; want the start-up call charged: its closest recording mock-12 is the answer the test lost", rest)
	}
}

// A gRPC test's response is read as an HTTP test's is.
func TestFlowGuard_AGrpcTestsResponseIsRead(t *testing.T) {
	tc := &models.TestCase{Name: "test-1", Kind: models.GRPC_EXPORT,
		GrpcReq:  models.GrpcReq{Body: models.GrpcLengthPrefixedMessage{DecodedData: `{"item":"pen"}`}},
		GrpcResp: models.GrpcResp{Body: models.GrpcLengthPrefixedMessage{DecodedData: `{"tenant":"tenantA7K29Q"}`}}}
	for _, c := range []struct {
		live    string
		charged bool
	}{{`{"tenant":""}`, true}, {`{"tenant":"tenantA7K29Q"}`, false}} {
		var s setCalls
		s.reset(newFlowGuard([]*models.TestCase{tc}, nil, []*models.Mock{guardMock(`{"tenant":"tenantA7K29Q"}`)}))
		got := &models.GrpcResp{Body: models.GrpcLengthPrefixedMessage{DecodedData: c.live}}
		rest := s.take(context.Background(), []models.UnmatchedCall{startUpCall()}, "test-1", got)
		if charged := len(rest) == 1 && strings.Contains(rest[0].ReachesTest, "this test's response"); charged != c.charged {
			t.Fatalf("live %s: kept %+v; want charged %v", c.live, rest, c.charged)
		}
	}
}

// The instant a test's response was taken at is no value of it: a response
// whose body is as recorded, taken at another instant, changed nothing, though
// the call's recording holds the numbers of that instant.
func TestFlowGuard_TheInstantAResponseWasTakenAtIsNoChange(t *testing.T) {
	tc := guardCase("test-1", `{"item":"pen"}`, `{"ok":true}`)
	tc.HTTPResp.Timestamp = time.Date(2026, 10, 29, 9, 0, 0, 17, time.UTC)
	got := live(`{"ok":true}`)
	got.Timestamp = time.Date(2026, 10, 30, 11, 42, 37, 500000000, time.UTC)
	var s setCalls
	s.reset(newFlowGuard([]*models.TestCase{tc}, nil, []*models.Mock{guardMock(`{"a":"11","b":"42","c":"37","d":"30","e":"5","f":"500000000","g":"2026-10-30T11:42:37.5Z"}`)}))
	if rest := s.take(context.Background(), []models.UnmatchedCall{startUpCall()}, "test-1", got); len(rest) != 0 {
		t.Fatalf("the test keeps %+v; a response taken at another instant changed nothing", rest)
	}
}

// Where keploy cannot tell whether the call's recorded answer reaches the
// test, the call is background work and its reason says keploy could not
// tell, and why: never that its answer reaches no test.
func TestFlowGuard_WhenKeployCannotTellTheReasonSaysSo(t *testing.T) {
	cases := []*models.TestCase{guardCase("test-1", `{"item":"pen"}`, `{"tenant":"tenantA7K29Q"}`)}
	parked := guardMock(`{"tenant":"tenantA7K29Q"}`)
	parked.LeavePayloadOnDisk(noPayloads{}, models.PayloadAt{Name: "mock-12"})
	noClosest := startUpCall()
	noClosest.ClosestMock = ""
	for _, c := range []struct {
		name  string
		guard *flowGuard
		call  models.UnmatchedCall
		want  string
	}{
		{"no guard", nil, startUpCall(), "the dataflow guard did not run for this set"},
		{"no closest recording", newFlowGuard(cases, nil, []*models.Mock{guardMock(`{"tenant":"tenantA7K29Q"}`)}), noClosest, "no recording is closest to it"},
		{"not stored", newFlowGuard(cases, nil), startUpCall(), "its closest recording mock-12 is not in the set's stored recordings"},
		{"unreadable", newFlowGuard(cases, nil, []*models.Mock{parked}), startUpCall(), "its closest recording mock-12 could not be read back"},
	} {
		var s setCalls
		s.reset(c.guard)
		if rest := s.take(context.Background(), []models.UnmatchedCall{c.call}, "test-1", live(`{"tenant":""}`)); len(rest) != 0 {
			t.Fatalf("%s: the test keeps %+v; want the call as background work", c.name, rest)
		}
		want := madeBeforeFirst + ", and keploy could not tell whether its recorded answer reaches a test: " + c.want
		if got := s.backgroundCalls(); len(got) != 1 || got[0].BackgroundWhy != want || strings.Contains(got[0].BackgroundWhy, "reaches no test") {
			t.Fatalf("%s: the set lists %+v; want it saying %q", c.name, got, want)
		}
	}
}

// A call made before the set's first test that no test's read took (the
// first read was cut short, or no test of the set was read) reaches keploy
// at the set's end, which reads it for no test (unclaimedCalls: take with
// no test and no response). Whatever the guard holds, it cannot say whether
// the call's recorded answer reaches a test, and the call's reason says so,
// and why: never that its answer reaches no test.
func TestFlowGuard_ACallReadOnlyAtTheSetsEndSaysKeployCouldNotTell(t *testing.T) {
	var s setCalls
	s.reset(newFlowGuard([]*models.TestCase{guardCase("test-1", `{"item":"pen"}`, `{"tenant":"tenantA7K29Q"}`)}, nil,
		[]*models.Mock{guardMock(`{"tenant":"tenantA7K29Q"}`)}))
	if rest := s.take(context.Background(), []models.UnmatchedCall{startUpCall()}, "", nil); len(rest) != 0 {
		t.Fatalf("the set's end keeps %+v as unclaimed; want the call as background work", rest)
	}
	want := madeBeforeFirst + ", and keploy could not tell whether its recorded answer reaches a test: it reached keploy only at the set's end, after every test was read"
	if got := s.backgroundCalls(); len(got) != 1 || got[0].BackgroundWhy != want {
		t.Fatalf("the set lists %+v; want it saying %q", got, want)
	}
}

// The guard holds one test's words at a time: what it read for a test is
// dropped as it reads the next, so what it keeps does not grow with the
// set (keploy's no-unbounded-memory rule). 64 tests, each with a 96 KiB
// response of its own words, one of which changed this run, keep about what
// one does.
func TestFlowGuard_KeepsOneTestsWordsAtATime(t *testing.T) {
	const n, words = 64, 8000
	body := func(prefix string, i int) string {
		var b strings.Builder
		b.WriteString(`{"words":"`)
		for w := 0; w < words; w++ {
			fmt.Fprintf(&b, "%s%02dx%05d ", prefix, i, w)
		}
		b.WriteString(`"}`)
		return b.String()
	}
	cases := make([]*models.TestCase, n)
	for i := range cases {
		cases[i] = guardCase(fmt.Sprintf("test-%d", i+1), `{"item":"pen"}`, body("rec", i))
	}
	var s setCalls
	g := newFlowGuard(cases, nil, []*models.Mock{guardMock(`{"tenant":"tenantA7K29Q"}`)})
	s.reset(g)
	heap := func() uint64 {
		runtime.GC()
		runtime.GC()
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		return ms.HeapAlloc
	}
	guard := func(i int) {
		test := fmt.Sprintf("test-%d", i+1)
		got := strings.Replace(body("rec", i), "rec", "got", 1)
		if rest := s.take(context.Background(), []models.UnmatchedCall{startUpCall()}, test, live(got)); len(rest) != 0 {
			t.Fatalf("%s keeps %+v; the start-up call's answer holds none of its words", test, rest)
		}
		if g.dcTest != test {
			t.Fatalf("the guard's words are %q's after reading %s", g.dcTest, test)
		}
	}
	base := heap()
	guard(0)
	one := heap()
	for i := 1; i < n; i++ {
		guard(i)
	}
	all := heap()
	if one <= base {
		t.Fatalf("the row's premise: one test's words take room on the heap (%d -> %d)", base, one)
	}
	perTest := one - base
	if all > base && all-base > 3*perTest+(1<<20) {
		t.Fatalf("the guard keeps %d bytes after %d tests, %d after one: it grows with the set", all-base, n, perTest)
	}
}

// The application stamps its answer with its clock (an HTTP Date header) in
// each run, inside the span from the test's request to its answer: this
// run's stamp is no word a start-up call's recording gave the test, however
// its minute or second falls. Read by F's lead alone, the stamp's second (12)
// was a word of what changed, the start-up answer held 12 by chance (here, in
// its length), and the call, made before the set's first test, was charged to
// it: the compose set-end lane's restart scenario, red one replay in eight. An
// instant the start-up answer gave, which the response lost this run, made
// outside the test's span, still charges it.
func TestFlowGuard_TheClockStampOfTheTestsAnswerInItsSpanIsNoWordOfAStartUpCall(t *testing.T) {
	date := func(at time.Time) map[string]string {
		return map[string]string{"Date": at.UTC().Format(time.RFC1123), "Content-Type": "application/json"}
	}
	recAt := time.Date(2026, 10, 30, 15, 21, 46, 310000000, time.UTC)
	runAt := time.Date(2026, 10, 30, 15, 24, 12, 792000000, time.UTC)
	for _, c := range []struct {
		name             string
		recBody, runBody string
		boot             string
		charged          string
	}{
		{name: "the stamp alone changed", recBody: `{"price":42}`, runBody: `{"price":42}`,
			boot: `{"published":{"msg":"b"},"length":"12"}`},
		{name: "a start-up instant the response lost", recBody: `{"price":42,"booted":"Fri, 30 Oct 2026 15:21:40 GMT"}`, runBody: `{"price":42,"booted":""}`,
			boot: `{"booted":"Fri, 30 Oct 2026 15:21:40 GMT"}`, charged: "this test's response"},
	} {
		t.Run(c.name, func(t *testing.T) {
			tc := guardCase("get-price-r1a-1", "", c.recBody)
			tc.HTTPReq.Timestamp, tc.HTTPResp.Timestamp = recAt, recAt.Add(2*time.Millisecond)
			tc.HTTPResp.Header = date(recAt)
			got := live(c.runBody)
			got.Header, got.Timestamp = date(runAt), runAt.Add(3*time.Millisecond)
			var s setCalls
			s.reset(newFlowGuard([]*models.TestCase{tc}, nil, []*models.Mock{guardMock(c.boot)}))
			rest := s.take(context.Background(), []models.UnmatchedCall{startUpCall()}, "get-price-r1a-1",
				ranLive{resp: got, from: runAt, to: runAt.Add(4 * time.Millisecond)})
			if c.charged == "" {
				if len(rest) != 0 {
					t.Fatalf("the test keeps %+v; its answer's clock stamp, made in its span in each run, came from no start-up call", rest)
				}
				if bg := s.backgroundCalls(); len(bg) != 1 || !strings.Contains(bg[0].BackgroundWhy, madeBeforeFirst) {
					t.Fatalf("the set lists %+v; want the start-up call among its background calls, made before its first test", bg)
				}
				return
			}
			if len(rest) != 1 || !strings.Contains(rest[0].ReachesTest, c.charged) {
				t.Fatalf("the test keeps %+v; want the start-up call charged to it, through %s", rest, c.charged)
			}
		})
	}
}

// stampedAt is an HTTP answer's headers, stamped (Date) at at by the
// application's clock, at the second, as an HTTP server writes it.
func stampedAt(at time.Time) map[string]string {
	return map[string]string{"Date": at.UTC().Format(time.RFC1123), "Content-Type": "application/json"}
}

// stamped is a test recorded from recFrom to its answer at recTo, stamped
// with the application's clock at recStamp, and the response it got this run,
// stamped at runStamp, keploy having sent its request at runFrom and read the
// test's misses at runTo.
type stamped struct {
	recFrom, recTo, recStamp time.Time
	runFrom, runTo, runStamp time.Time
	recBody, runBody         string
}

// charged runs the guard on the start-up call, whose recording answered
// boot, for the test of s, with own the test's own calls; the calls the test
// keeps.
func (s stamped) charged(t *testing.T, boot string, own ...models.UnmatchedCall) []models.UnmatchedCall {
	t.Helper()
	tc := guardCase("get-price-r1a-1", "", s.recBody)
	tc.HTTPReq.Timestamp, tc.HTTPResp.Timestamp = s.recFrom, s.recTo
	tc.HTTPResp.Header = stampedAt(s.recStamp)
	got := live(s.runBody)
	got.Header, got.Timestamp = stampedAt(s.runStamp), s.runTo
	var calls setCalls
	calls.reset(newFlowGuard([]*models.TestCase{tc}, nil, []*models.Mock{guardMock(boot)}))
	read := append([]models.UnmatchedCall{startUpCall()}, own...)
	rest := calls.take(context.Background(), read, "get-price-r1a-1", ranLive{resp: got, from: s.runFrom, to: s.runTo})
	var startUp []models.UnmatchedCall
	for _, c := range rest {
		if c.BeforeSet {
			startUp = append(startUp, c)
		}
	}
	return startUp
}

// quick is the compose lane's shape: an answer 2 ms after its request,
// stamped as it is sent, recorded at recAt and run at runAt, the body
// recorded as rec and got as run.
func quick(recAt, runAt time.Time, rec, run string) stamped {
	return stamped{recFrom: recAt, recTo: recAt.Add(2 * time.Millisecond), recStamp: recAt,
		runFrom: runAt, runTo: runAt.Add(4 * time.Millisecond), runStamp: runAt.Add(time.Millisecond), recBody: rec, runBody: run}
}

// The answer's clock stamp is a read of each run's clock at its own place,
// and nowhere else: a start-up answer's number that the first test's body
// carried, and lost this run (the start-up call missed), still charges the
// test when it equals a part of the stamp (its day, hour, minute, second or
// year), as it does when it equals none.
func TestFlowGuard_AStartUpWordTheBodyLostChargesTheTestWhateverTheStampHolds(t *testing.T) {
	recAt := time.Date(2026, 10, 30, 15, 21, 46, 310000000, time.UTC)
	runAt := time.Date(2026, 10, 31, 9, 2, 3, 120000000, time.UTC)
	for _, n := range []string{"30", "15", "21", "46", "2026", "17", "58"} {
		s := quick(recAt, runAt, `{"price":42,"shard":`+n+`}`, `{"price":42,"shard":0}`)
		if rest := s.charged(t, `{"shard":`+n+`}`); len(rest) != 1 || !strings.Contains(rest[0].ReachesTest, n) {
			t.Errorf("shard %s: the test keeps %+v; want the start-up call, its answer's %s lost from the body", n, rest, n)
		}
	}
}

// An instant the start-up answer gave, which the first test's body carried
// and lost this run, charges the test however near the test's span it was
// made, at the nanosecond or at the second: the response's own stamp, at its
// own place, takes none of its words. So does one that this run's body holds
// the application's own clock in, in the test's span, at its place: the
// recorded instant is no read of the clock in the test's span when it was
// made before the second (or written unit) the test's request was sent in,
// however near. One whose written unit overlaps the span (made in the
// request's own second, or in its tenth where trailing zeros are dropped) is
// taken as a read of the clock there, and the call stays background: the
// stated limit, pinned.
func TestFlowGuard_AStartUpInstantTheBodyLostChargesTheTestNearItsSpan(t *testing.T) {
	recAt := time.Date(2026, 10, 30, 15, 21, 46, 310000000, time.UTC)
	runAt := time.Date(2026, 10, 31, 9, 2, 3, 120000000, time.UTC)
	layouts := []string{time.RFC3339Nano, time.RFC1123, time.RFC3339}
	for _, off := range []time.Duration{-30 * time.Second, -5 * time.Second, -1500 * time.Millisecond, -900 * time.Millisecond,
		-300 * time.Millisecond, -10 * time.Millisecond, 0, time.Millisecond} {
		for _, layout := range layouts {
			stamp := recAt.Add(off).UTC().Format(layout)
			s := quick(recAt, runAt, `{"price":42,"since":"`+stamp+`"}`, `{"price":42,"since":""}`)
			if rest := s.charged(t, `{"booted":"`+stamp+`"}`); len(rest) != 1 {
				t.Errorf("a start-up instant %s (%v from the request) lost from the body: the test keeps %+v; want the start-up call", stamp, off, rest)
			}
		}
	}
	for _, c := range []struct {
		off     time.Duration
		charged bool
	}{{-30 * time.Second, true}, {-5 * time.Second, true}, {-1500 * time.Millisecond, true}, {-900 * time.Millisecond, true},
		{-10 * time.Millisecond, false}, {0, false}} {
		for _, layout := range layouts {
			stamp := recAt.Add(c.off).UTC().Format(layout)
			own := runAt.Add(time.Millisecond).UTC().Format(layout)
			s := quick(recAt, runAt, `{"price":42,"since":"`+stamp+`"}`, `{"price":42,"since":"`+own+`"}`)
			rest := s.charged(t, `{"booted":"`+stamp+`"}`)
			switch {
			case c.charged && len(rest) != 1:
				t.Errorf("a start-up instant %s (%v from the request), the app's own clock %s at its place this run: the test keeps %+v; want the start-up call", stamp, c.off, own, rest)
			case !c.charged && len(rest) != 0:
				t.Errorf("a start-up instant %s (%v from the request, its written unit in the span), the app's own clock %s at its place this run: the test keeps %+v; want it read as the clock (the stated limit)", stamp, c.off, own, rest)
			}
		}
	}
}

// The stamp alone changed: whatever instant the test ran at, the start-up
// answer's numbers (a length, a version, a list) are no word of it.
func TestFlowGuard_TheAnswersStampAloneNeverChargesAStartUpCall(t *testing.T) {
	recAt := time.Date(2026, 10, 30, 15, 21, 46, 310000000, time.UTC)
	boots := []string{`{"published":{"msg":"b"},"length":"12"}`, `{"ok":true,"version":"3.47"}`, `{"n":21,"m":7,"id":"a-15"}`,
		`{"seq":[1,2,3,4,5,6,7,8,9,10,11,12,13,14,15,16,17,18,19,20,21,22,23,24,25,26,27,28,29,30,31,32,33,34,35,36,37,38,39,40,41,42,43,44,45,46,47,48,49,50,51,52,53,54,55,56,57,58,59]}`}
	for _, boot := range boots {
		// Each second of two minutes, then each minute of two hours.
		for _, step := range []struct {
			n   int
			per time.Duration
		}{{120, time.Second}, {120, time.Minute}} {
			for i := 0; i < step.n; i++ {
				runAt := recAt.Add(3*time.Minute + time.Duration(i)*step.per + time.Duration(i%997)*time.Millisecond)
				if rest := quick(recAt, runAt, `{"price":42}`, `{"price":42}`).charged(t, boot); len(rest) != 0 {
					t.Fatalf("run at %v, start-up answer %s: the test keeps %+v; the answer's stamp came from no start-up call", runAt, boot, rest)
				}
			}
		}
	}
}

// The span's edges. A test whose span either run does not know (a stamp
// missing or out of order) reads its answer's stamp by the lead alone, so a
// start-up answer holding a word of it charges the test (failing closed). A
// request the test sent never takes its answer's span. An instant a
// start-up answer gave as now plus an offset is no read of the clock in the
// span, and charges the test. A slow answer, stamped at its end past the
// second its request was sent in, is in the span in either run.
func TestFlowGuard_TheSpanOfATestsAnswerAtItsEdges(t *testing.T) {
	recAt := time.Date(2026, 10, 30, 15, 21, 46, 310000000, time.UTC)
	runAt := time.Date(2026, 10, 31, 9, 2, 3, 120000000, time.UTC)
	stampSecond := `{"length":"46"}` // the recorded stamp's second
	t.Run("no recorded request stamp", func(t *testing.T) {
		s := quick(recAt, runAt, `{"price":42}`, `{"price":42}`)
		s.recFrom = time.Time{}
		if rest := s.charged(t, stampSecond); len(rest) != 1 {
			t.Fatalf("the test keeps %+v; with no recorded span its stamp is read by the lead alone", rest)
		}
	})
	t.Run("no recorded answer stamp", func(t *testing.T) {
		s := quick(recAt, runAt, `{"price":42}`, `{"price":42}`)
		s.recTo = time.Time{}
		if rest := s.charged(t, stampSecond); len(rest) != 1 {
			t.Fatalf("the test keeps %+v; with no recorded span its stamp is read by the lead alone", rest)
		}
	})
	t.Run("this run's send not known", func(t *testing.T) {
		s := quick(recAt, runAt, `{"price":42}`, `{"price":42}`)
		s.runFrom = time.Time{}
		if rest := s.charged(t, stampSecond); len(rest) != 1 {
			t.Fatalf("the test keeps %+v; with no span this run its stamp is read by the lead alone", rest)
		}
	})
	t.Run("recorded stamps out of order", func(t *testing.T) {
		// The answer recorded half a second before its request: inside
		// the stamp's second, so only the refusal keeps it from a span.
		s := quick(recAt, runAt, `{"price":42}`, `{"price":42}`)
		s.recFrom = recAt.Add(500 * time.Millisecond)
		if rest := s.charged(t, stampSecond); len(rest) != 1 {
			t.Fatalf("the test keeps %+v; an answer recorded before its request gives no span", rest)
		}
	})
	t.Run("a request the test sent", func(t *testing.T) {
		// The test sent an instant a start-up answer gave, inside the
		// test's span in each run: the request is read by the lead alone.
		sent := recAt.Add(time.Millisecond).Format(time.RFC3339Nano)
		own := models.UnmatchedCall{Protocol: "HTTP", ActualSummary: "POST /quote", Kind: models.MissInTest, ClosestMock: "mock-7",
			ClosestMockReq: `{"since":"` + sent + `"}`, ReceivedReq: `{"since":"` + runAt.Add(time.Millisecond).Format(time.RFC3339Nano) + `"}`}
		s := quick(recAt, runAt, `{"price":42}`, `{"price":42}`)
		if rest := s.charged(t, `{"lease":"`+sent+`"}`, own); len(rest) != 1 || !strings.Contains(rest[0].ReachesTest, "the request this test sent") {
			t.Fatalf("the test keeps %+v; want the start-up call, through the request the test sent", rest)
		}
	})
	t.Run("a start-up instant now plus an hour", func(t *testing.T) {
		exp := recAt.Add(time.Hour).Format(time.RFC1123)
		s := quick(recAt, runAt, `{"price":42,"expires":"`+exp+`"}`, `{"price":42,"expires":"`+runAt.Add(time.Hour).Format(time.RFC1123)+`"}`)
		if rest := s.charged(t, `{"expires":"`+exp+`"}`); len(rest) != 1 {
			t.Fatalf("the test keeps %+v; an instant an hour past the span is no read of the clock in it", rest)
		}
	})
	t.Run("a slow answer recorded", func(t *testing.T) {
		s := quick(recAt, runAt, `{"price":42}`, `{"price":42}`)
		s.recTo = recAt.Add(2590 * time.Millisecond)
		s.recStamp = s.recTo // 15:21:48
		if rest := s.charged(t, `{"length":"48"}`); len(rest) != 0 {
			t.Fatalf("the test keeps %+v; the stamp at the slow answer's end is in its span", rest)
		}
	})
	t.Run("a slow answer this run", func(t *testing.T) {
		s := quick(recAt, runAt, `{"price":42}`, `{"price":42}`)
		s.runTo = runAt.Add(2780 * time.Millisecond)
		s.runStamp = s.runTo.Add(-time.Millisecond) // 09:02:05
		if rest := s.charged(t, `{"length":"05"}`); len(rest) != 0 {
			t.Fatalf("the test keeps %+v; the stamp at the slow answer's end is in its span", rest)
		}
	})
}

// Two instants of one layout in each run, the answer's Date stamp and a body
// stamp (RFC1123 both), are each read against the recorded one at its own
// place: the k-th of the layout in each. A slow answer stamped as it ends
// (recorded 15:21:48, this run 09:02:05), its body stamped at the request
// (15:21:46, 09:02:03), all in their spans: a start-up answer holding the
// second of either recorded instant reaches nothing. A body instant whose
// recorded side is in its span and this run's is not (made five seconds
// before this run's request) is no read of the clock in the span, and its
// lost second charges the test.
func TestFlowGuard_EachInstantOfALayoutIsReadAtItsOwnPlace(t *testing.T) {
	recAt := time.Date(2026, 10, 30, 15, 21, 46, 310000000, time.UTC)
	runAt := time.Date(2026, 10, 31, 9, 2, 3, 120000000, time.UTC)
	slow := func(runBody time.Time) stamped {
		return stamped{
			recFrom: recAt, recTo: recAt.Add(2590 * time.Millisecond), recStamp: recAt.Add(2590 * time.Millisecond),
			runFrom: runAt, runTo: runAt.Add(2780 * time.Millisecond), runStamp: runAt.Add(2779 * time.Millisecond),
			recBody: `{"price":42,"at":"` + recAt.Format(time.RFC1123) + `"}`,
			runBody: `{"price":42,"at":"` + runBody.Format(time.RFC1123) + `"}`,
		}
	}
	for _, boot := range []string{`{"length":"48"}`, `{"length":"46"}`} {
		if rest := slow(runAt).charged(t, boot); len(rest) != 0 {
			t.Errorf("start-up answer %s: the test keeps %+v; each stamp, in its span in each run at its own place, came from no start-up call", boot, rest)
		}
	}
	if rest := slow(runAt.Add(-5*time.Second)).charged(t, `{"length":"46"}`); len(rest) != 1 || !strings.Contains(rest[0].ReachesTest, "46") {
		t.Errorf("the test keeps %+v; want the start-up call, the body's recorded 46 lost to an instant before this run's span", rest)
	}
}

// A gRPC answer's clock stamp (a date header) is read in its span as an
// HTTP answer's is; a start-up word its body lost still charges the test.
// keploy's own read time of the answer (its Timestamp, set this run) is no
// word of it.
func TestFlowGuard_AGRPCAnswersStampIsReadInItsSpan(t *testing.T) {
	recAt := time.Date(2026, 10, 30, 15, 21, 46, 310000000, time.UTC)
	runAt := time.Date(2026, 10, 31, 9, 2, 3, 120000000, time.UTC)
	answer := func(at time.Time, body string) models.GrpcResp {
		return models.GrpcResp{
			Headers:  models.GrpcHeaders{PseudoHeaders: map[string]string{":status": "200"}, OrdinaryHeaders: map[string]string{"date": at.UTC().Format(time.RFC1123), "content-type": "application/grpc"}},
			Body:     models.GrpcLengthPrefixedMessage{MessageLength: uint32(len(body)), DecodedData: body},
			Trailers: models.GrpcHeaders{OrdinaryHeaders: map[string]string{"grpc-status": "0"}},
		}
	}
	run := func(t *testing.T, rec, got string, boot string) []models.UnmatchedCall {
		tc := &models.TestCase{Name: "get-price-r1a-1", Kind: models.GRPC_EXPORT,
			GrpcReq:  models.GrpcReq{Headers: models.GrpcHeaders{PseudoHeaders: map[string]string{":path": "/price.Price/Get"}}, Timestamp: recAt},
			GrpcResp: answer(recAt, rec)}
		tc.GrpcResp.Timestamp = recAt.Add(2 * time.Millisecond)
		now := answer(runAt.Add(time.Millisecond), got)
		now.Timestamp = runAt.Add(3 * time.Millisecond)
		var calls setCalls
		calls.reset(newFlowGuard([]*models.TestCase{tc}, nil, []*models.Mock{guardMock(boot)}))
		return calls.take(context.Background(), []models.UnmatchedCall{startUpCall()}, "get-price-r1a-1",
			ranLive{resp: &now, from: runAt, to: runAt.Add(4 * time.Millisecond)})
	}
	if rest := run(t, "price:42", "price:42", `{"length":"46"}`); len(rest) != 0 {
		t.Fatalf("the test keeps %+v; its answer's date stamp, in its span in each run, came from no start-up call", rest)
	}
	// keploy read this run's answer at 2026-10-31T09:02:03.123Z: a start-up
	// answer holding its year, day, minute or millisecond reaches nothing.
	for _, boot := range []string{`{"n":2026}`, `{"n":31}`, `{"n":2}`, `{"n":123}`} {
		if rest := run(t, "price:42", "price:42", boot); len(rest) != 0 {
			t.Fatalf("start-up answer %s: the test keeps %+v; keploy's read time of the answer is no word of it", boot, rest)
		}
	}
	if rest := run(t, "price:42 shard:46", "price:42 shard:0", `{"shard":46}`); len(rest) != 1 {
		t.Fatalf("the test keeps %+v; want the start-up call, its answer's 46 lost from the body", rest)
	}
}

// stampingHooks answer each test as an application that stamps its answer
// (Date) with its clock as it starts answering, and sends it in the next
// second.
type stampingHooks struct{ prHooks }

func (stampingHooks) SimulateRequest(context.Context, *models.TestCase, string) (interface{}, error) {
	stamp := time.Now()
	time.Sleep(time.Until(stamp.Truncate(time.Second).Add(time.Second + 50*time.Millisecond)))
	return &models.HTTPResp{StatusCode: 200, Header: map[string]string{"Date": stamp.UTC().Format(http.TimeFormat)},
		Body: `{"ok":true}`, Timestamp: time.Now()}, nil
}

// keploy test hands the guard the span the test ran in this run, from its
// send to its read of the test's misses: the application's stamp, made as
// it started answering, is no start-up call's word, however the recorded
// stamp's second falls. Handed the bare response, or a span from the read
// alone, the stamp's second was a word of what changed, and the start-up
// answer that held it charged the test.
func TestRunTestSet_TheAnswersClockStampChargesNoStartUpCall(t *testing.T) {
	provenOn(t, true)
	h := newPartialRunHarness(t, 1, 0)
	h.replayer.hookImpl = stampingHooks{}
	// Recorded a day, an hour, a minute and seven seconds before: no word of
	// its stamp is this run's.
	recAt := time.Now().Add(-(25*time.Hour + time.Minute + 7*time.Second))
	tc := h.cases[0]
	tc.HTTPReq.Timestamp, tc.HTTPResp.Timestamp = recAt, recAt.Add(2*time.Millisecond)
	tc.HTTPResp.Header = map[string]string{"Date": recAt.UTC().Format(http.TimeFormat)}
	h.mocks.filtered = []*models.Mock{guardMock(fmt.Sprintf(`{"length":"%02d"}`, recAt.UTC().Second()))}
	boot := startUpCall()
	h.instr.mockErrorsFor = misses(reads(boot))

	h.run(t)
	rep := h.report.report
	if len(rep.Tests) != 1 {
		t.Fatalf("results %+v; want the one test", rep.Tests)
	}
	for _, c := range rep.Tests[0].FailureInfo.UnmatchedCalls {
		if c.BeforeSet {
			t.Fatalf("the test is charged %+v; its answer's clock stamp came from no start-up call", c)
		}
	}
	if len(rep.BackgroundCalls) != 1 || rep.BackgroundCalls[0].ActualSummary != boot.ActualSummary ||
		!strings.Contains(rep.BackgroundCalls[0].BackgroundWhy, madeBeforeFirst) {
		t.Fatalf("background calls %+v; want the start-up call, made before the set's first test", rep.BackgroundCalls)
	}
}
