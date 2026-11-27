package manager

import (
	"errors"
	"fmt"
	"math/rand"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"weak"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils/stall"
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
	onPut      func()
	onPeek     func()
	onDiscard  func()
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
	if l.factory.onPut != nil {
		l.factory.onPut()
	}
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
	if l.factory.onPeek != nil {
		l.factory.onPeek()
	}
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
	if l.factory.onDiscard != nil {
		l.factory.onDiscard()
	}
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

	// Force spilling after ~2 KiB queued in RAM so the burst spans RAM and spill.
	spill := &fakeSpillFactory{ramLimit: 2048}
	mgr.SetOutQueueSpill(spill)

	const total = 40
	var delivered atomic.Int64
	now := time.Now().Add(-time.Second)
	for i := 0; i < total; i++ {
		m := makeSizedMock(fmt.Sprintf("mock-%03d", i), 2048, now.Add(time.Duration(i)*time.Millisecond))
		res := mgr.send(m, fmt.Sprintf("tc-%03d", i), func(ok bool) {
			if ok {
				delivered.Add(1)
			}
		})
		if res != MockTaken {
			t.Fatalf("send(%d) = %v, want MockTaken", i, res)
		}
	}
	if spill.puts.Load() == 0 {
		t.Fatal("expected overflow mocks to spill when RAM threshold exceeded")
	}

	// Drain consumer and verify exact FIFO order across RAM and spill boundary.
	for i := 0; i < total; i++ {
		select {
		case got := <-out:
			want := fmt.Sprintf("mock-%03d", i)
			if got.Name != want {
				t.Fatalf("position %d: got %q, want %q (FIFO violated across RAM/spill)", i, got.Name, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for mock %d/%d", i, total)
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for mgr.OutQueued() {
		if time.Now().After(deadline) {
			t.Fatal("OutQueued remained true after draining all mocks")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if delivered.Load() != total {
		t.Fatalf("delivered callbacks = %d, want %d", delivered.Load(), total)
	}
	mgr.CloseOutChan()
}

// TestOutQueueSpill_InterleaveRAMAndSpillExactFIFO drains concurrently while
// sending so processOutQueued repeatedly drops below RAMLimit while spill is
// non-empty, killing MF2 (dropping the alreadySpilled FIFO guard).
func TestOutQueueSpill_InterleaveRAMAndSpillExactFIFO(t *testing.T) {
	mgr := New(zap.NewNop())
	mgr.outClock, mgr.outStall = &stall.ManualClock{}, time.Hour
	out := make(chan *models.Mock, 2)
	mgr.SetOutputChannel(out)

	spill := &fakeSpillFactory{ramLimit: 8192}
	mgr.SetOutQueueSpill(spill)

	const total = 1200
	got := make([]string, 0, total)
	doneC := make(chan struct{})
	go func() {
		defer close(doneC)
		r := rand.New(rand.NewSource(42))
		for len(got) < total {
			mk := <-out
			got = append(got, mk.Name)
			if r.Intn(32) == 0 {
				time.Sleep(time.Duration(r.Intn(1500)) * time.Microsecond)
			}
		}
	}()

	r := rand.New(rand.NewSource(99))
	now := time.Now().Add(-time.Second)
	var delivered atomic.Int64
	for i := 0; i < total; i++ {
		m := makeSizedMock(fmt.Sprintf("iv-%05d", i), 512+r.Intn(3072), now.Add(time.Duration(i)*time.Microsecond))
		if d := mgr.send(m, fmt.Sprintf("tc-%d", i), func(ok bool) {
			if ok {
				delivered.Add(1)
			}
		}); d != MockTaken {
			t.Fatalf("send(%d) dropped: %v", i, d)
		}
		if r.Intn(64) == 0 {
			time.Sleep(time.Duration(r.Intn(2000)) * time.Microsecond)
		}
	}
	select {
	case <-doneC:
	case <-time.After(15 * time.Second):
		t.Fatalf("timed out draining interleaved stream at %d/%d", len(got), total)
	}
	deadline := time.Now().Add(5 * time.Second)
	for mgr.OutQueued() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	for i, name := range got {
		want := fmt.Sprintf("iv-%05d", i)
		if name != want {
			t.Fatalf("position %d: got %q, want %q (interleave FIFO violated)", i, name, want)
		}
	}
	if delivered.Load() != total {
		t.Fatalf("delivered = %d, want %d", delivered.Load(), total)
	}
	if spill.puts.Load() == 0 || spill.puts.Load() >= total {
		t.Fatalf("expected mix of RAM and spill sends; puts=%d of %d", spill.puts.Load(), total)
	}
	mgr.CloseOutChan()
}

// TestOutQueueSpill_ExhaustedSpillUsesRAMTailAndFailsClosedPastBound proves S2:
// when spill declines because the shared disk budget is full, SyncMockManager
// buffers behind the spill in a RAM tail up to queueBound in exact FIFO order
// (RAM head -> spill -> RAM tail) and fails closed once queueBound is exceeded.
func TestOutQueueSpill_ExhaustedSpillUsesRAMTailAndFailsClosedPastBound(t *testing.T) {
	mgr := New(zap.NewNop())
	mgr.outClock, mgr.outStall = &stall.ManualClock{}, time.Hour
	out := make(chan *models.Mock, 1)
	out <- &models.Mock{Name: "filler"}
	mgr.SetOutputChannel(out)

	// Leave ~20 KiB of headroom below the non-shared queueBound (48 MiB) so:
	// - mock 0 enters RAM head (~4 KiB)
	// - mocks 1..2 spill (maxItems=2)
	// - mocks 3..5 fall back to the RAM tail (~12 KiB, still within bound!)
	// - mocks 6..11 exceed bound and fail closed via MockDroppedOutputFull.
	nonSharedBound := outQueueMaxBytes - outQueueSharedRoom
	baseQueued := processOutQueued.Load()
	reserve := (nonSharedBound - 20*1024) - baseQueued
	if reserve > 0 {
		processOutQueued.Add(reserve)
		defer processOutQueued.Add(-reserve)
	}

	spill := &fakeSpillFactory{ramLimit: 1024, maxItems: 2}
	mgr.SetOutQueueSpill(spill)

	var droppedOwners []string
	var droppedMu sync.Mutex
	var onDropCount atomic.Int64
	mgr.OnDrop(func(_ *models.Mock) { onDropCount.Add(1) })

	now := time.Now().Add(-time.Second)
	var keptNames []string
	for i := 0; i < 12; i++ {
		owner := fmt.Sprintf("tc-tail-%02d", i)
		name := fmt.Sprintf("tail-%02d", i)
		m := makeSizedMock(name, 4096, now.Add(time.Duration(i)*time.Millisecond))
		switch res := mgr.send(m, owner, nil); res {
		case MockDroppedOutputFull:
			droppedMu.Lock()
			droppedOwners = append(droppedOwners, owner)
			droppedMu.Unlock()
		case MockTaken:
			keptNames = append(keptNames, name)
		default:
			t.Fatalf("unexpected send result %v", res)
		}
	}
	if spill.puts.Load() != 2 {
		t.Fatalf("spill puts = %d, want 2", spill.puts.Load())
	}
	// Must have buffered RAM tail mocks behind the 2 spilled mocks (kept > 3: RAM head + 2 spill + RAM tail).
	if len(keptNames) <= 3 {
		t.Fatalf("expected RAM tail to absorb mocks behind full spill up to bound; kept only %d (%v)", len(keptNames), keptNames)
	}
	if len(droppedOwners) == 0 || onDropCount.Load() != int64(len(droppedOwners)) {
		t.Fatalf("expected fail-closed drops past bound; droppedOwners=%d onDrop=%d", len(droppedOwners), onDropCount.Load())
	}
	for _, owner := range droppedOwners {
		if !mgr.WasMockDroppedForTC(owner) {
			t.Fatalf("dropped owner %q missing from WasMockDroppedForTC", owner)
		}
	}

	// Drain filler + all kept mocks and verify exact RAM head -> spill -> RAM tail FIFO order.
	<-out
	for i, want := range keptNames {
		select {
		case got := <-out:
			if got.Name != want {
				t.Fatalf("kept position %d: got %q, want %q (RAM head -> spill -> RAM tail FIFO violated)", i, got.Name, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out draining kept mock %d/%d", i, len(keptNames))
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for mgr.OutQueued() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	mgr.CloseOutChan()
	wantQueued := baseQueued
	if reserve > 0 {
		wantQueued += reserve
	}
	if got := processOutQueued.Load(); got != wantQueued {
		t.Fatalf("processOutQueued after tail drain = %d, want %d (RAM tail accounting leaked)", got, wantQueued)
	}
}

// TestOutQueueSpill_RAMLimitClampedToQueueBound kills MR1 (removing rl < ramCap clamp).
func TestOutQueueSpill_RAMLimitClampedToQueueBound(t *testing.T) {
	mgr := New(zap.NewNop())
	mgr.outClock, mgr.outStall = &stall.ManualClock{}, time.Hour
	out := make(chan *models.Mock, 1)
	out <- &models.Mock{Name: "filler"}
	mgr.SetOutputChannel(out)

	nonSharedBound := outQueueMaxBytes - outQueueSharedRoom
	baseQueued := processOutQueued.Load()
	reserve := (nonSharedBound - 8*1024) - baseQueued
	if reserve > 0 {
		processOutQueued.Add(reserve)
		defer processOutQueued.Add(-reserve)
	}

	// RAMLimit > queueBound must clamp to queueBound: once RAM hits queueBound,
	// subsequent mocks must spill (not exceed queueBound in RAM).
	spill := &fakeSpillFactory{ramLimit: outQueueMaxBytes * 4}
	mgr.SetOutQueueSpill(spill)

	now := time.Now().Add(-time.Second)
	for i := 0; i < 6; i++ {
		if res := mgr.send(makeSizedMock(fmt.Sprintf("clamp-%02d", i), 4096, now), fmt.Sprintf("tc-%d", i), nil); res != MockTaken {
			t.Fatalf("send(%d) = %v, want MockTaken", i, res)
		}
	}
	if spill.puts.Load() == 0 {
		t.Fatal("RAMLimit above queueBound was not clamped to queueBound (spill was never invoked)")
	}
	<-out
	for i := 0; i < 6; i++ {
		<-out
	}
	deadline := time.Now().Add(5 * time.Second)
	for mgr.OutQueued() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	mgr.CloseOutChan()
}

func TestOutQueueSpill_ConsumerStallStreamsDropsWithoutSlurp(t *testing.T) {
	baseQueued := processOutQueued.Load()
	mgr := New(zap.NewNop())
	mgr.outStall = 40 * time.Millisecond
	mgr.outCloseStall = 20 * time.Millisecond

	out := make(chan *models.Mock, 1)
	out <- &models.Mock{Name: "filler"}
	mgr.SetOutputChannel(out)

	// Allow 4 items in spill and let the rest land in RAM tail so giveUpOutQueue
	// exercises RAM head -> detached spill -> RAM tail in exact FIFO order.
	spill := &fakeSpillFactory{ramLimit: 1024, maxItems: 4}
	mgr.SetOutQueueSpill(spill)

	const total = 10
	var doneFalse atomic.Int64
	var dropOrder []string
	var dropMu sync.Mutex
	mgr.OnDrop(func(m *models.Mock) {
		dropMu.Lock()
		dropOrder = append(dropOrder, m.Name)
		dropMu.Unlock()
	})
	now := time.Now().Add(-time.Second)
	for i := 0; i < total; i++ {
		m := makeSizedMock(fmt.Sprintf("stall-%02d", i), 2048, now.Add(time.Duration(i)*time.Millisecond))
		mgr.send(m, fmt.Sprintf("tc-stall-%02d", i), func(ok bool) {
			if !ok {
				doneFalse.Add(1)
			}
		})
	}

	deadline := time.Now().Add(5 * time.Second)
	for mgr.OutQueued() {
		if time.Now().After(deadline) {
			t.Fatal("drainer did not give up stalled consumer")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if doneFalse.Load() != total {
		t.Fatalf("done(false) count = %d, want %d", doneFalse.Load(), total)
	}
	dropMu.Lock()
	defer dropMu.Unlock()
	if len(dropOrder) != total {
		t.Fatalf("OnDrop count = %d, want %d", len(dropOrder), total)
	}
	for i, name := range dropOrder {
		want := fmt.Sprintf("stall-%02d", i)
		if name != want {
			t.Fatalf("stall drop order %d: got %q, want %q", i, name, want)
		}
		if !mgr.WasMockDroppedForTC(fmt.Sprintf("tc-stall-%02d", i)) {
			t.Fatalf("tc-stall-%02d missing from WasMockDroppedForTC", i)
		}
	}
	mgr.CloseOutChan()
	if got := processOutQueued.Load(); got != baseQueued {
		t.Fatalf("processOutQueued after stall give-up = %d, want %d (head/tail bytes leaked)", got, baseQueued)
	}
}

// TestOutQueueSpill_CorruptPeekFailsClosedWithFullMetadataAndAsyncReusable
// proves B2(a), B2(b), and kills MC1/MC2/MC4/MC5:
//   - OnDrop receives ConnectionID and full Spec.Metadata (connID, keploy.pod, keploy.ns)
//   - owned spilled mocks are marked in WasMockDroppedForTC
//   - startup/shared mocks open an orphan span
//   - under KEPLOY_PROVEN=1, both an async session mock and a MySQL session-
//     reusable command (COM_PING with stated lifetime) leave out later test cases
func TestOutQueueSpill_CorruptPeekFailsClosedWithFullMetadataAndAsyncReusable(t *testing.T) {
	t.Run("owned_shared_and_ondrop_metadata", func(t *testing.T) {
		mgr := New(zap.NewNop())
		mgr.outClock, mgr.outStall = &stall.ManualClock{}, time.Hour
		out := make(chan *models.Mock, 1)
		out <- &models.Mock{Name: "filler"}
		mgr.SetOutputChannel(out)

		spill := &fakeSpillFactory{ramLimit: 1, failPeekAt: 1}
		mgr.SetOutQueueSpill(spill)

		var toldMu sync.Mutex
		var told []*models.Mock
		mgr.OnDrop(func(m *models.Mock) {
			toldMu.Lock()
			told = append(told, m)
			toldMu.Unlock()
		})

		now := time.Now().Add(-time.Second)
		var doneFalse atomic.Int64
		for i := 0; i < 6; i++ {
			m := makeSizedMock(fmt.Sprintf("corrupt-%02d", i), 2048, now.Add(time.Duration(i)*time.Millisecond))
			m.ConnectionID = "conn-42"
			m.Spec.Metadata["connID"] = "conn-42"
			m.Spec.Metadata["keploy.pod"] = "orders-pod-0"
			m.Spec.Metadata["keploy.ns"] = "shop"
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

		// Exercise concurrent SetLogger while dropCorruptSpill logs so -race
		// verifies loggerMu synchronization (dropLogger).
		stopLogger := make(chan struct{})
		loggerDone := make(chan struct{})
		go func() {
			defer close(loggerDone)
			for {
				select {
				case <-stopLogger:
					return
				default:
					mgr.SetLogger(zap.NewNop())
				}
			}
		}()

		<-out // filler
		<-out // corrupt-00 (in RAM head)
		deadline := time.Now().Add(5 * time.Second)
		for mgr.OutQueued() && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		close(stopLogger)
		<-loggerDone
		if doneFalse.Load() == 0 {
			t.Fatal("expected done(false) callbacks on corrupt spill")
		}
		if !mgr.WasMockDroppedForTC("tc-corrupt-05") {
			t.Fatal("owned spilled mock missing from WasMockDroppedForTC after corrupt Peek")
		}
		if orphaned, _ := mgr.WasMockOrphanedInWindow(now.Add(10*time.Millisecond), now.Add(20*time.Millisecond)); !orphaned {
			t.Fatal("startup/shared spilled mock did not open orphan span after corrupt Peek")
		}
		toldMu.Lock()
		defer toldMu.Unlock()
		if len(told) == 0 {
			t.Fatal("OnDrop was never called for corrupt spill mocks (MC1)")
		}
		for _, mk := range told {
			if mk.ConnectionID != "conn-42" || mk.Spec.Metadata["connID"] != "conn-42" ||
				mk.Spec.Metadata["keploy.pod"] != "orders-pod-0" || mk.Spec.Metadata["keploy.ns"] != "shop" {
				t.Fatalf("OnDrop stub %s lost attribution metadata: connID=%q md=%v", mk.Name, mk.ConnectionID, mk.Spec.Metadata)
			}
		}
		mgr.CloseOutChan()
	})

	t.Run("proven_async_and_mysql_session_reusable_leave_out_later_tests", func(t *testing.T) {
		t.Setenv(provenswitch.Env, "1")
		cases := []struct {
			name string
			make func(req time.Time) *models.Mock
		}{
			{
				name: "async_session_mock",
				make: func(req time.Time) *models.Mock {
					return &models.Mock{
						Kind: models.HTTP,
						Name: "async-sess",
						Spec: models.MockSpec{
							Metadata:         map[string]string{"type": "mocks"},
							ReqTimestampMock: req,
							Async:            &models.AsyncMeta{Lane: "events", Seq: 1},
						},
						TestModeInfo: models.TestModeInfo{Lifetime: models.LifetimeSession},
					}
				},
			},
			{
				name: "mysql_session_reusable_command",
				make: func(req time.Time) *models.Mock {
					m := &models.Mock{
						Kind: models.MySQL,
						Name: "mysql-ping",
						Spec: models.MockSpec{
							Metadata: map[string]string{
								"type":                     "mocks",
								models.MetadataLifetimeKey: models.MetadataLifetimeSession,
							},
							ReqTimestampMock: req,
							MySQLRequests: []mysql.Request{{
								PacketBundle: mysql.PacketBundle{
									Header: &mysql.PacketInfo{Type: "COM_PING"},
								},
							}},
						},
						TestModeInfo: models.TestModeInfo{Lifetime: models.LifetimeSession},
					}
					return m
				},
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				req := time.Now().Add(-time.Minute)
				mgr := New(zap.NewNop())
				mgr.outClock, mgr.outStall = &stall.ManualClock{}, time.Hour
				out := make(chan *models.Mock, 1)
				out <- &models.Mock{Name: "filler"}
				mgr.SetOutputChannel(out)
				mgr.SetOutQueueSpill(&fakeSpillFactory{ramLimit: 1, failPeekAt: 1})

				mgr.send(makeSizedMock("head", 1024, req), "", nil) // RAM head
				mgr.send(tc.make(req.Add(time.Millisecond)), "", nil)
				<-out // filler
				<-out // head
				deadline := time.Now().Add(5 * time.Second)
				for mgr.OutQueued() && time.Now().Before(deadline) {
					time.Sleep(5 * time.Millisecond)
				}
				if mgr.SendingTestCase("later", req.Add(10*time.Second), req.Add(11*time.Second)) {
					t.Fatalf("%s lost in corrupt spill did NOT leave out later test case under KEPLOY_PROVEN=1", tc.name)
				}
				mgr.CloseOutChan()
			})
		}
	})
}

func TestSetDefaultOutQueueSpill_AppliesToNewManagers(t *testing.T) {
	spill := &fakeSpillFactory{ramLimit: 1024}
	SetDefaultOutQueueSpill(spill)
	t.Cleanup(func() { SetDefaultOutQueueSpill(nil) })

	mgr := New(zap.NewNop())
	out := make(chan *models.Mock, 1)
	mgr.SetOutputChannel(out)

	now := time.Now().Add(-time.Second)
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

// TestOutQueueSpill_SpilledMocksNotPinnedOnHeap proves B1 across AddMock AND
// SendConfigMock (both of which register m.sending and pass done closures):
// spilled *models.Mock structs are GC-collected while sitting in spill and live
// heap stays bounded far below burst size.
func TestOutQueueSpill_SpilledMocksNotPinnedOnHeap(t *testing.T) {
	for _, via := range []string{"AddMock", "SendConfigMock", "sendTaken"} {
		t.Run(via, func(t *testing.T) {
			mgr := New(zap.NewNop())
			mgr.outClock, mgr.outStall = &stall.ManualClock{}, time.Hour
			out := make(chan *models.Mock, 1)
			out <- &models.Mock{Name: "filler"}
			mgr.SetOutputChannel(out)
			mgr.SetOutQueueSpill(&nonRetainingSpillFactory{ramLimit: 1024})

			runtime.GC()
			runtime.GC()
			var baseMS runtime.MemStats
			runtime.ReadMemStats(&baseMS)

			const total = 120
			const payloadSize = 256 << 10 // 30 MiB burst
			now := time.Now().Add(-time.Second)
			ptrs := make([]weak.Pointer[models.Mock], 0, total)
			for i := 0; i < total; i++ {
				m := makeSizedMock(fmt.Sprintf("gc-%03d", i), payloadSize, now.Add(time.Duration(i)*time.Millisecond))
				if i >= 5 {
					ptrs = append(ptrs, weak.Make(m))
				}
				switch via {
				case "AddMock":
					mgr.AddMock(m)
				case "SendConfigMock":
					mgr.SendConfigMock(m)
				case "sendTaken":
					batch := []ownedMock{{mock: m, owner: fmt.Sprintf("tc-%d", i), noted: false}}
					mgr.mu.Lock()
					mgr.noteTaken(batch)
					mgr.mu.Unlock()
					mgr.sendTaken(batch)
				}
			}
			if !mgr.PendingIn(now, now.Add(time.Second)) {
				t.Fatal("PendingIn returned false while spilled mocks are still pending")
			}

			collected := 0
			for attempt := 0; attempt < 6; attempt++ {
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
				t.Fatalf("via=%s: only %d/%d spilled mocks were GC-collected while in spill", via, collected, len(ptrs))
			}

			var afterMS runtime.MemStats
			runtime.ReadMemStats(&afterMS)
			held := int64(afterMS.HeapAlloc) - int64(baseMS.HeapAlloc)
			if held > 10<<20 {
				t.Fatalf("via=%s: live heap grew by %d MiB on a 30 MiB spilled burst; expected < 10 MiB", via, held>>20)
			}

			<-out // filler
			for i := 0; i < total; i++ {
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
		})
	}
}

// TestOutQueueSpill_SpillIODoesNotHoldOutQueueMu proves S3: a slow spill Put
// does not hold SyncMockManager.outQueueMu, so OutQueued() and RAM-head reads
// remain non-blocking while disk I/O is in flight.
func TestOutQueueSpill_SpillIODoesNotHoldOutQueueMu(t *testing.T) {
	mgr := New(zap.NewNop())
	mgr.outClock, mgr.outStall = &stall.ManualClock{}, time.Hour
	out := make(chan *models.Mock, 1)
	out <- &models.Mock{Name: "filler"}
	mgr.SetOutputChannel(out)

	inPut := make(chan struct{})
	releasePut := make(chan struct{})
	var once sync.Once
	spill := &fakeSpillFactory{
		ramLimit: 1024,
		onPut: func() {
			once.Do(func() {
				close(inPut)
				<-releasePut
			})
		},
	}
	mgr.SetOutQueueSpill(spill)

	now := time.Now().Add(-time.Second)
	// First queued mock goes to RAM head.
	mgr.send(makeSizedMock("ram-head", 2048, now), "", nil)
	// Second mock triggers Put() on a goroutine and blocks inside onPut.
	sendDone := make(chan struct{})
	go func() {
		defer close(sendDone)
		mgr.send(makeSizedMock("slow-spill", 2048, now.Add(time.Millisecond)), "", nil)
	}()

	<-inPut
	// While Put() is blocked in "disk I/O", OutQueued() must NOT block on outQueueMu.
	checked := make(chan bool, 1)
	go func() { checked <- mgr.OutQueued() }()
	select {
	case q := <-checked:
		if !q {
			t.Fatal("OutQueued() returned false while spill Put was in flight")
		}
	case <-time.After(2 * time.Second):
		close(releasePut)
		t.Fatal("OutQueued() blocked on outQueueMu while spill Put() was executing")
	}

	close(releasePut)
	<-sendDone
	<-out // filler
	if got := <-out; got.Name != "ram-head" {
		t.Fatalf("first delivered = %q, want ram-head", got.Name)
	}
	if got := <-out; got.Name != "slow-spill" {
		t.Fatalf("second delivered = %q, want slow-spill", got.Name)
	}
	deadline := time.Now().Add(5 * time.Second)
	for mgr.OutQueued() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	mgr.CloseOutChan()

	// Slow-Peek variant (kills ML2): while the drainer is blocked inside
	// lane.spill.Peek(), OutQueued() must not block on outQueueMu.
	t.Run("slow_peek", func(t *testing.T) {
		mgr := New(zap.NewNop())
		mgr.outClock, mgr.outStall = &stall.ManualClock{}, time.Hour
		out := make(chan *models.Mock, 1)
		out <- &models.Mock{Name: "filler"}
		mgr.SetOutputChannel(out)

		inPeek := make(chan struct{})
		releasePeek := make(chan struct{})
		var once sync.Once
		spill := &fakeSpillFactory{
			ramLimit: 1024,
			onPeek: func() {
				once.Do(func() {
					close(inPeek)
					<-releasePeek
				})
			},
		}
		mgr.SetOutQueueSpill(spill)

		now := time.Now().Add(-time.Second)
		mgr.send(makeSizedMock("ram-head", 2048, now), "", nil)
		mgr.send(makeSizedMock("slow-peek", 2048, now.Add(time.Millisecond)), "", nil)
		<-out // filler
		<-out // ram-head; drainer now enters Peek() for slow-peek
		<-inPeek

		checked := make(chan bool, 1)
		go func() { checked <- mgr.OutQueued() }()
		select {
		case q := <-checked:
			if !q {
				t.Fatal("OutQueued() returned false while spill Peek was in flight")
			}
		case <-time.After(2 * time.Second):
			close(releasePeek)
			t.Fatal("OutQueued() blocked on outQueueMu while spill Peek() was executing")
		}
		close(releasePeek)
		if got := <-out; got.Name != "slow-peek" {
			t.Fatalf("delivered = %q, want slow-peek", got.Name)
		}
		deadline := time.Now().Add(5 * time.Second)
		for mgr.OutQueued() && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		mgr.CloseOutChan()
	})
}

// TestOutQueueSpill_NilHookMidSpillKeepsFIFO proves R2-S1: clearing the spill
// hook while a lane already holds spilled mocks routes subsequent sends to
// lane.tail (up to bound) so they never overtake spilled mocks via lane.queue.
func TestOutQueueSpill_NilHookMidSpillKeepsFIFO(t *testing.T) {
	SetDefaultOutQueueSpill(nil)
	mgr := New(zap.NewNop())
	mgr.outClock, mgr.outStall = &stall.ManualClock{}, time.Hour
	out := make(chan *models.Mock, 1)
	out <- &models.Mock{Name: "filler"}
	mgr.SetOutputChannel(out)
	mgr.SetOutQueueSpill(&fakeSpillFactory{ramLimit: 1024})
	base := processOutQueued.Load()

	now := time.Now().Add(-time.Second)
	for i := 0; i < 4; i++ {
		if res := mgr.send(makeSizedMock(fmt.Sprintf("a-%d", i), 2048, now.Add(time.Duration(i)*time.Millisecond)), "", nil); res != MockTaken {
			t.Fatalf("send(a-%d) = %v, want MockTaken", i, res)
		}
	}
	// Clear hook while a-1..a-3 are spilled; b-0 must stay behind them in FIFO order.
	mgr.SetOutQueueSpill(nil)
	if res := mgr.send(makeSizedMock("b-0", 2048, now.Add(10*time.Millisecond)), "", nil); res != MockTaken {
		t.Fatalf("send(b-0) = %v, want MockTaken", res)
	}

	<-out // filler
	want := []string{"a-0", "a-1", "a-2", "a-3", "b-0"}
	for i, w := range want {
		select {
		case got := <-out:
			if got.Name != w {
				t.Fatalf("position %d: got %q, want %q (nil hook mid-spill broke FIFO)", i, got.Name, w)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for %q at position %d", w, i)
		}
	}
	waitOutQueue(t, mgr)
	if delta := processOutQueued.Load() - base; delta != 0 {
		t.Fatalf("processOutQueued off by %d after nil-hook mid-spill drain", delta)
	}
	mgr.CloseOutChan()
}

// TestOutQueueSpill_StallWithNilHookTail verifies that when head, spill, and a
// nil-hook RAM tail all hold mocks and the consumer stalls, every mock gets
// done(false) once, OnDrop and WasMockDroppedForTC record every drop,
// processOutQueued returns to its start (killing MS3 and MS2), and a mock sent
// after give-up is delivered cleanly.
func TestOutQueueSpill_StallWithNilHookTail(t *testing.T) {
	SetDefaultOutQueueSpill(nil)
	ch := make(chan *models.Mock, 1)
	ch <- &models.Mock{Name: "filler"}
	clk := &stall.ManualClock{}
	mgr := New(zap.NewNop())
	mgr.outClock, mgr.outStall = clk, time.Hour
	mgr.SetOutputChannel(ch)
	mgr.SetOutQueueSpill(&fakeSpillFactory{ramLimit: 1024})
	var told atomic.Int64
	mgr.OnDrop(func(*models.Mock) { told.Add(1) })
	base := processOutQueued.Load()
	now := time.Now().Add(-time.Minute)

	var mu sync.Mutex
	outcome := map[string]int{}
	var names []string
	sendOne := func(name string) {
		names = append(names, name)
		if d := mgr.send(makeSizedMock(name, 2048, now.Add(time.Duration(len(names))*time.Millisecond)), "tc-"+name, func(ok bool) {
			mu.Lock()
			if ok {
				outcome[name]++
			} else {
				outcome[name] += 100
			}
			mu.Unlock()
		}); d != MockTaken {
			t.Fatalf("%s: send = %v, want MockTaken", name, d)
		}
	}
	for i := 0; i < 4; i++ {
		sendOne(fmt.Sprintf("a-%d", i))
	}
	mgr.SetOutQueueSpill(nil)
	for i := 0; i < 5; i++ {
		sendOne(fmt.Sprintf("b-%d", i))
	}

	waitClockPending(t, clk)
	clk.Advance(time.Hour)
	waitOutQueue(t, mgr)

	mu.Lock()
	for _, n := range names {
		if outcome[n] != 100 {
			t.Errorf("%s outcome = %d, want 100 (done(false) once)", n, outcome[n])
		}
		if !mgr.WasMockDroppedForTC("tc-" + n) {
			t.Errorf("tc-%s not recorded as dropped", n)
		}
	}
	mu.Unlock()
	if got := told.Load(); got != int64(len(names)) {
		t.Errorf("OnDrop told %d, want %d", got, len(names))
	}
	if delta := processOutQueued.Load() - base; delta != 0 {
		t.Fatalf("processOutQueued leaked %d after a stall with a nil-hook tail", delta)
	}
	if mgr.OutQueued() {
		t.Fatal("OutQueued still true after give-up")
	}

	<-ch // filler
	if d := mgr.send(makeSizedMock("c-0", 2048, now.Add(time.Second)), "", nil); d != MockTaken {
		t.Fatalf("c-0 after give-up = %v, want MockTaken", d)
	}
	select {
	case got := <-ch:
		if got.Name != "c-0" {
			t.Fatalf("got %q after give-up, want c-0", got.Name)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for c-0 after give-up")
	}
	waitOutQueue(t, mgr)
	if delta := processOutQueued.Load() - base; delta != 0 {
		t.Fatalf("processOutQueued off by %d after follow-up delivery", delta)
	}
	mgr.CloseOutChan()
}

// TestOutQueueSpill_NilHookMidSpillEnforcesBound verifies that the first mock
// entering a nil-hook tail behind spilled mocks still respects the per-class
// byte bound (killing MS4).
func TestOutQueueSpill_NilHookMidSpillEnforcesBound(t *testing.T) {
	SetDefaultOutQueueSpill(nil)
	ch := make(chan *models.Mock, 1)
	ch <- &models.Mock{Name: "filler"}
	mgr := New(zap.NewNop())
	mgr.outClock, mgr.outStall = &stall.ManualClock{}, time.Hour
	mgr.SetOutputChannel(ch)
	mgr.SetOutQueueSpill(&fakeSpillFactory{ramLimit: 1024})
	var told atomic.Int64
	mgr.OnDrop(func(*models.Mock) { told.Add(1) })
	base := processOutQueued.Load()
	now := time.Now().Add(-time.Minute)

	for i := 0; i < 4; i++ {
		if d := mgr.send(makeSizedMock(fmt.Sprintf("a-%d", i), 2048, now.Add(time.Duration(i)*time.Millisecond)), "", nil); d != MockTaken {
			t.Fatalf("a-%d: %v", i, d)
		}
	}
	mgr.SetOutQueueSpill(nil)

	nonShared := outQueueMaxBytes - outQueueSharedRoom
	reserve := nonShared - processOutQueued.Load()
	processOutQueued.Add(reserve)
	defer processOutQueued.Add(-reserve)

	if d := mgr.send(makeSizedMock("b-0", 1<<20, now.Add(time.Second)), "tc-b0", nil); d != MockDroppedOutputFull {
		t.Fatalf("first nil-hook tail mock with no room = %v, want MockDroppedOutputFull", d)
	}
	if !mgr.WasMockDroppedForTC("tc-b0") || told.Load() != 1 {
		t.Fatalf("dropped=%v told=%d, want true and 1", mgr.WasMockDroppedForTC("tc-b0"), told.Load())
	}

	<-ch // filler
	for i := 0; i < 4; i++ {
		select {
		case got := <-ch:
			if want := fmt.Sprintf("a-%d", i); got.Name != want {
				t.Fatalf("pos %d: got %q want %q", i, got.Name, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out draining a-%d", i)
		}
	}
	waitOutQueue(t, mgr)
	if delta := processOutQueued.Load() - base - reserve; delta != 0 {
		t.Fatalf("processOutQueued off by %d", delta)
	}
	mgr.CloseOutChan()
}

type flippingSpillFactory struct {
	ramLimit int64
	allow    atomic.Bool
}

func (f *flippingSpillFactory) RAMLimit() int64 { return f.ramLimit }
func (f *flippingSpillFactory) NewLane() OutQueueSpillLane {
	return &flippingSpillLane{factory: f}
}

type flippingSpillLane struct {
	factory *flippingSpillFactory
	items   []fakeSpillItem
	closed  bool
}

func (l *flippingSpillLane) Put(mock *models.Mock, owner string, size int64) bool {
	if l.closed || !l.factory.allow.Load() {
		return false
	}
	l.items = append(l.items, fakeSpillItem{mock: mock, owner: owner, size: size})
	return true
}
func (l *flippingSpillLane) Len() int { return len(l.items) }
func (l *flippingSpillLane) Peek() (*models.Mock, string, int64, error) {
	if len(l.items) == 0 {
		return nil, "", 0, nil
	}
	it := l.items[0]
	return it.mock, it.owner, it.size, nil
}
func (l *flippingSpillLane) Pop() {
	if len(l.items) == 0 {
		return
	}
	l.items[0] = fakeSpillItem{}
	l.items = l.items[1:]
}
func (l *flippingSpillLane) Detach() OutQueueSpillLane {
	if len(l.items) == 0 {
		l.closed = true
		return nil
	}
	d := &flippingSpillLane{factory: l.factory, items: l.items}
	l.items = nil
	l.closed = true
	return d
}
func (l *flippingSpillLane) Discard() int {
	n := len(l.items)
	l.items = nil
	l.closed = true
	return n
}

// TestOutQueueSpill_FlippingBudgetStressExactFIFOAndZeroAccounting kills MT1,
// MT8, and MT9: while multiple senders push bursts and the spill budget flips
// repeatedly between open and full (forcing transitions across RAM head, spill,
// RAM tail, tail promotion, and drainer exit), every sender's sequence arrives
// exactly once in FIFO order with zero stranded mocks and exact processOutQueued
// accounting.
func TestOutQueueSpill_FlippingBudgetStressExactFIFOAndZeroAccounting(t *testing.T) {
	baseQueued := processOutQueued.Load()
	mgr := New(zap.NewNop())
	mgr.outClock, mgr.outStall = &stall.ManualClock{}, time.Hour
	out := make(chan *models.Mock, 4)
	mgr.SetOutputChannel(out)

	spill := &flippingSpillFactory{ramLimit: 4096}
	spill.allow.Store(true)
	mgr.SetOutQueueSpill(spill)

	const senders = 4
	const perSender = 400
	const total = senders * perSender

	// Flipper alternates spill budget open/full so sends hit spill, decline
	// into lane.tail, and later hit len(lane.tail) > 0 while spill is open again.
	stopFlip := make(chan struct{})
	flipDone := make(chan struct{})
	go func() {
		defer close(flipDone)
		on := true
		for {
			select {
			case <-stopFlip:
				return
			default:
				on = !on
				spill.allow.Store(on)
				time.Sleep(150 * time.Microsecond)
			}
		}
	}()

	gotBySender := make([][]int, senders)
	consumerDone := make(chan struct{})
	go func() {
		defer close(consumerDone)
		for count := 0; count < total; count++ {
			select {
			case mk := <-out:
				var sid, seq int
				if _, err := fmt.Sscanf(mk.Name, "s%d-%d", &sid, &seq); err != nil {
					t.Errorf("unexpected mock name %q: %v", mk.Name, err)
					return
				}
				gotBySender[sid] = append(gotBySender[sid], seq)
				if count%29 == 0 {
					time.Sleep(200 * time.Microsecond)
				}
			case <-time.After(15 * time.Second):
				t.Errorf("timed out draining flipping-budget stress at %d/%d", count, total)
				return
			}
		}
	}()

	var wg sync.WaitGroup
	var delivered atomic.Int64
	now := time.Now().Add(-time.Second)
	for s := 0; s < senders; s++ {
		wg.Add(1)
		go func(sid int) {
			defer wg.Done()
			for seq := 0; seq < perSender; seq++ {
				mk := makeSizedMock(fmt.Sprintf("s%d-%d", sid, seq), 1024, now)
				if res := mgr.send(mk, "", func(ok bool) {
					if ok {
						delivered.Add(1)
					}
				}); res != MockTaken {
					t.Errorf("sender %d seq %d dropped: %v", sid, seq, res)
					return
				}
				if seq%37 == 0 {
					// Let the drainer drain down to empty so senders race drainer exit (kills MT9).
					time.Sleep(300 * time.Microsecond)
				}
			}
		}(s)
	}
	wg.Wait()
	close(stopFlip)
	<-flipDone
	<-consumerDone

	deadline := time.Now().Add(5 * time.Second)
	for mgr.OutQueued() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	mgr.CloseOutChan()

	if delivered.Load() != total {
		t.Fatalf("delivered callbacks = %d, want %d", delivered.Load(), total)
	}
	for s := 0; s < senders; s++ {
		if len(gotBySender[s]) != perSender {
			t.Fatalf("sender %d received %d mocks, want %d", s, len(gotBySender[s]), perSender)
		}
		for seq, got := range gotBySender[s] {
			if got != seq {
				t.Fatalf("sender %d position %d: got seq %d, want %d (FIFO violated under flipping budget)", s, seq, got, seq)
			}
		}
	}
	if got := processOutQueued.Load(); got != baseQueued {
		t.Fatalf("processOutQueued after flipping-budget stress = %d, want %d", got, baseQueued)
	}
}

// TestOutQueueSpill_SendDuringDrainerExitDiscardNotStranded kills MT9
// deterministically: a mock sent while drainOutQueue has released outQueueMu to
// Discard an empty spill handle must be observed by the exit re-check and
// delivered rather than stranded when draining turns false.
func TestOutQueueSpill_SendDuringDrainerExitDiscardNotStranded(t *testing.T) {
	baseQueued := processOutQueued.Load()
	mgr := New(zap.NewNop())
	mgr.outClock, mgr.outStall = &stall.ManualClock{}, time.Hour
	out := make(chan *models.Mock, 1)
	out <- &models.Mock{Name: "filler"}
	mgr.SetOutputChannel(out)

	now := time.Now().Add(-time.Second)
	var once sync.Once
	var sentInWindow atomic.Bool
	spill := &fakeSpillFactory{
		ramLimit: 1024,
		onDiscard: func() {
			once.Do(func() {
				// Drainer released outQueueMu and holds only spillMu; sending a
				// RAM-head mock while lane.draining==true enqueues it without
				// starting a new drainer, so only the exit re-check delivers it.
				if res := mgr.send(makeSizedMock("during-exit", 512, now.Add(5*time.Millisecond)), "", nil); res != MockTaken {
					t.Errorf("send during drainer exit Discard = %v, want MockTaken", res)
					return
				}
				sentInWindow.Store(true)
			})
		},
	}
	mgr.SetOutQueueSpill(spill)

	mgr.send(makeSizedMock("head-0", 2048, now), "", nil)
	mgr.send(makeSizedMock("spilled-1", 2048, now.Add(time.Millisecond)), "", nil)
	<-out // filler
	if got := <-out; got.Name != "head-0" {
		t.Fatalf("first = %q, want head-0", got.Name)
	}
	if got := <-out; got.Name != "spilled-1" {
		t.Fatalf("second = %q, want spilled-1", got.Name)
	}
	select {
	case got := <-out:
		if got.Name != "during-exit" {
			t.Fatalf("third = %q, want during-exit", got.Name)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("mock sent during drainer exit Discard was stranded (exit re-check missing)")
	}
	if !sentInWindow.Load() {
		t.Fatal("onDiscard hook did not fire")
	}
	deadline := time.Now().Add(5 * time.Second)
	for mgr.OutQueued() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	mgr.CloseOutChan()
	if got := processOutQueued.Load(); got != baseQueued {
		t.Fatalf("processOutQueued = %d, want %d", got, baseQueued)
	}
}

// TestOutQueueSpill_ManySmallMocksBoundedInRAM proves B3 (R5): each spilled
// mock's in-RAM spillItemMeta is counted against processOutQueued, so a burst
// of many small mocks with realistic metadata cannot outgrow the process RAM
// bound (outQueueMaxBytes / outQueueMaxBytes-outQueueSharedRoom). Once
// metadata reaches the bound, spilling stops and fail-closed drops take over,
// and draining, corrupt-peek, or consumer-stall returns processOutQueued to
// its exact baseline (killing MR5_no_meta_check, MR5_no_meta_add,
// MR5_no_meta_pop_sub, MR5_no_meta_giveup_sub, MR5_no_meta_corrupt_sub).
func TestOutQueueSpill_ManySmallMocksBoundedInRAM(t *testing.T) {
	SetDefaultOutQueueSpill(nil)
	makeSmallMetaMock := func(i int, ts time.Time) *models.Mock {
		return &models.Mock{
			Version:      models.GetVersion(),
			Kind:         models.GENERIC,
			Name:         fmt.Sprintf("small-%06d", i),
			ConnectionID: fmt.Sprintf("conn-%06d", i),
			Spec: models.MockSpec{
				Metadata: map[string]string{
					"type":              "mocks",
					"connID":            fmt.Sprintf("conn-%06d", i),
					"keploy.pod":        "orders-0",
					"keploy.ns":         "shop",
					"keploy.deployment": "orders",
					"operation":         "find",
				},
				ReqTimestampMock: ts,
				ResTimestampMock: ts.Add(time.Millisecond),
				GenericRequests: []models.Payload{{
					Message: []models.OutputBinary{{Data: strings.Repeat("x", 1024)}},
				}},
			},
		}
	}

	t.Run("bounded_heap_and_fail_closed_drop", func(t *testing.T) {
		base := processOutQueued.Load()
		ch := make(chan *models.Mock, 1)
		ch <- &models.Mock{Name: "filler"}
		mgr := New(zap.NewNop())
		mgr.outClock, mgr.outStall = &stall.ManualClock{}, time.Hour
		mgr.SetOutputChannel(ch)
		// nonRetainingSpillFactory discards payload bytes just like an on-disk
		// spill, isolating OSS's in-RAM spillItemMeta growth.
		mgr.SetOutQueueSpill(&nonRetainingSpillFactory{ramLimit: 8 << 20})

		var onDropCount atomic.Int64
		mgr.OnDrop(func(*models.Mock) { onDropCount.Add(1) })

		runtime.GC()
		var beforeMS runtime.MemStats
		runtime.ReadMemStats(&beforeMS)

		const total = 100_000
		nonSharedBound := outQueueMaxBytes - outQueueSharedRoom
		now := time.Now().Add(-time.Minute)
		var kept, dropped int64
		var droppedOwners []string
		for i := 0; i < total; i++ {
			owner := fmt.Sprintf("tc-%d", i%64)
			res := mgr.send(makeSmallMetaMock(i, now.Add(time.Duration(i)*time.Microsecond)), owner, nil)
			cur := processOutQueued.Load() - base
			if cur > nonSharedBound {
				t.Fatalf("at mock %d: processOutQueued delta %d exceeded non-shared bound %d", i, cur, nonSharedBound)
			}
			switch res {
			case MockTaken:
				kept++
			case MockDroppedOutputFull:
				dropped++
				if len(droppedOwners) < 16 {
					droppedOwners = append(droppedOwners, owner)
				}
			default:
				t.Fatalf("mock %d: unexpected send decision %v", i, res)
			}
		}
		if kept == 0 || dropped == 0 {
			t.Fatalf("expected bounded spill then fail-closed drop across %d small mocks: kept=%d dropped=%d", total, kept, dropped)
		}
		if onDropCount.Load() != dropped {
			t.Fatalf("OnDrop=%d != dropped=%d", onDropCount.Load(), dropped)
		}
		for _, o := range droppedOwners {
			if !mgr.WasMockDroppedForTC(o) {
				t.Fatalf("dropped owner %q missing from WasMockDroppedForTC", o)
			}
		}

		runtime.GC()
		var peakMS runtime.MemStats
		runtime.ReadMemStats(&peakMS)
		var heapGrowth int64
		if peakMS.HeapAlloc > beforeMS.HeapAlloc {
			heapGrowth = int64(peakMS.HeapAlloc - beforeMS.HeapAlloc)
		}
		t.Logf("100k small mocks: kept=%d dropped=%d poqDelta=%d MiB heapGrowth=%.1f MiB",
			kept, dropped, (processOutQueued.Load()-base)/(1<<20), float64(heapGrowth)/(1<<20))
		if heapGrowth > 64<<20 {
			t.Fatalf("live heap grew %.1f MiB for %d small mocks (want <= 64 MiB)", float64(heapGrowth)/(1<<20), total)
		}

		// Drain every kept mock and confirm processOutQueued returns exactly to base.
		<-ch // filler
		for i := int64(0); i < kept; i++ {
			select {
			case <-ch:
			case <-time.After(10 * time.Second):
				t.Fatalf("timed out draining kept mock %d/%d", i, kept)
			}
		}
		waitOutQueue(t, mgr)
		mgr.CloseOutChan()
		if delta := processOutQueued.Load() - base; delta != 0 {
			t.Fatalf("processOutQueued leaked %d bytes after draining %d small mocks", delta, kept)
		}
	})

	t.Run("corrupt_peek_releases_meta_bytes", func(t *testing.T) {
		base := processOutQueued.Load()
		ch := make(chan *models.Mock, 1)
		ch <- &models.Mock{Name: "filler"}
		mgr := New(zap.NewNop())
		mgr.outClock, mgr.outStall = &stall.ManualClock{}, time.Hour
		mgr.SetOutputChannel(ch)
		mgr.SetOutQueueSpill(&fakeSpillFactory{ramLimit: 1024, failPeekAt: 1})

		now := time.Now().Add(-time.Minute)
		for i := 0; i < 6; i++ {
			if res := mgr.send(makeSmallMetaMock(i, now.Add(time.Duration(i)*time.Millisecond)), "tc-corrupt", nil); res != MockTaken {
				t.Fatalf("send %d = %v, want MockTaken", i, res)
			}
		}
		<-ch // filler
		select {
		case <-ch: // head mock delivered; next Peek fails corrupt and discards spilled meta
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for head mock")
		}
		waitOutQueue(t, mgr)
		mgr.CloseOutChan()
		if delta := processOutQueued.Load() - base; delta != 0 {
			t.Fatalf("processOutQueued leaked %d bytes after corrupt spill discard", delta)
		}
	})
}
