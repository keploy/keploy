package replay

import (
	"context"
	"encoding/gob"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"go.keploy.io/server/v3/pkg/agent/routes"
	"go.keploy.io/server/v3/pkg/models"
	httpclient "go.keploy.io/server/v3/pkg/platform/http"
	agentsvc "go.keploy.io/server/v3/pkg/service/agent"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// Nothing consumes a connection failure at replay yet, so the per-test
// dependency assertion must leave the kind out of BOTH sides, as it does DNS.
// Left in the expected side alone, every test that maps one demotes to
// OBSOLETE ("mock mapping mismatch ... Re-record"), which is what a released
// keploy (v3.6.107) does on meeting one; left in the consumed side alone, a
// test that did consume one (a later keploy that replays them) would be
// flagged for consuming a mock it was not expected to.

// With the kind excluded, a test that maps a connection failure gets exactly
// the verdict it gets without one, whatever the response did and whatever
// the knobs say.
func TestAnUnconsumedConnectionFailureLeavesTheVerdictAsIfItWereAbsent(t *testing.T) {
	http := models.MockEntry{Name: "mock-1", Kind: string(models.HTTP)}
	cf := models.MockEntry{Name: "mock-2", Kind: string(models.ConnectionFailure)}
	consumedSet := []models.MockState{consumed("mock-1", models.HTTP)}

	mismatch := func(expected []models.MockEntry, kinds map[string]models.Kind) bool {
		var exp []string
		for _, m := range eligibleExpectedEntries(expected, kinds, nil) {
			exp = append(exp, m.Name)
		}
		var got []string
		for _, m := range consumedSet {
			if !consumedOutsideAssertion(m) {
				got = append(got, m.Name)
			}
		}
		return !isMockSubset(got, exp)
	}

	for _, tc := range []struct {
		name  string
		entry models.MockEntry
		kinds map[string]models.Kind
	}{
		{"the mapping names the kind", cf, nil},
		// The kind recovered from the loaded mock, for a mapping written
		// without kinds.
		{"the loaded mock names the kind", models.MockEntry{Name: "mock-2"}, map[string]models.Kind{"mock-2": models.ConnectionFailure}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if mismatch([]models.MockEntry{http, tc.entry}, tc.kinds) {
				t.Fatal("an unconsumed connection failure made the mock set diverge")
			}
			for _, responseMatched := range []bool{true, false} {
				for _, assertDeps := range []bool{true, false} {
					with := resolveTestOutcome(responseMatched, mismatch([]models.MockEntry{http, tc.entry}, tc.kinds), false, assertDeps, false)
					without := resolveTestOutcome(responseMatched, mismatch([]models.MockEntry{http}, nil), false, assertDeps, false)
					if with != without {
						t.Fatalf("response matched=%v, --assert-dependencies=%v: %+v with the connection failure, %+v without",
							responseMatched, assertDeps, with, without)
					}
				}
			}
			dep := buildDepResults([]models.MockEntry{http, tc.entry}, consumedSet, true, nil, tc.kinds, nil)
			if len(dep.Rows) != 0 || dep.Consumed != 1 || !dep.Checked {
				t.Fatalf("dependency rows = %+v; want none missing, one consumed, checked", dep)
			}
		})
	}

	// Control: an unconsumed per-test HTTP mock in the same place does diverge.
	if !mismatch([]models.MockEntry{http, {Name: "mock-3", Kind: string(models.HTTP)}}, nil) {
		t.Fatal("control: a missing per-test HTTP mock must diverge")
	}
}

