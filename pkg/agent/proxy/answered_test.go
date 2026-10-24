package proxy

import (
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// keyedRecording is a per-test recording of kind, keyed by url, recorded at
// at.
func keyedRecording(name string, kind models.Kind, id int, url string, at time.Time) *models.Mock {
	return &models.Mock{
		Name: name,
		Kind: kind,
		Spec: models.MockSpec{
			HTTPReq:          &models.HTTPReq{Method: "GET", URL: url},
			ReqTimestampMock: at,
			ResTimestampMock: at.Add(time.Millisecond),
		},
		TestModeInfo: models.TestModeInfo{ID: id, SortOrder: int64(id), IsFiltered: true, Lifetime: models.LifetimePerTest},
	}
}

// byURL declares, for the test, a request key of HTTP and Generic recordings:
// their URL.
func byURL(t *testing.T) {
	t.Helper()
	keys := func(m *models.Mock) []string {
		if m.Spec.HTTPReq == nil {
			return nil
		}
		return []string{"url:" + m.Spec.HTTPReq.URL}
	}
	t.Cleanup(models.RegisterRequestKeys(models.HTTP, keys))
	t.Cleanup(models.RegisterRequestKeys(models.GENERIC, keys))
}

// A per-test recording that answered a call in an earlier test is found by a
// later test's lookup under its key, with when it answered; the open test's
// own recording, one of another kind, and one
// the match refuses are not. A new session or a new test set forgets them.
func TestFindAnsweredEarlierNamesAnEarlierTestsAnsweredRecording(t *testing.T) {
	byURL(t)
	t0 := time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)
	earlier := keyedRecording("mock-1", models.HTTP, 1, "/count", t0)
	own := keyedRecording("mock-2", models.HTTP, 2, "/count", t0.Add(10*time.Second))
	generic := keyedRecording("mock-3", models.GENERIC, 3, "/count", t0.Add(-time.Second))
	all := func(*models.Mock) bool { return true }

	mm := NewMockManager(nil, nil, zap.NewNop())
	defer mm.Close()
	// answer consumes the staged copy of mk, as a parser consumes the copy
	// its read handed it.
	answer := func(mk *models.Mock) bool {
		staged, _ := mm.GetFilteredMocks()
		for _, s := range staged {
			if s.Name == mk.Name {
				return mm.DeleteFilteredMock(*s)
			}
		}
		return false
	}
	stage := func() {
		mm.SetMocksWithWindow([]*models.Mock{earlier, own, generic}, nil, t0.Add(-2*time.Second), t0.Add(time.Second))
	}
	stage()
	if _, ok := mm.FindAnsweredEarlier(models.HTTP, []string{"url:/count"}, all); ok {
		t.Fatal("found a recording before any answered")
	}
	// The first test's calls: its own recording, and a Generic one.
	if !answer(earlier) || !answer(generic) {
		t.Fatal("the first test's recordings did not answer")
	}
	// The second test opens, and its own recording of the call answers too.
	start := t0.Add(5 * time.Second)
	mm.SetMocksWithWindow([]*models.Mock{own}, nil, start, t0.Add(15*time.Second))
	if !answer(own) {
		t.Fatal("the second test's recording did not answer")
	}

	got, ok := mm.FindAnsweredEarlier(models.HTTP, []string{"url:/count"}, all)
	if !ok || got.Mock.Name != earlier.Name {
		t.Fatalf("found %v (%v), want the earlier test's %s: not the open test's own, nor a Generic one", got.Mock, ok, earlier.Name)
	}
	if got.AnsweredAt.IsZero() {
		t.Fatal("the recording found carries no time it answered")
	}
	// A parser reads the store through the worker-scope wrap, which must
	// forward the lookup (integrations.AnsweredFinder).
	var wrapped integrations.MockMemDb = &scopedMockDb{MockMemDb: mm}
	if f, ok := wrapped.(integrations.AnsweredFinder); !ok {
		t.Fatal("the worker-scope wrap is no AnsweredFinder")
	} else if w, ok := f.FindAnsweredEarlier(models.HTTP, []string{"url:/count"}, all); !ok || w.Mock.Name != earlier.Name || !w.AnsweredAt.Equal(got.AnsweredAt) {
		t.Fatalf("through the worker-scope wrap found %v (%v), want %s as the store found it", w.Mock, ok, earlier.Name)
	}
	if _, ok := mm.FindAnsweredEarlier(models.HTTP, []string{"url:/other"}, all); ok {
		t.Fatal("found a recording under a key it is not under")
	}
	if _, ok := mm.FindAnsweredEarlier(models.HTTP, []string{"url:/count"}, func(*models.Mock) bool { return false }); ok {
		t.Fatal("found a recording the match refused")
	}
	if got, ok := mm.FindAnsweredEarlier(models.GENERIC, []string{"url:/count"}, all); !ok || got.Mock.Name != generic.Name {
		t.Fatalf("a Generic lookup found %v (%v), want %s", got.Mock, ok, generic.Name)
	}

	mm.ResetForReplaySession()
	if got, ok := mm.FindAnsweredEarlier(models.HTTP, []string{"url:/count"}, all); ok {
		t.Fatalf("a new session still found %s", got.Mock.Name)
	}
	stage()
	if !answer(earlier) {
		t.Fatal("the recording did not answer after the reset")
	}
	mm.SetMocksWithWindow([]*models.Mock{own}, nil, start, t0.Add(15*time.Second))
	if _, ok := mm.FindAnsweredEarlier(models.HTTP, []string{"url:/count"}, all); !ok {
		t.Fatal("a recording that answered after the reset was not found")
	}
	mm.dropPreviousSetConsumes()
	if got, ok := mm.FindAnsweredEarlier(models.HTTP, []string{"url:/count"}, all); ok {
		t.Fatalf("a new test set still found %s", got.Mock.Name)
	}
}

