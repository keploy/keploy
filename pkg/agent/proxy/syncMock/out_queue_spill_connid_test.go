package manager

import (
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils/stall"
	"go.uber.org/zap"
)

// makeConnIDShapeMock builds a small mock with a chosen ConnectionID /
// Spec.Metadata["connID"] combination for testing connID folding and exact
// metadata accounting across all parser shapes.
func makeConnIDShapeMock(name string, payload, keys int, at time.Time, shape string, i, conns int) *models.Mock {
	if conns <= 0 {
		conns = 1
	}
	c := "conn-" + strconv.Itoa(i%conns)
	pod := func() map[string]string {
		md := map[string]string{"type": "mocks", "keploy.pod": "orders-0", "keploy.ns": "shop", "operation": "find", "db": "orders"}
		for k := 0; len(md) < keys-1; k++ {
			md["keploy.label."+strconv.Itoa(k)] = "value-" + strconv.Itoa(k)
		}
		return md
	}
	var md map[string]string
	cid := ""
	switch shape {
	case "match", "labels50":
		md = pod()
		md["connID"] = c
		cid = c
	case "metaonly":
		md = pod()
		md["connID"] = c
	case "mismatch":
		md = pod()
		md["connID"] = c
		cid = "x-" + c
	case "connonly":
		md = map[string]string{"connID": c}
	case "cidempty":
		md = pod()
		md["connID"] = ""
	case "nocid":
		md = pod()
		cid = c
	case "nilmd":
		cid = c
	case "unique":
		md = pod()
		md["reqID"] = "req-" + name
		md["connID"] = c
		cid = c
	default:
		panic("makeConnIDShapeMock: unknown shape " + shape)
	}
	return &models.Mock{
		Version:      models.GetVersion(),
		Kind:         models.GENERIC,
		Name:         name,
		ConnectionID: cid,
		Spec: models.MockSpec{
			Metadata:         md,
			ReqTimestampMock: at,
			ResTimestampMock: at.Add(time.Millisecond),
			GenericRequests:  []models.Payload{{Message: []models.OutputBinary{{Data: string(make([]byte, payload))}}}},
		},
	}
}

func waitSpillFirstInHand(t *testing.T, m *SyncMockManager) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		m.outQueueMu.Lock()
		ok := m.out != nil && m.out.inHand && len(m.out.queue) == 0
		m.outQueueMu.Unlock()
		if ok {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for drainer to take first mock in hand")
}

func drainSpillNames(t *testing.T, ch chan *models.Mock, n int) []string {
	t.Helper()
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		select {
		case mk, ok := <-ch:
			if !ok {
				t.Fatalf("channel closed after %d/%d items", i, n)
			}
			out = append(out, mk.Name)
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out draining item %d/%d", i, n)
		}
	}
	return out
}

