package http

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"unsafe"
	"weak"

	"go.keploy.io/server/v3/pkg/agent/containerkey"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
)

// parsesOf is how many recordings f parsed for recordedParts.
func parsesOf(f func()) int64 {
	before := recordedParsings.Load()
	f()
	return recordedParsings.Load() - before
}

// A learn call parses only the recordings it has not seen: the first call
// parses the scope's every recording, the next none, at 100 recordings and
// at 2000 alike (it parsed every one on every call, its cost growing with
// the set).
func TestALearnCallParsesOnlyTheRecordingsItHasNotSeen(t *testing.T) {
	for _, n := range []int{100, 2000} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			h, noise, opts := learnOn(t)
			own := make([]*models.Mock, n)
			for i := range own {
				own[i] = learnRecorded(fmt.Sprintf("mock-%d", i), 0, "GET", fmt.Sprintf("/v1/items/%d", i), "", nil, `{"ok":true}`)
			}
			db := &learnDb{perTest: own}
			call := func(i int) {
				served, miss, err := h.learnMatch(context.Background(), live("GET", fmt.Sprintf("/v1/items/%d", i), "", nil), db, noise, opts)
				if err != nil || miss != nil || served == nil {
					t.Fatalf("call %d: served %v, miss %v, err %v", i, served != nil, miss, err)
				}
			}
			if got := parsesOf(func() { call(0) }); got < int64(n) {
				t.Fatalf("the first call parsed %d recordings, want each of the %d", got, n)
			}
			if got := parsesOf(func() { call(1) }); got != 0 {
				t.Fatalf("the second call parsed %d recordings again, want 0 (of %d)", got, n)
			}
		})
	}
}

// A recording is parsed again when its recorded request is another (a copy,
// a re-decode) or the container keyer took another tally (its destination
// reads which recorded keys are ambiguous), and not otherwise.
func TestARecordingIsParsedAgainOnlyWhenItsRequestOrTheKeyersTallyChanged(t *testing.T) {
	m := learnRecorded("mock-0", 0, "GET", "/v1/items?page=1", "", nil, `{}`)
	if got := parsesOf(func() { recordedParts(m) }); got != 1 {
		t.Fatalf("first read parsed %d, want 1", got)
	}
	if got := parsesOf(func() { recordedParts(m) }); got != 0 {
		t.Fatalf("second read parsed %d, want 0", got)
	}
	next := *m.Spec.HTTPReq
	next.URL = "/v1/orders?page=2"
	m.Spec.HTTPReq = &next
	var p httpParts
	if got := parsesOf(func() { p = recordedParts(m) }); got != 1 || p.path != "/v1/orders" || p.query["page"][0] != "2" {
		t.Fatalf("after a new request: parsed %d, path %q, query %v; want 1, /v1/orders, page=2", got, p.path, p.query)
	}
	containerkey.Default.SetRecorded(&containerkey.RecordedTally{})
	if got := parsesOf(func() { recordedParts(m) }); got != 1 {
		t.Fatalf("after a new tally: parsed %d, want 1", got)
	}
}

// In verify mode a recording changed in place, which the kept parts no
// longer describe, panics instead of answering from the old parts.
func TestVerifyModeCatchesARecordingChangedInPlace(t *testing.T) {
	was := integrations.ExactViewVerify.Load()
	integrations.ExactViewVerify.Store(true)
	t.Cleanup(func() { integrations.ExactViewVerify.Store(was) })
	m := learnRecorded("mock-0", 0, "GET", "/v1/items", "", nil, `{}`)
	recordedParts(m)
	m.Spec.HTTPReq.URL = "/v1/orders"
	defer func() {
		if r := recover(); r == nil || !strings.Contains(fmt.Sprint(r), "recorded parts of mock-0") {
			t.Fatalf("recover() = %v, want the verify panic", r)
		}
	}()
	recordedParts(m)
}

// An entry is a recording's only while its weak pointer names it: an entry
// at the recording's address left by another mock (one the collector freed
// there) is not read, and that mock's cleanup does not drop the recording's.
func TestAnEntryAtARecordingsAddressIsItsOnlyWhileItNamesIt(t *testing.T) {
	other := learnRecorded("other", 0, "GET", "/v1/other", "", nil, `{}`)
	m := learnRecorded("mock-0", 0, "GET", "/v1/items", "", nil, `{}`)
	addr := uintptr(unsafe.Pointer(m))
	stale := &parsedRecording{mock: weak.Make(other), req: weak.Make(m.Spec.HTTPReq), gen: containerkey.Default.RecordedGeneration(), parts: parseRecordedParts(other)}
	parsedRecordings.Store(addr, stale)
	if p := recordedParts(m); p.path != "/v1/items" {
		t.Fatalf("read %q through another mock's entry, want /v1/items", p.path)
	}
	forgetParsedRecording(parsedKey{addr: addr, mock: stale.mock})
	if got := parsesOf(func() { recordedParts(m) }); got != 0 {
		t.Fatalf("another mock's cleanup dropped the recording's entry: parsed %d again, want 0", got)
	}
	runtime.KeepAlive(other)
}