// Of several recordings that answered under a key, the one answered last is
// found; a key keeps its newest answeredPerKey, and the log the newest that
// fit in answeredMaxBytes.
func TestAnsweredLogKeepsTheNewest(t *testing.T) {
	byURL(t)
	t0 := time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)
	var l answeredLog
	for i := 0; i < answeredPerKey+4; i++ {
		l.note(keyedRecording(fmt.Sprintf("m%d", i), models.HTTP, i, "/a", t0), t0.Add(time.Duration(i)*time.Second))
	}
	c := l.candidates([]string{"url:/a"})
	if len(c) != answeredPerKey || c[0].mock.Name != fmt.Sprintf("m%d", answeredPerKey+3) || c[len(c)-1].mock.Name != "m4" {
		t.Fatalf("kept %d, newest %s, oldest %s; want %d from m%d down to m4", len(c), c[0].mock.Name, c[len(c)-1].mock.Name, answeredPerKey, answeredPerKey+3)
	}
	// Newer keys, until the byte bound has pushed out every recording of /a,
	// the oldest noted: no newer one went first, and the log never held more
	// than its bound.
	n := 0
	for len(l.candidates([]string{"url:/a"})) > 0 {
		if n > answeredMaxBytes/answeredEntryOverhead {
			t.Fatalf("%d newer keys, %d bytes held, and /a is still kept: no byte bound", n, l.bytes)
		}
		l.note(keyedRecording(fmt.Sprintf("k%d", n), models.HTTP, n, fmt.Sprintf("/k%d", n), t0), t0)
		n++
		if l.bytes > answeredMaxBytes {
			t.Fatalf("the log holds %d bytes, over its bound %d", l.bytes, answeredMaxBytes)
		}
	}
	if c := l.candidates([]string{"url:/k0"}); len(c) != 1 {
		t.Fatal("k0 was pushed out before the older recordings of /a")
	}
	if _, ok := l.byKey["url:/a"]; ok {
		t.Fatal("the key of the recordings pushed out still has a list")
	}
	checkAnsweredCounts(t, &l)

	// Across keys the newest wins: the manager compares answer times.
	mm := NewMockManager(nil, nil, zap.NewNop())
	defer mm.Close()
	mm.answered.note(keyedRecording("old", models.HTTP, 1, "/x", t0), t0)
	mm.answered.note(keyedRecording("new", models.HTTP, 2, "/y", t0), t0.Add(time.Second))
	got, ok := mm.FindAnsweredEarlier(models.HTTP, []string{"url:/x", "url:/y"}, func(*models.Mock) bool { return true })
	if !ok || got.Mock.Name != "new" {
		t.Fatalf("found %v, want the one answered last", got.Mock)
	}
	// Whichever key comes first: the newest is not the last one looked at.
	got, ok = mm.FindAnsweredEarlier(models.HTTP, []string{"url:/y", "url:/x"}, func(*models.Mock) bool { return true })
	if !ok || got.Mock.Name != "new" {
		t.Fatalf("with the newest's key first, found %v, want the one answered last", got.Mock)
	}
}

