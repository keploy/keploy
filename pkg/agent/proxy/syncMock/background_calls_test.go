package manager

import (
	"context"
	"testing"
	"time"

	"go.uber.org/zap"

	"go.keploy.io/server/v3/pkg/models"
)

// A background call is one the app makes on its own, with no request in
// flight: a timer, a poller, a lease sweep. Static dedup skips repeated
// requests and drops the calls those requests made; a background call is
// none of theirs, and replay needs it (the app makes it there too), so it
// must reach the recording, owned by no test.

// dedupRig drives a manager the way enterprise capture.go's async static-dedup
// path does once the first request is seen: mocks are buffered, a kept request
// resolves through ResolveKept, a duplicate closes its window and resolves
// through the dedup queue.
type dedupRig struct {
	m     *SyncMockManager
	out   chan *models.Mock
	maps  chan models.TestMockMapping
	q     *DedupQueue
	sent  []*models.Mock
	mapOf map[string][]string
}

func newDedupRig(t *testing.T) *dedupRig {
	t.Helper()
	r := &dedupRig{
		m:     New(zap.NewNop()),
		out:   make(chan *models.Mock, 4096),
		maps:  make(chan models.TestMockMapping, 4096),
		mapOf: map[string][]string{},
	}
	r.m.SetOutputChannel(r.out)
	r.m.SetMappingChannel(context.Background(), r.maps)
	r.q = r.m.DedupQueue()
	r.m.SetFirstRequestSignaled()
	// Past the startup window, so no call here is kept as a startup one.
	for i := 0; i < models.StartupMockTestCaseWindow; i++ {
		r.kept(t, "boot")
	}
	r.drain()
	r.sent, r.mapOf = nil, map[string][]string{}
	return r
}

// kept runs one request that becomes a test case, making calls during it.
func (r *dedupRig) kept(t *testing.T, name string, during ...func()) {
	t.Helper()
	start := time.Now()
	w := r.m.OpenWindow(start, nil)
	time.Sleep(time.Millisecond)
	for _, f := range during {
		f()
	}
	time.Sleep(time.Millisecond)
	if !w.Keep() {
		t.Fatal("kept request's window was given up")
	}
	r.m.ResolveKept(w, start, time.Now(), name, true)
}

// dup runs one request static dedup skips, making calls during it.
func (r *dedupRig) dup(during ...func()) (start, end time.Time) {
	start = time.Now()
	w := r.m.OpenWindow(start, nil)
	time.Sleep(time.Millisecond)
	for _, f := range during {
		f()
	}
	time.Sleep(time.Millisecond)
	end = time.Now()
	w.Close()
	job := r.q.Enqueue(start)
	r.q.ResolveJob(job, true, end, "", true, r.m)
	return start, end
}

// call hands the manager a per-test call made now.
func (r *dedupRig) call(name string) *models.Mock {
	req := time.Now()
	return r.callAt(name, req, req.Add(200*time.Microsecond))
}

func (r *dedupRig) callAt(name string, req, res time.Time) *models.Mock {
	mk := &models.Mock{Kind: models.Mongo, Name: name}
	mk.Spec.ReqTimestampMock, mk.Spec.ResTimestampMock = req, res
	r.m.AddMock(mk)
	return mk
}

func (r *dedupRig) drain() {
	for {
		select {
		case mk := <-r.out:
			r.sent = append(r.sent, mk)
		case mp := <-r.maps:
			r.mapOf[mp.TestName] = append(r.mapOf[mp.TestName], mp.MockIDs...)
		default:
			return
		}
	}
}

// sentAs reports the name mk was sent under (the manager renames per-test
// mocks on the way out), matched by its request time.
func (r *dedupRig) sentAs(mk *models.Mock) (string, bool) {
	r.drain()
	for _, s := range r.sent {
		if s.Kind == mk.Kind && s.Spec.ReqTimestampMock.Equal(mk.Spec.ReqTimestampMock) {
			return s.Name, true
		}
	}
	return "", false
}