// The consumed side, for when a keploy does consume one: it is neither a
// mismatch on the streaming path nor counted as a consumed dependency.
func TestAConsumedConnectionFailureIsOutsideTheAssertion(t *testing.T) {
	cf := consumed("mock-2", models.ConnectionFailure)
	if !consumedOutsideAssertion(cf) {
		t.Fatal("a consumed connection failure must be outside the assertion")
	}
	if consumedOutsideAssertion(consumed("mock-1", models.HTTP)) {
		t.Fatal("control: a consumed per-test HTTP mock is inside it")
	}
	if !isMockSubsetWithConfig([]models.MockState{consumed("mock-1", models.HTTP), cf}, []string{"mock-1"}) {
		t.Fatal("streaming path: a consumed connection failure the mapping does not name must not be a mismatch")
	}
	dep := buildDepResults([]models.MockEntry{{Name: "mock-1", Kind: "Http"}}, []models.MockState{consumed("mock-1", models.HTTP), cf}, true, nil, nil, nil)
	if dep.Consumed != 1 {
		t.Fatalf("consumed dependencies = %d, want 1: a connection failure is not one", dep.Consumed)
	}
}

func TestMismatchReportsLeaveConnectionFailuresOut(t *testing.T) {
	expected := buildExpectedMockInfos([]models.MockEntry{
		{Name: "mock-1", Kind: "Http"},
		{Name: "mock-2", Kind: "ConnectionFailure"},
		{Name: "mock-3"},
	}, map[string]models.Kind{"mock-3": models.ConnectionFailure})
	assert.Equal(t, []models.MockMismatchMock{{Name: "mock-1", Kind: "Http"}}, expected)

	actual := buildActualMockInfos([]models.MockState{consumed("mock-1", models.HTTP), consumed("mock-2", models.ConnectionFailure)}, true)
	assert.Equal(t, []models.MockMismatchMock{{Name: "mock-1", Kind: "Http"}}, actual)
}

// A mutator that re-tags a connection failure reusable must not move it to
// the reusable pool, which every test reads.
func TestRebalanceKeepsConnectionFailuresPerTest(t *testing.T) {
	cf := &models.Mock{Name: "cf", Kind: models.ConnectionFailure, Spec: models.MockSpec{Metadata: map[string]string{"type": "config"}}}
	cf.TestModeInfo.Lifetime = models.LifetimeSession
	f, u := rebalanceReusableMocks([]*models.Mock{perTest("pt-1"), cf, configTagged("promoted")}, nil)
	if got := names(f); !eq(got, []string{"pt-1", "cf"}) {
		t.Fatalf("filtered pool = %v, want [pt-1 cf]", got)
	}
	if got := names(u); !eq(got, []string{"promoted"}) {
		t.Fatalf("unfiltered pool = %v, want [promoted] (the control is moved)", got)
	}
}

// Every consumed-side filter of the assertion goes through
// consumedOutsideAssertion, the twin of eligibleExpectedEntries. Written out
// again at any one site, the two sides can exclude different kinds, which is
// the one-sided exclusion the comment at the top of this file is about.
func TestEveryConsumedSideFilterUsesTheSharedPredicate(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse package: %v", err)
	}
	funcs := map[string]*ast.FuncDecl{}
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			for _, d := range file.Decls {
				if fn, ok := d.(*ast.FuncDecl); ok {
					funcs[fn.Name.Name] = fn
				}
			}
		}
	}
	for _, name := range []string{"buildDepResults", "isMockSubsetWithConfig"} {
		fn := funcs[name]
		if fn == nil {
			t.Fatalf("%s not found", name)
		}
		if len(callArgIdents(fn, "consumedOutsideAssertion")) == 0 {
			t.Errorf("%s does not filter the consumed mocks through consumedOutsideAssertion", name)
		}
	}

	// In RunTestSet: the loop that builds filteredMockNames.
	_, run := runTestSetSource(t)
	found := false
	ast.Inspect(run, func(n ast.Node) bool {
		loop, ok := n.(*ast.RangeStmt)
		if !ok {
			return true
		}
		// Only the loop that appends itself, not the loops around it.
		builds := false
		ast.Inspect(loop.Body, func(m ast.Node) bool {
			switch m.(type) {
			case *ast.RangeStmt, *ast.ForStmt, *ast.FuncLit:
				return false
			}
			assign, ok := m.(*ast.AssignStmt)
			if !ok || len(assign.Lhs) != 1 {
				return true
			}
			ident, ok := assign.Lhs[0].(*ast.Ident)
			if !ok || ident.Name != "filteredMockNames" || len(assign.Rhs) != 1 {
				return true
			}
			if call, ok := assign.Rhs[0].(*ast.CallExpr); ok && calleeName(call) == "append" {
				builds = true
			}
			return true
		})
		if !builds {
			return true
		}
		found = true
		uses := false
		ast.Inspect(loop.Body, func(m ast.Node) bool {
			if call, ok := m.(*ast.CallExpr); ok && calleeName(call) == "consumedOutsideAssertion" {
				uses = true
			}
			return true
		})
		if !uses {
			t.Errorf("RunTestSet builds filteredMockNames without consumedOutsideAssertion")
		}
		return true
	})
	if !found {
		t.Fatal("the loop that builds filteredMockNames was not found in RunTestSet")
	}
}