// What the log keeps of a recording is what a match reads: its request, name,
// kind, times and noise, never its response, nor the loader that would read
// one back from disk.
func TestAnsweredLogKeepsNoResponse(t *testing.T) {
	byURL(t)
	t0 := time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)
	mk := keyedRecording("mock-1", models.HTTP, 1, "/rates", t0)
	mk.Noise = []string{"secret"}
	mk.Spec.Metadata = map[string]string{"type": "http-client"}
	mk.Spec.ReqBodyNoise = map[string][]string{"body.ts": nil}
	mk.Spec.HTTPReq.Body = `{"q":1}`
	mk.Spec.HTTPResp = &models.HTTPResp{StatusCode: 200, Body: strings.Repeat("r", 64<<10)}
	mk.Spec.MongoResponses = []models.MongoResponse{{}}
	mk.SetResponseHydrator(func() (*models.HTTPResp, []models.MongoResponse, error) {
		return &models.HTTPResp{Body: "from disk"}, nil, nil
	})
	var l answeredLog
	l.note(mk, t0.Add(time.Second))
	c := l.candidates([]string{"url:/rates"})
	if len(c) != 1 {
		t.Fatalf("kept %d, want the one", len(c))
	}
	p := c[0].mock
	if p.Spec.HTTPResp != nil || p.Spec.MongoResponses != nil || p.HasSpilledResponse() {
		t.Fatal("the log keeps a response, or the loader of one")
	}
	if p.Name != mk.Name || p.Kind != mk.Kind || p.Spec.HTTPReq == nil || p.Spec.HTTPReq.Body != mk.Spec.HTTPReq.Body ||
		!p.Spec.ReqTimestampMock.Equal(t0) || len(p.Noise) != 1 || p.Spec.Metadata["type"] != "http-client" || len(p.Spec.ReqBodyNoise) != 1 {
		t.Fatalf("the log lost what a match or the note reads: %+v", p)
	}
	if c[0].size >= 8<<10 {
		t.Fatalf("an entry with a small request counts %d bytes", c[0].size)
	}
	if mk.Spec.HTTPResp == nil || !mk.HasSpilledResponse() {
		t.Fatal("noting the recording changed it")
	}
}

// A set of 20,000 answered recordings, each with a 4 KiB request and a 64 KiB
// response, leaves the log holding no more than its byte bound: the newest
// are kept, the oldest pushed out, and no response is held.
func TestAnsweredLogStaysSmall(t *testing.T) {
	byURL(t)
	t0 := time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)
	var l answeredLog
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	const n = 20000
	for i := 0; i < n; i++ {
		mk := keyedRecording(fmt.Sprintf("m%d", i), models.HTTP, i, fmt.Sprintf("/c/%d", i), t0)
		body := make([]byte, 4<<10)
		resp := make([]byte, 64<<10)
		body[0], resp[0] = byte(i), byte(i)
		mk.Spec.HTTPReq.Body = string(body)
		mk.Spec.HTTPResp = &models.HTTPResp{StatusCode: 200, Body: string(resp)}
		l.note(mk, t0.Add(time.Duration(i)*time.Millisecond))
	}
	runtime.GC()
	runtime.ReadMemStats(&after)
	if l.bytes > answeredMaxBytes {
		t.Fatalf("the log counts %d bytes, over its bound %d", l.bytes, answeredMaxBytes)
	}
	if grew := int64(after.HeapAlloc) - int64(before.HeapAlloc); grew > 3*answeredMaxBytes {
		t.Fatalf("%d answered recordings left %d MiB on the heap; the bound is %d MiB", n, grew>>20, answeredMaxBytes>>20)
	}
	if c := l.candidates([]string{fmt.Sprintf("url:/c/%d", n-1)}); len(c) != 1 {
		t.Fatal("the newest recording was not kept")
	}
	if c := l.candidates([]string{"url:/c/0"}); len(c) != 0 {
		t.Fatal("the oldest recording outlived the byte bound")
	}
	runtime.KeepAlive(&l)
}

