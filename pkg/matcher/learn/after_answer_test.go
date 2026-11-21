package learn

import (
	"errors"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// hangGuard fails the row where done is not closed: an item that waited for
// the queue it runs from would hang the test binary instead.
func hangGuard(t *testing.T, done <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatalf("%s never returned: the bookkeeping waits for itself", what)
	}
}

// TestAfterAnswerABookedServeDecidesAsAnInlineOne: a serve's view told through the
// after-answer queue (Book) gives a later call the verdict the same view
// told inline gives, whether the worker ran it (Kick) or the later call's
// read settled it.
func TestAfterAnswerABookedServeDecidesAsAnInlineOne(t *testing.T) {
	const path = "bind.$8"
	rec := Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: "82"}}}
	live := Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: "6c7d"}}}
	m := jobsMock(chanceSrcRec, "5")
	m.Spec.ReqTimestampMock, m.Spec.ResTimestampMock = eRecAt, eRecAt.Add(time.Millisecond)
	view := func(id string) func() (*models.Mock, error) {
		return func() (*models.Mock, error) {
			v := jobsMock(id, "9")
			v.Spec.ReqTimestampMock, v.Spec.ResTimestampMock = m.Spec.ReqTimestampMock, m.Spec.ResTimestampMock
			return v, nil
		}
	}
	served := func(n uint64) Earlier {
		return Earlier{Mock: m.Name, At: m.Spec.ReqTimestampMock, Own: true, Payload: RawOf(m, true),
			Seen: []Seen{{Side: Response, Leaf: Leaf{Path: "row.0.n", Type: String, Value: "5"}, Live: "5"}}}
	}
	tells := []struct {
		name string
		tell func(b Book, n uint64)
	}{
		{"told nothing", func(b Book, n uint64) { b.ServedNamed(n, served(n), nil, nil) }},
		{"told as recorded", func(b Book, n uint64) { b.ServedNamed(n, served(n), nil, nil); b.ServedMockAsRecorded(m) }},
		{"told a view with no word of the value", func(b Book, n uint64) {
			b.ServedNamed(n, served(n), nil, nil)
			b.ServedMockLive(m, "a", view(chanceSrcRec))
		}},
		{"told a view that holds F's value", func(b Book, n uint64) {
			b.ServedNamed(n, served(n), nil, nil)
			b.ServedMockLive(m, "a", view("6c7d"))
		}},
		{"told a view keploy cannot read", func(b Book, n uint64) {
			b.ServedNamed(n, served(n), nil, nil)
			b.ServedMockLive(m, "a", func() (*models.Mock, error) { return nil, errors.New("gone") })
		}},
	}
	verdict := func(tr *Tracker) string {
		pl := tr.Placed()
		in := Input{Drifts: clockedDrifts(), Drift: []string{path}, Live: live, Rec: rec, At: eRecAt, Arrived: eRecAt,
			Earlier: InServeOrder(pl.Earlier, []*models.Mock{m}, pl.Order), Raw: eStartup(), Test: "test-2", Mock: "mock-300",
			Caps: Caps{BindAtWord: true, AnswerAtWord: true, FViewStated: true}}
		return oDescribe([]Decision{oWord(t, Decide(in), "82", "6c7d")})
	}
	for _, c := range tells {
		inline := NewTracker()
		inline.Begin()
		inline.ServedMock(m)
		n := inline.Placed().Test
		c.tell(Book{t: inline}, n)
		want := verdict(inline)
		for _, kick := range []bool{false, true} {
			tr := NewTracker()
			tr.Begin()
			tr.ServedMock(m)
			n := tr.Placed().Test
			ran := make(chan struct{})
			tr.AfterAnswer(func(b Book) { c.tell(b, n); close(ran) })
			if kick {
				tr.Kick()
				hangGuard(t, ran, "the worker")
			}
			if got := verdict(tr); got != want {
				t.Errorf("%s (worker %v): booked after the answer the call decides %s; inline it decides %s", c.name, kick, got, want)
			}
		}
	}
}