func inspectSpillAccounting(m *SyncMockManager) (ram, meta, metaRe int64, inFlight, metas, pooled, distinct int, bad []string) {
	m.outQueueMu.Lock()
	var lanes []*outLane
	if m.out != nil {
		lanes = append(lanes, m.out)
	}
	for l := range m.outOld {
		lanes = append(lanes, l)
	}
	for _, l := range lanes {
		ram += l.bytes + l.tailBytes
		inFlight += l.spillInFlight
	}
	m.outQueueMu.Unlock()
	for li, l := range lanes {
		l.spillMu.Lock()
		meta += l.spillMetaBytes
		refs := map[*spillInternedMap]int64{}
		for _, im := range l.spillMeta {
			re := spillItemMetaBaseBytes + int64(len(im.owner)+len(im.name)+len(im.connID)+len(im.kind))
			if im.done != nil {
				re += spillItemDoneBytes
			}
			if im.async != nil {
				re += spillItemAsyncBase + int64(len(im.async.Lane)+len(im.async.AnchorAfter))
			}
			if re != im.metaBytes {
				bad = append(bad, fmt.Sprintf("lane %d: meta %q stored %d recomputed %d", li, im.name, im.metaBytes, re))
			}
			metaRe += im.metaBytes
			if im.md != nil {
				refs[im.md]++
			}
		}
		for md, n := range refs {
			metaRe += md.bytes
			if md.refs != n {
				bad = append(bad, fmt.Sprintf("lane %d: interned map refs=%d, kept metas referencing it=%d", li, md.refs, n))
			}
			if want := spillMetadataMapBytes(md.m); md.bytes != want {
				bad = append(bad, fmt.Sprintf("lane %d: interned map bytes=%d sizer=%d", li, md.bytes, want))
			}
			if md.pooled && l.metaPool[md.key] != md {
				bad = append(bad, fmt.Sprintf("lane %d: pooled map referenced by kept metas is not the pool's entry for its key", li))
			}
		}
		for k, e := range l.metaPool {
			if e.refs <= 0 {
				bad = append(bad, fmt.Sprintf("lane %d: pool entry with refs=%d", li, e.refs))
			}
			if _, ok := refs[e]; !ok {
				bad = append(bad, fmt.Sprintf("lane %d: pool entry referenced by no kept meta (refs=%d)", li, e.refs))
			}
			if e.key != k || !e.pooled {
				bad = append(bad, fmt.Sprintf("lane %d: pool entry key/pooled mismatch", li))
			}
		}
		if len(l.metaPool) > maxSpillInternedMaps {
			bad = append(bad, fmt.Sprintf("lane %d: pool holds %d > %d", li, len(l.metaPool), maxSpillInternedMaps))
		}
		pooled += len(l.metaPool)
		distinct += len(refs)
		metas += len(l.spillMeta)
		if l.spill != nil && l.spill.Len() != len(l.spillMeta) {
			bad = append(bad, fmt.Sprintf("lane %d: spill.Len()=%d != len(spillMeta)=%d", li, l.spill.Len(), len(l.spillMeta)))
		}
		l.spillMu.Unlock()
	}
	return
}

func assertSpillAccountingExact(t *testing.T, where string, m *SyncMockManager, base int64) {
	t.Helper()
	ram, meta, metaRe, inFlight, _, _, _, bad := inspectSpillAccounting(m)
	poq := processOutQueued.Load() - base
	if inFlight != 0 {
		t.Fatalf("%s: not quiescent (inFlight=%d)", where, inFlight)
	}
	for _, b := range bad {
		t.Errorf("%s: %s", where, b)
	}
	if meta != metaRe {
		t.Errorf("%s: lane.spillMetaBytes=%d != recomputed %d", where, meta, metaRe)
	}
	if poq != ram+meta {
		t.Errorf("%s: processOutQueued delta %d != ram %d + meta %d", where, poq, ram, meta)
	}
}

type doneTracker struct {
	counts []atomic.Int32
}

func newDoneTracker(n int) *doneTracker {
	return &doneTracker{counts: make([]atomic.Int32, n)}
}

func (d *doneTracker) fn(i int) func(bool) {
	return func(bool) { d.counts[i].Add(1) }
}

func (d *doneTracker) assertAllOnce(t *testing.T, label string) {
	t.Helper()
	for i := range d.counts {
		if c := d.counts[i].Load(); c != 1 {
			t.Fatalf("%s: done[%d] called %d times, want 1", label, i, c)
		}
	}
}