func (r *dedupRig) stillHeld(mk *models.Mock) bool {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	for _, b := range r.m.buffer {
		if b == mk {
			return true
		}
	}
	for _, h := range r.m.held {
		if h.mock == mk {
			return true
		}
	}
	return false
}

func (r *dedupRig) mappedTo(name string) []string {
	r.drain()
	var tests []string
	for test, ids := range r.mapOf {
		for _, id := range ids {
			if id == name {
				tests = append(tests, test)
			}
		}
	}
	return tests
}

// The reported shape (k8s-proxy#989): a timer call between requests, then a
// skipped duplicate. The duplicate's cleanup used to drop it as the
// duplicate's leftover; it is nobody's leftover.
func TestDuplicateCleanupSendsACallNoRequestMade(t *testing.T) {
	r := newDedupRig(t)
	bg := r.call("timer")
	time.Sleep(time.Millisecond)
	r.dup()

	name, ok := r.sentAs(bg)
	if !ok {
		t.Fatalf("a call made with no request in flight was not sent after a duplicate's cleanup (still held: %v)", r.stillHeld(bg))
	}
	if tests := r.mappedTo(name); len(tests) > 0 {
		t.Fatalf("a call no request made is mapped to test(s) %v; it belongs to none", tests)
	}
}

// What static dedup is for must still happen: a skipped request's own calls
// are dropped, not sent as background ones.
func TestDuplicateCleanupStillDropsTheSkippedRequestsOwnCalls(t *testing.T) {
	r := newDedupRig(t)
	var own *models.Mock
	r.dup(func() { own = r.call("dup-find") })
	r.dup() // its cleanup's horizon is past the first duplicate's calls
	r.dup()

	if _, ok := r.sentAs(own); ok {
		t.Fatal("a skipped duplicate's own call was sent")
	}
	if r.stillHeld(own) {
		t.Fatal("a skipped duplicate's own call is still held after later cleanups passed it")
	}
}

// A parser can hand a call over after its request was decided (an async
// decode). A skipped request's call that arrives late is still that request's
// leftover: its window is what says so, not when the call arrived.
func TestDuplicateCleanupDropsALateArrivingCallOfASkippedRequest(t *testing.T) {
	r := newDedupRig(t)
	start, end := r.dup()
	mid := start.Add(end.Sub(start) / 2)
	late := r.callAt("late-dup-find", mid, mid.Add(100*time.Microsecond))
	r.dup()
	r.dup()

	if name, ok := r.sentAs(late); ok {
		t.Fatalf("a late call inside a skipped request's window was sent as %s", name)
	}
	if r.stillHeld(late) {
		t.Fatal("a late call inside a skipped request's window is still held")
	}
}

// A kept request's calls stay its own, the background call between them is
// sent owned by no test, and neither is lost to the duplicates around them.
func TestKeptCallsAndBackgroundCallsBothSurviveDuplicates(t *testing.T) {
	r := newDedupRig(t)
	var keptCall *models.Mock
	r.dup()
	r.kept(t, "test-1", func() { keptCall = r.call("kept-find") })
	bg := r.call("timer")
	time.Sleep(time.Millisecond)
	r.dup()
	r.dup()

	kn, ok := r.sentAs(keptCall)
	if !ok {
		t.Fatal("the kept request's call was not sent")
	}
	if got := r.mappedTo(kn); len(got) != 1 || got[0] != "test-1" {
		t.Fatalf("the kept request's call is mapped to %v, want [test-1]", got)
	}
	bn, ok := r.sentAs(bg)
	if !ok {
		t.Fatal("the background call was not sent")
	}
	if got := r.mappedTo(bn); len(got) > 0 {
		t.Fatalf("the background call is mapped to %v, want none", got)
	}
}

