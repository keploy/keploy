package manager

import (
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils/stall"
)

// stalledOutClock has m's out-queue drainer measure its stall on a clock on
// which each look is past the bound: a consumer that does not take the mock
// in hand at once is given up on.
func stalledOutClock(m *SyncMockManager) {
	m.outClock = &stall.ManualClock{Step: stall.OnTime}
	m.outStall = time.Millisecond
}

// sendOwnedAndWait sends mock for owner, and waits for its outcome.
func sendOwnedAndWait(t *testing.T, m *SyncMockManager, mock *models.Mock, owner string) bool {
	t.Helper()
	done := make(chan bool, 1)
	m.send(mock, owner, func(delivered bool) { done <- delivered })
	select {
	case d := <-done:
		return d
	case <-time.After(time.Minute): // only turns a hang into a failure
		t.Fatal("a sent mock was neither delivered nor dropped")
		return false
	}
}

// waitOutQueue waits for m's out queues, every lane's, to be delivered or
// given up.
func waitOutQueue(t *testing.T, m *SyncMockManager) {
	t.Helper()
	deadline := time.After(time.Minute) // only turns a hang into a failure
	for {
		m.outQueueMu.Lock()
		var drained chan struct{}
		if m.out != nil && m.out.draining {
			drained = m.out.drained
		}
		for l := range m.outOld {
			drained = l.drained
		}
		m.outQueueMu.Unlock()
		if drained == nil {
			return
		}
		select {
		case <-drained:
		case <-deadline:
			t.Fatal("the out queue was neither delivered nor given up")
		}
	}
}

// A consumer that pauses for longer than the old 200 ms send budget, with
// outChan full, loses nothing: what comes meanwhile is queued, and delivered
// in order as it takes again. The sender never waits. Before, each mock that
// found the channel full for 200 ms of wall time was dropped, and its test
// case left out of a recording that otherwise succeeded.
func TestAConsumerThatPausesLosesNoMock(t *testing.T) {
	ch := make(chan *models.Mock, 2)
	m := New(nil)
	m.SetOutputChannel(ch)
	const n = 50
	start := time.Now()
	for i := 0; i < n; i++ {
		m.sendToOutChan(&models.Mock{Name: string(rune('a' + i%26)), Version: models.Version(itoa(i))})
	}
	if took := time.Since(start); took > 10*time.Second { // only that no send waited on the consumer
		t.Fatalf("sending to a full channel took %v: a sender waited for the consumer", took)
	}
	time.Sleep(500 * time.Millisecond) // the consumer's pause, past the old budget
	for i := 0; i < n; i++ {
		select {
		case got := <-ch:
			if string(got.Version) != itoa(i) {
				t.Fatalf("mock %d came as %q: the queue delivers in order", i, got.Version)
			}
		case <-time.After(time.Minute): // only turns a hang into a failure
			t.Fatalf("mock %d never came", i)
		}
	}
	if d := m.DropCount(); d != 0 {
		t.Fatalf("%d mocks dropped for a consumer that paused", d)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for ; i > 0; i /= 10 {
		b = append([]byte{byte('0' + i%10)}, b...)
	}
	return string(b)
}

// A consumer starved of CPU with its channel full is not given up on: the
// stall is time the process could run. One that takes nothing for the bound
// of it has stopped, and every mock queued for it is dropped, its owner
// recorded and its drop told.
func TestOnlyAConsumerStalledInRunnableTimeLosesItsMocks(t *testing.T) {
	ch := make(chan *models.Mock, 1)
	ch <- &models.Mock{Name: "filler"}
	clk := &stall.ManualClock{}
	m := New(nil)
	m.outClock, m.outStall = clk, time.Second
	m.SetOutputChannel(ch)
	var mu sync.Mutex
	var told []string
	m.OnDrop(func(mk *models.Mock) { mu.Lock(); told = append(told, mk.Name); mu.Unlock() })
	outcome := make(chan bool, 2)
	m.send(&models.Mock{Name: "a"}, "test-a", func(d bool) { outcome <- d })
	m.send(&models.Mock{Name: "b"}, "test-b", func(d bool) { outcome <- d })
	for i := 0; i < 100; i++ { // 25 bounds of wall time, no CPU
		waitClockPending(t, clk)
		clk.Pass()
	}
	waitClockPending(t, clk)
	select {
	case d := <-outcome:
		t.Fatalf("a mock for a consumer starved of CPU was decided (delivered %v)", d)
	default:
	}
	clk.Advance(time.Second)
	for i := 0; i < 2; i++ {
		select {
		case d := <-outcome:
			if d {
				t.Fatal("a mock no consumer took was said to be delivered")
			}
		case <-time.After(time.Minute): // only turns a hang into a failure
			t.Fatal("the mocks of a consumer stalled for the bound were not given up")
		}
	}
	if !m.WasMockDroppedForTC("test-a") || !m.WasMockDroppedForTC("test-b") || m.DropCount() != 2 {
		t.Fatalf("dropped %d, owners recorded a=%v b=%v; want both dropped and recorded", m.DropCount(), m.WasMockDroppedForTC("test-a"), m.WasMockDroppedForTC("test-b"))
	}
	mu.Lock()
	defer mu.Unlock()
	if len(told) != 2 {
		t.Fatalf("drops told %v, want both", told)
	}
}

// waitClockPending waits for a drainer to arm its next look on clk.
func waitClockPending(t *testing.T, clk *stall.ManualClock) {
	t.Helper()
	deadline := time.Now().Add(time.Minute) // only turns a hang into a failure
	for clk.Pending() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the drainer never armed its next look")
		}
		time.Sleep(time.Millisecond)
	}
}

