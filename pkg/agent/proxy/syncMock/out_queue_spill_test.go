package manager

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"weak"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

type fakeSpillItem struct {
	mock  *models.Mock
	owner string
	size  int64
}

type fakeSpillFactory struct {
	ramLimit   int64
	maxItems   int // 0 = unlimited
	failPeekAt int // >0 = fail Peek on Nth call
	lanes      atomic.Int64
	puts       atomic.Int64
	discards   atomic.Int64
}

func (f *fakeSpillFactory) RAMLimit() int64 { return f.ramLimit }

func (f *fakeSpillFactory) NewLane() OutQueueSpillLane {
	f.lanes.Add(1)
	return &fakeSpillLane{factory: f}
}

type fakeSpillLane struct {
	factory *fakeSpillFactory
	items   []fakeSpillItem
	closed  bool
	peeks   int
}

func (l *fakeSpillLane) Put(mock *models.Mock, owner string, size int64) bool {
	if l.closed {
		return false
	}
	if l.factory.maxItems > 0 && len(l.items) >= l.factory.maxItems {
		return false
	}
	l.items = append(l.items, fakeSpillItem{mock: mock, owner: owner, size: size})
	l.factory.puts.Add(1)
	return true
}

func (l *fakeSpillLane) Len() int { return len(l.items) }

func (l *fakeSpillLane) Peek() (*models.Mock, string, int64, error) {
	if len(l.items) == 0 {
		return nil, "", 0, nil
	}
	l.peeks++
	if l.factory.failPeekAt > 0 && l.peeks >= l.factory.failPeekAt {
		return nil, "", 0, errors.New("injected corrupt spill record")
	}
	it := l.items[0]
	return it.mock, it.owner, it.size, nil
}

func (l *fakeSpillLane) Pop() {
	if len(l.items) == 0 {
		return
	}
	l.items[0] = fakeSpillItem{}
	l.items = l.items[1:]
}

func (l *fakeSpillLane) Detach() OutQueueSpillLane {
	if len(l.items) == 0 {
		l.closed = true
		return nil
	}
	d := &fakeSpillLane{factory: l.factory, items: l.items}
	l.items = nil
	l.closed = true
	return d
}

func (l *fakeSpillLane) Discard() int {
	n := len(l.items)
	l.items = nil
	l.closed = true
	l.factory.discards.Add(1)
	return n
}

func makeSizedMock(name string, payloadBytes int, reqTime time.Time) *models.Mock {
	return &models.Mock{
		Kind: models.GENERIC,
		Name: name,
		Spec: models.MockSpec{
			Metadata:         map[string]string{"type": "mocks"},
			ReqTimestampMock: reqTime,
			GenericRequests: []models.Payload{{
				Message: []models.OutputBinary{{Data: string(make([]byte, payloadBytes))}},
			}},
		},
	}
}

func TestOutQueueSpill_ExactFIFOAcrossRAMAndSpill(t *testing.T) {
	mgr := New(zap.NewNop())
	out := make(chan *models.Mock, 1)
	mgr.SetOutputChannel(out)

	// Force spilling after ~2 KiB queued in RAM so the burst interleaves RAM and spill.
	spill := &fakeSpillFactory{ramLimit: 2048}
	mgr.SetOutQueueSpill(spill)

	const total = 40
	now := time.Now()
	var deliveredMu sync.Mutex
	doneDelivered := 0

	for i := 0; i < total; i++ {
		m := makeSizedMock(fmt.Sprintf("mock-%02d", i), 1024, now.Add(time.Duration(i)*time.Millisecond))
		res := mgr.send(m, fmt.Sprintf("tc-%02d", i), func(ok bool) {
			if ok {
				deliveredMu.Lock()
				doneDelivered++
				deliveredMu.Unlock()
			}
		})
		if res != MockTaken {
			t.Fatalf("mock %d: send returned %q, want MockTaken", i, res)
		}
	}
	if spill.puts.Load() == 0 {
		t.Fatal("expected spill Put calls > 0 under low RAMLimit")
	}
	if !mgr.OutQueued() {
		t.Fatal("expected OutQueued() == true while spill holds unread mocks")
	}

	got := make([]string, 0, total)
	for len(got) < total {
		select {
		case m := <-out:
			got = append(got, m.Name)
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out draining mocks: got %d/%d", len(got), total)
		}
	}
	mgr.CloseOutChan()

	for i, name := range got {
		want := fmt.Sprintf("mock-%02d", i)
		if name != want {
			t.Fatalf("position %d: got %q, want %q (FIFO violated across RAM/spill boundary)", i, name, want)
		}
	}
	deliveredMu.Lock()
	dc := doneDelivered
	deliveredMu.Unlock()
	if dc != total {
		t.Fatalf("done callbacks delivered = %d, want %d", dc, total)
	}
	if mgr.DropCount() != 0 {
		t.Fatalf("dropCount = %d, want 0", mgr.DropCount())
	}
	if spill.discards.Load() == 0 {
		t.Fatal("expected drained spill lane to be discarded on idle")
	}
}