// A call made while a request is still in flight may be that request's: it is
// held for it, as before, and goes to it when it is kept.
func TestCallDuringARequestInFlightIsStillHeldForIt(t *testing.T) {
	r := newDedupRig(t)
	start := time.Now()
	w := r.m.OpenWindow(start, nil)
	time.Sleep(time.Millisecond)
	during := r.call("in-flight-find")
	time.Sleep(time.Millisecond)
	// A duplicate decided meanwhile, whose cleanup horizon is past the call.
	job := r.q.Enqueue(time.Now())
	r.q.ResolveJob(job, true, time.Now(), "", true, r.m)

	if _, ok := r.sentAs(during); ok {
		t.Fatal("a call made while a request was in flight was sent before that request was decided")
	}
	if !r.stillHeld(during) {
		t.Fatal("a call made while a request was in flight was dropped before that request was decided")
	}
	if !w.Keep() {
		t.Fatal("window given up")
	}
	r.m.ResolveKept(w, start, time.Now(), "test-in-flight", true)
	name, ok := r.sentAs(during)
	if !ok {
		t.Fatal("the in-flight request's call was not sent once it was kept")
	}
	if got := r.mappedTo(name); len(got) != 1 || got[0] != "test-in-flight" {
		t.Fatalf("the in-flight request's call is mapped to %v, want [test-in-flight]", got)
	}
}

// The staging shape: the app's own polling arrives as a stream of duplicates,
// a timer call lands between them every cycle, and a real request is kept now
// and then. Every timer call reaches the recording.
func TestEveryBackgroundCallSurvivesAStreamOfDuplicates(t *testing.T) {
	r := newDedupRig(t)
	var bgs []*models.Mock
	for i := 0; i < 250; i++ {
		r.dup()
		bgs = append(bgs, r.call("timer"))
		time.Sleep(100 * time.Microsecond)
		r.dup()
		if i%60 == 59 {
			r.kept(t, "real-test")
		}
		r.drain()
	}
	r.dup()
	lost := 0
	for _, b := range bgs {
		if _, ok := r.sentAs(b); !ok {
			lost++
		}
	}
	if lost > 0 {
		t.Fatalf("%d of %d background calls did not reach the recording", lost, len(bgs))
	}
}

// Without --sync, conn.CaptureGRPC ends a duplicate gRPC call by closing its
// window, with no dedup job and no resolve. The span its window leaves
// (Window.Close) is what still marks the call's calls as its own: the next
// HTTP duplicate's prune drops them, as it did before calls no request made
// were kept.
func TestAnAsyncGRPCDuplicatesCallsAreStillDropped(t *testing.T) {
	r := newDedupRig(t)
	const n = 100
	var own []*models.Mock
	for i := 0; i < n; i++ {
		w := r.m.OpenWindow(time.Now(), nil)
		time.Sleep(200 * time.Microsecond)
		own = append(own, r.call("grpc-dup-find"))
		time.Sleep(200 * time.Microsecond)
		w.Close() // CaptureGRPC's duplicate without --sync: no job, no resolve
		r.dup()   // an HTTP duplicate, whose prune passes the gRPC call's
	}
	r.dup()
	stored := 0
	for _, m := range own {
		if _, ok := r.sentAs(m); ok {
			stored++
		}
	}
	if stored > 0 {
		t.Fatalf("%d of %d async gRPC duplicates' own calls were stored as calls no request made", stored, n)
	}
}

// A duplicate's job is enqueued at its prune horizon, which is earlier than
// its own request when another request is in flight (enterprise PruneHorizon).
// The duplicate's span starts at its own request (its window's start), so a
// call made after the other request started but before the duplicate did is
// not taken for the duplicate's.
func TestADuplicatesSpanStartsAtItsOwnRequestNotItsPruneHorizon(t *testing.T) {
	r := newDedupRig(t)
	other := time.Now() // a request in flight with no window of its own (one no test case is recorded for)
	time.Sleep(2 * time.Millisecond)
	timer := r.call("timer")
	time.Sleep(2 * time.Millisecond)
	dStart := time.Now()
	w := r.m.OpenWindow(dStart, nil)
	time.Sleep(time.Millisecond)
	dEnd := time.Now()
	w.Close()
	job := r.q.Enqueue(other) // PruneHorizon(dStart) == other
	r.q.ResolveJob(job, true, dEnd, "", true, r.m)
	r.dup()
	r.dup()
	if _, ok := r.sentAs(timer); !ok {
		t.Fatal("a call made before the duplicate's own request was dropped as the duplicate's")
	}
}

