package replay

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// redisRecording is a set an older keploy recorded: a go-redis connection
// (HELLO, then a PING the application makes at boot, a worker-scope call, and
// a GET test-1 made) and a memcached one (a get at boot and one test-1 made).
func redisRecording() (stored []*models.Mock, mappings map[string][]models.MockEntry, worker []models.MockEntry) {
	t0 := time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC)
	generic := func(name, conn string, s int, data string) *models.Mock {
		return &models.Mock{Name: name, Kind: models.GENERIC, Spec: models.MockSpec{
			Metadata:         map[string]string{"connID": conn, "type": "mocks"},
			ReqTimestampMock: t0.Add(time.Duration(s) * time.Second),
			GenericRequests:  []models.Payload{{Origin: models.FromClient, Message: []models.OutputBinary{{Type: models.String, Data: data}}}},
		}}
	}
	stored = []*models.Mock{
		generic("mock-1", "1", 1, "*2\r\n$5\r\nhello\r\n$1\r\n3\r\n"),
		generic("mock-2", "1", 2, "*1\r\n$4\r\nping\r\n"),
		generic("mock-3", "2", 3, "get boot\r\n"),
		generic("mock-4", "1", 4, "*2\r\n$3\r\nget\r\n$1\r\nk\r\n"),
		generic("mock-5", "2", 5, "get k\r\n"),
	}
	mappings = map[string][]models.MockEntry{"test-1": {{Name: "mock-4", Kind: string(models.GENERIC)}, {Name: "mock-5", Kind: string(models.GENERIC)}}}
	worker = []models.MockEntry{{Name: "mock-2", Kind: string(models.GENERIC)}, {Name: "mock-3", Kind: string(models.GENERIC)}}
	return stored, mappings, worker
}

// In a build without a Redis parser, a set's Redis mocks are no test's and
// no worker scope's: the call each recorded is made for real (its connection
// relayed to the real Redis), so under exact serving the set's end does not
// wait for the boot PING, and test-1 owns its memcached get alone. Every
// other mock keeps its place. A build with a Redis parser keeps them all.
func TestARedisMockTheAgentLeavesOutIsNoTestsAndNoWorkerScopes(t *testing.T) {
	stored, mappings, worker := redisRecording()
	// A Keploy Enterprise Redis mock keploy test cannot read (the store
	// skipped its document) is left out too, and a mapping may name it; a
	// document of another kind the store skipped is not the agent's to
	// judge.
	mappings["test-1"] = append(mappings["test-1"], models.MockEntry{Name: "mock-6", Kind: string(models.REDIS)})
	worker = append(worker, models.MockEntry{Name: "mock-7", Kind: "Foo"})
	skipped := map[string]models.Kind{"mock-6": models.REDIS, "mock-7": "Foo"}
	// The agent, which skips Redis, says it left the Redis connection's
	// mocks out; the Enterprise one it was never sent is left out too.
	left := agentLeftOut(models.StoreMocksResp{SkipsRedis: true, LeftOut: []string{"mock-1", "mock-2", "mock-4"}}, skipped)
	if want := map[string]bool{"mock-1": true, "mock-2": true, "mock-4": true, "mock-6": true}; !reflect.DeepEqual(left, want) {
		t.Fatalf("the agent leaves out %v, want %v", left, want)
	}
	kept, keptWorker, unowned := withoutLeftOutRedis(mappings, worker, left)
	if unowned != 2 {
		t.Fatalf("the strip took %d entries out of the per-test mappings, want test-1's Redis GET and Enterprise Redis mock", unowned)
	}
	if got := kept["test-1"]; len(got) != 1 || got[0].Name != "mock-5" {
		t.Fatalf("test-1 owns %v, want its memcached get alone", got)
	}
	if len(keptWorker) != 2 || keptWorker[0].Name != "mock-3" || keptWorker[1].Name != "mock-7" {
		t.Fatalf("the worker scope holds %v, want the memcached boot get and mock-7", keptWorker)
	}
	keptWorker = keptWorker[:1]
	// The plan is of what the agent holds: what it leaves out is not the
	// pool's either.
	p := newExactPlan(kept, keptWorker, withoutNames(stored, left))
	if len(p.pool) != 0 {
		t.Fatalf("the pool is %v, want nothing: every mock is a test's, the worker scope's or left out", p.pool)
	}
	p.bootSeen = true
	p.begin("test-1")
	if got := p.unansweredWorker(map[string]models.MockState{}, false); !reflect.DeepEqual(got, []string{"mock-3"}) {
		t.Fatalf("the set's end waits for %v, want the memcached boot get alone", got)
	}
	if got := p.owned["test-1"]; !reflect.DeepEqual(got, []string{"mock-5"}) {
		t.Fatalf("under exact serving test-1 owns %v, want [mock-5]", got)
	}

	// An agent that does not skip Redis (Keploy Enterprise's) leaves
	// nothing out, the Redis documents this keploy could not read
	// included: keploy test holds its tests to them as recorded.
	if left := agentLeftOut(models.StoreMocksResp{}, skipped); left != nil {
		t.Fatalf("with an agent that does not skip Redis keploy test leaves out %v", left)
	}
	kept, keptWorker, unowned = withoutLeftOutRedis(mappings, worker, nil)
	if unowned != 0 {
		t.Fatalf("with nothing left out the strip took %d entries", unowned)
	}
	if !reflect.DeepEqual(kept, mappings) || !reflect.DeepEqual(keptWorker, worker) {
		t.Fatalf("a build with a Redis parser changed the mappings to %v and the worker scope to %v", kept, keptWorker)
	}
}