// The queue is bounded by bytes: a mock past it is dropped at once, and its
// owner recorded, while what is queued waits on.
func TestTheOutQueueIsBoundedByBytes(t *testing.T) {
	ch := make(chan *models.Mock, 1)
	ch <- &models.Mock{Name: "filler"}
	m := New(nil)
	m.outClock, m.outStall = &stall.ManualClock{}, time.Hour
	m.SetOutputChannel(ch)
	big := &models.Mock{Name: "big", Spec: models.MockSpec{Metadata: map[string]string{"body": string(make([]byte, 40<<20))}}}
	if sz := mockSize(big); sz < outQueueMaxBytes/2+1 {
		t.Fatalf("setup: the big mock is %d bytes, want more than half the bound", sz)
	}
	queued := make(chan bool, 2)
	// A small mock first, for the drainer's hand (out of the queue, and not
	// counted): big is then queued behind it, and counted, whenever the
	// drainer runs.
	m.send(&models.Mock{Name: "small"}, "test-0", func(d bool) { queued <- d })
	m.send(big, "test-1", func(d bool) { queued <- d })
	if sendOwnedAndWait(t, m, &models.Mock{Name: "big-2", Spec: big.Spec}, "test-2") {
		t.Fatal("a mock past the queue's byte bound was delivered")
	}
	if !m.WasMockDroppedForTC("test-2") || m.WasMockDroppedForTC("test-1") {
		t.Fatalf("owners recorded: test-1 %v, test-2 %v; want only test-2's", m.WasMockDroppedForTC("test-1"), m.WasMockDroppedForTC("test-2"))
	}
	for i := 0; i < 3; i++ {
		<-ch // the consumer takes again: the filler, small, big
	}
	for i := 0; i < 2; i++ {
		select {
		case d := <-queued:
			if !d {
				t.Fatal("a queued mock was dropped")
			}
		case <-time.After(time.Minute): // only turns a hang into a failure
			t.Fatal("a queued mock never came")
		}
	}
}