// TestOutQueueSpill_ConnIDFoldRoundTrip verifies that on a corrupt spill read,
// OnDrop receives each spilled mock's exact ConnectionID, Spec.Metadata, Kind,
// and ReqTimestampMock across every connID-folding shape (including empty
// ConnectionID with metadata connID and mismatching ConnectionID vs metadata
// connID), with exact accounting before and after the corrupt discard.
func TestOutQueueSpill_ConnIDFoldRoundTrip(t *testing.T) {
	SetDefaultOutQueueSpill(nil)
	now := time.Now().Add(-time.Minute)
	shapes := []string{"match", "metaonly", "mismatch", "connonly", "cidempty", "nocid", "nilmd", "unique"}
	for _, shape := range shapes {
		t.Run(shape, func(t *testing.T) {
			base := processOutQueued.Load()
			ch := make(chan *models.Mock, 1)
			ch <- &models.Mock{Name: "filler"}
			m := New(zap.NewNop())
			m.outClock, m.outStall = &stall.ManualClock{}, time.Hour
			m.SetOutputChannel(ch)
			m.SetOutQueueSpill(&fakeSpillFactory{ramLimit: 1024, failPeekAt: 1})
			registerSpillCleanup(t, m, ch, base, nil)

			var mu sync.Mutex
			told := map[string]*models.Mock{}
			m.OnDrop(func(mk *models.Mock) {
				mu.Lock()
				told[mk.Name] = mk
				mu.Unlock()
			})

			const n = 40
			sent := map[string]*models.Mock{}
			for i := 0; i < n; i++ {
				mk := makeConnIDShapeMock(fmt.Sprintf("%s-%03d", shape, i), 2048, 8, now.Add(time.Duration(i)*time.Millisecond), shape, i, 7)
				cp := *mk
				if mk.Spec.Metadata != nil {
					cp.Spec.Metadata = make(map[string]string, len(mk.Spec.Metadata))
					for k, v := range mk.Spec.Metadata {
						cp.Spec.Metadata[k] = v
					}
				}
				sent[mk.Name] = &cp
				if r := m.send(mk, fmt.Sprintf("tc-%02d", i%5), nil); r != MockTaken {
					t.Fatalf("send %d = %v, want MockTaken", i, r)
				}
				if i == 0 {
					waitSpillFirstInHand(t, m)
				}
			}
			assertSpillAccountingExact(t, "before corrupt", m, base)
			_, _, _, _, spilled, _, _, _ := inspectSpillAccounting(m)

			drainSpillNames(t, ch, 2) // filler + head mock; corrupt Peek drops every spilled mock
			waitOutQueue(t, m)
			assertSpillAccountingExact(t, "after corrupt", m, base)

			mu.Lock()
			defer mu.Unlock()
			if len(told) < spilled {
				t.Fatalf("told %d drops, want >= %d", len(told), spilled)
			}
			names := make([]string, 0, len(told))
			for k := range told {
				names = append(names, k)
			}
			sort.Strings(names)
			for _, name := range names {
				got, want := told[name], sent[name]
				if want == nil {
					t.Fatalf("told unknown mock %q", name)
				}
				gm, wm := got.Spec.Metadata, want.Spec.Metadata
				if len(gm) == 0 && len(wm) == 0 {
					gm, wm = nil, nil
				}
				if got.ConnectionID != want.ConnectionID || !reflect.DeepEqual(gm, wm) || got.Kind != want.Kind || !got.Spec.ReqTimestampMock.Equal(want.Spec.ReqTimestampMock) {
					t.Fatalf("%s: stub ConnectionID=%q md=%v; sent ConnectionID=%q md=%v", name, got.ConnectionID, got.Spec.Metadata, want.ConnectionID, want.Spec.Metadata)
				}
			}
		})
	}
}