// Under the switch, a set whose test cases own no recorded call but Redis
// mocks the agent leaves out says that every call to a dependency other
// than Redis misses, and that nothing is wrong when Redis is all they call.
func TestASetThatOwnsOnlyLeftOutRedisMocksSaysRedisIsRelayed(t *testing.T) {
	core, logs := observer.New(zap.WarnLevel)
	r := &Replayer{logger: zap.New(core), exact: &exactPlan{}}
	r.noteOwnsNothing("test-set-0", 3)
	said := logs.FilterMessageSnippet("no test case of this test set owns a recorded call").All()
	if len(said) != 1 || !strings.Contains(said[0].Message, "other than Redis") ||
		said[0].ContextMap()["redisEntries"] != int64(3) || !strings.Contains(fmt.Sprint(said[0].ContextMap()["next_step"]), "but Redis") {
		t.Fatalf("want one warning that Redis is relayed and how many Redis mocks are left out, got %+v", said)
	}
}

// Under the switch keploy test plans the set from what the agent holds: a
// test case whose mapping names only a Redis mock the agent leaves out owns
// nothing, the set says Redis is relayed, and the pool is the memcached mocks
// alone.
func TestUnderTheSwitchTheExactPlanHoldsNoLeftOutRedisMock(t *testing.T) {
	setSwitch(t, true)
	h := newPartialRunHarness(t, 2, 0)
	pinWindows(h)
	core, logs := observer.New(zap.WarnLevel)
	h.replayer.logger = zap.New(core)
	h.mocks.filtered, _, _ = redisRecording()
	mappingFile(t, h, map[string][]models.MockEntry{"test-1": {{Name: "mock-4", Kind: string(models.GENERIC)}}})
	_ = h.run(t)

	if rep := h.report.report; rep == nil || rep.Success != 2 {
		var failed []string
		if rep != nil {
			for _, r := range rep.Tests {
				if r.Status != models.TestStatusPassed {
					failed = append(failed, fmt.Sprintf("%s %s %v", r.TestCaseID, r.Status, r.FailureInfo.UnconsumedMocks))
				}
			}
		}
		t.Fatalf("both tests must pass, the Redis call test-1 recorded being made for real: failed %v", failed)
	}
	plan := h.replayer.exact
	if plan == nil {
		t.Fatal("precondition: the run made no exact plan")
	}
	if got := strings.Join(plan.pool, ","); got != "mock-3,mock-5" {
		t.Errorf("the exact plan's pool is %s, want the memcached mocks alone (mock-3,mock-5)", got)
	}
	if len(plan.owned["test-1"]) != 0 {
		t.Errorf("test-1 owns %v, want nothing: its mapping names only a Redis mock the agent leaves out", plan.owned["test-1"])
	}
	said := logs.FilterMessageSnippet("no test case of this test set owns a recorded call").All()
	if len(said) != 1 || !strings.Contains(said[0].Message, "other than Redis") {
		t.Fatalf("want one warning that the set's Redis calls are relayed, got %+v", said)
	}
}