// A new session's channel does not give the old one's queue up: the old
// session's consumer may still read (its stop drains its stream), and gets
// every mock queued for it, in order, while the new channel gets the new
// session's. Before, the switch dropped the old queue: a DaemonSet agent's
// manager whose session changed lost what its old consumer was a channel
// behind on.
func TestANewChannelLetsTheOldQueueReachItsConsumer(t *testing.T) {
	old := make(chan *models.Mock, 1)
	old <- &models.Mock{Name: "filler"}
	clk := &stall.ManualClock{}
	m := New(nil)
	m.outClock, m.outStall = clk, time.Hour
	m.SetOutputChannel(old)
	outcome := make(chan bool, 2)
	m.send(&models.Mock{Name: "a"}, "test-a", func(d bool) { outcome <- d })
	m.send(&models.Mock{Name: "b"}, "test-b", func(d bool) { outcome <- d })
	waitClockPending(t, clk) // the drainer has a in hand, the old channel full
	fresh := make(chan *models.Mock, 4)
	m.SetOutputChannel(fresh)
	if !m.OutQueuedTo(old) || m.OutQueuedTo(fresh) {
		t.Fatalf("after the switch: queued to the old channel %v, to the new %v; want the old only", m.OutQueuedTo(old), m.OutQueuedTo(fresh))
	}
	if !sendOwnedAndWait(t, m, &models.Mock{Name: "c"}, "test-c") {
		t.Fatal("the new session's mock was not delivered")
	}
	if got := <-fresh; got.Name != "c" {
		t.Fatalf("the new channel carried %q, want c", got.Name)
	}
	for _, want := range []string{"filler", "a", "b"} { // the old consumer reads on
		select {
		case got := <-old:
			if got.Name != want {
				t.Fatalf("the old channel carried %q, want %q", got.Name, want)
			}
		case <-time.After(time.Minute): // only turns a hang into a failure
			t.Fatalf("the old channel never carried %q", want)
		}
	}
	for i := 0; i < 2; i++ {
		if !<-outcome {
			t.Fatal("a mock the old consumer took was said to be dropped")
		}
	}
	waitOutQueue(t, m)
	if d := m.DropCount(); d != 0 || m.OutQueued() {
		t.Fatalf("%d dropped, still queued %v: want none of either", d, m.OutQueued())
	}
}

// CloseOutChan closes the channel only once what is queued has gone to the
// consumer, which still reads at a stop: closing at once dropped it.
func TestCloseOutChanDeliversTheQueueFirst(t *testing.T) {
	ch := make(chan *models.Mock, 1)
	ch <- &models.Mock{Name: "filler"}
	m := New(nil)
	m.outClock, m.outStall = &stall.ManualClock{}, time.Hour
	m.SetOutputChannel(ch)
	for _, n := range []string{"a", "b", "c"} {
		m.sendToOutChan(&models.Mock{Name: n})
	}
	closed := make(chan struct{})
	go func() { m.CloseOutChan(); close(closed) }()
	var got []string
	for mk := range ch {
		got = append(got, mk.Name)
	}
	<-closed
	if len(got) != 4 || got[1] != "a" || got[3] != "c" {
		t.Fatalf("the consumer got %v, want the filler then a, b, c", got)
	}
	if d := m.DropCount(); d != 0 {
		t.Fatalf("%d mocks dropped at the close", d)
	}
}

// A control frame (a revoke) does not overtake the mocks queued before it:
// while the drainer has them, it is not sent, though the channel has room.
func TestAControlFrameDoesNotOvertakeTheQueue(t *testing.T) {
	ch := make(chan *models.Mock, 1)
	m := New(nil)
	m.SetOutputChannel(ch)
	m.outQueueMu.Lock()
	m.out.draining = true // a drainer with mocks in hand
	m.outQueueMu.Unlock()
	if m.trySendControlFrame(&models.Mock{Name: "revoke"}) {
		t.Fatal("a control frame was sent before the mocks queued ahead of it")
	}
	m.outQueueMu.Lock()
	m.out.draining = false
	m.outQueueMu.Unlock()
	if !m.trySendControlFrame(&models.Mock{Name: "revoke"}) {
		t.Fatal("a control frame with nothing queued before it was not sent")
	}
}