// A recording whose request alone is over answeredMaxEntryBytes is not kept:
// it would push out many smaller ones. One just under is.
func TestAnsweredLogKeepsNoOversizedRequest(t *testing.T) {
	byURL(t)
	t0 := time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)
	var l answeredLog
	big := keyedRecording("big", models.HTTP, 1, "/big", t0)
	big.Spec.HTTPReq.Body = strings.Repeat("b", answeredMaxEntryBytes)
	l.note(big, t0)
	if c := l.candidates([]string{"url:/big"}); len(c) != 0 {
		t.Fatalf("kept a recording of %d bytes, over the %d an entry may hold", c[0].size, answeredMaxEntryBytes)
	}
	// Of any kind: a Generic recording's request is in its payloads.
	gen := keyedRecording("gen", models.GENERIC, 3, "/gen", t0)
	gen.Spec.GenericRequests = []models.Payload{{Message: []models.OutputBinary{{Data: strings.Repeat("g", answeredMaxEntryBytes)}}}}
	l.note(gen, t0)
	if c := l.candidates([]string{"url:/gen"}); len(c) != 0 {
		t.Fatalf("kept a Generic recording of %d bytes, over the %d an entry may hold", c[0].size, answeredMaxEntryBytes)
	}
	fits := keyedRecording("fits", models.HTTP, 2, "/fits", t0)
	fits.Spec.HTTPReq.Body = strings.Repeat("f", answeredMaxEntryBytes-4*answeredEntryOverhead)
	l.note(fits, t0)
	if c := l.candidates([]string{"url:/fits"}); len(c) != 1 {
		t.Fatal("a recording under the entry bound was not kept")
	}
}

// A key answered again and again holds its newest answeredPerKey, and the
// log counts and queues only those: the ones the key's bound dropped give
// their bytes back and leave the queue.
func TestAnsweredLogShedsWhatAKeyDropped(t *testing.T) {
	byURL(t)
	t0 := time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)
	var l answeredLog
	size := 0
	for i := 0; i < 10000; i++ {
		l.note(keyedRecording(fmt.Sprintf("m%d", i), models.HTTP, i, "/same", t0), t0)
		if i == 0 {
			size = l.bytes
		}
	}
	if l.live != answeredPerKey || l.bytes > answeredPerKey*(size+16) {
		t.Fatalf("one key answered 10000 times: %d entries and %d bytes counted, want %d of about %d", l.live, l.bytes, answeredPerKey, size)
	}
	if len(l.queue) > 2*answeredPerKey+64+1 {
		t.Fatalf("one key answered 10000 times queues %d entries", len(l.queue))
	}
	checkAnsweredCounts(t, &l)
}

// checkAnsweredCounts fails t unless l's counts are those of the entries its
// key lists hold.
func checkAnsweredCounts(t *testing.T, l *answeredLog) {
	t.Helper()
	seen := map[*answeredEntry]bool{}
	bytes := 0
	for _, list := range l.byKey {
		for _, e := range list {
			if !seen[e] {
				seen[e] = true
				bytes += e.size
			}
		}
	}
	if l.live != len(seen) || l.bytes != bytes {
		t.Fatalf("the log counts %d entries and %d bytes; its lists hold %d and %d", l.live, l.bytes, len(seen), bytes)
	}
}

