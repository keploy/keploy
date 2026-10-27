package proxy

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"testing"
	"time"

	httpint "go.keploy.io/server/v3/pkg/agent/proxy/integrations/http"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// sharedPoolRun stages pool recordings shown to every test under the
// background rules, in the session tier, and tests recordings each of their
// own, in the per-test tier, one recorded window per test, and serves exactly
// with the pool between tests. extra is staged in the session tier too.
func sharedPoolRun(t *testing.T, pool, tests, own int, extra ...*models.Mock) (*Proxy, *MockManager, []string) {
	t.Helper()
	t.Setenv(provenswitch.Env, "1")
	p := startedDrainProxy(t, 100)
	p.learnTracker = learn.NewTracker()
	now := time.Now()
	var staged, session []*models.Mock
	var poolNames []string
	for i := 0; i < pool; i++ {
		name := fmt.Sprintf("mock-pool-%d", i)
		session = append(session, r3Mock(name, now.Add(time.Duration(i)*time.Microsecond), "GET", fmt.Sprintf("/pool/%d", i), "", fmt.Sprintf(`{"pool":%d}`, i)))
		poolNames = append(poolNames, name)
	}
	var windows []models.TestWindow
	for k := 0; k < tests; k++ {
		start := now.Add(time.Duration(k+1) * time.Second)
		windows = append(windows, models.TestWindow{Start: start, End: start.Add(900 * time.Millisecond)})
		for j := 0; j < own; j++ {
			staged = append(staged, r3Mock(fmt.Sprintf("mock-own-%d-%d", k, j), start.Add(time.Duration(j+1)*100*time.Millisecond), "GET", fmt.Sprintf("/own/%d/%d", k, j), "", fmt.Sprintf(`{"own":"%d-%d"}`, k, j)))
		}
	}
	session = append(session, extra...)
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	mm.SeedRecordedWindows(windows)
	mm.SetMocksWithWindow(staged, session, models.BaseTime, now.Add(time.Hour))
	mm.ServeExactBackground(models.RecordedWithS12Routing, poolNames, nil)
	p.setMockManager(mm)
	t.Cleanup(func() { p.setMockManager(nil) })
	mm.ServeExactPool(models.PoolKeyNone, poolNames)
	return p, mm, poolNames
}

// A test's open moves the staging generation, but not what every test's view
// shows beside its own recordings: under the background rules, the pool. The
// HTTP parser's indexes of that shared part are built once for the run, not
// once per test: over 20 tests of 3 calls each (two of the test's own, one of
// the pool) the store's reads hand the parser at most a few staged sets'
// worth of recordings, where an index rebuilt at each test's open reads the
// whole set at least twice a test.
func TestATestsOpenDoesNotRebuildTheHTTPIndexesOfTheSharedPool(t *testing.T) {
	const pool, tests, own = 500, 20, 2
	defer exactViewVerify.Store(exactViewVerify.Load())
	exactViewVerify.Store(false) // the verify mode reads the whole set on every call: counted alone
	p, mm, poolNames := sharedPoolRun(t, pool, tests, own)
	h := httpint.New(zap.NewNop())
	get := func(target string) string {
		return "GET " + target + " HTTP/1.1\r\nHost: deps.example\r\nContent-Type: application/json\r\n\r\n"
	}
	// The reads the calls make: a test's open and close narrow the staged
	// set themselves, whatever the parser keeps.
	var narrowed uint64
	send := func(target string) (int, string) {
		before := viewNarrowed.Load()
		defer func() { narrowed += viewNarrowed.Load() - before }()
		return r3SendVia(t, context.Background(), h, p, mm, models.OutgoingOptions{}, get(target))
	}
	for k := 0; k < tests; k++ {
		var owned []*models.Mock
		for j := 0; j < own; j++ {
			owned = append(owned, roundMock(mm, fmt.Sprintf("mock-own-%d-%d", k, j)))
		}
		mm.ServeExactTest(fmt.Sprintf("test-%d", k), owned)
		p.ContinueTestErrorCapture()
		for j := 0; j < own; j++ {
			if code, body := send(fmt.Sprintf("/own/%d/%d", k, j)); code != http.StatusOK || body != fmt.Sprintf(`{"own":"%d-%d"}`, k, j) {
				t.Fatalf("test-%d's call %d got %d %s", k, j, code, body)
			}
		}
		at := k * 7 % pool
		if code, body := send(fmt.Sprintf("/pool/%d", at)); code != http.StatusOK || body != fmt.Sprintf(`{"pool":%d}`, at) {
			t.Fatalf("test-%d's pool call got %d %s", k, code, body)
		}
		mm.ServeExactPool(models.PoolKeyNone, poolNames)
	}
	if misses, _ := p.GetMockErrors(context.Background()); len(misses) != 0 {
		t.Fatalf("misses %+v", misses)
	}
	staged := pool + tests*own
	t.Logf("the reads handed the parser %d recordings over %d tests of %d staged", narrowed, tests, staged)
	if narrowed > uint64(4*staged) {
		t.Fatalf("the store's reads handed the HTTP parser %d recordings over %d tests, %.1f staged sets: its indexes of the shared pool are rebuilt as each test opens", narrowed, tests, float64(narrowed)/float64(staged))
	}
}