// A new session's channel decides nothing of the old queue's mocks, under no
// lock: a queued mock's done takes mu (sendTaken's sentClaimDone), which
// AddMock holds while it reads the channel (outChanStatus), and a done told
// under outChanMu's writer lock would wait on it for ever.
func TestANewChannelDecidesNothingUnderItsLock(t *testing.T) {
	old := make(chan *models.Mock, 1)
	old <- &models.Mock{Name: "filler"}
	m := New(nil)
	m.outClock, m.outStall = &stall.ManualClock{}, time.Hour
	m.SetOutputChannel(old)
	m.send(&models.Mock{Name: "a"}, "", nil) // the drainer's mock in hand
	decided := make(chan struct{}, 1)
	m.send(&models.Mock{Name: "b"}, "", func(bool) {
		decided <- struct{}{}
		m.mu.Lock() // as sentClaimDone does
		m.mu.Unlock()
	})
	m.mu.Lock() // AddMock, about to read the channel
	switched := make(chan struct{})
	go func() { m.SetOutputChannel(make(chan *models.Mock, 1)); close(switched) }()
	select {
	case <-switched:
	case <-time.After(10 * time.Second): // only turns a deadlock into a failure
		m.mu.Unlock()
		t.Fatal("SetOutputChannel waited while AddMock held mu")
	}
	read := make(chan struct{})
	go func() { m.outChanStatus(); close(read) }()
	select {
	case <-read:
	case <-time.After(10 * time.Second): // only turns a deadlock into a failure
		m.mu.Unlock()
		t.Fatal("AddMock's read of the channel waited on the switch")
	}
	m.mu.Unlock()
	select {
	case <-decided:
		t.Fatal("the switch decided a mock of the old queue")
	default:
	}
}

// The old queue's drops (its consumer stopped taking) are the old session's,
// and reported as any drop: none of their owners is revoked on the new
// session's stream, whose tests may carry the same names.
func TestANewChannelRevokesNothingOfTheOldQueueOnIt(t *testing.T) {
	old := make(chan *models.Mock, 1)
	old <- &models.Mock{Name: "filler"}
	clk := &stall.ManualClock{}
	m := New(nil)
	m.outClock, m.outStall = clk, time.Hour
	m.SetOutputChannel(old)
	m.SetRevokeCapable(true)
	outcome := make(chan bool, 2)
	m.send(&models.Mock{Name: "a"}, "test-a", func(d bool) { outcome <- d })
	m.send(&models.Mock{Name: "b"}, "test-b", func(d bool) { outcome <- d })
	waitClockPending(t, clk)
	fresh := make(chan *models.Mock, 4)
	m.SetOutputChannel(fresh)
	clk.Advance(time.Hour) // the old consumer took nothing for the bound
	for i := 0; i < 2; i++ {
		select {
		case d := <-outcome:
			if d {
				t.Fatal("a mock no consumer took was said to be delivered")
			}
		case <-time.After(time.Minute): // only turns a hang into a failure
			t.Fatal("the old session's queue was not given up")
		}
	}
	if !m.WasMockDroppedForTC("test-a") || m.DropCount() != 2 {
		t.Fatalf("the old queue's drops were not reported: %d dropped, test-a recorded %v", m.DropCount(), m.WasMockDroppedForTC("test-a"))
	}
	m.drainPendingRevokes()
	select {
	case f := <-fresh:
		t.Fatalf("the new session's stream carried a %s frame %v for the old session's drops", f.Kind, f.Spec.Metadata)
	default:
	}
}

