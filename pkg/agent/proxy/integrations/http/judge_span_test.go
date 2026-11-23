package http

import (
	"context"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
)

// A learned clock read in an HTTP call is checked in the span of the
// recording it is checked against (learn.ClockSpanOf). The test read its
// clock after its own previous serve, then a worker: serve (a timer's
// recording the store serves outside the test) landed before the call
// reached keploy. Checked against the test's own recording, the read is in
// the span from the test's own serve, and the call is answered. Checked
// against a recording the store does not call the test's own, the span
// starts at the worker: serve, as from any flow's serve before, and the read
// is refused, naming the field.
func TestStrictJudgeSpanOfAnOwnRecordingStartsAtTheTestsOwnServe(t *testing.T) {
	began := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	ownServe, read, workerServe, arrival := began.Add(time.Second), began.Add(2*time.Second), began.Add(5*time.Second), began.Add(6*time.Second)
	stamp := func(at time.Time) string { return at.Format(time.RFC3339) }
	recAt := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	usage := func(name string) *models.Mock {
		m := recorded(name, "/v1/usage", map[string]string{"Content-Type": "application/json"}, `{"since":"`+stamp(recAt)+`","q":"x"}`)
		m.Spec.Clocks = map[string]models.ClockOffset{"body.since": {Res: time.Second}}
		return m
	}
	conn := func() context.Context {
		tr := learn.NewTracker()
		tr.SetAppClock(func(r time.Time) (time.Time, bool) { return r, true })
		tr.SetServeOwner(func(m *models.Mock) bool { return m.Name == "own-earlier" || m.Name == "usage-own" })
		tr.NewSetAt(began.Add(-time.Second))
		tr.BeginAt(began)
		tr.ServedMockAt(httpMock("own-earlier", "GET", "/v1/account"), ownServe)
		tr.ServedMockAt(httpMock("worker-tick", "POST", "/v1/reconcile"), workerServe)
		tr.SetLearnEnded(true)
		return learn.WithTracker(context.Background(), tr)
	}
	call := func() *req {
		r := sent("/v1/usage", map[string]string{"Content-Type": "application/json"}, `{"q":"x","since":"`+stamp(read)+`"}`)
		r.arrived = arrival
		return r
	}

	if got, diag := strictMatchIn(t, conn(), call(), nil, nil, nil, usage("usage-own")); got == nil {
		t.Fatalf("checked against the test's own recording: refused (%v); want it answered, its read after the test's own serve", differs(diag))
	}
	got, diag := strictMatchIn(t, conn(), call(), nil, nil, nil, usage("usage-worker"))
	if got != nil || !hasField(differs(diag), "body.since") {
		t.Fatalf("checked against a recording not the test's own: answered %v, differs %v; want it refused naming body.since", got != nil, differs(diag))
	}
}
