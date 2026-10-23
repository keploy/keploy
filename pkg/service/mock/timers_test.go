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
// scope, placed at the test, with the stretch of writes it was recorded in,
// and the load stays the test's, though the two calls are the same.
func TestTimerScopeMovesTheReportedCallWithItsStretch(t *testing.T) {
	poll := timerKey("POST", "localhost:8083", "/query", pollBody)
	mocks := []capturedMock{
		{name: "mock-0", ts: at(1), fp: timerKey("POST", "localhost:8083", "/query", "create"), write: true},
		{name: "mock-1", ts: at(2), fp: poll},
		{name: "mock-2", ts: at(3), fp: timerKey("POST", "localhost:8083", "/query", "toggle"), write: true},
		{name: "mock-3", ts: at(4), fp: poll},
		{name: "mock-4", ts: at(5), fp: timerKey("POST", "localhost:8083", "/query", "delete"), write: true},
	}
	windows := []models.ScopeWindow{{Name: "test-1", Timers: []models.TimerCall{{Method: "POST", Host: "localhost:8083", URL: "/query", Body: pollBody, N: 1}}}}
	byTest := map[string][]models.MockEntry{"test-1": {{Name: "mock-0"}, {Name: "mock-1"}, {Name: "mock-2"}, {Name: "mock-3"}, {Name: "mock-4"}}}
	got, startup, worker := timerScope(windows, mocks, byTest, nil)
	if want := []string{"mock-0", "mock-1", "mock-2", "mock-4"}; !reflect.DeepEqual(entryNamesOf(got["test-1"]), want) {
		t.Fatalf("the test keeps %v, want %v", entryNamesOf(got["test-1"]), want)
	}
	if len(startup) != 0 {
		t.Fatalf("startup %v", entryNamesOf(startup))
	}
	want := []models.MockEntry{{Name: "mock-3", Kind: "Http", Place: "test-1", Cause: models.CauseTimer, Stretch: &models.Stretch{After: "mock-2", Until: "mock-4"}}}
	if !reflect.DeepEqual(worker, want) {
		t.Fatalf("worker %+v, want %+v", worker, want)
	}
}

// A poll recorded between tests (in the start-up pool) of a call a harness
// reported a timer made is the worker scope's too, placed nowhere; a timer's
// own write (a session ping) moves no stretch; and a call before every write
// has its stretch from the set's start.
func TestTimerScopeTakesThePoolsTicksAndSkipsATimersWrites(t *testing.T) {
	poll := timerKey("GET", "localhost:8083", "/jobs", pollBody)
	ping := timerKey("POST", "localhost:8083", "/session/activity", pollBody)
	mocks := []capturedMock{
		{name: "mock-0", ts: at(1), fp: poll},
		{name: "mock-1", ts: at(2), fp: ping, write: true},
		{name: "mock-2", ts: at(3), fp: timerKey("POST", "localhost:8083", "/apps", "x"), write: true},
		{name: "mock-3", ts: at(4), fp: poll},
	}
	windows := []models.ScopeWindow{{Name: "test-1", Timers: []models.TimerCall{
		{Method: "GET", Host: "localhost:8083", URL: "/jobs", Body: pollBody, N: 0},
		{Method: "POST", Host: "localhost:8083", URL: "/session/activity", Body: pollBody, N: 0},
	}}}
	byTest := map[string][]models.MockEntry{"test-1": {{Name: "mock-1"}, {Name: "mock-2"}, {Name: "mock-3"}}}
	got, startup, worker := timerScope(windows, mocks, byTest, []models.MockEntry{{Name: "mock-0"}})
	if want := []string{"mock-2"}; !reflect.DeepEqual(entryNamesOf(got["test-1"]), want) {
		t.Fatalf("the test keeps %v, want %v", entryNamesOf(got["test-1"]), want)
	}
	if len(startup) != 0 {
		t.Fatalf("the pool keeps %v, want none", entryNamesOf(startup))
	}
	stretch, place := map[string]models.Stretch{}, map[string]string{}
	for _, e := range worker {
		stretch[e.Name], place[e.Name] = *e.Stretch, e.Place
	}
	want := map[string]models.Stretch{"mock-0": {Until: "mock-2"}, "mock-1": {Until: "mock-2"}, "mock-3": {After: "mock-2"}}
	if !reflect.DeepEqual(stretch, want) {
		t.Fatalf("stretches %+v, want %+v", stretch, want)
	}
	if want := map[string]string{"mock-0": "", "mock-1": "test-1", "mock-3": "test-1"}; !reflect.DeepEqual(place, want) {
		t.Fatalf("places %v, want %v", place, want)
	}
}

// With no timer reported, nothing moves.
func TestTimerScopeWithNoReportChangesNothing(t *testing.T) {
	byTest := map[string][]models.MockEntry{"test-1": {{Name: "mock-0"}}}
	startup := []models.MockEntry{{Name: "mock-1"}}
	got, pool, worker := timerScope([]models.ScopeWindow{{Name: "test-1"}}, []capturedMock{{name: "mock-0"}, {name: "mock-1"}}, byTest, startup)
	if !reflect.DeepEqual(got, byTest) || !reflect.DeepEqual(pool, startup) || worker != nil {
		t.Fatalf("got %v %v %v", got, pool, worker)
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