// Under the switch a Redis mock the agent leaves out charges no test, not
// even one keploy record's full round marked ambiguous, a mark a replay that
// does not share charges the test that lists it with (annotateConcurrent):
// the call it recorded is made for real, so what the round could not tell of
// its recording answers nothing. The set's concurrency, like its mappings, is
// read without the Redis mocks the agent leaves out: its groups and its
// other entries stay.
func TestALeftOutRedisMockMarkedAmbiguousChargesNoTest(t *testing.T) {
	setSwitch(t, true)
	h := newPartialRunHarness(t, 2, 0)
	pinWindows(h)
	h.mocks.filtered, _, _ = redisRecording()
	db := mappingFile(t, h, map[string][]models.MockEntry{"test-1": {
		{Name: "mock-4", Kind: string(models.GENERIC), Group: "g1", Unresolved: models.MockEntryUnresolvedAmbiguous},
	}})
	// The HELLO (Redis) and the memcached boot get were recorded while both
	// tests were open, and no kept test lists them.
	if err := db.UpsertConcurrency(context.Background(), "test-set-0",
		[]models.ConcurrencyGroup{{ID: "g1", Tests: []string{"test-1", "test-2"}}},
		[]models.MockEntry{{Name: "mock-1", Kind: string(models.GENERIC), Group: "g1"}, {Name: "mock-3", Kind: string(models.GENERIC), Group: "g1"}}); err != nil {
		t.Fatalf("writing the set's concurrency: %v", err)
	}
	_ = h.run(t)

	rep := h.report.report
	if rep == nil || rep.Success != 2 {
		var failed []string
		if rep != nil {
			for _, r := range rep.Tests {
				if r.Status != models.TestStatusPassed {
					failed = append(failed, fmt.Sprintf("%s %s %+v", r.TestCaseID, r.Status, r.FailureInfo.UnmatchedCalls))
				}
			}
		}
		t.Fatalf("both tests must pass, a Redis mock the agent relays charging none: failed %v", failed)
	}
	plan := h.replayer.exact
	if plan == nil || plan.share == nil {
		t.Fatal("precondition: the run made no share plan, so the set's concurrency was never read")
	}
	for _, n := range []string{"mock-4", "mock-1"} {
		if _, ok := plan.share.conc.entries[n]; ok {
			t.Errorf("the set's concurrency holds %s, a Redis mock the agent leaves out", n)
		}
	}
	if _, ok := plan.share.conc.entries["mock-3"]; !ok {
		t.Errorf("the set's concurrency lost mock-3, the shared memcached get: only what the agent leaves out is stripped")
	}
	if _, ok := plan.share.conc.groups["g1"]; !ok {
		t.Errorf("the set's concurrency lost group g1")
	}
}

