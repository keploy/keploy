package http

import (
	"context"
	"fmt"
	"math/rand"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unsafe"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// strictDiffsOf is how many recordings strictDiff compared while f ran.
func strictDiffsOf(f func()) int64 {
	before := strictDiffs.Load()
	f()
	return strictDiffs.Load() - before
}

// verifyOff turns the exact view's verify mode off for a counting row: verify
// compares every recording again, by design.
func verifyOff(t *testing.T) {
	t.Helper()
	was := integrations.ExactViewVerify.Load()
	integrations.ExactViewVerify.Store(false)
	t.Cleanup(func() { integrations.ExactViewVerify.Store(was) })
}

// A strict call that is one recording of a large pool compares that one: at
// 100 recordings and at 2000 alike, one strictDiff, a recording of the same
// path with another method passed over too (it compared every
// recording of the pool on every call, looking for the closest one even when
// a recording was the call). A miss still compares every recording, and
// names the closest.
func TestAStrictCallComparesOnlyTheRecordingsThatMayBeIt(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	verifyOff(t)
	const host = "localhost:50001"
	ctx := context.Background()
	for _, n := range []int{100, 2000} {
		t.Run(strconv.Itoa(n), func(t *testing.T) {
			mocks := make([]*models.Mock, n)
			for i := range mocks {
				m := onHost(fmt.Sprintf("mock-%d", i), "/item/"+strconv.Itoa(i)+"?size=small", host)
				mocks[i] = answered(m, time.Duration(i)*time.Millisecond, 200, `{"id":`+strconv.Itoa(i)+`}`)
			}
			// The same path, posted: another method is not compared either.
			post := onHost("mock-post", "/item/"+strconv.Itoa(n/2)+"?size=small", host)
			post.Spec.HTTPReq.Method = "POST"
			mocks = append([]*models.Mock{answered(post, 0, 201, `{}`)}, mocks...)
			db := &mockMemDb{mocks: mocks, updateUnFilteredReturn: true}
			call := getTo("/item/"+strconv.Itoa(n/2), host)
			call.url.RawQuery = "size=small"
			var ok bool
			var stub *models.Mock
			var err error
			got := strictDiffsOf(func() {
				ok, stub, _, err = newHTTP().match(ctx, call, db, nil, nil, nil, true, false, false, true, true)
			})
			if err != nil || !ok || stub == nil || stub.Name != fmt.Sprintf("mock-%d", n/2) {
				t.Fatalf("match: ok %v, stub %v, err %v; want mock-%d", ok, stub, err, n/2)
			}
			if got != 1 {
				t.Fatalf("a call that is one recording of %d compared %d; want 1", n, got)
			}

			miss := getTo("/item/"+strconv.Itoa(n/2), host)
			miss.url.RawQuery = "size=large"
			var diag *matchDiag
			got = strictDiffsOf(func() {
				ok, _, diag, err = newHTTP().match(ctx, miss, db, nil, nil, nil, true, false, false, true, true)
			})
			if err != nil || ok {
				t.Fatalf("miss: ok %v, err %v; want a miss", ok, err)
			}
			if got < int64(n) {
				t.Fatalf("a miss compared %d of %d; want every recording, for the closest", got, n)
			}
			if diag == nil || diag.closest == nil || diag.closest.Name != fmt.Sprintf("mock-%d", n/2) {
				t.Fatalf("miss diagnosis %+v; want mock-%d the closest", diag, n/2)
			}
		})
	}
}

// strictFits finds what strictCompare finds, in the same order, for calls and
// pools drawn at random from the places the sieve reads: methods, paths with
// and without a leading slash, a path-only "//" URL, an absolute URL, a
// percent-encoded slash, keploy.yml's url noise, and recordings that learned
// a URL place (url.path.<i>, url.path, url) or something else.
func TestStrictFitsAreWhatEveryRecordingComparedFinds(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	verifyOff(t)
	const host = "localhost:50001"
	ctx := context.Background()
	h := newHTTP()
	rng := rand.New(rand.NewSource(7))
	methods := []string{"GET", "POST"}
	recorded := []string{"/a/1", "/a/2", "a/1", "/a/1/", "//a/1", "/a%2F1", "http://" + host + "/a/1", "/b/1", "/a/1?x=1", "/a/22", "/"}
	live := []string{"/a/1", "/a/2", "a/1", "//a/1", "/a/1/", "/a/1%2F", "/b/1", "/a/22", "/a/3", "/", ""}
	learned := [][]string{nil, {"url.path.1"}, {"url.path"}, {"url"}, {"url.path.0"}, {"body.id"}, {"header.x"}}
	noises := [][]string{nil, {`[0-9]+`}, {`a/`}}
	compared := 0
	for round := 0; round < 400; round++ {
		urlNoise := noises[rng.Intn(len(noises))]
		n := h.newStrictNoise(nil, nil, urlNoise)
		pool := make([]*models.Mock, 1+rng.Intn(12))
		for i := range pool {
			m := onHost(fmt.Sprintf("mock-%d", i), recorded[rng.Intn(len(recorded))], host)
			m.Spec.HTTPReq.Method = models.Method(methods[rng.Intn(len(methods))])
			if keys := learned[rng.Intn(len(learned))]; keys != nil {
				m.Spec.ReqBodyNoise = map[string][]string{}
				for _, k := range keys {
					m.Spec.ReqBodyNoise[k] = []string{}
				}
			}
			if rng.Intn(8) == 0 {
				m.Spec.HTTPReq = nil
			}
			pool[i] = m
		}
		call := getTo(live[rng.Intn(len(live))], host)
		call.method = methods[rng.Intn(len(methods))]
		if rng.Intn(5) == 0 {
			call.url.RawQuery = "x=1"
		}
		dest := liveDestination(ctx, call)
		want, _, err := strictCompare(ctx, call, dest, pool, n)
		if err != nil {
			t.Fatal(err)
		}
		got, err := strictFits(ctx, call, dest, pool, n)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(got, want) {
			t.Fatalf("round %d: %s %q, url noise %q: sieved %v, compared %v", round, call.method, call.url.Path, urlNoise, strictNames(got), strictNames(want))
		}
		compared += len(want)
	}
	if compared == 0 {
		t.Fatal("no round had a fit: the rows compared nothing")
	}
}

// In verify mode a recording's strict URL that no longer reads as kept (a
// stale entry) panics instead of answering from the kept URL.
func TestVerifyModeCatchesAStaleStrictURL(t *testing.T) {
	was := integrations.ExactViewVerify.Load()
	integrations.ExactViewVerify.Store(true)
	t.Cleanup(func() { integrations.ExactViewVerify.Store(was) })
	m := onHost("mock-0", "//v1/charge", "pay")
	if path, _ := recordedStrictURL(m); path != "//v1/charge" {
		t.Fatalf("strict path %q; want the path-only URL as a request line reads it", path)
	}
	v, _ := parsedRecordings.Load(uintptr(unsafe.Pointer(m)))
	v.(*parsedRecording).strict = strictURL{path: "/charge"}
	defer func() {
		if r := recover(); r == nil || !strings.Contains(fmt.Sprint(r), "recorded strict URL of mock-0") {
			t.Fatalf("recover() = %v, want the verify panic", r)
		}
	}()
	recordedStrictURL(m)
}

// A recording that learned a read of the clock in a path segment is still
// compared with a call whose segment there differs: the call carrying this
// run's read of the application's clock there is answered (the sieve passed
// it over as a path that is not the call's), one carrying another instant is
// refused naming the path.
func TestAClockReadInAPathSegmentIsComparedThoughThePathDiffers(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	base := time.Date(2026, 10, 26, 15, 50, 35, 0, time.UTC)
	now, old := base.Add(time.Second).Format(time.RFC3339), time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC).Format(time.RFC3339)
	mk := func() *models.Mock {
		m := recorded("m", "/v1/u/"+old, map[string]string{"Content-Type": "application/json"}, `{}`)
		m.Spec.Clocks = map[string]models.ClockOffset{"url.path.2": {Res: time.Second}}
		return m
	}
	call := func(seg string) *req {
		r := sent("/v1/u/"+seg, map[string]string{"Content-Type": "application/json"}, `{}`)
		r.arrived = base.Add(2 * time.Second)
		return r
	}
	if got, diag := strictMatchIn(t, clockConn(true, base), call(now), nil, nil, nil, mk()); got == nil {
		t.Fatalf("refused (%v); want this run's read of the clock in the path answered", differs(diag))
	}
	if got, diag := strictMatchIn(t, clockConn(true, base), call("2026-09-02T09:00:00Z"), nil, nil, nil, mk()); got != nil || !hasField(differs(diag), "path") {
		t.Fatalf("answered %v, differs %v; want another instant refused naming the path", got != nil, differs(diag))
	}
}