// TestOutQueueSpill_ConnIDFoldExactAccounting verifies exact accounting and
// single-call done semantics across decline-to-tail, repeated stall give-ups,
// corrupt-then-reuse, close-with-gone-consumer, and 1000-connection interning
// for every connID-folding metadata shape.
func TestOutQueueSpill_ConnIDFoldExactAccounting(t *testing.T) {
	SetDefaultOutQueueSpill(nil)
	now := time.Now().Add(-time.Minute)
	const n = 12

	for _, shape := range []string{"match", "metaonly", "mismatch", "connonly", "cidempty", "nilmd"} {
		sendBatch := func(t *testing.T, m *SyncMockManager, d *doneTracker, prefix string) {
			t.Helper()
			_, manual := m.outClock.(*stall.ManualClock)
			for i := 0; i < n; i++ {
				mk := makeConnIDShapeMock(fmt.Sprintf("%s-%02d", prefix, i), 2048, 8, now.Add(time.Duration(i)*time.Millisecond), shape, i*97, 1000)
				if r := m.send(mk, fmt.Sprintf("tc-%02d", i), d.fn(i)); r != MockTaken {
					t.Fatalf("send %d = %v, want MockTaken", i, r)
				}
				if i == 0 && manual {
					waitSpillFirstInHand(t, m)
				}
			}
		}

		t.Run(shape+"/decline-to-tail-then-drain", func(t *testing.T) {
			base := processOutQueued.Load()
			ch := make(chan *models.Mock, 1)
			ch <- &models.Mock{Name: "filler"}
			m := New(zap.NewNop())
			m.outClock, m.outStall = &stall.ManualClock{}, time.Hour
			m.SetOutputChannel(ch)
			m.SetOutQueueSpill(&fakeSpillFactory{ramLimit: 1024, maxItems: 3})
			registerSpillCleanup(t, m, ch, base, nil)

			d := newDoneTracker(n)
			sendBatch(t, m, d, "a")
			assertSpillAccountingExact(t, "after sends", m, base)
			got := drainSpillNames(t, ch, n+1)
			waitOutQueue(t, m)
			for i := 0; i < n; i++ {
				if got[i+1] != fmt.Sprintf("a-%02d", i) {
					t.Fatalf("FIFO mismatch at %d: %v", i, got)
				}
			}
			assertSpillAccountingExact(t, "after drain", m, base)
			d.assertAllOnce(t, "decline")
		})

		t.Run(shape+"/stall-giveup-twice", func(t *testing.T) {
			base := processOutQueued.Load()
			ch := make(chan *models.Mock, 1)
			ch <- &models.Mock{Name: "filler"}
			clk := &stall.ManualClock{}
			m := New(zap.NewNop())
			m.outClock, m.outStall = clk, time.Hour
			m.SetOutputChannel(ch)
			m.SetOutQueueSpill(&fakeSpillFactory{ramLimit: 1024, maxItems: 4})
			registerSpillCleanup(t, m, ch, base, nil)

			waitGiveUp := func(label string) {
				dl := time.Now().Add(5 * time.Second)
				for m.OutQueued() && time.Now().Before(dl) {
					clk.Advance(2 * time.Hour)
					time.Sleep(2 * time.Millisecond)
				}
				if m.OutQueued() {
					t.Fatalf("%s: drainer did not give up", label)
				}
			}

			d1 := newDoneTracker(n)
			sendBatch(t, m, d1, "s1")
			waitGiveUp("round1")
			d1.assertAllOnce(t, "stall1")
			assertSpillAccountingExact(t, "after giveup 1", m, base)

			d2 := newDoneTracker(n)
			sendBatch(t, m, d2, "s2")
			waitGiveUp("round2")
			d2.assertAllOnce(t, "stall2")
			assertSpillAccountingExact(t, "after giveup 2", m, base)
		})

		t.Run(shape+"/corrupt-then-reuse", func(t *testing.T) {
			base := processOutQueued.Load()
			ch := make(chan *models.Mock, 1)
			ch <- &models.Mock{Name: "filler"}
			m := New(zap.NewNop())
			m.outClock, m.outStall = &stall.ManualClock{}, time.Hour
			m.SetOutputChannel(ch)
			fac := &fakeSpillFactory{ramLimit: 1024, maxItems: 3, failPeekAt: 1}
			m.SetOutQueueSpill(fac)
			registerSpillCleanup(t, m, ch, base, nil)

			d1 := newDoneTracker(n)
			sendBatch(t, m, d1, "c1")
			assertSpillAccountingExact(t, "before corrupt", m, base)
			drainSpillNames(t, ch, 2)
			drainSpillNames(t, ch, n-1-3)
			waitOutQueue(t, m)
			d1.assertAllOnce(t, "corrupt1")
			assertSpillAccountingExact(t, "after corrupt", m, base)

			fac.failPeekAt = 0
			ch <- &models.Mock{Name: "filler2"}
			d2 := newDoneTracker(n)
			sendBatch(t, m, d2, "c2")
			drainSpillNames(t, ch, n+1)
			waitOutQueue(t, m)
			d2.assertAllOnce(t, "corrupt2")
			assertSpillAccountingExact(t, "after second drain", m, base)
		})

		t.Run(shape+"/close-with-gone-consumer", func(t *testing.T) {
			base := processOutQueued.Load()
			ch := make(chan *models.Mock, 1)
			ch <- &models.Mock{Name: "filler"}
			clk := &stall.ManualClock{}
			m := New(zap.NewNop())
			m.outClock, m.outStall, m.outCloseStall = clk, time.Hour, time.Millisecond
			m.SetOutputChannel(ch)
			m.SetOutQueueSpill(&fakeSpillFactory{ramLimit: 1024, maxItems: 4})
			registerSpillCleanup(t, m, ch, base, nil)

			d := newDoneTracker(n)
			sendBatch(t, m, d, "x")
			assertSpillAccountingExact(t, "before close", m, base)
			closeDone := make(chan struct{})
			go func() {
				ticker := time.NewTicker(time.Millisecond)
				defer ticker.Stop()
				for {
					select {
					case <-closeDone:
						return
					case <-ticker.C:
						clk.Advance(time.Hour)
					}
				}
			}()
			m.CloseOutChan()
			close(closeDone)
			d.assertAllOnce(t, "close")
			if delta := processOutQueued.Load() - base; delta != 0 {
				t.Fatalf("leak %d after CloseOutChan", delta)
			}
		})
	}

	t.Run("1000-conns-one-pool-entry-then-drain", func(t *testing.T) {
		for _, shape := range []string{"match", "metaonly", "labels50"} {
			t.Run(shape, func(t *testing.T) {
				base := processOutQueued.Load()
				ch := make(chan *models.Mock, 1)
				ch <- &models.Mock{Name: "filler"}
				m := New(zap.NewNop())
				m.outClock, m.outStall = &stall.ManualClock{}, time.Hour
				m.SetOutputChannel(ch)
				m.SetOutQueueSpill(&fakeSpillFactory{ramLimit: 1})
				registerSpillCleanup(t, m, ch, base, nil)

				keys := 8
				if shape == "labels50" {
					keys = 50
				}
				const total = 5000
				for i := 0; i < total; i++ {
					if r := m.send(makeConnIDShapeMock(fmt.Sprintf("p-%05d", i), 64, keys, now, shape, i, 1000), "tc-p", nil); r != MockTaken {
						t.Fatalf("%s send %d = %v", shape, i, r)
					}
					if i == 0 {
						waitSpillFirstInHand(t, m)
					}
				}
				_, _, _, _, _, pooled, distinct, _ := inspectSpillAccounting(m)
				if distinct != 1 || pooled != 1 {
					t.Fatalf("%s: distinct=%d pooled=%d over 1000 conns of one pod, want 1, 1", shape, distinct, pooled)
				}
				assertSpillAccountingExact(t, shape+" 5000 over 1000 conns", m, base)
				drainSpillNames(t, ch, total+1)
				waitOutQueue(t, m)
				assertSpillAccountingExact(t, shape+" drained", m, base)
			})
		}
	})
}