// TestAfterAnswerEveryLockRunsWhatWasQueuedBeforeIt: with no worker, a read of
// the tracker runs every item queued before it, once each and in the order
// queued, before it reads; a Begin, an End and a NewSet do too.
func TestAfterAnswerEveryLockRunsWhatWasQueuedBeforeIt(t *testing.T) {
	for _, c := range []struct {
		name string
		read func(tr *Tracker)
	}{
		{"Placed", func(tr *Tracker) { tr.Placed() }},
		{"Begin", func(tr *Tracker) { tr.Begin() }},
		{"End", func(tr *Tracker) { tr.End() }},
		{"NewSet", func(tr *Tracker) { tr.NewSet() }},
		{"ServedMock", func(tr *Tracker) { tr.ServedMock(jobsMock("mock-9", "1")) }},
		{"Served", func(tr *Tracker) { tr.Served(tr.epoch, Earlier{Mock: "mock-9"}) }},
		{"ServedNamed", func(tr *Tracker) { tr.ServedNamed(tr.epoch, Earlier{Mock: "mock-9"}, nil, nil) }},
		{"ServedMockAsRecorded", func(tr *Tracker) { tr.ServedMockAsRecorded(jobsMock("mock-9", "1")) }},
		{"ServedMockLive", func(tr *Tracker) { tr.ServedMockLive(jobsMock("mock-9", "1"), "", nil) }},
	} {
		tr := NewTracker()
		tr.Begin()
		var ran []int
		for i := 0; i < 5; i++ {
			tr.AfterAnswer(func(Book) { ran = append(ran, i) })
		}
		if len(ran) != 0 || tr.after.pending.Load() != 5 {
			t.Fatalf("%s: %d items ran and %d wait before any read; want 0 and 5", c.name, len(ran), tr.after.pending.Load())
		}
		c.read(tr)
		if len(ran) != 5 || tr.after.pending.Load() != 0 {
			t.Fatalf("%s: %d items ran and %d wait after the read; want 5 and 0", c.name, len(ran), tr.after.pending.Load())
		}
		for i, v := range ran {
			if v != i {
				t.Fatalf("%s: items ran in the order %v; want the order queued", c.name, ran)
			}
		}
	}
}

// TestAfterAnswerTheWorkerRunsTheQueueOnceTheAnswerIsWritten: Kick runs every
// waiting item on a worker, with no read of the tracker, and starts no
// worker for an empty queue; an item queued after the worker emptied the
// queue waits for the next Kick or read, and is never lost.
func TestAfterAnswerTheWorkerRunsTheQueueOnceTheAnswerIsWritten(t *testing.T) {
	tr := NewTracker()
	tr.Kick()
	tr.after.mu.Lock()
	started := tr.after.working
	tr.after.mu.Unlock()
	if started {
		t.Fatalf("Kick started a worker for an empty queue")
	}
	done := make(chan struct{})
	var ran []int
	for i := 0; i < 3; i++ {
		tr.AfterAnswer(func(Book) {
			ran = append(ran, i)
			if i == 2 {
				close(done)
			}
		})
	}
	tr.Kick()
	hangGuard(t, done, "the worker")
	tr.Placed()
	if len(ran) != 3 || ran[0] != 0 || ran[1] != 1 || ran[2] != 2 {
		t.Fatalf("the worker ran %v; want [0 1 2]", ran)
	}
	tr.after.mu.Lock()
	working := tr.after.working
	tr.after.mu.Unlock()
	if working {
		t.Fatalf("the worker still runs after a read settled an empty queue")
	}
	late := false
	tr.AfterAnswer(func(Book) { late = true })
	tr.Placed()
	if !late {
		t.Fatalf("an item queued after the worker ended was not run by the next read")
	}
}

