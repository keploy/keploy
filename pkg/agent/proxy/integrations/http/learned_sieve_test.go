package http

import (
	"context"
	"fmt"
	"math/rand"
	"slices"
	"strconv"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
)

// learnShapesOf is how many candidates the learn step's scope shaped while f
// ran.
func learnShapesOf(f func()) int64 {
	before := learnShapes.Load()
	f()
	return learnShapes.Load() - before
}

// A call the learn step's noise serves, one recording of a large pool, shapes
// that one: at 100 recordings and at 2000 alike, one shape (it shaped every
// recording of the same method, destination and segment count on every call,
// and compared each). A call no recording is shapes none, and is not served.
func TestALearnedCallShapesOnlyTheRecordingsThatMayBeIt(t *testing.T) {
	h, n, _ := learnOn(t)
	verifyOff(t)
	for _, size := range []int{100, 2000} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			mocks := make([]*models.Mock, size)
			for i := range mocks {
				m := learnRecorded(fmt.Sprintf("mock-%d", i), 0, "GET", "/item/"+strconv.Itoa(i)+"?size=small", "", map[string]string{"X-Trace": "t" + strconv.Itoa(i)}, `{"id":`+strconv.Itoa(i)+`}`)
				m.Spec.ReqBodyNoise = map[string][]string{"header.x-trace": {}}
				mocks[i] = m
			}
			db := &learnDb{perTest: mocks}
			var served *models.Mock
			var err error
			got := learnShapesOf(func() {
				served, err = h.learnedMatch(context.Background(), live("GET", "/item/"+strconv.Itoa(size/2)+"?size=small", "", map[string]string{"X-Trace": "live"}), db, n, models.OutgoingOptions{})
			})
			if err != nil || served == nil || served.Name != fmt.Sprintf("mock-%d", size/2) {
				t.Fatalf("served %v err %v; want mock-%d", served, err, size/2)
			}
			if got != 1 {
				t.Fatalf("a call that is one recording of %d shaped %d; want 1", size, got)
			}
			got = learnShapesOf(func() {
				served, err = h.learnedMatch(context.Background(), live("GET", "/item/none?size=small", "", map[string]string{"X-Trace": "live"}), db, n, models.OutgoingOptions{})
			})
			if err != nil || served != nil || got != 0 {
				t.Fatalf("a call no recording is: served %v err %v shaped %d; want none of each", served, err, got)
			}
		})
	}
}

// A recording whose path holds a value its obfuscation noise (Mock.Noise)
// matches is still compared with a call whose segment differs there: the
// learn step excuses the value, and serves the call.
func TestAnObfuscatedPathSegmentIsComparedThoughThePathDiffers(t *testing.T) {
	h, n, _ := learnOn(t)
	m := learnRecorded("mock-0", 0, "GET", "/users/secret-1/orders", "", map[string]string{"X-Trace": "t0"}, `{}`)
	m.Spec.ReqBodyNoise = map[string][]string{"header.x-trace": {}}
	m.Noise = []string{`^secret-[0-9]+$`}
	db := &learnDb{perTest: []*models.Mock{m}}
	served, err := h.learnedMatch(context.Background(), live("GET", "/users/secret-2/orders", "", map[string]string{"X-Trace": "live"}), db, n, models.OutgoingOptions{})
	if err != nil || served == nil || served.Name != "mock-0" {
		t.Fatalf("served %v err %v; want mock-0", served, err)
	}
	// A segment the obfuscation noise does not match is still compared.
	db = &learnDb{perTest: []*models.Mock{m}}
	if served, _ := h.learnedMatch(context.Background(), live("GET", "/users/secret-2/carts", "", map[string]string{"X-Trace": "live"}), db, n, models.OutgoingOptions{}); served != nil {
		t.Fatalf("served %v for another path", served.Name)
	}
}

