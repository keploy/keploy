package mock

import (
	"reflect"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

func at(s int) time.Time { return time.Date(2026, 10, 23, 10, 0, s, 0, time.UTC) }

const pollBody = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

func entryNamesOf(es []models.MockEntry) []string {
	var out []string
	for _, e := range es {
		out = append(out, e.Name)
	}
	return out
}

// A test whose page fetched the job list on load (its own call) and then
// polled it on a timer: the poll its harness reported moves to the worker
// scope, placed at the test, with the stretch of writes its answer holds in,
// and the load stays the test's, though the two calls are the same.
func TestTimerScopeMovesTheReportedCallWithItsStretch(t *testing.T) {
	poll := timerKey("POST", "localhost:8083", "/query", pollBody)
	mocks := []capturedMock{
		{name: "mock-0", ts: at(1), fp: timerKey("POST", "localhost:8083", "/query", "create"), write: true},
		{name: "mock-1", ts: at(2), fp: poll, ans: "one"},
		{name: "mock-2", ts: at(3), fp: timerKey("POST", "localhost:8083", "/query", "toggle"), write: true},
		{name: "mock-3", ts: at(4), fp: poll, ans: "one"},
		{name: "mock-4", ts: at(5), fp: timerKey("POST", "localhost:8083", "/query", "delete"), write: true},
	}
	windows := []models.ScopeWindow{{Name: "test-1", Timers: []models.TimerCall{{Method: "POST", Host: "localhost:8083", URL: "/query", Body: pollBody, N: 1}}}}
	byTest := map[string][]models.MockEntry{"test-1": {{Name: "mock-0"}, {Name: "mock-1"}, {Name: "mock-2"}, {Name: "mock-3"}, {Name: "mock-4"}}}
	got, worker := timerScope(windows, mocks, byTest)
	if want := []string{"mock-0", "mock-1", "mock-2", "mock-4"}; !reflect.DeepEqual(entryNamesOf(got["test-1"]), want) {
		t.Fatalf("the test keeps %v, want %v", entryNamesOf(got["test-1"]), want)
	}
	want := []models.MockEntry{{Name: "mock-3", Kind: "Http", Place: "test-1", Cause: models.CauseTimer, Stretch: &models.Stretch{After: "mock-2", Until: "mock-4"}}}
	if !reflect.DeepEqual(worker, want) {
		t.Fatalf("worker %+v, want %+v", worker, want)
	}
}

// A write ends a timer's stretch only where the call's recorded answer changed
// across it. Polls that answered alike on both sides of a session's keep-alive
// ping share one stretch, so the ping cuts nothing; the write after which the
// poll answered differently ends it, and the next answer's stretch starts at
// the last write before it. A timer's own write (reported like the polls)
// never ends a stretch, and a poll made between tests stays the pool's.
func TestTimerScopeEndsAStretchOnlyWhereTheAnswerChanged(t *testing.T) {
	poll := timerKey("POST", "localhost:8083", "/query", pollBody)
	ping := timerKey("POST", "localhost:8083", "/auth/session/activity", pollBody)
	ticker := timerKey("POST", "localhost:8083", "/session/heartbeat", pollBody)
	create := timerKey("POST", "localhost:8083", "/query", "create")
	mocks := []capturedMock{
		{name: "mock-0", ts: at(1), fp: poll, ans: "none"},   // between tests: the pool's
		{name: "mock-1", ts: at(2), fp: poll, ans: "none"},   // T1, reported
		{name: "mock-2", ts: at(3), fp: ping, write: true},   // T1 keep-alive
		{name: "mock-3", ts: at(4), fp: poll, ans: "none"},   // T1, reported: same answer
		{name: "mock-4", ts: at(5), fp: ticker, write: true}, // T1, a timer's own write, reported
		{name: "mock-5", ts: at(6), fp: create, write: true}, // T1 creates a job
		{name: "mock-6", ts: at(7), fp: ping, write: true},   // T1 keep-alive
		{name: "mock-7", ts: at(8), fp: poll, ans: "one"},    // T1, reported: the job shows
	}
	windows := []models.ScopeWindow{{Name: "T1", Timers: []models.TimerCall{
		{Method: "POST", Host: "localhost:8083", URL: "/query", Body: pollBody, N: 0},
		{Method: "POST", Host: "localhost:8083", URL: "/query", Body: pollBody, N: 1},
		{Method: "POST", Host: "localhost:8083", URL: "/query", Body: pollBody, N: 2},
		{Method: "POST", Host: "localhost:8083", URL: "/session/heartbeat", Body: pollBody, N: 0},
	}}}
	byTest := map[string][]models.MockEntry{"T1": {{Name: "mock-1"}, {Name: "mock-2"}, {Name: "mock-3"}, {Name: "mock-4"}, {Name: "mock-5"}, {Name: "mock-6"}, {Name: "mock-7"}}}
	got, worker := timerScope(windows, mocks, byTest)
	if want := []string{"mock-2", "mock-5", "mock-6"}; !reflect.DeepEqual(entryNamesOf(got["T1"]), want) {
		t.Fatalf("T1 keeps %v, want %v", entryNamesOf(got["T1"]), want)
	}
	stretch := map[string]models.Stretch{}
	for _, e := range worker {
		stretch[e.Name] = *e.Stretch
	}
	want := map[string]models.Stretch{
		"mock-1": {Until: "mock-5"},
		"mock-3": {Until: "mock-5"},
		"mock-4": {After: "mock-2", Until: "mock-5"},
		"mock-7": {After: "mock-6"},
	}
	if !reflect.DeepEqual(stretch, want) {
		t.Fatalf("stretches %+v, want %+v", stretch, want)
	}
}

// Two answers alike never join when one of them was not kept (its body was
// too large to record): what it held is unknown.
func TestTimerScopeJoinsNoAnswerItCannotRead(t *testing.T) {
	poll := timerKey("GET", "localhost:8083", "/jobs", pollBody)
	mocks := []capturedMock{
		{name: "mock-0", ts: at(1), fp: poll},
		{name: "mock-1", ts: at(2), fp: timerKey("POST", "localhost:8083", "/ping", pollBody), write: true},
		{name: "mock-2", ts: at(3), fp: poll},
	}
	windows := []models.ScopeWindow{{Name: "T1", Timers: []models.TimerCall{
		{Method: "GET", Host: "localhost:8083", URL: "/jobs", Body: pollBody, N: 0},
		{Method: "GET", Host: "localhost:8083", URL: "/jobs", Body: pollBody, N: 1},
	}}}
	_, worker := timerScope(windows, mocks, map[string][]models.MockEntry{"T1": {{Name: "mock-0"}, {Name: "mock-1"}, {Name: "mock-2"}}})
	stretch := map[string]models.Stretch{}
	for _, e := range worker {
		stretch[e.Name] = *e.Stretch
	}
	if want := map[string]models.Stretch{"mock-0": {Until: "mock-1"}, "mock-2": {After: "mock-1"}}; !reflect.DeepEqual(stretch, want) {
		t.Fatalf("stretches %+v, want %+v", stretch, want)
	}
}

// With no timer reported, nothing moves.
func TestTimerScopeWithNoReportChangesNothing(t *testing.T) {
	byTest := map[string][]models.MockEntry{"test-1": {{Name: "mock-0"}}}
	got, worker := timerScope([]models.ScopeWindow{{Name: "test-1"}}, []capturedMock{{name: "mock-0"}, {name: "mock-1"}}, byTest)
	if !reflect.DeepEqual(got, byTest) || worker != nil {
		t.Fatalf("got %v %v", got, worker)
	}
}

// answerOf tells answers apart by status and body, and never names one whose
// body was not kept.
func TestAnswerOf(t *testing.T) {
	a := &models.Mock{Kind: models.HTTP, Spec: models.MockSpec{HTTPResp: &models.HTTPResp{StatusCode: 200, Body: "{}"}}}
	b := &models.Mock{Kind: models.HTTP, Spec: models.MockSpec{HTTPResp: &models.HTTPResp{StatusCode: 500, Body: "{}"}}}
	if answerOf(a) == "" || answerOf(a) == answerOf(b) {
		t.Fatalf("answers %q %q", answerOf(a), answerOf(b))
	}
	if answerOf(&models.Mock{Kind: models.HTTP, Spec: models.MockSpec{HTTPResp: &models.HTTPResp{StatusCode: 200, BodySkipped: true}}}) != "" {
		t.Fatal("an answer whose body was not kept is named")
	}
}

// The fingerprint names a call as the harness does: method, Host, path and
// query, body hash.
func TestCallFingerprintNamesTheCallAsTheHarnessDoes(t *testing.T) {
	mk := &models.Mock{Kind: models.HTTP, Spec: models.MockSpec{HTTPReq: &models.HTTPReq{Method: "GET", URL: "/cluster/list?deploymentType=self-hosted", Header: map[string]string{"Host": "localhost:8083"}}}}
	fp, _ := callFingerprint(mk)
	if want := timerKey("GET", "localhost:8083", "/cluster/list?deploymentType=self-hosted", pollBody); fp != want {
		t.Fatalf("fingerprint %q, want %q", fp, want)
	}
	if fp, write := callFingerprint(&models.Mock{Kind: models.Mongo}); fp != "" || write {
		t.Fatalf("a Mongo mock got %q %v", fp, write)
	}
}