// CloseOutChan, waiting on the queue, closes the channel it was asked to
// close: not a new session's channel set meanwhile, whose senders still send
// on it.
func TestCloseOutChanLeavesANewSessionsChannelOpen(t *testing.T) {
	old := make(chan *models.Mock, 1)
	old <- &models.Mock{Name: "filler"}
	m := New(nil)
	m.outClock, m.outStall = &stall.ManualClock{}, time.Hour
	m.SetOutputChannel(old)
	m.sendToOutChan(&models.Mock{Name: "a"}) // queued: the close waits on its drainer
	closed := make(chan struct{})
	go func() { m.CloseOutChan(); close(closed) }()
	deadline := time.Now().Add(time.Minute) // only turns a hang into a failure
	for {
		m.outChanMu.RLock()
		c := m.outChanClosed
		m.outChanMu.RUnlock()
		if c {
			break // CloseOutChan waits on the drainer now
		}
		if time.Now().After(deadline) {
			t.Fatal("CloseOutChan never closed the channel to senders")
		}
		time.Sleep(time.Millisecond)
	}
	fresh := make(chan *models.Mock, 1)
	m.SetOutputChannel(fresh) // the old queue goes on to its consumer
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("a send on the new session's channel panicked: %v", r)
			}
		}()
		m.sendToOutChan(&models.Mock{Name: "b"})
	}()
	if got := <-fresh; got.Name != "b" {
		t.Fatalf("the new channel carried %q, want b", got.Name)
	}
	for _, want := range []string{"filler", "a"} { // the old consumer reads on
		if got := <-old; got.Name != want {
			t.Fatalf("the old channel carried %q, want %q", got.Name, want)
		}
	}
	select {
	case <-closed:
	case <-time.After(time.Minute): // only turns a hang into a failure
		t.Fatal("CloseOutChan never returned once its queue was delivered")
	}
	select {
	case _, ok := <-old:
		if ok {
			t.Fatal("the old channel carried a mock past its queue")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the channel CloseOutChan was asked to close was never closed: its consumer never ends")
	}
	m.sendToOutChan(&models.Mock{Name: "c"}) // the new channel is open
	if got := <-fresh; got.Name != "c" {
		t.Fatalf("the new channel carried %q, want c", got.Name)
	}
}

// A mock alone in the queue is queued whatever its size: the byte bound is on
// what waits behind another. Dropped at once, a mock past the bound could
// never wait for a consumer a moment behind, as the old 200 ms let it.
func TestAMockAloneIsQueuedWhateverItsSize(t *testing.T) {
	ch := make(chan *models.Mock, 1)
	ch <- &models.Mock{Name: "filler"}
	m := New(nil)
	m.outClock, m.outStall = &stall.ManualClock{}, time.Hour
	m.SetOutputChannel(ch)
	huge := &models.Mock{Name: "huge", Spec: models.MockSpec{Metadata: map[string]string{"body": string(make([]byte, outQueueMaxBytes+1))}}}
	outcome := make(chan bool, 1)
	m.send(huge, "test-huge", func(d bool) { outcome <- d })
	<-ch // the consumer takes again
	select {
	case d := <-outcome:
		if !d {
			t.Fatal("a mock past the byte bound, alone in the queue, was dropped")
		}
	case <-time.After(time.Minute): // only turns a hang into a failure
		t.Fatal("the mock never came")
	}
	if got := <-ch; got != huge {
		t.Fatalf("the channel carried %q, want huge", got.Name)
	}
}

