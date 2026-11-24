package models

import (
	"testing"
	"time"
)

// A window [request, response] whose response is dated before its request
// ends when the agent received the completed exchange, never before its
// request; any other window is as its stamps say.
func TestInvertedWindowEnd(t *testing.T) {
	t0 := time.Unix(1000, 0)
	cases := []struct {
		name                string
		start, end, arrived time.Time
		want                time.Time
	}{
		{"in order", t0, t0.Add(time.Second), t0.Add(2 * time.Second), t0.Add(time.Second)},
		{"response at its request", t0, t0, t0.Add(time.Second), t0},
		{"no response time", t0, time.Time{}, t0.Add(time.Second), time.Time{}},
		{"no request time", time.Time{}, t0, t0.Add(time.Second), t0},
		{"inverted: ends at its arrival", t0.Add(time.Second), t0, t0.Add(3 * time.Second), t0.Add(3 * time.Second)},
		// The clock read as it arrived is set back past the request: the
		// window ends where it starts, not before.
		{"inverted, arrival read before the request", t0.Add(time.Second), t0, t0.Add(500 * time.Millisecond), t0.Add(time.Second)},
		{"inverted, no arrival", t0.Add(time.Second), t0, time.Time{}, t0.Add(time.Second)},
	}
	for _, c := range cases {
		if got := InvertedWindowEnd(c.start, c.end, c.arrived); !got.Equal(c.want) {
			t.Errorf("%s: InvertedWindowEnd = %v, want %v", c.name, got, c.want)
		}
	}
}

// A test case's window reads the end the agent gave its window
// (TestCase.WindowEnd), by its kind; the received end is given only to one
// whose response is dated before its request and whose capture gave it none,
// and its stamps stay as captured.
func TestTestCaseWindowEndsWhereTheAgentEndedIt(t *testing.T) {
	t0 := time.Unix(1000, 0)
	inOrder := &TestCase{Kind: HTTP, HTTPReq: HTTPReq{Timestamp: t0}, HTTPResp: HTTPResp{Timestamp: t0.Add(time.Second)}}
	EndInvertedWindow(inOrder, t0.Add(5*time.Second))
	if !inOrder.WindowEnd.IsZero() {
		t.Fatalf("a test case in order was given a window end %v", inOrder.WindowEnd)
	}
	if s, e := TestCaseWindow(inOrder); !s.Equal(t0) || !e.Equal(t0.Add(time.Second)) {
		t.Fatalf("window of a test case in order = [%v, %v]", s, e)
	}

	inverted := &TestCase{Kind: HTTP, HTTPReq: HTTPReq{Timestamp: t0.Add(time.Second)}, HTTPResp: HTTPResp{Timestamp: t0}}
	EndInvertedWindow(inverted, t0.Add(5*time.Second))
	if s, e := TestCaseWindow(inverted); !s.Equal(t0.Add(time.Second)) || !e.Equal(t0.Add(5*time.Second)) {
		t.Fatalf("window of an inverted test case = [%v, %v], want from its request to when it was received", s, e)
	}
	if !inverted.HTTPResp.Timestamp.Equal(t0) || !inverted.HTTPReq.Timestamp.Equal(t0.Add(time.Second)) {
		t.Fatal("the inverted test case's stamps were rewritten")
	}
	if w := RecordedWindow(inverted); !w.Start.Equal(t0.Add(time.Second)) || !w.End.Equal(t0.Add(5*time.Second)) {
		t.Fatalf("recorded window of an inverted test case = [%v, %v]", w.Start, w.End)
	}

	// The end its capture gave it stays.
	captured := &TestCase{Kind: GRPC_EXPORT, GrpcReq: GrpcReq{Timestamp: t0.Add(time.Second)}, GrpcResp: GrpcResp{Timestamp: t0}, WindowEnd: t0.Add(2 * time.Second)}
	EndInvertedWindow(captured, t0.Add(5*time.Second))
	if s, e := TestCaseWindow(captured); !s.Equal(t0.Add(time.Second)) || !e.Equal(t0.Add(2*time.Second)) {
		t.Fatalf("window of a gRPC test case its capture ended = [%v, %v], want to the capture's end", s, e)
	}
	EndInvertedWindow(nil, t0) // nil-safe
}