// prSkippingMockDB is a mock store that reads every pool in one pass
// (pkg.TestSetMocksReader) and whose decoders skipped the named documents.
type prSkippingMockDB struct {
	prMockDB
	skipped map[string]models.Kind
}

func (m *prSkippingMockDB) GetTestSetMocks(context.Context, string, time.Time, time.Time, map[string]bool, map[string]bool) (models.TestSetMocks, error) {
	return models.TestSetMocks{Filtered: m.filtered, AllPerTest: m.filtered, Skipped: m.skipped}, nil
}

// This keploy never consumes a connection failure, so a mapping write built
// from what it consumed would strip them from the recording for good; it keeps
// the ones the file lists for each test it rewrites, by the entry's kind or,
// for an entry recorded without one, by the kind of the document it names. Such
// a kind-less entry naming a connection failure the decoders skipped is also
// left out of the test's expected side, as one with the kind is.
func TestRunTestSetKeepsTheMappingsConnectionFailures(t *testing.T) {
	session := []models.MockState{{Name: "mock-session", Kind: models.HTTP, Lifetime: models.LifetimeSession}}
	h := newPartialRunHarness(t, 2, 0)
	h.replayer.mockDB = &prSkippingMockDB{skipped: map[string]models.Kind{"cf-kindless": models.ConnectionFailure, "mock-acme": "Acme-Queue"}}
	h.replayer.hookImpl = prHooks{consumed: session}
	h.replayer.config.Test.UpdateTestMapping = true
	h.mappings.exists = true
	h.mappings.onDisk = map[string][]models.MockEntry{
		"test-1": {
			{Name: "mock-session", Kind: string(models.HTTP)},
			{Name: "cf-1", Kind: string(models.ConnectionFailure)},
			{Name: "cf-kindless"},
			{Name: "mock-acme"},
			{Name: "mock-gone", Kind: string(models.HTTP)},
		},
		"test-2": {{Name: "mock-session", Kind: string(models.HTTP)}},
	}
	// The run's boot consumed mock-session, so the write replaces the
	// startup section too; the boot-time connection failure stays.
	h.mappings.onDiskStartup = []models.MockEntry{
		{Name: "cf-boot", Kind: string(models.ConnectionFailure)},
		{Name: "mock-old-boot", Kind: string(models.HTTP)},
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
	got := strings.Join(written["test-1"], ",")
	for _, want := range []string{"mock-session", "cf-1", "cf-kindless"} {
		if !strings.Contains(","+got+",", ","+want+",") {
			t.Fatalf("test-1 is written with %q; want it to keep %s", got, want)
		}
	}
	for _, gone := range []string{"mock-gone", "mock-acme"} {
		if strings.Contains(","+got+",", ","+gone+",") {
			t.Fatalf("test-1 is written with %q; only connection failures are carried forward, not %s", got, gone)
		}
	}
	var startup []string
	for _, m := range h.mappings.last.Startup {
		startup = append(startup, m.Name)
	}
	if s := "," + strings.Join(startup, ",") + ","; !strings.Contains(s, ",mock-session,") || !strings.Contains(s, ",cf-boot,") || strings.Contains(s, ",mock-old-boot,") {
		t.Fatalf("the startup section is written as %v; want the boot this run consumed (mock-session) and the boot-time connection failure (cf-boot), not mock-old-boot", startup)
	}
	if h.mappings.getCalls != 2 {
		t.Fatalf("precondition: the mapping file is read once for the run and once more to keep its connection failures, got %d reads", h.mappings.getCalls)
	}

	var expected []string
	for _, r := range h.report.results {
		if r.TestCaseID == "test-1" && r.MockMismatches != nil {
			for _, m := range r.MockMismatches.ExpectedMocks {
				expected = append(expected, m.Name)
			}
		}
	}
	if len(expected) == 0 {
		t.Fatal("precondition: test-1's result lists no expected mocks")
	}
	for _, e := range expected {
		if e == "cf-1" || e == "cf-kindless" {
			t.Fatalf("test-1's expected side %v holds the connection failure %s", expected, e)
		}
	}
	// A skipped document of another kind keeps the report it had: listed,
	// with no kind looked up for it.
	for _, r := range h.report.results {
		if r.TestCaseID != "test-1" || r.MockMismatches == nil {
			continue
		}
		for _, m := range r.MockMismatches.ExpectedMocks {
			if m.Name == "mock-acme" && m.Kind != "" {
				t.Fatalf("the kind-less entry for the skipped mock-acme is reported as %q; other skipped kinds keep their reports", m.Kind)
			}
		}
	}
	if !strings.Contains(","+strings.Join(expected, ",")+",", ",mock-acme,") {
		t.Fatalf("precondition: test-1's expected side %v lists mock-acme", expected)
	}
}

// A connection failure this keploy decodes is never sent to the agent, but
// the replay learns its kind first, from every mock the set holds: a set whose
// only connection failures decode holds them, a kind-less mapping entry naming
// one (recorded before entries had kinds) is kept off the test's expected side,
// and --update-test-mapping carries it forward, in a test's entries and in the
// startup section, as it does one whose entry gives its kind.
func TestRunTestSetKeepsTheMappingsDecodedConnectionFailures(t *testing.T) {
	at := time.Now().Add(-time.Hour)
	session := []models.MockState{{Name: "mock-session", Kind: models.HTTP, Lifetime: models.LifetimeSession}}
	h := newPartialRunHarness(t, 2, 0)
	h.replayer.mockDB = &prSkippingMockDB{prMockDB: prMockDB{filtered: []*models.Mock{
		cfAt("cf-1", at), cfAt("cf-kindless", at.Add(time.Second)), cfAt("cf-boot", at.Add(2*time.Second)),
	}}}
	h.replayer.hookImpl = prHooks{consumed: session}
	h.replayer.config.Test.UpdateTestMapping = true
	h.mappings.exists = true
	h.mappings.onDisk = map[string][]models.MockEntry{
		"test-1": {
			{Name: "mock-session", Kind: string(models.HTTP)},
			{Name: "cf-1", Kind: string(models.ConnectionFailure)},
			{Name: "cf-kindless"},
			{Name: "mock-gone", Kind: string(models.HTTP)},
		},
		"test-2": {{Name: "mock-session", Kind: string(models.HTTP)}},
	}
	h.mappings.onDiskStartup = []models.MockEntry{{Name: "cf-boot"}, {Name: "mock-old-boot", Kind: string(models.HTTP)}}
	if status := h.run(t); status != models.TestSetStatusPassed {
		t.Fatalf("precondition: the run should pass, got %q", status)
	}

	h.instr.mu.Lock()
	stored := append(append([]*models.Mock(nil), h.instr.storedFiltered...), h.instr.storedUnfiltered...)
	h.instr.mu.Unlock()
	for _, m := range stored {
		if m.Kind == models.ConnectionFailure {
			t.Fatalf("the agent was sent the connection failure %s", m.Name)
		}
	}
	if h.mappings.last == nil {
		t.Fatal("precondition: --update-test-mapping wrote no mapping")
	}
	if h.mappings.getCalls != 2 {
		t.Fatalf("a set whose connection failures all decode holds them: want the mapping file read again to keep them, got %d reads", h.mappings.getCalls)
	}
	var test1 []string
	for _, tc := range h.mappings.last.TestCases {
		if tc.ID == "test-1" {
			for _, m := range tc.Mocks {
				test1 = append(test1, m.Name)
			}
		}
	}
	if got := "," + strings.Join(test1, ",") + ","; !strings.Contains(got, ",cf-1,") || !strings.Contains(got, ",cf-kindless,") || strings.Contains(got, ",mock-gone,") {
		t.Fatalf("test-1 is written with %v; want its connection failures (cf-1, and cf-kindless by the kind of the mock it names) kept, and mock-gone not", test1)
	}
	var startup []string
	for _, m := range h.mappings.last.Startup {
		startup = append(startup, m.Name)
	}
	if s := "," + strings.Join(startup, ",") + ","; !strings.Contains(s, ",mock-session,") || !strings.Contains(s, ",cf-boot,") || strings.Contains(s, ",mock-old-boot,") {
		t.Fatalf("the startup section is written as %v; want mock-session and the kind-less cf-boot, not mock-old-boot", startup)
	}
	var expected []string
	for _, r := range h.report.results {
		if r.TestCaseID == "test-1" && r.MockMismatches != nil {
			for _, m := range r.MockMismatches.ExpectedMocks {
				expected = append(expected, m.Name)
			}
		}
	}
	if !slices.Contains(expected, "mock-gone") {
		t.Fatalf("precondition: test-1's expected side %v lists mock-gone", expected)
	}
	if slices.Contains(expected, "cf-kindless") || slices.Contains(expected, "cf-1") {
		t.Fatalf("test-1's expected side %v holds a connection failure", expected)
	}
}

// prNeedsMockDB records the maps the replay reads the set's mocks with.
type prNeedsMockDB struct {
	prSkippingMockDB
	mapped, needed map[string]bool
}

func (m *prNeedsMockDB) GetTestSetMocks(ctx context.Context, id string, after, before time.Time, mapped, needed map[string]bool) (models.TestSetMocks, error) {
	m.mu.Lock()
	m.mapped, m.needed = maps.Clone(mapped), maps.Clone(needed)
	m.mu.Unlock()
	return m.prSkippingMockDB.GetTestSetMocks(ctx, id, after, before, mapped, needed)
}

// The replay reads, of the mocks a set's mapping names, those of the tests it
// runs and those of the set's boot (the startup section), which every test
// needs: a run of some of the tests needs theirs and the boot's, a run of all
// of them every one. One loader reads the mocks for the compose and the native
// path alike.
func TestRunTestSetNeedsTheMocksOfTheTestsItRunsAndOfTheBoot(t *testing.T) {
	for _, tc := range []struct {
		name     string
		selected []string
		want     []string
	}{
		{"some tests", []string{"test-1"}, []string{"mock-1", "mock-boot"}},
		{"all tests", nil, []string{"mock-1", "mock-2", "mock-boot"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newPartialRunHarness(t, 2, 0)
			db := &prNeedsMockDB{}
			h.replayer.mockDB = db
			h.mappings.exists = true
			h.mappings.onDisk = map[string][]models.MockEntry{
				"test-1": {{Name: "mock-1", Kind: string(models.HTTP)}},
				"test-2": {{Name: "mock-2", Kind: string(models.HTTP)}},
			}
			h.mappings.onDiskStartup = []models.MockEntry{{Name: "mock-boot", Kind: string(models.HTTP)}}
			if tc.selected != nil {
				h.replayer.config.Test.SelectedTests = map[string][]string{"test-set-0": tc.selected}
			}
			h.run(t)
			db.mu.Lock()
			defer db.mu.Unlock()
			if got := slices.Sorted(maps.Keys(db.mapped)); !slices.Equal(got, []string{"mock-1", "mock-2", "mock-boot"}) {
				t.Fatalf("the mocks the mapping names: got %v", got)
			}
			if got := slices.Sorted(maps.Keys(db.needed)); !slices.Equal(got, tc.want) {
				t.Fatalf("the mocks the run needs: got %v, want %v", got, tc.want)
			}
		})
	}
}

// A mapping write with no startup section (the run's boot consumed nothing)
// leaves the one on disk as it is: mapdb.Insert replaces it only when the write
// has one, so the write must not get one made of its connection failures alone.
func TestKeepConnFailureMappingsLeavesTheStartupSectionToAWriteWithOne(t *testing.T) {
	kinds := map[string]models.Kind{"cf-boot": models.ConnectionFailure, "cf-1": models.ConnectionFailure}
	onDiskStartup := []models.MockEntry{{Name: "cf-boot"}, {Name: "mock-boot", Kind: string(models.HTTP)}}
	write := &models.Mapping{TestCases: []models.MappedTestCase{{ID: "test-1", Mocks: []models.MockEntry{{Name: "mock-1", Kind: string(models.HTTP)}}}}}
	keepConnFailureMappings(write, map[string][]models.MockEntry{"test-1": {{Name: "cf-1"}}}, onDiskStartup, kinds)
	if len(write.Startup) != 0 {
		t.Fatalf("a write with no startup section got %v, which would replace the one on disk (cf-boot and mock-boot)", write.Startup)
	}
	if len(write.TestCases[0].Mocks) != 2 {
		t.Fatalf("precondition: test-1 keeps its connection failure, got %v", write.TestCases[0].Mocks)
	}
	write.Startup = []models.MockEntry{{Name: "mock-boot-now", Kind: string(models.HTTP)}}
	keepConnFailureMappings(write, nil, onDiskStartup, kinds)
	if len(write.Startup) != 2 || write.Startup[1].Name != "cf-boot" {
		t.Fatalf("a write with a startup section keeps the boot-time connection failure: got %v", write.Startup)
	}
}

// A set with no connection failure has no mapping entries for them to keep:
// its mapping file is read once, for the run, as before, and written from what
// the run consumed alone.
func TestRunTestSetReadsTheMappingFileOnceWithoutConnectionFailures(t *testing.T) {
	session := []models.MockState{{Name: "mock-session", Kind: models.HTTP, Lifetime: models.LifetimeSession}}
	h := newPartialRunHarness(t, 2, 0)
	h.replayer.hookImpl = prHooks{consumed: session}
	h.replayer.config.Test.UpdateTestMapping = true
	h.mappings.exists = true
	h.mappings.onDisk = map[string][]models.MockEntry{"test-1": {{Name: "mock-session", Kind: string(models.HTTP)}, {Name: "mock-gone", Kind: string(models.HTTP)}}}
	if status := h.run(t); status != models.TestSetStatusPassed {
		t.Fatalf("precondition: the run should pass, got %q", status)
	}
	if h.mappings.last == nil {
		t.Fatal("precondition: --update-test-mapping wrote no mapping")
	}
	if h.mappings.getCalls != 1 {
		t.Fatalf("the mapping file was read %d times; a set without connection failures reads it once", h.mappings.getCalls)
	}
}

// composeAgent is an agent service whose loaded count is what it was sent, as
// the real agent's is (pkg/service/agent).
type composeAgent struct {
	agentsvc.Service
	mu     sync.Mutex
	loaded int
}

func (s *composeAgent) StoreMocks(_ context.Context, f, u []*models.Mock) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loaded = len(f) + len(u)
	return nil
}