// A store of another kind's changed copy (MySQL's matcher stores one of a
// protocol-phase recording on each connection's match, generic's on each
// match) moves every kind's stored generation, but not HTTP's, and keeps the
// HTTP parser's indexes of the shared pool: the count row's 20 tests, with
// two such stores a test, hand the parser what they hand it without, not a
// staged set per store.
func TestAnotherKindsStoreKeepsTheHTTPIndexesOfTheSharedPool(t *testing.T) {
	const pool, tests, own = 500, 20, 2
	defer exactViewVerify.Store(exactViewVerify.Load())
	exactViewVerify.Store(false) // the verify mode reads the whole set on every call: counted alone
	cfg := newMockForTest("mysql-handshake", time.Now().Add(-time.Second), models.LifetimeSession)
	cfg.Kind = models.MySQL
	cfg.Spec.Metadata = map[string]string{"type": "config"}
	p, mm, poolNames := sharedPoolRun(t, pool, tests, own, cfg)
	h := httpint.New(zap.NewNop())
	var narrowed uint64
	send := func(target, want string) {
		t.Helper()
		before := viewNarrowed.Load()
		code, body := r3SendVia(t, context.Background(), h, p, mm, models.OutgoingOptions{}, httpGet(target))
		narrowed += viewNarrowed.Load() - before
		if code != http.StatusOK || body != want {
			t.Fatalf("%s got %d %s; want %s", target, code, body, want)
		}
	}
	cur, stores := cfg, 0
	for k := 0; k < tests; k++ {
		var owned []*models.Mock
		for j := 0; j < own; j++ {
			owned = append(owned, roundMock(mm, fmt.Sprintf("mock-own-%d-%d", k, j)))
		}
		mm.ServeExactTest(fmt.Sprintf("test-%d", k), owned)
		p.ContinueTestErrorCapture()
		for j := 0; j < own; j++ {
			httpGen, _ := mm.SharedGeneration(models.HTTP)
			cp := cur.ShallowCopy()
			cp.Spec.Metadata = map[string]string{"type": "config", "rev": fmt.Sprint(k, j)}
			if !mm.UpdateUnFilteredMock(cur, cp) {
				t.Fatalf("test-%d: the changed copy %d was not stored", k, j)
			}
			stores++
			cur = cp
			if again, _ := mm.SharedGeneration(models.HTTP); again != httpGen {
				t.Fatalf("a MySQL store moved HTTP's shared generation %v -> %v", httpGen, again)
			}
			send(fmt.Sprintf("/own/%d/%d", k, j), fmt.Sprintf(`{"own":"%d-%d"}`, k, j))
		}
		at := k * 7 % pool
		send(fmt.Sprintf("/pool/%d", at), fmt.Sprintf(`{"pool":%d}`, at))
		mm.ServeExactPool(models.PoolKeyNone, poolNames)
	}
	staged := pool + 1 + tests*own
	t.Logf("%d MySQL stores; the reads handed the HTTP parser %d recordings over %d tests of %d staged (%.1f sets)", stores, narrowed, tests, staged, float64(narrowed)/float64(staged))
	if narrowed > uint64(4*staged) {
		t.Fatalf("the reads handed the HTTP parser %d recordings over %d tests, %.1f staged sets: another kind's store rebuilt its indexes of the shared pool", narrowed, tests, float64(narrowed)/float64(staged))
	}
}

// httpGet is a GET of target as the app sends it.
func httpGet(target string) string {
	return "GET " + target + " HTTP/1.1\r\nHost: deps.example\r\nContent-Type: application/json\r\n\r\n"
}

