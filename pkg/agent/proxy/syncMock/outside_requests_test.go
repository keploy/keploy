package manager

import (
	"testing"
	"time"

	"go.uber.org/zap"

	"go.keploy.io/server/v3/pkg/models"
)

// addCall hands the manager a call that ran over [req, res], as a parser does,
// and says whether it came out marked as made outside every request.
func addCall(m *SyncMockManager, req, res time.Time) bool {
	mock := &models.Mock{Kind: models.Mongo}
	mock.Spec.ReqTimestampMock, mock.Spec.ResTimestampMock = req, res
	m.AddMock(mock)
	return mock.OutsideRequests
}

// endedWindow runs one request through the manager and says when it had ended.
func endedWindow(t *testing.T, m *SyncMockManager) time.Time {
	t.Helper()
	w := m.OpenWindow(time.Now(), nil)
	if w == nil {
		t.Fatal("no window opened")
	}
	w.Close()
	return time.Now()
}

func TestOutsideRequestsPollBetweenRequestsIsMarked(t *testing.T) {
	m := New(zap.NewNop())
	ended := endedWindow(t, m)
	if !addCall(m, ended.Add(time.Second), ended.Add(2*time.Second)) {
		t.Fatal("a call made after the last request ended, with none open, is not marked")
	}
}

// Every way a window ends tells the same: the kept resolve here, the close above.
func TestOutsideRequestsAfterAKeptRequestIsMarked(t *testing.T) {
	m := New(zap.NewNop())
	start := time.Now()
	w := m.OpenWindow(start, nil)
	if !w.Keep() {
		t.Fatal("a window just opened was given up")
	}
	m.ResolveKept(w, start, time.Now(), "test-1", false)
	ended := time.Now()
	if !addCall(m, ended.Add(time.Second), ended.Add(2*time.Second)) {
		t.Fatal("a call made after a kept request ended is not marked")
	}
}

func TestOutsideRequestsCallDuringAnOpenRequestIsNotMarked(t *testing.T) {
	m := New(zap.NewNop())
	ended := endedWindow(t, m)
	start := ended.Add(time.Second)
	w := m.OpenWindow(start, nil)
	defer w.Close()

	for name, c := range map[string]struct{ req, res time.Time }{
		"inside it":              {start.Add(time.Millisecond), start.Add(2 * time.Millisecond)},
		"answered as it began":   {start.Add(-time.Millisecond), start},
		"over its start":         {start.Add(-time.Millisecond), start.Add(time.Millisecond)},
		"begun exactly as it is": {start, start.Add(time.Millisecond)},
	} {
		if addCall(m, c.req, c.res) {
			t.Errorf("a call %s is marked outside every request", name)
		}
	}
}

// The oldest open window is the one that counts: a later one must not hide it.
func TestOutsideRequestsOldestOpenRequestCounts(t *testing.T) {
	m := New(zap.NewNop())
	ended := endedWindow(t, m)
	first := ended.Add(time.Second)
	a := m.OpenWindow(first.Add(time.Hour), nil)
	defer a.Close()
	b := m.OpenWindow(first, nil)
	defer b.Close()
	if addCall(m, first.Add(time.Minute), first.Add(2*time.Minute)) {
		t.Fatal("a call during the older of two open requests is marked")
	}
}

// A request read after the call was answered cannot have made it.
func TestOutsideRequestsRequestBegunAfterTheAnswerDoesNotCount(t *testing.T) {
	m := New(zap.NewNop())
	ended := endedWindow(t, m)
	res := ended.Add(2 * time.Second)
	w := m.OpenWindow(res.Add(time.Nanosecond), nil)
	defer w.Close()
	if !addCall(m, ended.Add(time.Second), res) {
		t.Fatal("a request that began after the call's answer kept it from being marked")
	}
}

// The mock of a request's own call can reach the manager after that request has
// ended: its times say it, not when it came.
func TestOutsideRequestsLateMockOfAnEndedRequestIsNotMarked(t *testing.T) {
	m := New(zap.NewNop())
	start := time.Now()
	ended := endedWindow(t, m)
	if addCall(m, start, ended.Add(-time.Nanosecond)) {
		t.Fatal("a call made during a request that has ended since is marked")
	}
	if addCall(m, m.lastWindowEnd, m.lastWindowEnd.Add(time.Second)) {
		t.Fatal("a call begun as the last request ended is marked")
	}
}

// An agent whose ingress opens no window, the proxyless one, says nothing.
func TestOutsideRequestsNoWindowEverMarksNothing(t *testing.T) {
	m := New(zap.NewNop())
	now := time.Now()
	if addCall(m, now.Add(time.Second), now.Add(2*time.Second)) {
		t.Fatal("a manager that never saw a window marked a call")
	}
}

func TestOutsideRequestsWindowlessIngressMarksNothingFromThenOn(t *testing.T) {
	m := New(zap.NewNop())
	ended := endedWindow(t, m)
	m.NoteWindowlessIngress()
	if addCall(m, ended.Add(time.Second), ended.Add(2*time.Second)) {
		t.Fatal("a call was marked after a request that opens no window came")
	}
	ended = endedWindow(t, m)
	if addCall(m, ended.Add(time.Second), ended.Add(2*time.Second)) {
		t.Fatal("a window ending lifted the latch")
	}
}

func TestOutsideRequestsNeedsBothTimes(t *testing.T) {
	m := New(zap.NewNop())
	ended := endedWindow(t, m)
	if addCall(m, time.Time{}, ended.Add(time.Second)) {
		t.Error("a call with no request time is marked")
	}
	if addCall(m, ended.Add(time.Second), time.Time{}) {
		t.Error("a call with no answer time is marked")
	}
}

func TestOutsideRequestsNilMockAndManagerAreSafe(t *testing.T) {
	m := New(zap.NewNop())
	endedWindow(t, m)
	m.stampOutsideRequestsLocked(nil)
	var none *SyncMockManager
	none.NoteWindowlessIngress()
}