func (s *composeAgent) StoreMocksStream(_ context.Context, h models.MockStreamHeader, dec *gob.Decoder) error {
	total := h.FilteredCount + h.UnfilteredCount
	for i := 0; i < total; i++ {
		var m models.Mock
		if err := dec.Decode(&m); err != nil {
			return err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loaded = total
	return nil
}

func (s *composeAgent) MockStats(context.Context) (models.MockStats, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return models.MockStats{Loaded: s.loaded}, nil
}

// composeAgentInstr is the harness's agent connection with the mock store and
// the agent's own count going through the real AgentClient to the real agent
// routes.
type composeAgentInstr struct {
	*prInstr
	client *httpclient.AgentClient
}

func (c *composeAgentInstr) StoreMocks(ctx context.Context, f, u []*models.Mock) error {
	if err := c.prInstr.StoreMocks(ctx, f, u); err != nil {
		return err
	}
	return c.client.StoreMocks(ctx, f, u)
}

func (c *composeAgentInstr) GetMockStats(ctx context.Context) (models.MockStats, error) {
	return c.client.GetMockStats(ctx)
}

func cfAt(name string, at time.Time) *models.Mock {
	return &models.Mock{
		Version: models.GetVersion(), Kind: models.ConnectionFailure, Name: name,
		Spec: models.MockSpec{
			Metadata:         map[string]string{"type": "mocks"},
			ConnFailure:      &models.ConnFailureSpec{Address: "127.0.0.1:5432", Phase: models.ConnFailurePhaseConnect, Outcome: models.ConnFailureRefused},
			ReqTimestampMock: at, ResTimestampMock: at.Add(time.Millisecond),
		},
	}
}

// Under docker compose, the replay checks after the bring-up that the agent
// still holds the mocks it stored (an agent compose replaced holds none). The
// agent is never sent a connection failure, so a set whose only mocks are
// connection failures stores nothing: the check has nothing to miss, and the
// tests run, as they did when this keploy could not read the kind at all. The
// check counts what was sent, not what was read.
func TestAComposeSetOfOnlyConnectionFailuresRunsItsTests(t *testing.T) {
	at := time.Now().Add(-time.Hour)
	for _, tc := range []struct {
		name       string
		mocks      []*models.Mock
		wantLoaded int
	}{
		{"only connection failures", []*models.Mock{cfAt("mock-0", at), cfAt("mock-1", at.Add(time.Second))}, 0},
		{"control: a connection failure and an HTTP mock", []*models.Mock{cfAt("mock-0", at), {Version: models.GetVersion(), Kind: models.HTTP, Name: "mock-1", Spec: models.MockSpec{Metadata: map[string]string{"type": "config"}}}}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agent := &composeAgent{}
			router := chi.NewRouter()
			routes.DefaultRoutes{}.New(router, agent, zap.NewNop())
			srv := httptest.NewServer(router)
			defer srv.Close()

			h := newPartialRunHarness(t, 2, 0)
			h.replayer.config.CommandType = string(utils.DockerCompose)
			h.replayer.config.Agent.AgentURI = srv.URL + "/agent"
			h.replayer.instrumentation = &composeAgentInstr{prInstr: h.instr, client: httpclient.New(zap.NewNop(), nil, h.replayer.config)}
			h.replayer.mockDB = &prSkippingMockDB{prMockDB: prMockDB{filtered: tc.mocks}}
			core, logs := observer.New(zapcore.InfoLevel)
			h.replayer.logger = zap.New(core)

			if status := h.run(t); status != models.TestSetStatusPassed {
				t.Fatalf("the set reported %q; want its tests run and passed. Logs: %v", status, logs.FilterLevelExact(zapcore.WarnLevel).All())
			}
			if len(h.report.results) != 2 {
				t.Fatalf("ran %d tests, want 2", len(h.report.results))
			}
			agent.mu.Lock()
			loaded := agent.loaded
			agent.mu.Unlock()
			if loaded != tc.wantLoaded {
				t.Fatalf("the agent holds %d mocks, want %d: it is never sent a connection failure", loaded, tc.wantLoaded)
			}
			if w := logs.FilterMessageSnippet("no longer holds").All(); len(w) != 0 {
				t.Fatalf("the check must not take the agent for replaced: %v", w)
			}
		})
	}
}