// A duplicate's span ends when its window closes, after its verdict. Work it
// does after responding but before then is its own and dropped with it; work
// it does after its window closed is outside every request's span and is sent
// as a call no request made. That is the trade-off of telling calls apart by
// time: a tail on the span would also swallow the timer calls that land in
// it, at every duplicate.
func TestADuplicatesCallsAfterItsWindowClosesAreSentAsBackground(t *testing.T) {
	r := newDedupRig(t)
	var inside, beforeClose *models.Mock
	start := time.Now()
	w := r.m.OpenWindow(start, nil)
	time.Sleep(200 * time.Microsecond)
	inside = r.call("dup-read")
	time.Sleep(200 * time.Microsecond)
	end := time.Now() // its response
	time.Sleep(200 * time.Microsecond)
	beforeClose = r.call("dup-audit-before-close")
	time.Sleep(200 * time.Microsecond)
	w.Close()
	job := r.q.Enqueue(start)
	r.q.ResolveJob(job, true, end, "", true, r.m)
	time.Sleep(200 * time.Microsecond)
	afterClose := r.call("dup-audit-after-close")
	time.Sleep(time.Millisecond)
	r.dup()
	r.dup()
	if _, ok := r.sentAs(inside); ok {
		t.Fatal("a duplicate's call inside its response was sent")
	}
	if _, ok := r.sentAs(beforeClose); ok {
		t.Fatal("a duplicate's call after its response but before its window closed was sent")
	}
	name, ok := r.sentAs(afterClose)
	if !ok {
		t.Fatal("a call after the duplicate's window closed, outside every span, was dropped")
	}
	if got := r.mappedTo(name); len(got) > 0 {
		t.Fatalf("a call outside every span is mapped to %v, want none", got)
	}
}

// Calls no request made are sent by a duplicate's prune. The stale cutoff of
// a kept resolve (resolveRange) still drops one more than StaleHorizon old:
// that cutoff also stands for requests whose windows the manager does not know
// (late proxyless windows, windows evicted from the ring), so whether such a
// call survives an idle stretch depends on whether a duplicate's prune reaches
// it first. Pinned so a change to the cutoff is a deliberate one.
func TestTheStaleCutoffStillDropsACallNoRequestMade(t *testing.T) {
	r := newDedupRig(t)
	old := time.Now().Add(-(StaleHorizon + 3*time.Second))
	bg := r.callAt("old-timer", old, old.Add(time.Millisecond))
	r.kept(t, "test-1")
	r.dup()
	if name, ok := r.sentAs(bg); ok {
		t.Fatalf("the stale cutoff sent a call no request made as %s; it is expected to drop it", name)
	}
	if r.stillHeld(bg) {
		t.Fatal("the stale cutoff neither dropped nor sent the call")
	}
}

// A request claimed kept (Window.Keep) whose window closes before its resolve
// leaves no unkept span: a prune in between must not drop its calls as an
// unkept request's.
func TestAClaimedRequestsWindowLeavesNoUnkeptSpan(t *testing.T) {
	r := newDedupRig(t)
	w := r.m.OpenWindow(time.Now(), nil)
	time.Sleep(200 * time.Microsecond)
	mk := r.call("kept-find")
	time.Sleep(200 * time.Microsecond)
	if !w.Keep() {
		t.Fatal("window given up")
	}
	w.Close()
	r.dup() // a duplicate's prune passes the call before the kept request resolves
	if _, sent := r.sentAs(mk); !sent && !r.stillHeld(mk) {
		t.Fatal("a claimed request's call was dropped as an unkept request's")
	}
}
