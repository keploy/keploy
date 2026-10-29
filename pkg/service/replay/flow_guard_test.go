package replay

import (
	"context"
	"strings"
	"testing"

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
		if !c.charged && (len(s.backgroundCalls()) != 1 || s.backgroundCalls()[0].BackgroundWhy != madeBeforeFirst) {
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