// CloseOutChan, a step of the agent's teardown, waits on a consumer that
// stopped taking for outCloseStall, not outChanStall: the CLI that stops the
// agent kills a teardown that takes no step for 30 of its beats, and the steps
// after the close (the CA's removal, the hooks' detach) are then not taken.
func TestCloseOutChanGivesUpSoonOnAConsumerThatStopped(t *testing.T) {
	ch := make(chan *models.Mock, 1)
	ch <- &models.Mock{Name: "filler"}
	clk := &stall.ManualClock{}
	m := New(nil)
	m.outClock, m.outStall, m.outCloseStall = clk, time.Hour, time.Second
	m.SetOutputChannel(ch)
	outcome := make(chan bool, 1)
	m.send(&models.Mock{Name: "a"}, "test-a", func(d bool) { outcome <- d })
	closed := make(chan struct{})
	go func() { m.CloseOutChan(); close(closed) }()
	deadline := time.Now().Add(time.Minute) // only turns a hang into a failure
	advanced := 0
	for decided := false; !decided; {
		select {
		case d := <-outcome:
			if d {
				t.Fatal("a mock no consumer took was said to be delivered")
			}
			decided = true
			continue
		default:
		}
		if clk.Pending() > 0 {
			if advanced == 30 {
				t.Fatal("CloseOutChan still waits on a consumer that took nothing for 30 s of runnable time")
			}
			clk.Advance(time.Second)
			advanced++
			continue
		}
		if time.Now().After(deadline) {
			t.Fatal("the drainer never armed its next look")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case <-closed:
	case <-time.After(time.Minute): // only turns a hang into a failure
		t.Fatal("CloseOutChan never returned")
	}
	if advanced > 3 {
		t.Fatalf("given up after %d s of runnable time, want about the close bound (1 s)", advanced)
	}
	if !m.WasMockDroppedForTC("test-a") {
		t.Fatal("the mock given up at the close was not recorded for its test")
	}
}

// The byte bound is the process's, not each manager's: a DaemonSet agent runs
// a manager per app, and one consumer stalled behind them all (the k8s-proxy
// upload) would otherwise have them queue it once per app.
func TestTheOutQueueBoundIsTheProcesss(t *testing.T) {
	full := func() (*SyncMockManager, chan *models.Mock) {
		ch := make(chan *models.Mock, 1)
		ch <- &models.Mock{Name: "filler"}
		m := New(nil)
		m.outClock, m.outStall = &stall.ManualClock{}, time.Hour
		m.SetOutputChannel(ch)
		return m, ch
	}
	a, chA := full()
	b, chB := full()
	big := models.MockSpec{Metadata: map[string]string{"body": string(make([]byte, 40<<20))}}
	outcome := make(chan bool, 4)
	tell := func(d bool) { outcome <- d }
	a.send(&models.Mock{Name: "a1"}, "", tell)            // a's mock in hand
	a.send(&models.Mock{Name: "a2", Spec: big}, "", tell) // queued behind it
	b.send(&models.Mock{Name: "b1"}, "", tell)            // b's mock in hand
	b.send(&models.Mock{Name: "b2"}, "", tell)            // queued behind it
	if sendOwnedAndWait(t, b, &models.Mock{Name: "b3", Spec: big}, "test-b3") {
		t.Fatal("a mock past the process's byte bound was delivered")
	}
	if !b.WasMockDroppedForTC("test-b3") {
		t.Fatal("the owner of a mock dropped for the process's byte bound was not recorded")
	}
	for _, ch := range []chan *models.Mock{chA, chB} {
		for i := 0; i < 3; i++ {
			<-ch // the consumers take again: the filler, then what was queued
		}
	}
	for i := 0; i < 4; i++ {
		select {
		case d := <-outcome:
			if !d {
				t.Fatal("a queued mock within the bound was dropped")
			}
		case <-time.After(time.Minute): // only turns a hang into a failure
			t.Fatal("a queued mock never came")
		}
	}
}

// OutQueued says a mock handed over is not on the channel yet, until the
// consumer has taken room for it: a consumer that ends its read when the
// channel is empty asks it, or ends with the mock still queued.
func TestOutQueuedUntilTheQueueIsOnTheChannel(t *testing.T) {
	ch := make(chan *models.Mock, 1)
	ch <- &models.Mock{Name: "filler"}
	m := New(nil)
	m.outClock, m.outStall = &stall.ManualClock{}, time.Hour
	m.SetOutputChannel(ch)
	if m.OutQueued() {
		t.Fatal("nothing handed over, and OutQueued")
	}
	m.sendToOutChan(&models.Mock{Name: "a"})
	if !m.OutQueued() {
		t.Fatal("a mock queued behind a full channel, and not OutQueued")
	}
	<-ch // the consumer takes again
	if got := <-ch; got.Name != "a" {
		t.Fatalf("the channel carried %q, want a", got.Name)
	}
	waitOutQueue(t, m)
	if m.OutQueued() {
		t.Fatal("the queue is on the channel, and still OutQueued")
	}
}

// A startup mock sent for a claim, taken by a test of the old session while
// it was on its way, and dropped by its replaced lane (its consumer stopped
// taking): the drop is told to that test, and revoked on no stream, least of
// all the new session's, whose tests may carry the same names.
func TestAReplacedLanesClaimedDropRevokesNothingOnTheNewStream(t *testing.T) {
	old := make(chan *models.Mock) // no reader
	maps := make(chan models.TestMockMapping, 16)
	mgr := lateMockTestManager(old, maps, true)
	clk := &stall.ManualClock{}
	mgr.outClock, mgr.outStall = clk, time.Hour
	mgr.SetRevokeCapable(true)
	start := time.Now().Add(-time.Second)
	w := mgr.OpenWindow(start, nil)
	call := httpMockAt(start.Add(10 * time.Millisecond))
	mgr.AddMock(call)
	mgr.FlushOwnedWindows()  // sent for the claim, noted, and queued (no reader)
	waitClockPending(t, clk) // its drainer has it in hand
	w.Keep()
	mgr.ResolveKept(w, start, time.Now(), "test-3", true) // takes it on its way
	w.Close()
	fresh := make(chan *models.Mock, 4)
	mgr.SetOutputChannel(fresh)
	clk.Advance(time.Hour) // the old consumer took nothing for the bound
	waitOutQueue(t, mgr)
	if !mgr.WasMockDroppedForTC("test-3") {
		t.Fatal("fixture: test-3 was not told its mock was dropped")
	}
	mgr.drainPendingRevokes()
	select {
	case f := <-fresh:
		t.Fatalf("the new session's stream carried a %s frame %v for the old session's drop", f.Kind, f.Spec.Metadata)
	default:
	}
}

// A channel set again while its replaced lane still delivers to it gets that
// lane back: one lane per channel, so what is sent to it now waits behind what
// was queued for it before, and is not raced to the channel by a second
// drainer.
func TestAChannelSetAgainKeepsItsQueueInOrder(t *testing.T) {
	a := make(chan *models.Mock, 1)
	a <- &models.Mock{Name: "filler"}
	clk := &stall.ManualClock{}
	m := New(nil)
	m.outClock, m.outStall = clk, time.Hour
	m.SetOutputChannel(a)
	m.sendToOutChan(&models.Mock{Name: "1"})
	m.sendToOutChan(&models.Mock{Name: "2"})
	waitClockPending(t, clk) // the drainer has 1 in hand, 2 queued
	m.SetOutputChannel(make(chan *models.Mock, 4))
	m.SetOutputChannel(a) // the first channel again
	m.outQueueMu.Lock()
	lanes := 0
	if m.out != nil && m.out.ch == a {
		lanes++
	}
	for l := range m.outOld {
		if l.ch == a {
			lanes++
		}
	}
	m.outQueueMu.Unlock()
	if lanes != 1 {
		t.Fatalf("%d lanes deliver to one channel, want 1", lanes)
	}
	m.sendToOutChan(&models.Mock{Name: "3"})
	for _, want := range []string{"filler", "1", "2", "3"} {
		select {
		case got := <-a:
			if got.Name != want {
				t.Fatalf("the channel carried %q, want %q", got.Name, want)
			}
		case <-time.After(time.Minute): // only turns a hang into a failure
			t.Fatalf("%q never came", want)
		}
	}
}

// A revoke queued for one channel is not sent on a channel set after it: the
// batch drainPendingRevokes took is its epoch's (trySendControlFrameFor).
func TestARevokeOfAnEarlierChannelIsNotSentOnALaterOne(t *testing.T) {
	m := New(nil)
	m.SetOutputChannel(make(chan *models.Mock, 1))
	before := m.revokeEpoch.Load()
	fresh := make(chan *models.Mock, 1)
	m.SetOutputChannel(fresh)
	if m.trySendControlFrameFor(&models.Mock{Name: "revoke"}, before) {
		t.Fatal("a revoke of the earlier channel's epoch was sent on the new channel")
	}
	if !m.trySendControlFrame(&models.Mock{Name: "revoke"}) {
		t.Fatal("a revoke of the current channel's epoch was not sent")
	}
}