// Under exact serving a test's recording that answered through any door (a
// delete, or the door that keeps a stateful recording, MarkMockAsUsed) is
// found by a later test's lookup, the later test's own is not, and the
// recorded start is not known (exact serving stages no window). Without exact
// serving, a per-test recording MarkMockAsUsed keeps answered too.
func TestFindAnsweredEarlierUnderExactServing(t *testing.T) {
	byURL(t)
	all := func(*models.Mock) bool { return true }
	doors := map[string]func(mm *MockManager, m *models.Mock) bool{
		"DeleteFilteredMock": func(mm *MockManager, m *models.Mock) bool { return mm.DeleteFilteredMock(*m) },
		"MarkMockAsUsed":     func(mm *MockManager, m *models.Mock) bool { return mm.MarkMockAsUsed(*m) },
	}
	for door, consume := range doors {
		t.Run(door, func(t *testing.T) {
			first := keyedRecording("mock-1", models.HTTP, 1, "/count", exactT0)
			second := keyedRecording("mock-2", models.HTTP, 2, "/count", exactT0.Add(10*time.Second))
			mm := stagedManager(t, []*models.Mock{first, second}, nil)
			mm.ServeExactTest("T1", []*models.Mock{first})
			pt, _ := mm.GetPerTestMocksInWindow()
			if len(pt) != 1 || !consume(mm, pt[0]) {
				t.Fatalf("the first test's recording did not answer (%d shown)", len(pt))
			}
			mm.ServeExactTest("T2", []*models.Mock{second})
			pt, _ = mm.GetPerTestMocksInWindow()
			if len(pt) != 1 || !consume(mm, pt[0]) {
				t.Fatal("the second test's recording did not answer")
			}
			got, ok := mm.FindAnsweredEarlier(models.HTTP, []string{"url:/count"}, all)
			if !ok || got.Mock.Name != first.Name {
				t.Fatalf("found %v (%v), want the first test's %s, not the open test's own", got.Mock, ok, first.Name)
			}
		})
	}

	t.Run("MarkMockAsUsed without exact serving", func(t *testing.T) {
		t0 := time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)
		kept := keyedRecording("mock-1", models.HTTP, 1, "/count", t0)
		mm := NewMockManager(nil, nil, zap.NewNop())
		defer mm.Close()
		// A session recording answers every test that asks: one it served
		// after is no earlier test's answer.
		session := keyedRecording("mock-s", models.HTTP, 2, "/count", t0)
		session.TestModeInfo.IsFiltered, session.TestModeInfo.Lifetime = false, models.LifetimeSession
		mm.SetMocksWithWindow([]*models.Mock{kept}, []*models.Mock{session}, t0.Add(-time.Second), t0.Add(time.Second))
		staged, _ := mm.GetFilteredMocks()
		if len(staged) != 1 || !mm.MarkMockAsUsed(*staged[0]) {
			t.Fatal("the kept recording did not answer")
		}
		if !mm.MarkMockAsUsed(*session) {
			t.Fatal("the session recording did not answer")
		}
		mm.SetMocksWithWindow(nil, []*models.Mock{session}, t0.Add(5*time.Second), t0.Add(15*time.Second))
		if got, ok := mm.FindAnsweredEarlier(models.HTTP, []string{"url:/count"}, all); !ok || got.Mock.Name != kept.Name {
			t.Fatalf("found %v (%v), want %s, not the session recording", got.Mock, ok, kept.Name)
		}
	})
}

// A consume of a mock the previous test set's staging pooled is no answer of
// this set (errPreviousSet): the parser matches again against this set's
// pool, and the log keeps nothing of it, however late after the fence the
// consume door that took it gets to the note (its flag passed before the
// fence), so a miss of this set is never named as a repeat of a call only
// the previous set made. One of this set's own is kept.
func TestFindAnsweredEarlierLeavesOutAPreviousSetsConsume(t *testing.T) {
	byURL(t)
	t0 := time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)
	all := func(*models.Mock) bool { return true }
	mm := NewMockManager(nil, nil, zap.NewNop())
	defer mm.Close()
	mm.SetMocksWithWindow([]*models.Mock{keyedRecording("mock-1", models.HTTP, 1, "/count", t0)}, nil, t0.Add(-time.Second), t0.Add(time.Second))
	staged, _ := mm.GetFilteredMocks()
	if len(staged) != 1 || staged[0].TestModeInfo.StagedSet == 0 {
		t.Fatalf("staged %v, want mock-1 stamped with its set", staged)
	}
	held := *staged[0]
	// The next set begins while a parser still holds the previous set's copy.
	mm.dropPreviousSetConsumes()
	if mm.DeleteFilteredMock(held) {
		t.Fatal("a mock the previous set pooled was consumed in this set")
	}
	mm.noteAnswered(held)
	// A later test of this set opens: mock-1 is no recording of its.
	mm.SetMocksWithWindow([]*models.Mock{keyedRecording("mock-2", models.HTTP, 2, "/count", t0.Add(2*time.Second))}, nil, t0.Add(time.Second), t0.Add(3*time.Second))
	if got, ok := mm.FindAnsweredEarlier(models.HTTP, []string{"url:/count"}, all); ok {
		t.Fatalf("the previous set's consume of %s is in this set's answered log", got.Mock.Name)
	}
	staged, _ = mm.GetFilteredMocks()
	if len(staged) != 1 || !mm.DeleteFilteredMock(*staged[0]) {
		t.Fatalf("this set's mock-2 did not answer: %v", staged)
	}
	mm.SetCurrentTestWindow(t0.Add(5*time.Second), t0.Add(15*time.Second))
	if got, ok := mm.FindAnsweredEarlier(models.HTTP, []string{"url:/count"}, all); !ok || got.Mock.Name != "mock-2" {
		t.Fatalf("found %v (%v), want this set's mock-2", got.Mock, ok)
	}
}