// The set's concurrency without what the agent leaves out: grouped, marked
// and shared entries naming a left-out mock go, the rest and the groups
// stay; a set left with nothing states no concurrency (nil); with nothing
// left out it is returned as it is.
func TestWithoutLeftOutConcurrency(t *testing.T) {
	tests := map[string][]models.MockEntry{
		"test-1": {{Name: "r1", Group: "g1"}, {Name: "m1", Group: "g1"}, {Name: "r2", Unresolved: models.MockEntryUnresolvedUnproven}},
	}
	shared := []models.MockEntry{{Name: "r3"}, {Name: "m2"}}
	conc := concurrencyOf([]models.ConcurrencyGroup{{ID: "g1", Tests: []string{"test-1", "test-2"}}}, tests, shared)
	left := map[string]bool{"r1": true, "r2": true, "r3": true}
	got := withoutLeftOutConcurrency(conc, left)
	var names []string
	for n := range got.entries {
		names = append(names, n)
	}
	sort.Strings(names)
	if strings.Join(names, ",") != "m1,m2" {
		t.Fatalf("entries %v, want m1,m2", names)
	}
	if _, ok := got.groups["g1"]; !ok || len(got.ofTest["test-2"]) != 1 {
		t.Fatalf("the groups changed: %+v %+v", got.groups, got.ofTest)
	}
	if len(conc.entries) != 5 {
		t.Fatalf("the set's concurrency as read was changed: %d entries, want 5", len(conc.entries))
	}
	if same := withoutLeftOutConcurrency(conc, nil); same != conc {
		t.Fatal("with nothing left out the concurrency is not returned as it is")
	}
	onlyRedis := concurrencyOf(nil, map[string][]models.MockEntry{"test-1": {{Name: "r2", Unresolved: models.MockEntryUnresolvedAmbiguous}}}, nil)
	if onlyRedis == nil {
		t.Fatal("precondition: a marked entry is concurrency")
	}
	if got := withoutLeftOutConcurrency(onlyRedis, left); got != nil {
		t.Fatalf("a set whose only concurrency is left out states %+v, want none (nil)", got)
	}
}

// --update-test-mapping writes each passing test's entries from what the run
// consumed, and keeps the entries of the Redis mocks the agent left out of
// the replay, as it keeps connection failures': no run of a build that skips Redis
// consumes one, and a keploy with a Redis parser replays them.
func TestRunTestSetKeepsTheLeftOutRedisMocksMappings(t *testing.T) {
	h := newPartialRunHarness(t, 2, 0)
	h.mocks.filtered, _, _ = redisRecording()
	h.replayer.hookImpl = prHooks{consumed: []models.MockState{{Name: "mock-5", Kind: models.GENERIC}}}
	h.replayer.config.Test.UpdateTestMapping = true
	h.mappings.exists = true
	h.mappings.onDisk = map[string][]models.MockEntry{
		"test-1": {{Name: "mock-4", Kind: string(models.GENERIC)}, {Name: "mock-5", Kind: string(models.GENERIC)}},
		"test-2": {{Name: "mock-5", Kind: string(models.GENERIC)}},
	}
	if status := h.run(t); status != models.TestSetStatusPassed {
		t.Fatalf("precondition: the run should pass, got %q", status)
	}
	if h.mappings.last == nil {
		t.Fatal("precondition: --update-test-mapping wrote no mapping")
	}
	written := map[string][]string{}
	for _, tc := range h.mappings.last.TestCases {
		for _, m := range tc.Mocks {
			written[tc.ID] = append(written[tc.ID], m.Name)
		}
	}
	if got := strings.Join(written["test-1"], ","); got != "mock-5,mock-4" {
		t.Fatalf("test-1 is written with %q, want its consumed memcached get and the Redis GET it recorded (mock-5,mock-4)", got)
	}
}