// A recording's entry goes with the recording.
func TestAParsedRecordingsEntryGoesWithTheRecording(t *testing.T) {
	addr, w := func() (uintptr, weak.Pointer[models.Mock]) {
		m := learnRecorded("gone", 0, "GET", "/v1/items", "", nil, `{}`)
		recordedParts(m)
		return uintptr(unsafe.Pointer(m)), weak.Make(m)
	}()
	held := func() bool {
		v, ok := parsedRecordings.Load(addr)
		return ok && v.(*parsedRecording).mock == w
	}
	if !held() {
		t.Fatal("no entry for the recording just parsed")
	}
	for i := 0; i < 1000 && held(); i++ {
		runtime.GC()
		runtime.Gosched()
	}
	if held() {
		t.Fatal("the entry of a recording the collector freed is still held")
	}
}

// A recording's entry holds neither its recorded request nor the request's
// body: the collection that frees the recording frees them, whether or not
// the cleanup that drops the entry has run. That cleanup runs on the
// runtime's cleanup goroutines after the collection, and what an entry held
// went only at a collection after it: keploy mock replay held a closed test's
// request bodies beside the next test's whenever the cleanups ran late (a
// loaded box). One collection, no wait: the row reads no cleanup's timing.
func TestAParsedRecordingsEntryHoldsNeitherItsRequestNorItsBody(t *testing.T) {
	const size = 1 << 20
	req, body := func() (weak.Pointer[models.HTTPReq], weak.Pointer[byte]) {
		m := learnRecorded("gone", 0, "POST", "/v1/upload", strings.Repeat("b", size), nil, `{}`)
		if p := recordedParts(m); len(p.body) != size || unsafe.SliceData(p.body) != unsafe.StringData(m.Spec.HTTPReq.Body) {
			t.Fatalf("the parts read a body of %d bytes that is not the recording's own (recorded %d)", len(p.body), size)
		}
		return weak.Make(m.Spec.HTTPReq), weak.Make(unsafe.StringData(m.Spec.HTTPReq.Body))
	}()
	runtime.GC()
	if req.Value() != nil {
		t.Error("the collection that freed the recording left its recorded request: its parsed entry holds it until the entry's cleanup and another collection")
	}
	if body.Value() != nil {
		t.Error("the collection that freed the recording left its request body: its parsed entry holds it until the entry's cleanup and another collection")
	}
}

// A recording's parts are kept as long as the recording (recordedParts), so
// they must hold no copy of what the recording already holds: a request body
// copied into each recording's parts doubles what a set's request bodies hold
// in the agent. Over 1.25 GiB of recorded request bodies (80 recordings of a
// 16 MiB body), parsing every recording's parts allocates under a fixed
// 8 MiB, whatever the bodies' size, and each part's body is the recording's.
// It allocates the bodies' size where the parts copy them: run it on a box.
func TestARecordingsPartsHoldNoCopyOfItsBody(t *testing.T) {
	const recordings, bodySize, limit = 80, 16 << 20, 8 << 20
	body := strings.Repeat("b", bodySize)
	mocks := make([]*models.Mock, recordings)
	for i := range mocks {
		mocks[i] = &models.Mock{Name: fmt.Sprintf("mock-%d", i), Kind: models.HTTP, Spec: models.MockSpec{HTTPReq: &models.HTTPReq{
			Method: "POST", URL: fmt.Sprintf("http://deps.example/upload/%d", i),
			Header: map[string]string{"Host": "deps.example", "Content-Type": "application/octet-stream"}, Body: body,
		}}}
	}
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	for _, m := range mocks {
		if p := recordedParts(m); len(p.body) != bodySize {
			t.Fatalf("%s's parts hold a body of %d bytes, recorded %d", m.Name, len(p.body), bodySize)
		}
	}
	runtime.ReadMemStats(&after)
	if got := after.TotalAlloc - before.TotalAlloc; got > limit {
		t.Fatalf("parsing %d recordings of a %d MiB body allocated %d MiB (limit %d MiB): their parts copy the body", recordings, bodySize>>20, got>>20, limit>>20)
	}
	for _, m := range mocks {
		if p := recordedParts(m); unsafe.SliceData(p.body) != unsafe.StringData(body) {
			t.Fatalf("%s's parts hold a body that is not the recording's", m.Name)
		}
	}
	runtime.KeepAlive(mocks)
}