// An own recording that answered is not shaped for the learned group, nor one
// the store let go since it answered: neither may answer again, and
// exactGroup never takes it. Each is still the test's own to the group's
// owner.
func TestASpentRecordingIsNotShapedForTheLearnedGroup(t *testing.T) {
	_, n, _ := learnOn(t)
	verifyOff(t)
	m := learnRecorded("mock-0", 0, "GET", "/item/1", "", nil, `{}`)
	m.Spec.ReqBodyNoise = map[string][]string{"header.x-trace": {}}
	gone := learnRecorded("mock-gone", 0, "GET", "/item/1", "", nil, `{}`)
	r := scopeReads{perTest: []*models.Mock{m}, open: true,
		answered:      func(string) bool { return true },
		answeredMocks: func() []*models.Mock { return []*models.Mock{m, gone} }}
	lp := liveParts(context.Background(), live("GET", "/item/1", "", nil))
	ls := n.shape(lp, sameDest)
	var group []*models.Mock
	var owner func(*models.Mock) bool
	got := learnShapesOf(func() { group, owner = r.learnedGroup(lp, ls, n, n.learnSieveOf(lp)) })
	if len(group) != 0 || got != 0 {
		t.Fatalf("group %v, shaped %d; want none of each", strictNames(group), got)
	}
	if !owner(m) || !owner(gone) {
		t.Fatalf("the spent recordings are the test's own to the owner: %v, %v", owner(m), owner(gone))
	}
}

// The learned group with the sieve is the group with every candidate shaped
// and compared, and so is its owner, over random pools and calls: methods,
// paths with and without a leading "//", url noise, learned URL places and
// other learned places, reads of the clock in a path, obfuscation noise,
// own and pool recordings, spent ones among them.
func TestLearnedGroupsAreWhatEveryCandidateComparedFinds(t *testing.T) {
	h, _, _ := learnOn(t)
	verifyOff(t)
	rnd := rand.New(rand.NewSource(7))
	segs := []string{"a", "b", "1", "2", "x-1", "x-2"}
	path := func() string {
		var b strings.Builder
		if rnd.Intn(5) == 0 {
			b.WriteString("/")
		}
		for i := 0; i <= rnd.Intn(3); i++ {
			b.WriteString("/" + segs[rnd.Intn(len(segs))])
		}
		if rnd.Intn(3) == 0 {
			b.WriteString("?q=" + segs[rnd.Intn(len(segs))])
		}
		return b.String()
	}
	method := func() string { return []string{"GET", "GET", "POST"}[rnd.Intn(3)] }
	learned := [][]string{nil, {"header.x-t"}, {"url.path.1"}, {"url"}, {"url.query.q", "header.x-t"}, {"url.path.0", "header.x-t"}}
	nonEmpty, sieved := 0, 0
	for trial := 0; trial < 400; trial++ {
		var urlNoise []string
		if rnd.Intn(3) == 0 {
			urlNoise = []string{`x-[0-9]`}
		}
		n := h.newLearnNoise(models.OutgoingOptions{Learn: true}, nil, urlNoise)
		var perTest, session []*models.Mock
		answered := map[string]bool{}
		for i := 0; i < 12; i++ {
			m := learnRecorded(fmt.Sprintf("m-%d", i), 0, method(), path(), "", map[string]string{"X-T": segs[rnd.Intn(2)]}, `{}`)
			if l := learned[rnd.Intn(len(learned))]; l != nil {
				m.Spec.ReqBodyNoise = map[string][]string{}
				for _, k := range l {
					m.Spec.ReqBodyNoise[k] = nil
				}
			}
			if rnd.Intn(6) == 0 {
				m.Spec.Clocks = map[string]models.ClockOffset{"url.path." + strconv.Itoa(rnd.Intn(2)): {}}
			}
			if rnd.Intn(6) == 0 {
				m.Noise = []string{`^[12]$`}
			}
			if rnd.Intn(5) == 0 {
				answered[m.Name] = true
			}
			if rnd.Intn(3) == 0 {
				m.TestModeInfo.Lifetime = models.LifetimeSession
				session = append(session, m)
			} else {
				perTest = append(perTest, m)
			}
		}
		r := scopeReads{perTest: perTest, session: session, open: rnd.Intn(4) != 0,
			answered:      func(name string) bool { return answered[name] },
			answeredMocks: func() []*models.Mock { return nil }}
		in := live(method(), path(), "", map[string]string{"X-T": segs[rnd.Intn(2)]})
		lp := liveParts(context.Background(), in)
		ls := n.shape(lp, sameDest)
		var group []*models.Mock
		var owner func(*models.Mock) bool
		shaped := learnShapesOf(func() { group, owner = r.learnedGroup(lp, ls, n, n.learnSieveOf(lp)) })
		full := r.scope(lp, ls, n, true)
		all := exactGroup(ls, full, false)
		if !slices.Equal(group, all) {
			t.Fatalf("trial %d: %s %s: sieved group %v, compared %v", trial, in.method, in.url, strictNames(group), strictNames(all))
		}
		fullOwner := full.owner()
		for _, m := range append(slices.Clone(perTest), session...) {
			if owner(m) != fullOwner(m) {
				t.Fatalf("trial %d: owner of %s: sieved %v, compared %v", trial, m.Name, owner(m), fullOwner(m))
			}
		}
		if len(all) > 0 {
			nonEmpty++
		}
		if int(shaped) < len(full.cands) {
			sieved++
		}
	}
	if nonEmpty < 20 || sieved < 100 {
		t.Fatalf("the random calls found a group %d times and sieved %d times of 400; the row compares too little", nonEmpty, sieved)
	}
}

