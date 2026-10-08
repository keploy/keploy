package manager

import (
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"
	"unsafe"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// The mocks the hold's budget is tested with: what a small HTTP call, a MySQL
// result set and a Mongo document look like once their parsers have built
// them. Each call allocates everything anew, as a parser does.

// smallHTTPMock is an outgoing JSON call: about 2 KiB of heap.
func smallHTTPMock(at time.Time, i int) *models.Mock {
	return &models.Mock{
		Version: "api.keploy.io/v1beta1", Name: "mocks", Kind: models.HTTP,
		ConnectionID: fmt.Sprintf("conn-%d", i),
		Spec: models.MockSpec{
			Metadata: map[string]string{"name": "Http", "operation": "GET", "type": "HTTP_CLIENT", "connID": fmt.Sprintf("%d", i)},
			HTTPReq: &models.HTTPReq{
				Method: "GET", ProtoMajor: 1, ProtoMinor: 1,
				URL: fmt.Sprintf("http://inventory.internal:8081/v1/items/%d?expand=price", i),
				Header: map[string]string{
					"Host": "inventory.internal:8081", "User-Agent": "Go-http-client/1.1", "Accept-Encoding": "gzip",
					"X-Request-Id": fmt.Sprintf("req-%032d", i), "Authorization": "Bearer " + strings.Repeat("t", 180+i%7),
				},
				Timestamp: at,
			},
			HTTPResp: &models.HTTPResp{
				StatusCode: 200, StatusMessage: "OK",
				Header: map[string]string{
					"Content-Type": "application/json", "Content-Length": "412", "Date": at.Format(time.RFC1123),
					"X-Trace-Id": fmt.Sprintf("trace-%032d", i),
				},
				Body:      fmt.Sprintf(`{"id":%d,"name":"%s","price":{"amount":1299,"currency":"INR"},"tags":["a","b","c"]}`, i, strings.Repeat("n", 330)),
				Timestamp: at.Add(3 * time.Millisecond),
			},
			ReqTimestampMock: at, ResTimestampMock: at.Add(3 * time.Millisecond),
		},
		TestModeInfo: models.TestModeInfo{Lifetime: models.LifetimePerTest, LifetimeDerived: true},
	}
}

// resultSetMock is a MySQL query answered with rows x cols text cells: with
// 400 rows of 12 columns, about 550 KiB of heap; with 4 rows, about 9.
func resultSetMock(at time.Time, i, rows, cols int) *models.Mock {
	set := &mysql.TextResultSet{ColumnCount: uint64(cols)}
	for c := 0; c < cols; c++ {
		set.Columns = append(set.Columns, &mysql.ColumnDefinition41{
			Catalog: "def", Schema: "shop", Table: "orders", OrgTable: "orders",
			Name: fmt.Sprintf("col_%d", c), OrgName: fmt.Sprintf("col_%d", c), CharacterSet: 45, ColumnLength: 255, Type: 253,
		})
	}
	for r := 0; r < rows; r++ {
		row := &mysql.TextRow{Header: mysql.Header{PayloadLength: uint32(cols * 12), SequenceID: byte(r)}}
		for c := 0; c < cols; c++ {
			row.Values = append(row.Values, mysql.ColumnEntry{
				Type: mysql.FieldTypeVarString, Name: fmt.Sprintf("col_%d", c),
				Value: fmt.Sprintf("v-%d-%d-%d", i, r, c),
			})
		}
		set.Rows = append(set.Rows, row)
	}
	return &models.Mock{
		Version: "api.keploy.io/v1beta1", Name: "mocks", Kind: models.MySQL,
		ConnectionID: fmt.Sprintf("conn-%d", i),
		Spec: models.MockSpec{
			Metadata: map[string]string{"type": "mocks", "connID": fmt.Sprintf("%d", i), "requestOperation": "COM_QUERY", "responseOperation": "TextResultSet"},
			MySQLRequests: []mysql.Request{{PacketBundle: mysql.PacketBundle{
				Header:  &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: 64}, Type: "COM_QUERY"},
				Message: &mysql.QueryPacket{Command: 3, Query: fmt.Sprintf("SELECT * FROM orders WHERE customer_id = %d ORDER BY created_at DESC LIMIT %d", i, rows)},
			}}},
			MySQLResponses: []mysql.Response{{PacketBundle: mysql.PacketBundle{
				Header:  &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: 1}, Type: "TextResultSet"},
				Message: set,
			}}},
			ReqTimestampMock: at, ResTimestampMock: at.Add(8 * time.Millisecond),
		},
		TestModeInfo: models.TestModeInfo{Lifetime: models.LifetimePerTest, LifetimeDerived: true},
	}
}