// TestOutQueueSpill_SendingMapShrinksAfterLargePeak verifies that once a large
// in-flight burst drains completely, SyncMockManager releases the m.sending
// map backing table on both the noteSendingAt and noteTaken paths instead of
// retaining it indefinitely.
func TestOutQueueSpill_SendingMapShrinksAfterLargePeak(t *testing.T) {
	t.Run("noteSendingAt", func(t *testing.T) {
		m := New(zap.NewNop())
		now := time.Now()
		ids := make([]uint64, sendingShrinkThreshold+128)
		for i := range ids {
			ids[i] = m.noteSendingAt(now)
		}
		for _, id := range ids {
			m.sentID(id)
		}
		m.sendingMu.Lock()
		isNil := m.sending == nil
		peak := m.sendingPeak
		m.sendingMu.Unlock()
		if !isNil || peak != 0 {
			t.Fatalf("after draining %d noteSendingAt entries: sending==nil=%v sendingPeak=%d, want true, 0", len(ids), isNil, peak)
		}
	})

	t.Run("noteTaken", func(t *testing.T) {
		m := New(zap.NewNop())
		now := time.Now()
		batch := make([]ownedMock, sendingShrinkThreshold+128)
		for i := range batch {
			batch[i] = ownedMock{
				mock:  &models.Mock{Name: fmt.Sprintf("u-%05d", i), Spec: models.MockSpec{ReqTimestampMock: now}},
				owner: "tc",
			}
		}
		m.mu.Lock()
		m.noteTaken(batch)
		m.mu.Unlock()
		for i := range batch {
			m.sentID(batch[i].sendID)
		}
		m.sendingMu.Lock()
		isNil := m.sending == nil
		peak := m.sendingPeak
		m.sendingMu.Unlock()
		if !isNil || peak != 0 {
			t.Fatalf("after draining %d noteTaken entries: sending==nil=%v sendingPeak=%d, want true, 0", len(batch), isNil, peak)
		}
	})
}