// --remove-unused-mocks keeps the set's Redis mocks the agent left out of
// the replay: they went unconsumed because their calls were made for real,
// so they say nothing about what the application still needs. A Keploy
// Enterprise Redis mock read from a gob mock file has no config type the
// prune would keep it by, and Keploy Enterprise replays it. What the run did
// not consume otherwise (the memcached boot get) is pruned as before.
func TestRunTestSetKeepsTheRedisMocksItLeftOutThroughThePrune(t *testing.T) {
	h := newPartialRunHarness(t, 2, 0)
	stored, _, _ := redisRecording()
	enterprise := &models.Mock{Name: "mock-6", Kind: models.REDIS, Spec: models.MockSpec{
		Metadata:         map[string]string{"connID": "3"},
		ReqTimestampMock: stored[4].Spec.ReqTimestampMock,
		GenericRequests:  []models.Payload{{Origin: models.FromClient, Message: []models.OutputBinary{{Type: models.String, Data: "*2\r\n$3\r\nGET\r\n$1\r\nk\r\n"}}}},
	}}
	h.mocks.filtered = append(stored, enterprise)
	h.replayer.config.Test.RemoveUnusedMocks = true
	_ = h.run(t)

	if rep := h.report.report; rep == nil || rep.Success != 2 {
		t.Fatalf("precondition: want both tests to pass, got %+v", rep)
	}
	if got := h.mocks.pruneCalls(); got != 1 {
		t.Fatalf("precondition: the run made %d UpdateMocks calls, want 1", got)
	}
	for _, n := range []string{"mock-1", "mock-2", "mock-4", "mock-6"} {
		if _, ok := h.mocks.kept[n]; !ok {
			t.Errorf("the prune keeps %v, which lacks %s, a Redis mock the agent left out of the replay", stateNames(h.mocks.kept), n)
		}
	}
	if _, ok := h.mocks.kept["mock-3"]; ok {
		t.Errorf("the prune keeps mock-3, the memcached boot get no test consumed")
	}
}

func stateNames(m map[string]models.MockState) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// The agent loads a test's mocks by its mapping as recorded: a test whose
// mapping names only a Redis mock the agent leaves out is loaded by that
// mapping (the agent leaves the mock out itself), not by its window, which a
// stripped, empty mapping would have it loaded by.
func TestTheAgentLoadsATestByItsMappingAsRecorded(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprintf("streaming=%v", streaming), func(t *testing.T) {
			h := newPartialRunHarness(t, 2, 0)
			if streaming {
				h.streaming("test-1")
			}
			h.mocks.filtered, _, _ = redisRecording()
			h.replayer.hookImpl = prHooks{consumed: []models.MockState{{Name: "mock-5", Kind: models.GENERIC}}}
			h.mappings.exists = true
			h.mappings.onDisk = map[string][]models.MockEntry{
				"test-1": {{Name: "mock-4", Kind: string(models.GENERIC)}},
				"test-2": {{Name: "mock-5", Kind: string(models.GENERIC)}},
			}
			_ = h.run(t)
			h.instr.mu.Lock()
			defer h.instr.mu.Unlock()
			var got [][]string
			sent := false
			for _, p := range h.instr.allParams {
				got = append(got, p.MockMapping)
				for _, n := range p.MockMapping {
					sent = sent || n == "mock-4"
				}
			}
			if !sent {
				t.Fatalf("no send named mock-4, test-1's recorded mapping (sent %v): the agent loads test-1 by its window", got)
			}
		})
	}
}

// keploy test takes what the agent leaves out from the agent's answer to the
// store, never from its own build: a keploy test on macOS or Windows, which
// links no Redis parser, drives Keploy Enterprise's agent in a Linux
// container, which records and mocks Redis and leaves nothing out. Its tests
// then keep the Redis mocks they recorded, under the switch too, where the
// exact plan has test-1 own its Redis GET: had keploy test stripped it while
// the agent serves it, test-1 would consume a mock it is not held to, a
// false red. An agent that skips Redis says so, and its Redis mocks are no
// test's.
func TestKeployTestLeavesOutWhatTheAgentSaysItLeftOut(t *testing.T) {
	if !integrations.SkipsRedis() {
		t.Fatal("precondition: this test binary must skip Redis, as a keploy test on macOS that links no Redis parser does")
	}
	for _, tc := range []struct {
		name   string
		answer *models.StoreMocksResp
		owns   string
		pool   string
	}{
		{"an agent that records and mocks Redis", &models.StoreMocksResp{IsSuccess: true}, "mock-4,mock-5", "mock-1,mock-2,mock-3"},
		{"an agent that skips Redis", &models.StoreMocksResp{IsSuccess: true, SkipsRedis: true, LeftOut: []string{"mock-1", "mock-2", "mock-4"}}, "mock-5", "mock-3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setSwitch(t, true)
			h := newPartialRunHarness(t, 2, 0)
			pinWindows(h)
			h.instr.leftOutAnswer = tc.answer
			h.mocks.filtered, _, _ = redisRecording()
			mappingFile(t, h, map[string][]models.MockEntry{"test-1": {
				{Name: "mock-4", Kind: string(models.GENERIC)}, {Name: "mock-5", Kind: string(models.GENERIC)},
			}})
			_ = h.run(t)
			plan := h.replayer.exact
			if plan == nil {
				t.Fatal("precondition: the run made no exact plan")
			}
			if got := strings.Join(plan.owned["test-1"], ","); got != tc.owns {
				t.Errorf("test-1 owns %s, want %s", got, tc.owns)
			}
			if got := strings.Join(plan.pool, ","); got != tc.pool {
				t.Errorf("the exact plan's pool is %s, want %s", got, tc.pool)
			}
		})
	}
}