// documentMock is a Mongo find answered with docs documents of nested BSON.
func documentMock(at time.Time, i, docs int) *models.Mock {
	batch := make(bson.A, 0, docs)
	for d := 0; d < docs; d++ {
		batch = append(batch, bson.D{
			{Key: "_id", Value: bson.NewObjectID()},
			{Key: "sku", Value: fmt.Sprintf("sku-%d-%d", i, d)},
			{Key: "qty", Value: int64(d)},
			{Key: "price", Value: 12.5},
			{Key: "attrs", Value: bson.D{{Key: "colour", Value: "red"}, {Key: "notes", Value: strings.Repeat("d", 96)}}},
			{Key: "tags", Value: bson.A{"a", "b", fmt.Sprintf("t-%d", d)}},
		})
	}
	return &models.Mock{
		Version: "api.keploy.io/v1beta1", Name: "mocks", Kind: models.Mongo,
		ConnectionID: fmt.Sprintf("conn-%d", i),
		Spec: models.MockSpec{
			Metadata: map[string]string{"operation": "find", "connID": fmt.Sprintf("%d", i)},
			MongoRequests: []models.MongoRequest{{
				Header:  &models.MongoHeader{Length: 120, RequestID: int32(i), Opcode: 2013},
				Message: &models.MongoOpMessage{FlagBits: 0, Sections: []string{fmt.Sprintf(`{ SectionSingle msg: {"find":"items","filter":{"owner":%d}} }`, i)}},
			}},
			MongoResponses: []models.MongoResponse{{
				Header:  &models.MongoHeader{Length: 4096, ResponseTo: int32(i), Opcode: 2013},
				Message: bson.D{{Key: "cursor", Value: bson.D{{Key: "firstBatch", Value: batch}, {Key: "id", Value: int64(0)}}}, {Key: "ok", Value: 1.0}},
			}},
			ReqTimestampMock: at, ResTimestampMock: at.Add(5 * time.Millisecond),
		},
		TestModeInfo: models.TestModeInfo{Lifetime: models.LifetimePerTest, LifetimeDerived: true},
	}
}

// heapOf is the live heap n values built by mk take, in bytes: the growth of
// HeapAlloc across building them, both ends read after a full collection.
func heapOf(n int, mk func(i int) *models.Mock) (keep []*models.Mock, bytes int64) {
	keep = make([]*models.Mock, 0, n)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.GC()
	runtime.ReadMemStats(&before)
	for i := 0; i < n; i++ {
		keep = append(keep, mk(i))
	}
	runtime.GC()
	runtime.GC()
	runtime.ReadMemStats(&after)
	return keep, int64(after.HeapAlloc) - int64(before.HeapAlloc)
}

var sizedMocks = []struct {
	name string
	n    int
	mk   func(at time.Time, i int) *models.Mock
}{
	{"small HTTP call", 4000, smallHTTPMock},
	{"MySQL result set of 400 rows", 60, func(at time.Time, i int) *models.Mock { return resultSetMock(at, i, 400, 12) }},
	{"MySQL result set of 4 rows", 2000, func(at time.Time, i int) *models.Mock { return resultSetMock(at, i, 4, 12) }},
	{"Mongo batch of 200 documents", 60, func(at time.Time, i int) *models.Mock { return documentMock(at, i, 200) }},
}

// mockSize is what the hold's byte budget counts, so it must follow the heap a
// mock really takes: not below 0.7 of it (the budget would then let the hold
// take more than it says) and not above 1.5 (it would give requests up early).
// Not parallel: it reads the process's heap.
func TestMockSizeFollowsTheHeapAMockTakes(t *testing.T) {
	at := time.Now()
	for _, c := range sizedMocks {
		keep, heap := heapOf(c.n, func(i int) *models.Mock { return c.mk(at, i) })
		var est int64
		for _, mk := range keep {
			est += mockSize(mk)
		}
		ratio := float64(est) / float64(heap)
		t.Logf("%-30s %5d mocks: heap %7.1f KiB each, mockSize %7.1f KiB each, ratio %.2f",
			c.name, c.n, float64(heap)/float64(c.n)/1024, float64(est)/float64(c.n)/1024, ratio)
		if ratio < 0.7 || ratio > 1.5 {
			t.Errorf("%s: mockSize says %d bytes for %d mocks that take %d bytes of heap (ratio %.2f, want 0.7 to 1.5)",
				c.name, est, c.n, heap, ratio)
		}
		runtime.KeepAlive(keep)
	}
}