func TestOutQueueSpill_ExhaustedSpillFailsClosed(t *testing.T) {
	mgr := New(zap.NewNop())
	out := make(chan *models.Mock, 1)
	mgr.SetOutputChannel(out)

	// Allow only 2 items in spill; once alreadySpilled is true and spill rejects
	// Put, subsequent mocks MUST fail closed (never overtaking spilled items in RAM).
	spill := &fakeSpillFactory{ramLimit: 1024, maxItems: 2}
	mgr.SetOutQueueSpill(spill)

	var onDropNames []string
	var mu sync.Mutex
	mgr.OnDrop(func(m *models.Mock) {
		mu.Lock()
		onDropNames = append(onDropNames, m.Name)
		mu.Unlock()
	})

	now := time.Now()
	dropped := 0
	for i := 0; i < 12; i++ {
		owner := fmt.Sprintf("tc-%02d", i)
		m := makeSizedMock(fmt.Sprintf("mock-%02d", i), 2048, now.Add(time.Duration(i)*time.Millisecond))
		res := mgr.send(m, owner, nil)
		if res == MockDroppedOutputFull {
			dropped++
			if !mgr.WasMockDroppedForTC(owner) {
				t.Fatalf("dropped mock for %q was not recorded in WasMockDroppedForTC", owner)
			}
		}
	}
	if dropped == 0 {
		t.Fatal("expected MockDroppedOutputFull once spill capacity was exhausted")
	}
	mu.Lock()
	od := len(onDropNames)
	mu.Unlock()
	if od != dropped {
		t.Fatalf("OnDrop called %d times, want %d", od, dropped)
	}

	// Drain surviving mocks cleanly.
	surviving := 12 - dropped
	for i := 0; i < surviving; i++ {
		select {
		case <-out:
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out draining surviving mock %d/%d", i, surviving)
		}
	}
	mgr.CloseOutChan()
}

func TestOutQueueSpill_ConsumerStallStreamsDropsWithoutSlurp(t *testing.T) {
	mgr := New(zap.NewNop())
	mgr.outStall = 40 * time.Millisecond
	out := make(chan *models.Mock, 1)
	mgr.SetOutputChannel(out)

	spill := &fakeSpillFactory{ramLimit: 1024}
	mgr.SetOutQueueSpill(spill)

	var onDropCount atomic.Int64
	mgr.OnDrop(func(_ *models.Mock) { onDropCount.Add(1) })

	// Fill outChan (1 item), then enqueue + spill 10 more items while nobody reads outChan.
	now := time.Now()
	var doneFalse atomic.Int64
	for i := 0; i < 12; i++ {
		m := makeSizedMock(fmt.Sprintf("stall-%02d", i), 2048, now.Add(time.Duration(i)*time.Millisecond))
		res := mgr.send(m, fmt.Sprintf("tc-stall-%02d", i), func(ok bool) {
			if !ok {
				doneFalse.Add(1)
			}
		})
		if res != MockTaken {
			t.Fatalf("mock %d: got %q, want MockTaken", i, res)
		}
	}

	deadline := time.Now().Add(5 * time.Second)
	for mgr.OutQueued() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for stalled consumer giveUpOutQueue")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Item 0 went directly into outChan; items 1..11 (both RAM and spilled) must all be dropped.
	if got := doneFalse.Load(); got != 11 {
		t.Fatalf("done(false) count = %d, want 11", got)
	}
	if got := onDropCount.Load(); got != 11 {
		t.Fatalf("OnDrop count = %d, want 11", got)
	}
	for i := 1; i < 12; i++ {
		tc := fmt.Sprintf("tc-stall-%02d", i)
		if !mgr.WasMockDroppedForTC(tc) {
			t.Fatalf("stalled spilled mock owner %q missing from WasMockDroppedForTC", tc)
		}
	}
	mgr.CloseOutChan()
}

