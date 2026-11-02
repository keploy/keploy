//go:build linux

package proxy

import (
	"testing"
)

// newStreamParserOn is newStreamParser on a connection tr keeps beside
// others.
func newStreamParserOn(t *testing.T, tr *inflight) *streamParser {
	app, proxy := acceptedPair(t, tr)
	p := &streamParser{t: t, tr: tr, app: app, proxy: proxy, got: make(chan string, 4)}
	go readOnce(proxy, p.got)
	return p
}

// Two test workers' tests open at once: worker 1's call is held on its own
// connection as worker 2's test begins. Worker 2's begin cuts its own
// worker's connections alone, so worker 1's end still waits for its call
// (a cut on every connection would leave it out: a lost call), and worker
// 2's end does not wait for worker 1's call. A connection whose stamp its
// handler has not read yet may be either's: each end waits for it, and no
// begin cuts it. One stamped with no worker (0) is neither's.
func TestEachWorkersWindowIsCutAndWaitedForOnItsOwnConnectionsOnly(t *testing.T) {
	px := &Proxy{}
	tr := &px.inflight
	one, two, unread, none := newStreamParserOn(t, tr), newStreamParserOn(t, tr), newStreamParserOn(t, tr), newStreamParserOn(t, tr)
	one.proxy.setWorker(1)
	two.proxy.setWorker(2)
	none.proxy.setWorker(0)
	release := one.open("HEADERS stream 1", false) // worker 1's test's call
	defer release()
	px.InFlightBeginWindowOf(2) // worker 2's test begins
	if n, _ := px.InFlightScopeOf(1).State(true); n != 1 {
		t.Fatalf("lost call: worker 1's call, held since before worker 2's test began, is held and unanswered, but worker 1's end's scope pending = %d, want 1", n)
	}
	if n, _ := px.InFlightScopeOf(2).State(true); n != 0 {
		t.Fatalf("worker 2's end waits for worker 1's call: pending = %d, want 0", n)
	}
	if n, _ := px.InFlightScope().State(true); n != 1 {
		t.Fatalf("a run of one test process waits for every connection's work: pending = %d, want 1", n)
	}

	held := unread.open("HEADERS stream 3", false)
	defer held()
	nobody := none.open("HEADERS stream 5", false)
	defer nobody()
	// Both workers' next tests begin: worker 1's call held before is not its
	// next test's.
	tr.beginWindow(1)
	tr.beginWindow(2)
	for _, w := range []uint32{1, 2} {
		if n, _ := tr.scope(w).State(true); n != 1 {
			t.Fatalf("worker %d's end: pending = %d, want 1 (the unread connection's call, which no worker's begin cut; not the call from no worker)", w, n)
		}
	}
}