// A slice that views a larger buffer is not charged the buffer: a decoder that
// hands out views of its read buffer makes many of them, and charging each the
// rest of the buffer sized a 64 KiB mock at 2 MiB, so the hold gave requests
// up with a thirtieth of its budget in use. One built by appending is charged
// its capacity: that is heap it holds.
func TestMockSizeOfViewsIntoOneBuffer(t *testing.T) {
	t.Parallel()
	buf := make([]byte, 64<<10)
	views := httpMockAt(time.Now())
	for i := 0; i < 64; i++ {
		views.Spec.MongoRequests = append(views.Spec.MongoRequests, models.MongoRequest{Message: buf[i<<10 : (i+1)<<10]})
	}
	if got := mockSize(views); got > 4*64<<10 || got < 64<<10 {
		t.Fatalf("a mock of 64 views of 1 KiB into one 64 KiB buffer is sized %d KiB, want 64 to 256", got>>10)
	}
	grown := httpMockAt(time.Now())
	grown.Spec.MongoRequests = []models.MongoRequest{{Message: append(make([]byte, 0, 48<<10), make([]byte, 40<<10)...)}}
	if got := mockSize(grown); got < 48<<10 {
		t.Fatalf("a mock holding a 48 KiB array is sized %d KiB", got>>10)
	}
}

// A mock past the walk's bound is sized as the hold's whole ceiling, never as
// small.
func TestMockSizeOfAHugeMockIsTheWholeCeiling(t *testing.T) {
	t.Parallel()
	mk := smallHTTPMock(time.Now(), 1)
	mk.Spec.Metadata = nil
	cells := make([]mysql.ColumnEntry, mockSizeNodes)
	mk.Spec.MySQLResponses = []mysql.Response{{PacketBundle: mysql.PacketBundle{Message: &mysql.TextRow{Values: cells}}}}
	if got := mockSize(mk); got < MaxHeldBytes {
		t.Fatalf("a mock of %d cells is sized %d bytes, below the hold's whole budget %d", len(cells), got, MaxHeldBytes)
	}
	if got := mockSize(nil); got != 0 {
		t.Fatalf("a nil mock is sized %d", got)
	}
}

// fillHold holds mocks built by mk, one at a time, for one request in flight
// since long before the stale horizon: each is requested while it runs and,
// older than the horizon and in no resolved window, is what a resolve's stale
// cutoff used to drop. It stops after limit mocks, or with the one that made
// the manager give the request up, and reports how many it added and whether
// the request was given up. The manager is returned so the hold stays alive.
func fillHold(mk func(at time.Time, i int) *models.Mock, limit int) (mgr *SyncMockManager, added int, gaveUp bool) {
	mgr = openWindowManager(make(chan *models.Mock, 16), make(chan models.TestMockMapping, 16))
	start := time.Now().Add(-60 * time.Second)
	slow := mgr.OpenWindow(start, nil)
	for added < limit && !gaveUp {
		mgr.AddMock(mk(start.Add(time.Duration(added+1)*time.Millisecond), added))
		added++
		k := time.Now()
		mgr.ResolveRange(k, k, "test-k", true, false)
		mgr.mu.Lock()
		gaveUp = slow.givenUp
		mgr.mu.Unlock()
	}
	return mgr, added, gaveUp
}

// liveHeap is the heap in use after a full collection.
func liveHeap() int64 {
	var ms runtime.MemStats
	runtime.GC()
	runtime.GC()
	runtime.ReadMemStats(&ms)
	return int64(ms.HeapAlloc)
}