func TestOutQueueSpill_CorruptPeekFailsClosedForOwnedAndSharedMocks(t *testing.T) {
	mgr := New(zap.NewNop())
	out := make(chan *models.Mock, 1)
	mgr.SetOutputChannel(out)

	// Fail Peek on the very first spilled mock read.
	spill := &fakeSpillFactory{ramLimit: 1024, failPeekAt: 1}
	mgr.SetOutQueueSpill(spill)

	now := time.Now().Add(-time.Second)
	// 0 -> outChan, 1 -> RAM queue, 2..5 -> spill (including a startup/shared mock with owner="").
	var doneFalse atomic.Int64
	for i := 0; i < 6; i++ {
		m := makeSizedMock(fmt.Sprintf("corrupt-%02d", i), 2048, now.Add(time.Duration(i)*time.Millisecond))
		owner := fmt.Sprintf("tc-corrupt-%02d", i)
		if i == 4 {
			m.TestModeInfo.IsStartup = true
			m.Spec.Metadata["type"] = "config"
			owner = ""
		}
		mgr.send(m, owner, func(ok bool) {
			if !ok {
				doneFalse.Add(1)
			}
		})
	}

	// Drain the non-spilled mocks so the drainer reaches the corrupt spill.
	for i := 0; i < 2; i++ {
		select {
		case <-out:
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out draining non-spilled mock %d", i)
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for mgr.OutQueued() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for corrupt spill drain")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if doneFalse.Load() == 0 {
		t.Fatal("expected done(false) callbacks on corrupt spill")
	}
	if !mgr.WasMockDroppedForTC("tc-corrupt-05") {
		t.Fatal("owned spilled mock missing from WasMockDroppedForTC after corrupt Peek")
	}
	// Shared/startup mock lost in corrupt spill must leave out subsequent test windows.
	if orphaned, _ := mgr.WasMockOrphanedInWindow(now.Add(10*time.Millisecond), now.Add(20*time.Millisecond)); !orphaned {
		t.Fatal("startup/shared spilled mock did not open orphan span after corrupt Peek")
	}
	mgr.CloseOutChan()
}

func TestSetDefaultOutQueueSpill_AppliesToNewManagers(t *testing.T) {
	spill := &fakeSpillFactory{ramLimit: 1024}
	SetDefaultOutQueueSpill(spill)
	t.Cleanup(func() { SetDefaultOutQueueSpill(nil) })

	mgr := New(zap.NewNop())
	out := make(chan *models.Mock, 1)
	mgr.SetOutputChannel(out)

	now := time.Now()
	for i := 0; i < 10; i++ {
		mgr.send(makeSizedMock(fmt.Sprintf("def-%02d", i), 2048, now), "", nil)
	}
	if spill.puts.Load() == 0 {
		t.Fatal("default OutQueueSpill was not used by New() manager")
	}
	for i := 0; i < 10; i++ {
		<-out
	}
	mgr.CloseOutChan()
}

// nonRetainingSpillLane discards the *models.Mock pointer on Put (modeling a
// disk-backed spill that serializes bytes and releases the Go struct) so the
// test can prove SyncMockManager itself (m.sending, done closures, spillMeta)
// does not pin spilled mocks on heap.
type nonRetainingSpillFactory struct{ ramLimit int64 }

func (f *nonRetainingSpillFactory) RAMLimit() int64            { return f.ramLimit }
func (f *nonRetainingSpillFactory) NewLane() OutQueueSpillLane { return &nonRetainingSpillLane{} }

type nonRetainingSpillLane struct{ count int }

func (l *nonRetainingSpillLane) Put(_ *models.Mock, _ string, _ int64) bool {
	l.count++
	return true
}
func (l *nonRetainingSpillLane) Len() int { return l.count }
func (l *nonRetainingSpillLane) Peek() (*models.Mock, string, int64, error) {
	if l.count == 0 {
		return nil, "", 0, nil
	}
	return &models.Mock{Kind: models.GENERIC, Name: "reconstructed"}, "", 64, nil
}
func (l *nonRetainingSpillLane) Pop() {
	if l.count > 0 {
		l.count--
	}
}
func (l *nonRetainingSpillLane) Detach() OutQueueSpillLane {
	n := l.count
	l.count = 0
	return &nonRetainingSpillLane{count: n}
}
func (l *nonRetainingSpillLane) Discard() int {
	n := l.count
	l.count = 0
	return n
}

func TestOutQueueSpill_SpilledMocksNotPinnedOnHeap(t *testing.T) {
	mgr := New(zap.NewNop())
	out := make(chan *models.Mock, 1)
	mgr.SetOutputChannel(out)
	mgr.SetOutQueueSpill(&nonRetainingSpillFactory{ramLimit: 1024})

	now := time.Now().Add(-time.Second)
	ptrs := make([]weak.Pointer[models.Mock], 0, 20)
	for i := 0; i < 20; i++ {
		m := makeSizedMock(fmt.Sprintf("gc-%02d", i), 4096, now.Add(time.Duration(i)*time.Millisecond))
		if i >= 5 {
			ptrs = append(ptrs, weak.Make(m))
		}
		mgr.AddMock(m)
	}
	// While mocks sit in spill, PendingIn must still report them in flight.
	if !mgr.PendingIn(now, now.Add(time.Second)) {
		t.Fatal("PendingIn returned false while spilled mocks are still pending")
	}
	// Force GC passes: spilled *models.Mock pointers must be reclaimable while
	// still sitting in spill (not pinned by m.sending or done closures).
	collected := 0
	for attempt := 0; attempt < 5; attempt++ {
		runtime.GC()
		collected = 0
		for _, wp := range ptrs {
			if wp.Value() == nil {
				collected++
			}
		}
		if collected == len(ptrs) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if collected != len(ptrs) {
		t.Fatalf("only %d/%d spilled mocks were GC-collected while in spill; SyncMockManager is still pinning *models.Mock on heap", collected, len(ptrs))
	}

	for i := 0; i < 20; i++ {
		select {
		case <-out:
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out draining mock %d", i)
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for mgr.OutQueued() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if mgr.PendingIn(now, now.Add(time.Second)) {
		t.Fatal("PendingIn still true after all spilled mocks drained")
	}
	mgr.CloseOutChan()
}