// In the exact view's verify mode a sieve that would drop a recording the
// call is panics; with verify off the sieved group is what is served.
func TestVerifyModeCatchesALearnedSieveThatDropsAMatch(t *testing.T) {
	_, n, _ := learnOn(t)
	m := learnRecorded("mock-0", 0, "GET", "/item/1", "", nil, `{}`)
	r := scopeReads{perTest: []*models.Mock{m},
		answered:      func(string) bool { return false },
		answeredMocks: func() []*models.Mock { return nil }}
	lp := liveParts(context.Background(), live("GET", "/item/1", "", nil))
	ls := n.shape(lp, sameDest)
	wrong := &learnSieve{masked: "item/2"}
	was := integrations.ExactViewVerify.Load()
	t.Cleanup(func() { integrations.ExactViewVerify.Store(was) })

	integrations.ExactViewVerify.Store(false)
	if group, _ := r.learnedGroup(lp, ls, n, wrong); len(group) != 0 {
		t.Fatalf("verify off: group %v; want the wrong sieve's none", strictNames(group))
	}
	integrations.ExactViewVerify.Store(true)
	func() {
		defer func() {
			if p := recover(); p == nil || !strings.Contains(fmt.Sprint(p), "learned group") {
				t.Fatalf("verify on: recovered %v; want the learned group's panic", p)
			}
		}()
		r.learnedGroup(lp, ls, n, wrong)
	}()
	// The right sieve agrees.
	if group, _ := r.learnedGroup(lp, ls, n, n.learnSieveOf(lp)); len(group) != 1 {
		t.Fatalf("verify on, the right sieve: group %v; want mock-0", strictNames(group))
	}
}

// Verify mode's two scopes read one account of what the test's own answered:
// a recording the tracker says answered between the two reads (another call
// of the test, served meanwhile) is no difference between them.
func TestVerifyModeReadsWhatTheTestAnsweredOnce(t *testing.T) {
	_, n, _ := learnOn(t)
	m := learnRecorded("mock-0", 0, "GET", "/item/1", "", nil, `{}`)
	asked := map[string]int{}
	r := scopeReads{perTest: []*models.Mock{m}, open: true,
		// The first read finds it unanswered, every later one answered.
		answered: func(name string) bool {
			asked[name]++
			return asked[name] > 1
		},
		answeredMocks: func() []*models.Mock { return nil }}
	lp := liveParts(context.Background(), live("GET", "/item/1", "", nil))
	ls := n.shape(lp, sameDest)
	was := integrations.ExactViewVerify.Load()
	t.Cleanup(func() { integrations.ExactViewVerify.Store(was) })
	integrations.ExactViewVerify.Store(true)
	if group, _ := r.learnedGroup(lp, ls, n, n.learnSieveOf(lp)); len(group) != 1 {
		t.Fatalf("group %v; want mock-0, as the first read found it", strictNames(group))
	}
}
