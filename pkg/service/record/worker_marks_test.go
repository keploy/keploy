package record

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml/mapdb"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// jwksRule is a user's rule for the app's JWKS fetch.
var jwksRule = models.WorkerMockRule{ID: "jwks", Kind: "Http", Match: map[string]string{"method": "GET", "path": "/.well-known/jwks.json"}}

func recordedHTTP(name, method, url string, at time.Time) *models.Mock {
	return &models.Mock{Name: name, Kind: models.HTTP, Spec: models.MockSpec{
		HTTPReq:          &models.HTTPReq{Method: models.Method(method), URL: url},
		ReqTimestampMock: at, ResTimestampMock: at.Add(time.Millisecond),
	}}
}

func workerOf(t *testing.T, db *mapdb.MappingDb, set string) []models.MockEntry {
	t.Helper()
	m, err := db.GetMapping(context.Background(), set)
	if err != nil {
		t.Fatal(err)
	}
	if m == nil {
		return nil
	}
	return m.Worker
}

// The first mock a worker-mock rule matches is marked in the set's worker
// scope, with the rule's reason, whether a test consumed it or none did; the
// other workers' recordings of the call are not (one is served to every test
// for the session), nor are the mocks no rule matches. A mark waits while the
// set has no mapping file yet, and is written by the next flush once it has
// one.
func TestAWorkerMockRuleMarksTheFirstMockItMatches(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	ctx := context.Background()
	db := mapdb.New(zap.NewNop(), t.TempDir(), "mappings")
	marks := newWorkerMarks([]models.WorkerMockRule{jwksRule}, db, zap.NewNop())
	at := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)

	marks.add(recordedHTTP("mock-0", "GET", "https://auth.example.com/.well-known/jwks.json", at))                    // before any test: no test owns it
	marks.add(recordedHTTP("mock-1", "GET", "https://api.example.com/orders", at.Add(time.Second)))                   // no rule
	marks.add(recordedHTTP("mock-2", "GET", "https://auth.example.com/.well-known/jwks.json", at.Add(2*time.Second))) // test-1's

	marks.flush(ctx, db, "set-0", false)
	if w := workerOf(t, db, "set-0"); len(w) != 0 {
		t.Fatalf("a set with no mapping file got worker entries %+v", w)
	}

	if err := db.UpsertBatch(ctx, "set-0", map[string][]models.MockEntry{"test-1": {{Name: "mock-1", Kind: "Http"}, {Name: "mock-2", Kind: "Http"}}}); err != nil {
		t.Fatal(err)
	}
	marks.flush(ctx, db, "set-0", false)
	w := workerOf(t, db, "set-0")
	if len(w) != 1 || w[0].Name != "mock-0" {
		t.Fatalf("worker scope %+v; want mock-0 alone, the rule's first match", w)
	}
	for _, e := range w {
		if e.Reason != models.WorkerRuleReason("jwks") || e.Kind != "Http" || e.ReqTimestampMock == "" {
			t.Fatalf("worker entry %+v; want kind, timestamps and the rule's reason", e)
		}
	}
	m, _ := db.GetMapping(ctx, "set-0")
	if got := m.WorkerRuleMockNames(); len(got) != 1 {
		t.Fatalf("the replay would serve %v to every test; want mock-0", got)
	}
	if tests, _, _ := db.Get(ctx, "set-0"); len(tests["test-1"]) != 2 {
		t.Fatalf("test-1's list is %+v; a marked mock stays in its test's list", tests["test-1"])
	}
	if marks.count() != 1 {
		t.Fatalf("count %d, want 1", marks.count())
	}
}

// The marks are written by the mapping stream's consumer right after it writes
// a batch of tests' mappings, so the mapping file exists and nothing else is
// rewriting it then.
func TestTheMappingConsumerWritesTheWorkerMarksAfterEachBatch(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	db := mapdb.New(zap.NewNop(), t.TempDir(), "mappings")
	r := &Recorder{logger: zap.NewNop(), config: &config.Config{}, mappingDb: db}
	marks := newWorkerMarks([]models.WorkerMockRule{jwksRule}, db, zap.NewNop())
	marks.add(recordedHTTP("mock-7", "GET", "http://auth/.well-known/jwks.json", time.Now()))

	var correlationMap, asyncMockIDs, shortPool sync.Map
	var dropped droppedMockSet
	correlationMap.Store("m1", models.MockEntry{Name: "mock-1", Kind: "Http"})
	mappings := make(chan models.TestMockMapping, 1)
	mappings <- models.TestMockMapping{TestName: "test-1", MockIDs: []string{"m1"}}
	close(mappings)
	if err := r.consumeMappingsFor(context.Background(), "set-0", mappings, &correlationMap, &asyncMockIDs, &dropped, nil, nil, &shortPool, nil, marks); err != nil {
		t.Fatal(err)
	}
	if w := workerOf(t, db, "set-0"); len(w) != 1 || w[0].Name != "mock-7" {
		t.Fatalf("worker scope after the consumer's batch %+v; want mock-7", w)
	}
}