// The hold keeps within its byte budget whatever the mocks are: the heap it
// really takes when it is full, with a request in flight far past the stale
// horizon, is MaxHeldBytes give or take the estimate (see
// TestMockSizeFollowsTheHeapAMockTakes), for 2 KiB HTTP mocks and for result
// sets of half a megabyte alike. Bounded by count, 8192 of those result sets
// were 4 GiB. A manager the test builds has a pool of its own of that budget
// (poolLocked), as one app alone in a process has the process's.
// Not parallel: it reads the process's heap.
func TestHoldKeepsWithinItsByteBudget(t *testing.T) {
	for _, c := range sizedMocks {
		// How many of them fill the hold: the next one has the request given
		// up, and what was held for it let go.
		mgr, n, gaveUp := fillHold(c.mk, 1<<20)
		if !gaveUp {
			t.Fatalf("%s: %d mocks held and the request was never given up", c.name, n)
		}
		mgr.mu.Lock()
		left, leftBytes := len(mgr.held)+len(mgr.buffer), mgr.heldBytes
		mgr.mu.Unlock()
		if left != 0 || leftBytes != 0 {
			t.Fatalf("%s: giving the only request in flight up left %d mocks, %d bytes counted", c.name, left, leftBytes)
		}

		// The full hold, one mock short of that, and the heap it takes.
		before := liveHeap()
		mgr, n, gaveUp = fillHold(c.mk, n-1)
		heap := liveHeap() - before
		mgr.mu.Lock()
		held, counted := len(mgr.held), mgr.heldBytes
		mgr.mu.Unlock()
		runtime.KeepAlive(mgr)
		t.Logf("%-30s the hold is full at %5d mocks: %5.1f MiB of heap, %5.1f MiB counted (budget %d MiB)",
			c.name, held, float64(heap)/(1<<20), float64(counted)/(1<<20), MaxHeldBytes>>20)
		if gaveUp || held != n {
			t.Fatalf("%s: holds %d of %d mocks, given up %v: the fixture did not fill the hold", c.name, held, n, gaveUp)
		}
		if counted > MaxHeldBytes {
			t.Errorf("%s: %d bytes held with the request not given up, over the budget %d", c.name, counted, MaxHeldBytes)
		}
		if limit := MaxHeldBytes * 3 / 2; heap > limit {
			t.Errorf("%s: the full hold takes %d bytes of heap, want at most %d (the budget %d and the estimate's slack)", c.name, heap, limit, MaxHeldBytes)
		}
		if heap < MaxHeldBytes/2 {
			t.Errorf("%s: the hold is full at %d bytes of heap, under half its budget %d: the estimate reads far too large", c.name, heap, MaxHeldBytes)
		}
	}
}

// The budget bounds what is held for a request younger than the stale horizon
// too: with droppable mocks arriving faster than the budget takes, the hold
// never takes more than MaxHeldBytes give or take the estimate. Not parallel:
// it reads the process's heap.
func TestHoldKeepsWithinItsBudgetForAYoungRequest(t *testing.T) {
	for _, c := range sizedMocks {
		// A duplicate's leftovers, a second old, held for a request two
		// seconds in flight: none of it is beyond the horizon.
		fill := func(limit int) (mgr *SyncMockManager, added int, gaveUp bool) {
			mgr = openWindowManager(make(chan *models.Mock, 16), make(chan models.TestMockMapping, 16))
			young := mgr.OpenWindow(time.Now().Add(-2*time.Second), nil)
			for added < limit && !gaveUp {
				mgr.AddMock(c.mk(time.Now().Add(-time.Second), added))
				added++
				mgr.DeleteMocksStrictlyBefore(time.Now())
				mgr.mu.Lock()
				gaveUp = young.givenUp
				mgr.mu.Unlock()
			}
			return mgr, added, gaveUp
		}
		_, n, gaveUp := fill(1 << 20)
		if !gaveUp {
			t.Fatalf("%s: %d mocks held and the request was never given up", c.name, n)
		}
		before := liveHeap()
		mgr, n, gaveUp := fill(n - 1)
		heap := liveHeap() - before
		mgr.mu.Lock()
		held, counted := len(mgr.held), mgr.heldBytes
		young := 0
		for _, h := range mgr.held {
			if !h.mock.Spec.ReqTimestampMock.Before(time.Now().Add(-StaleHorizon)) {
				young++
			}
		}
		mgr.mu.Unlock()
		runtime.KeepAlive(mgr)
		t.Logf("%-30s the hold is full at %5d mocks: %5.1f MiB of heap, %5.1f MiB counted (budget %d MiB)",
			c.name, held, float64(heap)/(1<<20), float64(counted)/(1<<20), MaxHeldBytes>>20)
		if gaveUp || held != n || young != held {
			t.Fatalf("%s: holds %d of %d mocks, %d of them younger than the horizon, given up %v: the fixture did not fill the hold with young mocks", c.name, held, n, young, gaveUp)
		}
		if counted > MaxHeldBytes {
			t.Errorf("%s: %d bytes held with the request not given up, over the budget %d", c.name, counted, MaxHeldBytes)
		}
		if limit := MaxHeldBytes * 3 / 2; heap > limit {
			t.Errorf("%s: the full hold takes %d bytes of heap, want at most %d (the budget %d and the estimate's slack)", c.name, heap, limit, MaxHeldBytes)
		}
	}
}