// TestAfterAnswerAFullQueueBooksInlineInOrder: maxAfterAnswer items wait unrun; the
// call that queues one more runs them all, its own last, before it returns,
// so the waiting bookkeeping stays bounded.
func TestAfterAnswerAFullQueueBooksInlineInOrder(t *testing.T) {
	tr := NewTracker()
	var ran []int
	for i := 0; i < maxAfterAnswer; i++ {
		tr.AfterAnswer(func(Book) { ran = append(ran, i) })
	}
	if len(ran) != 0 || tr.after.pending.Load() != maxAfterAnswer {
		t.Fatalf("%d items ran and %d wait at the bound; want 0 and %d", len(ran), tr.after.pending.Load(), maxAfterAnswer)
	}
	tr.AfterAnswer(func(Book) { ran = append(ran, maxAfterAnswer) })
	if len(ran) != maxAfterAnswer+1 || tr.after.pending.Load() != 0 {
		t.Fatalf("%d items ran and %d wait past the bound; want %d and 0", len(ran), tr.after.pending.Load(), maxAfterAnswer+1)
	}
	for i, v := range ran {
		if v != i {
			t.Fatalf("past the bound item %d ran as %d: not the order queued", i, v)
		}
	}
}

// TestAfterAnswerBookkeepingNeverWaitsForItsOwnQueue: an item's Book methods record
// without settling the queue they run from, while later items still wait,
// whether a read or the worker runs them.
func TestAfterAnswerBookkeepingNeverWaitsForItsOwnQueue(t *testing.T) {
	m := jobsMock(chanceSrcRec, "5")
	m.Spec.ReqTimestampMock, m.Spec.ResTimestampMock = eRecAt, eRecAt.Add(time.Millisecond)
	for _, kick := range []bool{false, true} {
		tr := NewTracker()
		tr.Begin()
		tr.ServedMock(m)
		n := tr.Placed().Test
		for i := 0; i < 3; i++ {
			tr.AfterAnswer(func(b Book) {
				b.ServedNamed(n, Earlier{Mock: m.Name, At: m.Spec.ReqTimestampMock, Own: true, Payload: RawOf(m, true)},
					models.MockVariables{"v": {Use: []string{"q.0#1"}}}, nil)
				b.ServedMockAsRecorded(m)
				b.ServedMockLive(m, "k", func() (*models.Mock, error) { return m, nil })
			})
		}
		done := make(chan struct{})
		go func() {
			if kick {
				tr.Kick()
			}
			tr.Placed()
			close(done)
		}()
		hangGuard(t, done, "the settle")
		if p := tr.after.pending.Load(); p != 0 {
			t.Fatalf("worker %v: %d items wait after the read; want 0", kick, p)
		}
	}
}

// TestAfterAnswerANilTrackerBooksAtOnce: with no tracker the bookkeeping runs as the
// call queues it.
func TestAfterAnswerANilTrackerBooksAtOnce(t *testing.T) {
	queued := NewTracker()
	queued.AfterAnswer(nil)
	if p := queued.after.pending.Load(); p != 0 {
		t.Fatalf("no bookkeeping queued %d items; want none", p)
	}
	var tr *Tracker
	ran := false
	tr.AfterAnswer(func(b Book) {
		b.ServedMockAsRecorded(jobsMock("mock-1", "1"))
		ran = true
	})
	tr.Kick()
	if !ran {
		t.Fatalf("a nil tracker did not run the bookkeeping at once")
	}
}

// TestAfterAnswerWaitingCountsTheQueueWithoutBookingIt: Waiting says how many
// items are queued and not yet booked, and reading it books none of them;
// a read of the tracker books them all, and a nil tracker has none.
func TestAfterAnswerWaitingCountsTheQueueWithoutBookingIt(t *testing.T) {
	tr := NewTracker()
	ran := 0
	for i := 0; i < 3; i++ {
		tr.AfterAnswer(func(Book) { ran++ })
	}
	for i := 0; i < 2; i++ {
		if w := tr.Waiting(); w != 3 || ran != 0 {
			t.Fatalf("Waiting %d with %d booked; want 3 waiting, none booked", w, ran)
		}
	}
	tr.Placed()
	if w := tr.Waiting(); w != 0 || ran != 3 {
		t.Fatalf("Waiting %d with %d booked after a read; want none waiting, 3 booked", w, ran)
	}
	var none *Tracker
	if w := none.Waiting(); w != 0 {
		t.Fatalf("a nil tracker has %d waiting; want none", w)
	}
}
