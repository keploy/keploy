package http

import (
	"context"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// A call answered with a recording its request matches exactly learns
// nothing, so the exchange the tracker keeps marks, at each request leaf the
// recording's earlier learning learned as a read of the application's clock
// (its Spec.Clocks, which the strict comparison checks), that read: plain
// (Seen.Clock) or plus an offset (Seen.Offset), as a paired call's learn
// step marks the reads it learned. A later call's learn step then reads a
// moment sent there as a read of the clock, never as a carried value: a
// last-use touch's second, sent again in the same second by a later write,
// is a copy of a read, which the time default checks by its form. A leaf no
// clock names stays unmarked. Both exact serves mark them: a write's, and a
// read of what the test wrote (the read-back serve: a GET of a resource).
func TestLearnMatchAnExactServeMarksTheRecordingsClockReads(t *testing.T) {
	const touch = `{"data":{"lastUsedAt":1794899281,"name":"k"}}`
	for _, row := range []struct {
		what, method, target, body, leaf string
		off                              models.ClockOffset
		plain                            bool
	}{
		{"a write's plain read", "PUT", "/v1/keys/k", touch, "body.data.lastUsedAt", models.ClockOffset{Res: time.Second}, true},
		{"a write's read cut to its second", "PUT", "/v1/keys/k", touch, "body.data.lastUsedAt", models.ClockOffset{Min: -time.Second, Res: time.Second}, false},
		{"a read-back's read cut to its second", "GET", "/v1/keys?since=1794899281", ``, "url.query.since", models.ClockOffset{Min: -time.Second, Res: time.Second}, false},
	} {
		h, n, opts := learnOn(t)
		tr := learn.NewTracker()
		tr.Begin()
		m := learnRecorded("mock-0", 0, row.method, row.target, row.body, nil, `{}`)
		m.Spec.Clocks = map[string]models.ClockOffset{row.leaf: row.off}
		db := &learnDb{perTest: []*models.Mock{m}}
		ctx := learn.WithTracker(context.Background(), tr)
		if served, miss, err := h.learnMatch(ctx, live(row.method, row.target, row.body, nil), db, n, opts); err != nil || served == nil || miss != nil {
			t.Fatalf("%s: the call is served: %v %+v %v", row.what, served, miss, err)
		}
		_, earlier, _, _ := tr.Test()
		if len(earlier) != 1 {
			t.Fatalf("%s: %d earlier exchanges, want the call's", row.what, len(earlier))
		}
		marked, other := 0, 0
		for _, s := range earlier[0].Seen {
			switch {
			case s.Leaf.Path == row.leaf:
				if s.Clock != row.plain || s.Offset == nil || s.Offset.Min != row.off.Min || s.Offset.Max != row.off.Max || s.Offset.Res != row.off.Res {
					t.Errorf("%s: %s marked Clock=%v Offset=%+v; want Clock=%v, the recording's read %+v", row.what, row.leaf, s.Clock, s.Offset, row.plain, row.off)
				}
				marked++
			case s.Clock || s.Offset != nil:
				other++
			}
		}
		if marked != 1 || other != 0 {
			t.Errorf("%s: %d %s places, %d other leaves marked as reads; want 1 and 0", row.what, marked, row.leaf, other)
		}
	}
}

// The read-back serve (a GET of a record the test's PUT wrote, its request
// exactly the recorded one, its answer rewritten with this run's record)
// marks the GET's clock reads too: a header its recording learned as a
// read of the clock cut to its second is marked so, and no other leaf.
func TestLearnMatchAReadBackServeMarksTheRecordingsClockReads(t *testing.T) {
	const sentAt = "1794899281"
	off := models.ClockOffset{Min: -time.Second, Res: time.Second}
	h, _, opts := learnOn(t)
	n := h.newLearnNoise(opts, nil, nil)
	recBegin, began := learnAt.Add(time.Second), learnAt.Add(10*time.Minute)
	tr := learn.NewTracker()
	tr.SetClientTests(true)
	tr.SetAppClock(func(r time.Time) (time.Time, bool) { return r, true })
	tr.RecordedBegin(recBegin)
	tr.BeginAt(began)
	recReq := &models.HTTPReq{Method: "POST", URL: "http://localhost:8083/query", Timestamp: recBegin.Add(100 * time.Millisecond),
		Header: map[string]string{"Content-Type": "application/json"}, Body: kpClient(kpRecBy, kpRecNano)}
	tr.SetCases([]learn.ClientCase{{Name: "post-query-83", Req: recReq}})
	liveReq := *recReq
	liveReq.Body, liveReq.Timestamp = kpClient(kpLiveBy, kpLiveNano), began.Add(100*time.Millisecond)
	if !tr.Incoming(&liveReq, "", liveReq.Timestamp) {
		t.Fatal("the client test's request was not paired with its test case")
	}
	getHeader := map[string]string{"X-Vault-Request": "true", "X-Sent-At": sentAt}
	mocks := kpMocks(kpGetAnswer(kpRecKey, kpRecBy, kpRecNano))
	mocks[1] = learnRecorded("mock-1", 1300*time.Millisecond, "GET", kpPath(kpRecKey), "", getHeader, kpGetAnswer(kpRecKey, kpRecBy, kpRecNano))
	mocks[1].Spec.Clocks = map[string]models.ClockOffset{"header.x-sent-at": off}
	ctx := learn.WithTracker(context.Background(), tr)
	db := &learnDb{perTest: mocks, observe: tr.ServedMock}
	var got string
	for i, c := range []*req{live("PUT", kpPath(kpRecKey), kpPut(kpRecKey, kpLiveBy, kpLiveNano), kpVault), live("GET", kpPath(kpRecKey), "", getHeader)} {
		c.arrived = began.Add(mocks[i].Spec.ReqTimestampMock.Sub(recBegin))
		served, miss, err := h.learnMatch(ctx, c, db, n, opts)
		if err != nil || miss != nil || served == nil {
			t.Fatalf("call %d: served %v miss %+v err %v", i, served != nil, miss, err)
		}
		got = served.Spec.HTTPResp.Body
	}
	if got != kpLiveRecord {
		t.Fatalf("the GET answered\n%s\nwant what this run's PUT stored (the read-back serve)", got)
	}
	_, earlier, _, _ := tr.Test()
	marked, other := 0, 0
	for _, e := range earlier {
		for _, s := range e.Seen {
			switch {
			case e.Mock == "mock-1" && s.Leaf.Path == "header.x-sent-at":
				if s.Clock || s.Offset == nil || *s.Offset != off {
					t.Errorf("the GET's x-sent-at marked Clock=%v Offset=%+v; want the recording's read %+v", s.Clock, s.Offset, off)
				}
				marked++
			case s.Clock || s.Offset != nil:
				other++
			}
		}
	}
	if marked != 1 || other != 0 {
		t.Errorf("%d x-sent-at places, %d other leaves marked as reads; want 1 and 0", marked, other)
	}
}

// servedClocks is the recording's clocks with the call's own learned ones
// over them: each alone is itself, a leaf in both takes the learned read,
// and a leaf in one only keeps it.
func TestServedClocksPutsTheCallsLearnedReadsOverTheRecordings(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	var n learnNoise
	rec := models.ClockOffset{Min: -time.Second, Res: time.Second}
	learned := models.ClockOffset{Res: time.Millisecond}
	m := httpMock("mock-0", "PUT", "/v1/keys/k")
	if got := n.servedClocks(m, map[string]models.ClockOffset{"body.a": learned}); len(got) != 1 || got["body.a"] != learned {
		t.Errorf("no recorded clocks: %+v; want the learned read alone", got)
	}
	m.Spec.Clocks = map[string]models.ClockOffset{"body.a": rec, "body.b": rec}
	if got := n.servedClocks(m, nil); len(got) != 2 || got["body.a"] != rec || got["body.b"] != rec {
		t.Errorf("nothing learned: %+v; want the recording's reads", got)
	}
	got := n.servedClocks(m, map[string]models.ClockOffset{"body.a": learned, "body.c": learned})
	if len(got) != 3 || got["body.a"] != learned || got["body.b"] != rec || got["body.c"] != learned {
		t.Errorf("both: %+v; want body.a and body.c learned, body.b the recording's", got)
	}
	if m.Spec.Clocks["body.a"] != rec || len(m.Spec.Clocks) != 2 {
		t.Errorf("the recording's clocks changed: %+v", m.Spec.Clocks)
	}
}