// The hold's counts follow it through every way a mock enters and leaves it.
func TestHoldCountsFollowTheHold(t *testing.T) {
	t.Parallel()
	out := make(chan *models.Mock, 256)
	mgr := openWindowManager(out, make(chan models.TestMockMapping, 16))
	pool := newHoldPool(1 << 40) // never over: what it counts is all that is checked
	mgr.pool = pool
	check := func(step string) {
		t.Helper()
		checkPool(t, pool, mgr)
		mgr.mu.Lock()
		defer mgr.mu.Unlock()
		var all int64
		for _, h := range mgr.held {
			if h.size != mockSize(h.mock) {
				t.Fatalf("%s: a held mock is counted as %d bytes, its size is %d", step, h.size, mockSize(h.mock))
			}
			all += h.size
		}
		if all != mgr.heldBytes {
			t.Fatalf("%s: counted %d bytes held; the hold has %d", step, mgr.heldBytes, all)
		}
	}
	start := time.Now().Add(-30 * time.Second)
	slow := mgr.OpenWindow(start, nil)
	later := mgr.OpenWindow(start.Add(10*time.Second), nil)
	// Held past the stale cutoff, old and young, out of request order.
	for _, at := range []time.Duration{9 * time.Second, time.Second, 15 * time.Second, 5 * time.Second, 12 * time.Second} {
		mgr.AddMock(smallHTTPMock(start.Add(at), int(at/time.Second)))
	}
	k := time.Now()
	mgr.ResolveRange(k, k, "test-k", true, false)
	check("after the stale cutoff held them")
	// A duplicate's leftovers younger than the horizon join them.
	for i := 0; i < 4; i++ {
		mgr.AddMock(smallHTTPMock(time.Now().Add(-time.Duration(i+1)*time.Second), 100+i))
	}
	mgr.DeleteMocksStrictlyBefore(time.Now())
	check("after a duplicate's prune held younger ones")
	// A kept window takes those inside it.
	mgr.ResolveRange(start.Add(4*time.Second), start.Add(9*time.Second), "test-mid", true, false)
	check("after a kept window took a part")
	// The later window yields: nothing it held changes hands (no held mock
	// has an owner), and the counts stay as they were.
	later.Yield(start.Add(13 * time.Second))
	check("after a window yielded")
	// The oldest window ends: what only it could own goes.
	slow.Close()
	check("after the oldest window ended")
	// Memory pressure lets go of the rest.
	mgr.SetMemoryPressure(true)
	mgr.SetMemoryPressure(false)
	check("after memory pressure")
	later.Close()
	check("after the last window ended")
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	if len(mgr.held) != 0 || mgr.heldBytes != 0 {
		t.Fatalf("no window is open and the hold counts %d mocks, %d bytes", len(mgr.held), mgr.heldBytes)
	}
}

// A captured exchange's window is one allocation of 112 B (BenchmarkOpenWindowClose):
// what a told loss keeps (its tally epoch, beside the flags) fits in it.
func TestWindowIsOneAllocationOf112Bytes(t *testing.T) {
	if n := unsafe.Sizeof(Window{}); n > 112 {
		t.Fatalf("a window is %d B, past the 112 B size class it was", n)
	}
	mgr := openWindowManager(make(chan *models.Mock, 1), make(chan models.TestMockMapping, 1))
	at := time.Now()
	if n := testing.AllocsPerRun(1000, func() { mgr.OpenWindow(at, nil).Close() }); n != 1 {
		t.Fatalf("opening and ending a window allocates %v times, want once", n)
	}
}

