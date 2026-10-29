package learn

import (
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// srcOf visits mocks in order, as the agent's staging of a set visits its
// recordings (MockManager.RangeRecordedBefore).
func srcOf(mocks []*models.Mock) func(visit func(*models.Mock) bool) error {
	return func(visit func(*models.Mock) bool) error {
		for _, m := range mocks {
			if !visit(m) {
				return nil
			}
		}
		return nil
	}
}

// readsOf are the payloads s's staging and its calls have read for the set
// (warmOne), and how often a call waited for another's read.
func readsOf(s *setDerived) (staged, called, waits int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.staged, s.called, s.waits
}

// heldUntil makes each read of text's decoding (decodeForms) wait until the
// returned release runs; reading is closed once a read of it has begun, and
// begun counts those reads. The row's end releases it too.
func heldUntil(t *testing.T, raw []Raw, text string) (counts *sync.Map, reading chan struct{}, begun *atomic.Int64, release func()) {
	reading = make(chan struct{})
	gate := make(chan struct{})
	begun = new(atomic.Int64)
	var once, released sync.Once
	release = func() { released.Do(func() { close(gate) }) }
	t.Cleanup(release)
	counts = countDecodes(t, raw, func(s string) {
		if s == text {
			begun.Add(1)
			once.Do(func() { close(reading) })
			<-gate
		}
	})
	return counts, reading, begun, release
}

// A set staged before its tests (WarmSet) is read there, off the calls'
// answer path: each of its payloads is decoded and read for its instants
// once, by the staging, and no call of any of its tests reads one for the
// set, its first neither (set-wide work: on api-server's round the set's
// first drifted call read every payload recorded before it while its client
// waited). The calls learn what they learn with no staging.
func TestASetsCallsReadNoPayloadItsStagingRead(t *testing.T) {
	const n, tests = 48, 6
	row := newHeldRow(n, 4)
	raw := row.raw()
	plain := NewTracker()
	plain.NewSet()
	plain.Begin()
	want := row.classify(plain.Drifts())
	plain.End()

	counts := countDecodes(t, raw, nil)
	spans := countSpans(t)
	tr := NewTracker()
	tr.NewSet()
	use := <-tr.WarmSet(srcOf(row.mocks))
	s := tr.Drifts().set
	staged, called, _ := readsOf(s)
	if use.Staged != n || use.Called != 0 || use.Held == 0 {
		t.Fatalf("the set's staging reported %+v: want %d read by it, none by a call, and what it keeps", use, n)
	}
	payloads, decodes := decodesOf(counts)
	if staged != n || called != 0 || payloads != n || decodes != n {
		t.Fatalf("the set's staging read %d of its %d payloads for the set (calls %d), decoding %d of them %d times: want each once, by the staging", staged, n, called, payloads, decodes)
	}
	read := spans.Load()
	for i := 0; i < tests; i++ {
		tr.Begin()
		got := row.classify(tr.Drifts())
		tr.End()
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("test %d of the staged set learned\n%v\nwhere the set with no staging learned\n%v", i+1, got, want)
		}
		if _, c, _ := readsOf(s); c != 0 {
			t.Fatalf("test %d's call read %d of the staged set's payloads for the set: want none (the staging read them)", i+1, c)
		}
	}
	if _, d := decodesOf(counts); d != decodes {
		t.Errorf("%d tests of the staged set decoded its payloads %d more times: want none", tests, d-decodes)
	}
	if r := spans.Load(); r != read {
		t.Errorf("%d tests of the staged set read its payloads' instants %d more times: want none", tests, r-read)
	}
}

// A call made while the set's staging is still reading waits for no part of
// the staging but a payload of its own the staging is reading: it reads the
// rest itself, and the staging reads none of them again. Counted, not timed:
// the staging is held in its first payload's read until the row lets it go;
// a call that waited for the staging to end would never end.
func TestACallWaitsOnlyForItsOwnPayloadTheStagingIsReading(t *testing.T) {
	const n = 24
	row := newHeldRow(n, 4)
	raw := row.raw()
	plain := NewTracker()
	plain.NewSet()
	plain.Begin()
	want := row.classify(plain.Drifts())
	plain.End()

	counts, reading, begun, release := heldUntil(t, raw, raw[0].payload().text)
	tr := NewTracker()
	tr.NewSet()
	done := tr.WarmSet(srcOf(row.mocks))
	<-reading
	tr.Begin()
	defer tr.End()
	dc := tr.Drifts()
	s := dc.set
	dc.warm(raw[1:])
	if staged, called, waits := readsOf(s); staged != 0 || called != n-1 || waits != 0 {
		t.Fatalf("a call of the %d payloads the staging had not taken read %d of them (the staging %d) and waited %d times: want each read by the call, no wait", n-1, called, staged, waits)
	}
	// The payload the staging is reading: a call of it waits for that read,
	// and reads it not again.
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		dc.warm(raw[:1])
	}()
	for {
		if _, _, waits := readsOf(s); waits == 1 {
			break
		}
		select {
		case <-ended:
			t.Fatal("a call of the payload the staging is reading ended before that read did: want it to wait for it")
		default:
		}
		if begun.Load() > 1 {
			t.Fatal("a call of the payload the staging is reading read it again: want it to wait for the staging's read")
		}
		runtime.Gosched()
	}
	release()
	<-ended
	<-done
	if staged, called, _ := readsOf(s); staged != 1 || called != n-1 {
		t.Errorf("the staging read %d payloads and the calls %d: want the staging's held one alone (1) and the calls' %d", staged, called, n-1)
	}
	if payloads, decodes := decodesOf(counts); payloads != n || decodes != n {
		t.Errorf("the staging and a call at once decoded %d of the set's %d payloads, %d times in all: want each once", payloads, n, decodes)
	}
	if got := row.classify(dc); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("the set read at once by its staging and a call learned\n%v\nwhere the set with no staging learned\n%v", got, want)
	}
}

// A set's staging stops at the next set (NewSet): it reads no payload of a
// set that is over, and the set it read keeps nothing, so the agent holds
// one set's reads at a time.
func TestASetsStagingStopsAtTheNextSet(t *testing.T) {
	const n = 24
	row := newHeldRow(n, 4)
	raw := row.raw()
	counts, reading, _, release := heldUntil(t, raw, raw[0].payload().text)
	tr := NewTracker()
	tr.NewSet()
	s := tr.Drifts().set
	var visits atomic.Int64
	src := srcOf(row.mocks)
	done := tr.WarmSet(func(visit func(*models.Mock) bool) error {
		return src(func(m *models.Mock) bool {
			visits.Add(1)
			return visit(m)
		})
	})
	<-reading
	tr.NewSet()
	release()
	<-done
	if v := visits.Load(); v > 2 {
		t.Errorf("the staging of a set that was over went on to visit %d of its %d recordings: want it to stop at the first after the set ended", v, n)
	}
	if staged, _, _ := readsOf(s); staged != 1 {
		t.Errorf("the staging of a set that was over read %d of its payloads: want only the one it was reading as the set ended", staged)
	}
	if payloads, _ := decodesOf(counts); payloads != 1 {
		t.Errorf("the staging of a set that was over decoded %d of its %d payloads: want only the one it was reading as the set ended", payloads, n)
	}
	if b := s.usedBytes(); b != 0 {
		t.Errorf("a set that is over keeps %d bytes its staging read: want none", b)
	}
}