// The HTTP parser's learn and strict reads through its index of the shared
// part of a test's reads are what a read of the store gives (the verify mode
// panics on any difference), over random runs under the background rules:
// pool recordings in either tier, shared by several paths and recorded at
// one instant, tests
// whose own recordings share the pool's paths or its instants and are
// per-test or session recordings, a test that
// owns a pool recording too (the session read holds it once), a recording a
// test kept for its repeats and is over with (Spend), calls of the test's
// own, the pool's, and another test's paths, repeats, calls past what the
// test and the pool recorded of a path, calls between tests, and a staging of
// the set again between tests. Every call ends at its deadline and reads the
// store a few times at most, so a parser that reads a recording the view
// refuses again and again fails the row at that call.
func TestTheHTTPIndexOfASharedPartIsTheReadUnderRandomTests(t *testing.T) {
	defer exactViewVerify.Store(exactViewVerify.Load())
	exactViewVerify.Store(true)
	for seed := int64(1); seed <= 6; seed++ {
		t.Run(fmt.Sprint(seed), func(t *testing.T) { walkSharedHTTP(t, seed) })
	}
}

func walkSharedHTTP(t *testing.T, seed int64) {
	const pool, tests, own, paths = 24, 8, 4, 6
	rng := rand.New(rand.NewSource(seed))
	t.Setenv(provenswitch.Env, "1")
	p := startedDrainProxy(t, 100)
	p.learnTracker = learn.NewTracker()
	now := time.Now()
	// staged and session are the per-test and session tiers: the pool's
	// recordings in either, a test's own in the tier its lifetime names.
	var staged, session []*models.Mock
	var poolNames []string
	poolAt := func(i int) time.Time { return now.Add(time.Duration(i%8) * time.Microsecond) }
	for i := 0; i < pool; i++ {
		name := fmt.Sprintf("mock-pool-%02d", i)
		m := r3Mock(name, poolAt(i), "GET", fmt.Sprintf("/p/%d", i%paths), "", fmt.Sprintf(`{"pool":%d}`, i))
		if rng.Intn(4) == 0 {
			staged = append(staged, m)
		} else {
			session = append(session, m)
		}
		poolNames = append(poolNames, name)
	}
	var windows []models.TestWindow
	ownPath := map[string]string{}
	for k := 0; k < tests; k++ {
		start := now.Add(time.Duration(k+1) * time.Second)
		windows = append(windows, models.TestWindow{Start: start, End: start.Add(900 * time.Millisecond)})
		for j := 0; j < own; j++ {
			name := fmt.Sprintf("mock-own-%d-%d", k, j)
			path, at := fmt.Sprintf("/own/%d/%d", k, j), start.Add(time.Duration(j+1)*100*time.Millisecond)
			if rng.Intn(3) == 0 {
				path = fmt.Sprintf("/p/%d", rng.Intn(paths))
			}
			if rng.Intn(4) == 0 {
				at = poolAt(rng.Intn(pool))
			}
			m := r3Mock(name, at, "GET", path, "", fmt.Sprintf(`{"own":"%d-%d"}`, k, j))
			if rng.Intn(2) == 0 {
				m.TestModeInfo.Lifetime = models.LifetimePerTest
				staged = append(staged, m)
			} else {
				session = append(session, m)
			}
			ownPath[name] = path
		}
	}
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	mm.SeedRecordedWindows(windows)
	stage := func() {
		mm.SetMocksWithWindow(staged, session, models.BaseTime, now.Add(time.Hour))
		mm.ServeExactBackground(models.RecordedWithS12Routing, poolNames, nil)
		mm.ServeExactPool(models.PoolKeyNone, poolNames)
	}
	stage()
	p.setMockManager(mm)
	t.Cleanup(func() { p.setMockManager(nil) })
	core, logs := observer.New(zap.DebugLevel)
	h := httpint.New(zap.New(core))
	// A call reads the store a few times at most (the hang row's bound,
	// with the learn step's reads beside the strict pass's); one that reads
	// a refused recording without end reads it thousands of times before
	// its deadline.
	const maxReads, readMsg = 16, "mocks under consideration for match function"
	send := func(target string) {
		t.Helper()
		before := logs.FilterMessage(readMsg).Len()
		_, _, err := answerWithin(h, p, mm, httpGet(target))
		if errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("GET %s: %v", target, err)
		}
		if reads := logs.FilterMessage(readMsg).Len() - before; reads > maxReads {
			t.Fatalf("GET %s read the store %d times: a recording the read shows is one the view refuses", target, reads)
		}
	}
	randomPath := func(k int) string {
		switch rng.Intn(3) {
		case 0:
			return ownPath[fmt.Sprintf("mock-own-%d-%d", k, rng.Intn(own))]
		case 1:
			return fmt.Sprintf("/p/%d", rng.Intn(paths))
		default:
			return ownPath[fmt.Sprintf("mock-own-%d-%d", rng.Intn(tests), rng.Intn(own))]
		}
	}
	for k := 0; k < tests; k++ {
		var owned []*models.Mock
		for j := 0; j < own; j++ {
			owned = append(owned, roundMock(mm, fmt.Sprintf("mock-own-%d-%d", k, j)))
		}
		past := fmt.Sprintf("/p/%d", rng.Intn(paths))
		if rng.Intn(3) == 0 {
			i := rng.Intn(pool)
			owned = append(owned, roundMock(mm, poolNames[i]))
			past = fmt.Sprintf("/p/%d", i%paths)
		}
		if rng.Intn(4) == 0 {
			mm.Spend(fmt.Sprintf("mock-own-%d-%d", k, rng.Intn(own)))
		}
		mm.ServeExactTest(fmt.Sprintf("test-%d", k), owned)
		p.ContinueTestErrorCapture()
		for c := 0; c < 6; c++ {
			send(randomPath(k))
		}
		// Past what was recorded: each of the test's own paths twice, and
		// one pool path (that of the pool recording the test owns, if it
		// owns one) once more than the pool and the test can answer of it.
		for j := 0; j < own; j++ {
			path := ownPath[fmt.Sprintf("mock-own-%d-%d", k, j)]
			send(path)
			send(path)
		}
		for c := 0; c <= pool/paths+own; c++ {
			send(past)
		}
		mm.ServeExactPool(models.PoolKeyNone, poolNames)
		if rng.Intn(2) == 0 {
			send(randomPath(k))
		}
		if rng.Intn(8) == 0 {
			stage()
		}
	}
}