// The cost a captured exchange pays for its window: opened at its first byte,
// ended at its verdict.
func BenchmarkOpenWindowClose(b *testing.B) {
	mgr := openWindowManager(make(chan *models.Mock, 1), make(chan models.TestMockMapping, 1))
	at := time.Now()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		mgr.OpenWindow(at, nil).Close()
	}
}

// The same with other requests in flight: the open windows are a heap.
func BenchmarkOpenWindowCloseAmong1000(b *testing.B) {
	mgr := openWindowManager(make(chan *models.Mock, 1), make(chan models.TestMockMapping, 1))
	at := time.Now()
	for i := 0; i < 1000; i++ {
		mgr.OpenWindow(at.Add(-time.Duration(i)*time.Millisecond), nil)
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		mgr.OpenWindow(at, nil).Close()
	}
}

// What sizing costs a mock as it enters the hold.
func BenchmarkMockSize(b *testing.B) {
	at := time.Now()
	for _, c := range []struct {
		name string
		mk   *models.Mock
	}{
		{"smallHTTP", smallHTTPMock(at, 1)},
		{"resultSet400rows", resultSetMock(at, 1, 400, 12)},
		{"mongo200docs", documentMock(at, 1, 200)},
	} {
		b.Run(c.name, func(b *testing.B) {
			b.ReportAllocs()
			var n int64
			for i := 0; i < b.N; i++ {
				n += mockSize(c.mk)
			}
			b.ReportMetric(float64(n)/float64(b.N), "bytes-sized")
		})
	}
}

// A duplicate's prune with a request in flight past the stale horizon and the
// hold near its budget: one more mock is held, the hold is fitted. What a
// reaper call costs at the bound, against the same call with nothing held.
func BenchmarkPruneWithTheHoldNearItsBudget(b *testing.B) {
	for _, full := range []bool{false, true} {
		name := "emptyHold"
		if full {
			name = "holdNearBudget"
		}
		b.Run(name, func(b *testing.B) {
			out := make(chan *models.Mock, 2048)
			mgr := openWindowManager(out, make(chan models.TestMockMapping, 1))
			start := time.Now().Add(-30 * time.Second)
			var slow *Window
			if full {
				slow = mgr.OpenWindow(start, nil)
				// Held beyond the horizon up to 4 MiB short of the budget: room
				// for the 1024 young ones held between the resolves below.
				for i, total := 0, int64(0); ; i++ {
					mk := smallHTTPMock(start.Add(time.Duration(i)*time.Microsecond), i)
					if total += mockSize(mk); total > MaxHeldBytes-4<<20 {
						break
					}
					mgr.AddMock(mk)
				}
				mgr.DeleteMocksStrictlyBefore(start.Add(10 * time.Second))
				if !slow.Claim() {
					b.Fatal("fixture: the request in flight was given up as the hold was filled")
				}
				mgr.mu.Lock()
				slow.kept = false // Claim pins the window; hand it back
				mgr.mu.Unlock()
			}
			// Pre-built, so the loop times the manager and not the fixture;
			// each is stamped as it is added.
			mocks := make([]*models.Mock, b.N)
			for i := range mocks {
				mocks[i] = smallHTTPMock(time.Time{}, i)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				// One duplicate: its egress mock, then its prune. With the slow
				// request in flight the mock is held (younger than the horizon,
				// so the hold stays just under its budget); without, dropped.
				now := time.Now()
				mocks[i].Spec.ReqTimestampMock = now.Add(-time.Second)
				mgr.AddMock(mocks[i])
				mgr.DeleteMocksStrictlyBefore(now)
				if full && i%1024 == 1023 {
					// Keep the young part from growing without bound across
					// b.N: a kept resolve takes it, as a real one would.
					b.StopTimer()
					k := time.Now()
					mgr.ResolveRange(k.Add(-2*time.Second), k, "test-k", true, false)
					for len(out) > 0 {
						<-out
					}
					b.StartTimer()
				}
			}
			b.StopTimer()
			if full && !slow.Claim() {
				b.Fatal("the request in flight was given up: the benchmark held more than it meant to")
			}
		})
	}
}