// A recording's last flush drops what it cannot write (no mapping file), and
// says so; a lost drain drops the marks too. No rules, or the proven switch,
// mark nothing.
func TestWorkerMarksAtTheEndAndWithoutRules(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	db := mapdb.New(zap.NewNop(), t.TempDir(), "mappings")
	marks := newWorkerMarks([]models.WorkerMockRule{jwksRule}, db, zap.NewNop())
	marks.add(recordedHTTP("mock-0", "GET", "http://auth/.well-known/jwks.json", time.Now()))
	marks.flush(context.Background(), db, "set-0", true)
	marks.mu.Lock()
	left := len(marks.pending)
	marks.mu.Unlock()
	if left != 0 {
		t.Fatalf("%d marks kept after the last flush", left)
	}

	marks = newWorkerMarks([]models.WorkerMockRule{jwksRule}, db, zap.NewNop())
	marks.add(recordedHTTP("mock-1", "GET", "http://auth/.well-known/jwks.json", time.Now()))
	marks.lost("set-0", "drain timed out")
	marks.mu.Lock()
	left = len(marks.pending)
	marks.mu.Unlock()
	if left != 0 {
		t.Fatalf("%d marks kept after the drain was lost", left)
	}

	if newWorkerMarks(nil, db, zap.NewNop()) != nil {
		t.Fatal("marks without rules")
	}
	var none *workerMarks
	none.add(recordedHTTP("m", "GET", "http://a/b", time.Now()))
	none.flush(context.Background(), db, "set-0", true)
	none.lost("set-0", "x")

	t.Setenv(provenswitch.Env, "1")
	if newWorkerMarks([]models.WorkerMockRule{jwksRule}, db, zap.NewNop()) != nil {
		t.Fatal("marks under the proven switch, which places the worker scope itself")
	}
}

// noWorkerStore is a mapping store that cannot add to a set's worker scope.
type noWorkerStore struct{ MappingDb }

// A store that cannot mark mocks in the worker scope gets no marks at all,
// rather than marks that wait for the whole recording; and the marks waiting
// for a mapping file are one per rule, however many mocks match.
func TestWorkerMarksAreBounded(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	if newWorkerMarks([]models.WorkerMockRule{jwksRule}, noWorkerStore{}, zap.NewNop()) != nil {
		t.Fatal("marks for a store that cannot write them")
	}
	db := mapdb.New(zap.NewNop(), t.TempDir(), "mappings")
	marks := newWorkerMarks([]models.WorkerMockRule{jwksRule}, db, zap.NewNop())
	at := time.Now()
	for i := 0; i < 500; i++ {
		marks.add(recordedHTTP("mock-"+strconv.Itoa(i), "GET", "http://auth/.well-known/jwks.json", at))
	}
	marks.mu.Lock()
	n := len(marks.pending)
	marks.mu.Unlock()
	if n != 1 {
		t.Fatalf("%d marks waiting; want 1, the rule's first match", n)
	}
}

// A rule that names several calls marks one mock of each (every table's
// schema read gets its first recording marked); the other workers' copies of
// a call it marked are not.
func TestAWorkerMockRuleMarksOneMockOfEachCallItNames(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	db := mapdb.New(zap.NewNop(), t.TempDir(), "mappings")
	schema := models.WorkerMockRule{ID: "schema", Kind: "Http", Match: map[string]string{"method": "GET", "path": "/schema/*"}}
	marks := newWorkerMarks([]models.WorkerMockRule{schema}, db, zap.NewNop())
	at := time.Now()
	marks.add(recordedHTTP("mock-0", "GET", "http://db-api/schema/users", at))
	marks.add(recordedHTTP("mock-1", "GET", "http://db-api/schema/reports", at.Add(time.Second)))
	marks.add(recordedHTTP("mock-2", "GET", "http://DB-API/schema/users", at.Add(2*time.Second))) // another worker's users read
	marks.add(recordedHTTP("mock-3", "GET", "http://db-api/schema/reports", at.Add(3*time.Second)))
	marks.mu.Lock()
	var names []string
	for _, e := range marks.pending {
		names = append(names, e.Name)
	}
	marks.mu.Unlock()
	if len(names) != 2 || names[0] != "mock-0" || names[1] != "mock-1" {
		t.Fatalf("marked %v; want mock-0 (users) and mock-1 (reports), one of each call", names)
	}
}

// A rule that matches a call per request marks at most maxWorkerMarkCalls
// calls a recording, and says once that it stopped; what it keeps of each
// call is a digest, not the call (a Redis value, an AUTH password).
func TestWorkerMarksStopAtTheirCap(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	core, logs := observer.New(zapcore.WarnLevel)
	rule := models.WorkerMockRule{ID: "sessions", Kind: "Http", Match: map[string]string{"method": "GET", "path": "/sessions/*"}}
	marks := newWorkerMarks([]models.WorkerMockRule{rule}, mapdb.New(zap.NewNop(), t.TempDir(), "mappings"), zap.New(core))
	at := time.Now()
	for i := 0; i < maxWorkerMarkCalls+50; i++ {
		marks.add(recordedHTTP("mock-"+strconv.Itoa(i), "GET", "http://cache/sessions/"+strconv.Itoa(i), at))
	}
	marks.mu.Lock()
	pending, calls := len(marks.pending), len(marks.marked)
	marks.mu.Unlock()
	if pending != maxWorkerMarkCalls || calls != maxWorkerMarkCalls {
		t.Fatalf("%d marks waiting for %d calls; want %d of each", pending, calls, maxWorkerMarkCalls)
	}
	if n := logs.FilterMessageSnippet("later calls are not marked").Len(); n != 1 {
		t.Fatalf("the cap was said %d times; want once", n)
	}
}
