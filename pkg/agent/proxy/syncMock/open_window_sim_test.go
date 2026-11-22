package manager

import (
	"fmt"
	"math/rand"
	"sort"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// The synchronous ingress driving the manager, simulated in time order, against
// the attribution rule written out as a reference. One lock: a stream yields at
// its headers and gives the lock back there; a plain exchange yields at its
// app's last byte and gives the lock back then, or later (a slow client). Each
// request is kept, a duplicate (Close, then ResolveRange keep=false) or not
// captured (Close). Apps make calls within their exchange, and background calls
// come anywhere; each mock is added after a decode delay; a flush ticks every
// second. Every kept request has a test name of its own.
//
// The rule: a mock requested at t goes to the kept request whose claim (from
// its start to its yield) covers t; else to the kept window spanning t that was
// decided first; else to no test case. Uploads (a request body of unknown
// length, which claims up to its response's headers though the lock went back
// at its request) and memory pressure are not simulated: the first resolved
// takes what such windows span, as on main.
func TestYieldedWindowsAttributeByTheRule(t *testing.T) {
	t.Parallel()
	var stats simStats
	for _, young := range []bool{false, true} {
		for seed := int64(1); seed <= 300; seed++ {
			name := fmt.Sprintf("%s/seed%d", map[bool]string{true: "young", false: "old"}[young], seed)
			simOne(t, name, seed, young, &stats)
		}
	}
	t.Logf("mocks: by claim %d, by decided-first %d (in a claim not kept %d, with 2+ kept spanning %d), to none %d, of %d",
		stats.byClaim, stats.byFirst, stats.inOther, stats.multi, stats.none, stats.total)
	if stats.byClaim == 0 || stats.byFirst == 0 || stats.inOther == 0 || stats.multi == 0 || stats.none == 0 {
		t.Fatal("the sequences no longer cover every kind of mock the rule decides")
	}
}

// simStats counts the mocks by what the rule decided for them.
type simStats struct {
	byClaim, byFirst, inOther, multi, none, total int
}

type simReq struct {
	s, h, e, rel, dec float64 // ms
	stream            bool
	verdict           int // 0 kept, 1 dup, 2 not captured
	yieldH, yieldE    float64
	w                 *Window
	name              string
}

type simEv struct {
	at   float64
	kind int // 0 open, 1 yield-h, 2 yield-e, 3 add, 4 decide, 5 flush
	i    int
	m    *models.Mock
}

func simOne(t *testing.T, name string, seed int64, young bool, stats *simStats) {
	rng := rand.New(rand.NewSource(seed))
	scale := 1.0
	if young {
		scale = 0.25
	}
	var reqs []*simReq
	lockFree := 0.0
	arr := 0.0
	n := 8 + rng.Intn(12)
	for i := 0; i < n; i++ {
		arr += rng.Float64() * 400 * scale
		a := arr
		if a < lockFree {
			a = lockFree
		}
		r := &simReq{}
		r.s = a + 0.5 + rng.Float64()*3*scale
		r.h = r.s + (5+rng.Float64()*200)*scale
		r.stream = rng.Float64() < 0.45
		if r.stream {
			r.e = r.h + (100+rng.Float64()*3000)*scale
			r.rel = r.h
		} else {
			r.e = r.h + rng.Float64()*100*scale
			r.rel = r.e
			if rng.Float64() < 0.25 {
				r.rel = r.e + rng.Float64()*300*scale // slow client holds the lock
			}
		}
		lockFree = r.rel + 0.3
		switch v := rng.Float64(); {
		case v < 0.6:
			r.verdict = 0
		case v < 0.85:
			r.verdict = 1
		default:
			r.verdict = 2
		}
		r.dec = r.e + (1+rng.Float64()*150)*scale
		r.yieldH = r.h + rng.Float64()*3*scale
		r.yieldE = r.e + rng.Float64()*3*scale
		r.name = fmt.Sprintf("test-%d", i+1)
		reqs = append(reqs, r)
	}
	end := 0.0
	for _, r := range reqs {
		if r.dec > end {
			end = r.dec
		}
	}

	out := make(chan *models.Mock, 1<<16)
	maps := make(chan models.TestMockMapping, 1<<16)
	mgr := openWindowManager(out, maps)
	var base time.Time
	if young {
		base = time.Now().Add(-time.Duration(end+500) * time.Millisecond)
	} else {
		base = time.Now().Add(-10*time.Minute - time.Duration(end)*time.Millisecond)
	}
	ts := func(ms float64) time.Time { return base.Add(time.Duration(ms * float64(time.Millisecond))) }

	var evs []simEv
	mockAt := map[*models.Mock]float64{}
	addCall := func(at float64) {
		m := httpMockAt(ts(at))
		mockAt[m] = at
		evs = append(evs, simEv{at: at + (1+rng.Float64()*400)*scale, kind: 3, m: m})
	}
	for i, r := range reqs {
		evs = append(evs, simEv{at: r.s, kind: 0, i: i})
		if r.stream {
			evs = append(evs, simEv{at: r.yieldH, kind: 1, i: i})
		}
		evs = append(evs, simEv{at: r.yieldE, kind: 2, i: i})
		evs = append(evs, simEv{at: r.dec, kind: 4, i: i})
		for k, c := 0, rng.Intn(5); k < c; k++ {
			addCall(r.s + 0.01 + rng.Float64()*(r.e-r.s-0.02))
		}
	}
	for k, c := 0, rng.Intn(10); k < c; k++ {
		addCall(rng.Float64() * end)
	}
	for f := 1000.0 * scale; f < end+2000; f += 1000 * scale {
		evs = append(evs, simEv{at: f, kind: 5})
	}
	sort.SliceStable(evs, func(a, b int) bool {
		if evs[a].at != evs[b].at {
			return evs[a].at < evs[b].at
		}
		return evs[a].kind < evs[b].kind
	})
	var decided []int // kept, in decision order
	for _, ev := range evs {
		switch ev.kind {
		case 0:
			reqs[ev.i].w = mgr.OpenWindow(ts(reqs[ev.i].s), nil)
		case 1:
			reqs[ev.i].w.Yield(ts(reqs[ev.i].h))
		case 2:
			r := reqs[ev.i]
			r.w.Yield(ts(r.e))
			if r.verdict == 2 {
				r.w.Close()
			}
		case 3:
			mgr.AddMock(ev.m)
		case 4:
			r := reqs[ev.i]
			switch r.verdict {
			case 0:
				if !r.w.Keep() {
					t.Fatalf("%s: window given up", name)
				}
				mgr.ResolveKept(r.w, ts(r.s), ts(r.e), r.name, true)
				r.w.Close()
				decided = append(decided, ev.i)
			case 1:
				r.w.Close()
				mgr.ResolveRange(ts(r.s), ts(r.e), "test-0", false, true)
			}
		case 5:
			mgr.FlushOwnedWindows()
		}
	}
	mgr.FlushOwnedWindows()

	sent := map[*models.Mock]bool{}
	for len(out) > 0 {
		sent[<-out] = true
	}
	mapped := map[string]string{}
	for len(maps) > 0 {
		e := <-maps
		for _, id := range e.MockIDs {
			if prev, ok := mapped[id]; ok && prev != e.TestName {
				t.Errorf("%s: mock mapped to %s and %s", name, prev, e.TestName)
			}
			mapped[id] = e.TestName
		}
	}
	order := map[int]int{}
	for k, i := range decided {
		order[i] = k
	}
	bad := 0
	for m, at := range mockAt {
		want := ""
		for i, r := range reqs {
			y := r.e
			if r.stream {
				y = r.h
			}
			if r.verdict == 0 && at >= r.s && at <= y {
				want = reqs[i].name
			}
		}
		stats.total++
		if want != "" {
			stats.byClaim++
		}
		if want == "" {
			inOther, spanning := false, 0
			for _, r := range reqs {
				y := r.e
				if r.stream {
					y = r.h
				}
				if r.verdict != 0 && at >= r.s && at <= y {
					inOther = true
				}
				if r.verdict == 0 && at >= r.s && at <= r.e {
					spanning++
				}
			}
			if spanning > 0 {
				stats.byFirst++
				if inOther {
					stats.inOther++
				}
				if spanning > 1 {
					stats.multi++
				}
			} else {
				stats.none++
			}
			best := -1
			for i, r := range reqs {
				if r.verdict == 0 && at >= r.s && at <= r.e && (best < 0 || order[i] < order[best]) {
					best = i
				}
			}
			if best >= 0 {
				want = reqs[best].name
			}
		}
		got := ""
		if sent[m] {
			got = mapped[m.Name]
		}
		if got != want {
			bad++
			if bad <= 3 {
				var desc string
				for _, r := range reqs {
					v := []string{"kept", "dup", "nocap"}[r.verdict]
					y := r.e
					if r.stream {
						y = r.h
					}
					if at >= r.s-1 && at <= r.e+1 {
						desc += fmt.Sprintf("\n    %s %s stream=%v s=%.1f yield=%.1f(called %.1f/%.1f) e=%.1f rel=%.1f dec=%.1f order=%d",
							r.name, v, r.stream, r.s, y, r.yieldH, r.yieldE, r.e, r.rel, r.dec, order[simIndex(reqs, r)])
					}
				}
				t.Errorf("%s: mock at %.2f: sent=%v mapped to %q, want %q%s", name, at, sent[m], got, want, desc)
			}
		}
	}
	if bad > 0 {
		t.Errorf("%s: %d of %d mocks off the rule", name, bad, len(mockAt))
	}
}

func simIndex(rs []*simReq, r *simReq) int {
	for i, x := range rs {
		if x == r {
			return i
		}
	}
	return -1
}