// A test that owns a pool recording too reads it once in its session read:
// a call of its path answered through the index of the shared part is
// answered as a read answers it, and the verify mode finds no difference.
func TestATestThatOwnsAPoolRecordingIsAnsweredThroughTheIndexAsFromARead(t *testing.T) {
	defer exactViewVerify.Store(exactViewVerify.Load())
	exactViewVerify.Store(true)
	p, mm, poolNames := sharedPoolRun(t, 4, 1, 1)
	h := httpint.New(zap.NewNop())
	mm.ServeExactTest("test-0", []*models.Mock{roundMock(mm, "mock-own-0-0"), roundMock(mm, "mock-pool-2")})
	p.ContinueTestErrorCapture()
	for _, c := range []struct{ target, want string }{
		{"/own/0/0", `{"own":"0-0"}`},
		{"/pool/2", `{"pool":2}`},
		{"/pool/1", `{"pool":1}`},
	} {
		if code, body := r3SendVia(t, context.Background(), h, p, mm, models.OutgoingOptions{}, httpGet(c.target)); code != http.StatusOK || body != c.want {
			t.Fatalf("%s got %d %s; want %s", c.target, code, body, c.want)
		}
	}
	mm.ServeExactPool(models.PoolKeyNone, poolNames)
}

// A recording the open test kept for its repeats and is over with (Spend) is
// in no read: a call of it is not answered with it through the index of the
// shared part either.
func TestAnOwnedRecordingATestIsOverWithIsNotAnsweredThroughTheIndex(t *testing.T) {
	for _, verify := range []bool{false, true} {
		t.Run(fmt.Sprint("verify=", verify), func(t *testing.T) {
			defer exactViewVerify.Store(exactViewVerify.Load())
			exactViewVerify.Store(verify)
			p, mm, _ := sharedPoolRun(t, 4, 1, 2)
			h := httpint.New(zap.NewNop())
			mm.Spend("mock-own-0-1")
			mm.ServeExactTest("test-0", []*models.Mock{roundMock(mm, "mock-own-0-0"), roundMock(mm, "mock-own-0-1")})
			p.ContinueTestErrorCapture()
			if code, body := r3SendVia(t, context.Background(), h, p, mm, models.OutgoingOptions{}, httpGet("/own/0/0")); code != http.StatusOK || body != `{"own":"0-0"}` {
				t.Fatalf("the test's own call got %d %s", code, body)
			}
			if code, body := r3SendVia(t, context.Background(), h, p, mm, models.OutgoingOptions{}, httpGet("/own/0/1")); code == http.StatusOK {
				t.Fatalf("a call of a recording the test is over with was answered: %d %s", code, body)
			}
		})
	}
}