// With the switch off, what keploy test keeps of the set's Redis mocks it
// takes from the agent's answer too. An agent that records Redis (Keploy
// Enterprise's, in a Linux container under a keploy test on macOS) left
// nothing out, so a Redis mock no test consumed is unused like any other:
// --update-test-mapping writes test-1 with what it consumed alone, and
// --remove-unused-mocks prunes the Redis mocks nothing consumed. An agent
// that skips Redis keeps them, as TestRunTestSetKeepsTheLeftOutRedisMocks-
// Mappings and TestRunTestSetKeepsTheRedisMocksItLeftOutThroughThePrune
// show.
func TestWithTheSwitchOffKeployTestKeepsOnlyWhatTheAgentLeftOut(t *testing.T) {
	t.Run("--update-test-mapping", func(t *testing.T) {
		h := newPartialRunHarness(t, 2, 0)
		h.instr.leftOutAnswer = &models.StoreMocksResp{IsSuccess: true}
		h.mocks.filtered, _, _ = redisRecording()
		h.replayer.hookImpl = prHooks{consumed: []models.MockState{{Name: "mock-5", Kind: models.GENERIC}}}
		h.replayer.config.Test.UpdateTestMapping = true
		h.mappings.exists = true
		h.mappings.onDisk = map[string][]models.MockEntry{
			"test-1": {{Name: "mock-4", Kind: string(models.GENERIC)}, {Name: "mock-5", Kind: string(models.GENERIC)}},
			"test-2": {{Name: "mock-5", Kind: string(models.GENERIC)}},
		}
		if status := h.run(t); status != models.TestSetStatusPassed {
			t.Fatalf("precondition: the run should pass, got %q", status)
		}
		if h.mappings.last == nil {
			t.Fatal("precondition: --update-test-mapping wrote no mapping")
		}
		for _, tc := range h.mappings.last.TestCases {
			if tc.ID != "test-1" {
				continue
			}
			var names []string
			for _, m := range tc.Mocks {
				names = append(names, m.Name)
			}
			if got := strings.Join(names, ","); got != "mock-5" {
				t.Fatalf("test-1 is written with %q, want what it consumed alone (mock-5): the agent left no Redis mock out", got)
			}
		}
	})
	t.Run("--remove-unused-mocks", func(t *testing.T) {
		h := newPartialRunHarness(t, 2, 0)
		h.instr.leftOutAnswer = &models.StoreMocksResp{IsSuccess: true}
		h.mocks.filtered, _, _ = redisRecording()
		h.replayer.config.Test.RemoveUnusedMocks = true
		_ = h.run(t)
		if got := h.mocks.pruneCalls(); got != 1 {
			t.Fatalf("precondition: the run made %d UpdateMocks calls, want 1", got)
		}
		for _, n := range []string{"mock-2", "mock-4"} {
			if _, ok := h.mocks.kept[n]; ok {
				t.Errorf("the prune keeps %s, a Redis mock nothing consumed, though the agent left none out", n)
			}
		}
	})
}
