package replay

import (
	"context"
	"fmt"
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
