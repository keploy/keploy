package proxy

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// startSet is a test set staged as the replay stages it: its boundary, its
// start-up cutoff at test-1's window, and a staging from models.BaseTime of
// its start-up band (two calls recorded before the first test, as an ORM's
// start-up selects are) and test-1's own call. With exact, the proven switch
// is on and the set is served exactly, its start-up band the pool, as the
// agent serves a keploy test set (ServeExactPool).
func startSet(t *testing.T, exact bool) *MockManager {
	t.Helper()
	on := ""
	if exact {
		on = "1"
	}
	t.Setenv(provenswitch.Env, on)
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	mm.ResetForReplaySession()
	mm.ArmSetFence()
	mm.SeedStartupCutoff(winStart(1))
	mm.SetMocksWithWindow([]*models.Mock{
		inFlightMock("boot-1", 300, 310),
		inFlightMock("boot-2", 320, 330),
		inFlightMock("own-1", 1010, 1020),
	}, nil, models.BaseTime, time.Now())
	if exact {
		mm.ServeExactPool(models.PoolKeyNone, []string{"boot-1", "boot-2"})
		if !mm.ExactServing() {
			t.Fatal("precondition: the set is not served exactly")
		}
	}
	return mm
}

// serveStart answers a call with the staged recording named name, through the
// delete door a parser consumes a per-test recording by, and reports
// whether it was served.
func serveStart(mm *MockManager, name string) bool {
	for _, mk := range mm.StagedMocks() {
		if mk.Name == name {
			return mm.DeleteFilteredMock(*mk)
		}
	}
	return false
}

func stagedNames(mm *MockManager) []string {
	var out []string
	for _, mk := range mm.StagedMocks() {
		out = append(out, mk.Name)
	}
	sort.Strings(out)
	return out
}

func modeName(exact bool) string {
	if exact {
		return "KEPLOY_PROVEN=1, served exactly"
	}
	return "KEPLOY_PROVEN off"
}

// The application dies as it starts and is started again before the set's
// first test (a replay pod's container the kubelet restarts). Its first
// instance was served the set's start-up calls; its second makes the same
// calls, and was answered nothing: each recording had been spent. Re-armed
// for the new instance, the set's start is served to it again, each
// recording once, in either state of the proven switch.
func TestANewInstanceOfTheAppIsServedTheSetsStartAgain(t *testing.T) {
	for _, exact := range []bool{false, true} {
		t.Run(modeName(exact), func(t *testing.T) {
			mm := startSet(t, exact)
			if !serveStart(mm, "boot-1") || !serveStart(mm, "boot-2") {
				t.Fatal("precondition: the first instance's start-up calls were not served")
			}
			if serveStart(mm, "boot-1") {
				t.Fatal("precondition: a start-up recording answered twice in one start")
			}
			if err := mm.RearmAppStart(); err != nil {
				t.Fatalf("RearmAppStart before any test: %v", err)
			}
			if !serveStart(mm, "boot-1") || !serveStart(mm, "boot-2") {
				t.Fatal("the new instance's start-up calls were not served: the first instance had spent them")
			}
			if serveStart(mm, "boot-1") {
				t.Fatal("the new instance was served a start-up recording twice")
			}
			if exact && !mm.ExactServing() {
				t.Fatal("the re-armed set is no longer served exactly")
			}
			// And again for a third instance.
			if err := mm.RearmAppStart(); err != nil || !serveStart(mm, "boot-1") {
				t.Fatalf("a third instance was not served the set's start (RearmAppStart: %v)", err)
			}
		})
	}
}

// A restart before any test stages the set's start as the set's staging
// staged it, through that staging's code: the same recordings in the trees,
// the start-up cutoff kept, a new staging epoch, no test fired, and what the
// first instance was served no longer counted as served.
func TestARestartBeforeAnyTestStagesTheSetsStartAsItsStagingDid(t *testing.T) {
	for _, exact := range []bool{false, true} {
		t.Run(modeName(exact), func(t *testing.T) {
			mm := startSet(t, exact)
			atStart := stagedNames(mm)
			cutoff := mm.FirstTestWindowStart()
			epoch := mm.StagingGeneration()
			isBoot1 := func(mk *models.Mock) bool { return mk.Name == "boot-1" }
			if !serveStart(mm, "boot-1") {
				t.Fatal("precondition: boot-1 was not served")
			}
			if unserved, _ := mm.HasUnservedRecording(models.HTTP, isBoot1); unserved {
				t.Fatal("precondition: boot-1 reads unserved once served")
			}
			if err := mm.RearmAppStart(); err != nil {
				t.Fatal(err)
			}
			if got := stagedNames(mm); !equalStrings(got, atStart) {
				t.Fatalf("the re-armed trees hold %v; the set's staging staged %v", got, atStart)
			}
			if got := mm.FirstTestWindowStart(); !got.Equal(cutoff) {
				t.Fatalf("the start-up cutoff moved from %v to %v", cutoff, got)
			}
			if mm.HasFirstTestFired() {
				t.Fatal("re-arming the set's start fired a test")
			}
			if mm.StagingGeneration() == epoch {
				t.Fatal("re-arming the set's start staged nothing")
			}
			if unserved, _ := mm.HasUnservedRecording(models.HTTP, isBoot1); !unserved {
				t.Fatal("boot-1 still reads served after the set's start was re-armed")
			}
		})
	}
}

// Once a test of the set has run, the set's start is not served again: what
// the test consumed is that test's, and what the start-up consumed stays
// spent. RearmAppStart says so (ErrSetStarted) and changes nothing.
func TestATestsMocksAreNeverServedAgain(t *testing.T) {
	for _, exact := range []bool{false, true} {
		t.Run(modeName(exact), func(t *testing.T) {
			mm := startSet(t, exact)
			if !serveStart(mm, "boot-1") {
				t.Fatal("precondition: boot-1 was not served")
			}
			own := inFlightMock("own-1", 1010, 1020)
			if exact {
				mm.ServeExactTest("T1", []*models.Mock{own})
			} else {
				mm.SetMocksWithWindow([]*models.Mock{own}, nil, winStart(1), winEnd(1))
			}
			if !serveStart(mm, "own-1") && !mm.DeleteFilteredMock(*own) {
				t.Fatal("precondition: test-1's own call was not served")
			}
			epoch := mm.StagingGeneration()
			if err := mm.RearmAppStart(); !errors.Is(err, ErrSetStarted) {
				t.Fatalf("RearmAppStart after a test ran: %v, want ErrSetStarted", err)
			}
			if mm.StagingGeneration() != epoch {
				t.Fatal("a refused re-arm staged the set again")
			}
			if serveStart(mm, "own-1") || mm.DeleteFilteredMock(*own) {
				t.Fatal("test-1's own recording was served again")
			}
			if serveStart(mm, "boot-1") {
				t.Fatal("a start-up recording the set's start consumed was served again after a test ran")
			}
		})
	}
}

// With no test set's start staged there is nothing to serve again: a
// manager that never staged one, and one between a session's reset and its
// set's staging.
func TestNoSetStartIsServedAgainWhereNoneIsStaged(t *testing.T) {
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	if err := mm.RearmAppStart(); !errors.Is(err, ErrNoSetStart) {
		t.Fatalf("a manager that staged nothing: %v, want ErrNoSetStart", err)
	}
	staged := startSet(t, false)
	staged.ResetForReplaySession()
	if err := staged.RearmAppStart(); !errors.Is(err, ErrNoSetStart) {
		t.Fatalf("a session reset before its set is staged: %v, want ErrNoSetStart", err)
	}
}

// A replay connection's process is handed to BeforeServing, with the re-arm
// of the manager that serves it, before the connection is served; the
// re-arm reaches that manager. A recording's connections are not handed.
func TestBeforeServingIsHandedAReplayConnectionsProcess(t *testing.T) {
	type call struct {
		proc Process
		set  uint64
		err  error
	}
	calls := make(chan call, 4)
	BeforeServing = func(proc Process, _ uint32, set uint64, rearm func() error) { calls <- call{proc, set, rearm()} }
	t.Cleanup(func() { BeforeServing = nil })

	upstream, _, _ := echoUpstream(t)
	dest := ipv4Dest(t, upstream)
	dest.KernelPid, dest.AgentNsPid = 9999, 4242
	cfg := config.New()
	cfg.Agent.Mode = models.MODE_TEST
	p := New(zap.NewNop(), fakeDestInfo{dest: dest}, cfg)
	p.nsswitchData = []byte("preseeded-by-test")
	if err := p.Mock(context.Background(), models.OutgoingOptions{Mocking: true}); err != nil {
		t.Fatal(err)
	}
	connect := func() call {
		app, src := acceptedConn(t)
		done := make(chan error, 1)
		go func() { done <- p.handleConnection(context.Background(), src) }()
		c := awaitEvent(t, calls, "the connection's process handed to BeforeServing")
		_ = app.Close()
		awaitEvent(t, done, "handleConnection returning once the application closed")
		return c
	}
	if c := connect(); c.proc != (Process{Pid: 4242, KernelPid: 9999}) || !errors.Is(c.err, ErrNoSetStart) {
		t.Fatalf("handed %+v, re-arm %v; want the connection's process, and ErrNoSetStart before the set is staged", c.proc, c.err)
	}
	p.getMockManager().SetMocksWithWindow([]*models.Mock{inFlightMock("boot-1", 300, 310)}, nil, models.BaseTime, time.Now())
	first := connect()
	if first.err != nil {
		t.Fatalf("the re-arm of a staged set's start: %v", first.err)
	}
	if want := p.getMockManager().setGen.Load(); first.set != want {
		t.Fatalf("the connection is handed set %d; want the serving manager's, %d", first.set, want)
	}
	// Within the set the number holds: after a re-arm, and after a
	// re-registration of the set's mocks (a Mock without StartsTestSet and
	// its staging), so a restart within the set is told as one.
	if again := connect(); again.set != first.set {
		t.Fatalf("a connection after the set's re-arm is handed set %d; want the same set, %d", again.set, first.set)
	}
	if err := p.Mock(context.Background(), models.OutgoingOptions{Mocking: true}); err != nil {
		t.Fatal(err)
	}
	p.getMockManager().SetMocksWithWindow([]*models.Mock{inFlightMock("boot-1", 300, 310)}, nil, models.BaseTime, time.Now())
	if again := connect(); again.set != first.set {
		t.Fatalf("a connection after a re-registration of the set's mocks is handed set %d; want the same set, %d", again.set, first.set)
	}
	// The next test set: its number moves, so an instance of the
	// application that started in the set before is told apart.
	if err := p.Mock(context.Background(), models.OutgoingOptions{Mocking: true, StartsTestSet: true}); err != nil {
		t.Fatal(err)
	}
	p.getMockManager().SetMocksWithWindow([]*models.Mock{inFlightMock("boot-1", 300, 310)}, nil, models.BaseTime, time.Now())
	if next := connect(); next.set == first.set || next.set == 0 || next.err != nil {
		t.Fatalf("the next set's connection is handed set %d (the set before's was %d), re-arm %v; want another set, re-armed", next.set, first.set, next.err)
	}

	rec := New(zap.NewNop(), fakeDestInfo{dest: dest}, config.New())
	rec.nsswitchData = []byte("preseeded-by-test")
	if err := rec.Record(context.Background(), make(chan *models.Mock, 16), models.OutgoingOptions{DisableMysqlAutoDetect: true}); err != nil {
		t.Fatal(err)
	}
	app, src := acceptedConn(t)
	done := make(chan error, 1)
	go func() { done <- rec.handleConnection(context.Background(), src) }()
	_ = app.Close()
	awaitEvent(t, done, "a recording's handleConnection returning")
	select {
	case c := <-calls:
		t.Fatalf("a recording's connection was handed to BeforeServing: %+v", c.proc)
	default:
	}
}

// BeforeServing is called before anything of the connection is served: in
// serveConnection, once its session is read, and before the parsers start.
func TestBeforeServingComesBeforeTheParsers(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "proxy.go", nil, 0)
	if err != nil {
		t.Fatalf("parse proxy.go: %v", err)
	}
	var serveConn *ast.FuncDecl
	for _, d := range file.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == "serveConnection" && fd.Recv != nil {
			serveConn = fd
		}
	}
	if serveConn == nil {
		t.Fatal("Proxy.serveConnection not found in proxy.go")
	}
	at := map[string][]token.Pos{}
	ast.Inspect(serveConn.Body, func(n ast.Node) bool {
		if c, ok := n.(*ast.CallExpr); ok {
			if sel, ok := c.Fun.(*ast.SelectorExpr); ok {
				at[sel.Sel.Name] = append(at[sel.Sel.Name], c.Pos())
			}
		}
		return true
	})
	before := at["beforeServing"]
	if len(before) != 1 {
		t.Fatalf("serveConnection calls beforeServing %d time(s), want once", len(before))
	}
	if gs := at["getSession"]; len(gs) == 0 || gs[0] > before[0] {
		t.Fatal("beforeServing must come once the connection's session is read")
	}
	for _, name := range []string{"WithContext", "NoteOriginIn", "Get"} {
		for _, pos := range at[name] {
			if pos < before[0] {
				t.Fatalf("serveConnection calls %s before beforeServing: the connection is served before a new instance of the application is told", name)
			}
		}
	}
}

// A carry-over recording the first instance consumed as it started (a
// server push its start-up read: bootTaken) is consumed once per start; the
// new instance's start is staged with it again, and it is served again.
func TestANewInstanceIsServedTheCarryOverItsEarlierInstanceConsumed(t *testing.T) {
	defer models.RegisterCarryOver(models.HTTP, func(m *models.Mock) bool { return m.Name == "push-1" })()
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	mm.ResetForReplaySession()
	mm.ArmSetFence()
	mm.SeedStartupCutoff(winStart(1))
	mm.SetMocksWithWindow([]*models.Mock{inFlightMock("push-1", 300, 310), inFlightMock("own-1", 1010, 1020)}, nil, models.BaseTime, time.Now())
	if !serveStart(mm, "push-1") {
		t.Fatal("precondition: the first instance was not served its start-up push")
	}
	if !mm.bootConsumed(inFlightMock("push-1", 300, 310)) {
		t.Fatal("precondition: the start-up push is not recorded as consumed as the application started")
	}
	if err := mm.RearmAppStart(); err != nil {
		t.Fatal(err)
	}
	if !serveStart(mm, "push-1") {
		t.Fatal("the new instance was not served the start-up push its earlier instance consumed")
	}
}

// Every stateful cursor starts over for the new instance, as the agent starts
// them over at the set's staging: a sequence the earlier instance's start-up
// walked (a reusable one, which a staging alone keeps) answers the new one
// from its first recorded response.
func TestANewInstanceWalksTheSetsSequencesFromTheirStart(t *testing.T) {
	mm := startSet(t, false)
	const key = "GET /config"
	mm.AdvanceMockCursor(key, 0, 2)
	if got := mm.MockCursorIndex(key, 2); got != 1 {
		t.Fatalf("precondition: the earlier instance's walk left the cursor at %d, want 1", got)
	}
	if err := mm.RearmAppStart(); err != nil {
		t.Fatal(err)
	}
	if got := mm.MockCursorIndex(key, 2); got != 0 {
		t.Fatalf("the new instance's walk starts at %d; want 0, its first recorded response", got)
	}
}

// Under the proven switch, what the earlier instance's start-up bound is not
// the new one's: a new variable set begins, as at the set's Mock, so the new
// instance's start-up binds anew; and the learn step begins a new set with
// it when the Proxy re-arms.
func TestANewInstanceBindsItsStartUpAnew(t *testing.T) {
	mm := startSet(t, true)
	if _, ok := mm.vars.Bind(models.VariableBinding{Name: "session", Value: "from-the-earlier-instance"}); !ok {
		t.Fatal("precondition: the earlier instance's start-up did not bind")
	}
	began := false
	if err := mm.rearmAppStart(func() error { began = true; return nil }, 0); err != nil {
		t.Fatal(err)
	}
	if b, ok := mm.vars.Lookup("session"); ok {
		t.Fatalf("the new instance reads the earlier instance's binding %+v", b)
	}
	if !began {
		t.Fatal("the learn step's new set was not begun")
	}
	if _, ok := mm.vars.Bind(models.VariableBinding{Name: "session", Value: "the-new-instance's"}); !ok {
		t.Fatal("the new instance's start-up could not bind its own value")
	}
}

// The Proxy's re-arm begins the learn step's new set, under the proven
// switch, on the tracker the connections are served with.
func TestTheProxysReArmBeginsTheLearnStepsNewSet(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	cfg := config.New()
	cfg.Agent.Mode = models.MODE_TEST
	p := New(zap.NewNop(), fakeDestInfo{}, cfg)
	p.nsswitchData = []byte("preseeded-by-test")
	if err := p.Mock(context.Background(), models.OutgoingOptions{Mocking: true}); err != nil {
		t.Fatal(err)
	}
	p.getMockManager().SetMocksWithWindow([]*models.Mock{inFlightMock("boot-1", 300, 310)}, nil, models.BaseTime, time.Now())
	before := p.learnTracker.SetAt()
	var err error
	BeforeServing = func(_ Process, _ uint32, _ uint64, rearm func() error) { err = rearm() }
	t.Cleanup(func() { BeforeServing = nil })
	p.beforeServing(Process{Pid: 1}, 0)
	if err != nil {
		t.Fatalf("the Proxy's re-arm: %v", err)
	}
	if p.learnTracker.SetAt() == before {
		t.Fatal("the Proxy's re-arm did not begin the learn step's new set")
	}
}

// Served exactly as the agent serves a keploy test set: the background rules
// staged around the set's staging (StageExactBackground, ServeExactBackground)
// and a worker scope beside the pool. The new instance is served the set's
// start again, and the rules and the worker scope stay as they were.
func TestANewInstanceIsServedTheSetsStartUnderTheBackgroundRules(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	mm.ResetForReplaySession()
	mm.ArmSetFence()
	mm.SeedStartupCutoff(winStart(1))
	mm.ServeExactPool(models.PoolKeyNone, nil)
	mm.StageExactBackground(models.RecordedWithS12Routing)
	mm.SetMocksWithWindow([]*models.Mock{
		inFlightMock("boot-1", 300, 310),
		inFlightMock("w-poll", 340, 350),
		inFlightMock("own-1", 1010, 1020),
	}, nil, models.BaseTime, time.Now())
	mm.ServeExactBackground(models.RecordedWithS12Routing, []string{"boot-1"}, []string{"w-poll"})
	mm.ServeExactWorker([]string{"w-poll"})
	mm.ServeExactPool(models.PoolKeyNone, []string{"boot-1"})
	if !serveStart(mm, "boot-1") || serveStart(mm, "boot-1") {
		t.Fatal("precondition: the earlier instance was not served boot-1 once")
	}
	if err := mm.RearmAppStart(); err != nil {
		t.Fatal(err)
	}
	if !serveStart(mm, "boot-1") {
		t.Fatal("the new instance was not served boot-1 under the background rules")
	}
	if mm.exact.bg.Load() == nil || mm.exact.worker.Load() == nil {
		t.Fatal("the re-arm let the background rules or the worker scope go")
	}
}

// No test opens between a re-arm's check and its staging: whichever way a
// test opens (a test's window staged, or opened with its variables, in three
// tiers, a keploy mock replay scope, an exact test served, opened, or
// opened beside others) while the re-arm runs, it waits for it, and opens on
// the re-armed set.
func TestNoTestOpensWhileTheSetsStartIsStagedAgain(t *testing.T) {
	own := func() *models.Mock { return inFlightMock("own-1", 1010, 1020) }
	for name, tc := range map[string]struct {
		exact bool
		open  func(mm *MockManager)
	}{
		"SetMocksWithWindow": {false, func(mm *MockManager) {
			mm.SetMocksWithWindow([]*models.Mock{own()}, nil, winStart(1), winEnd(1))
		}},
		"OpenTestWithWindow": {false, func(mm *MockManager) {
			mm.OpenTestWithWindow([]*models.Mock{own()}, nil, winStart(1), winEnd(1), nil)
		}},
		"SetMocksWithWindowThreeTier": {false, func(mm *MockManager) {
			mm.SetMocksWithWindowThreeTier([]*models.Mock{own()}, nil, nil, winStart(1), winEnd(1))
		}},
		"SetScopeMocks":  {false, func(mm *MockManager) { mm.SetScopeMocks([]*models.Mock{own()}, nil, true) }},
		"ServeExactTest": {true, func(mm *MockManager) { mm.ServeExactTest("T1", []*models.Mock{own()}) }},
		"OpenExactTest":  {true, func(mm *MockManager) { mm.OpenExactTest("T1", []*models.Mock{own()}, nil) }},
		"OpenExactTestConcurrent": {true, func(mm *MockManager) {
			mm.OpenExactTestConcurrent("T1", []*models.Mock{own()}, nil, nil)
		}},
	} {
		t.Run(name, func(t *testing.T) {
			mm := startSet(t, tc.exact)
			inRearm, release := make(chan struct{}), make(chan struct{})
			mm.testHookRearm = func() { close(inRearm); <-release }
			rearmed := make(chan error, 1)
			go func() { rearmed <- mm.RearmAppStart() }()
			<-inRearm
			opened := make(chan struct{})
			go func() { tc.open(mm); close(opened) }()
			select {
			case <-opened:
				t.Fatal("a test opened while the set's start was being staged again")
			case <-time.After(50 * time.Millisecond):
			}
			close(release)
			if err := <-rearmed; err != nil {
				t.Fatalf("the re-arm: %v", err)
			}
			<-opened
			if !mm.HasFirstTestFired() {
				t.Fatal("the test did not open once the re-arm was done")
			}
			if err := mm.RearmAppStart(); !errors.Is(err, ErrSetStarted) {
				t.Fatalf("a re-arm once the test opened: %v, want ErrSetStarted", err)
			}
		})
	}
}

// Under the proven switch the replay begins a test on the learn step (its
// capture window) before it stages the test's mocks: a re-arm then is
// refused, and the test stays begun.
func TestARestartOnceATestsCaptureBeganIsRefused(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	cfg := config.New()
	cfg.Agent.Mode = models.MODE_TEST
	p := New(zap.NewNop(), fakeDestInfo{}, cfg)
	p.nsswitchData = []byte("preseeded-by-test")
	if err := p.Mock(context.Background(), models.OutgoingOptions{Mocking: true}); err != nil {
		t.Fatal(err)
	}
	p.getMockManager().SetMocksWithWindow([]*models.Mock{inFlightMock("boot-1", 300, 310)}, nil, models.BaseTime, time.Now())
	p.BeginTestCaseCapture()
	var err error
	BeforeServing = func(_ Process, _ uint32, _ uint64, rearm func() error) { err = rearm() }
	t.Cleanup(func() { BeforeServing = nil })
	p.beforeServing(Process{Pid: 1}, 0)
	if !errors.Is(err, ErrSetStarted) {
		t.Fatalf("a re-arm once test-1's capture began: %v, want ErrSetStarted", err)
	}
	if _, open, began := p.learnTracker.State(); !open || !began {
		t.Fatalf("the re-arm ended the begun test on the learn step (open %v, began %v)", open, began)
	}
}

// A test worker's manager is re-armed without the learn step, which is the
// run's and follows no worker's tests: the tracker stays as it was.
func TestAWorkersReArmLeavesTheLearnStepAlone(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	cfg := config.New()
	cfg.Agent.Mode = models.MODE_TEST
	p := New(zap.NewNop(), fakeDestInfo{}, cfg)
	p.nsswitchData = []byte("preseeded-by-test")
	if err := p.Mock(context.Background(), models.OutgoingOptions{Mocking: true}); err != nil {
		t.Fatal(err)
	}
	if err := p.SetMocksWithWindow(context.Background(), []*models.Mock{inFlightMock("boot-1", 300, 310)}, nil, models.BaseTime, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := p.ServeInstance(1); err != nil {
		t.Fatal(err)
	}
	before := p.learnTracker.SetAt()
	// The worker's own count, apart from the run's.
	worker := p.instanceManager(1)
	worker.setGen.Add(5)
	var err error
	var handed uint64
	var handedWorker uint32
	BeforeServing = func(_ Process, w uint32, set uint64, rearm func() error) { handedWorker, handed, err = w, set, rearm() }
	t.Cleanup(func() { BeforeServing = nil })
	p.beforeServing(Process{Pid: 1}, 1)
	if err != nil {
		t.Fatalf("the worker's re-arm: %v", err)
	}
	if want := worker.setGen.Load(); handed != want || handed == p.getMockManager().setGen.Load() || handedWorker != 1 {
		t.Fatalf("a worker's connection is handed worker %d, set %d; want worker 1 and its manager's set, %d", handedWorker, handed, want)
	}
	if p.learnTracker.SetAt() != before {
		t.Fatal("a worker's re-arm began the run's learn step's new set")
	}
}

// A keploy mock replay scope open is a test running: no set's start is
// served again then.
func TestAReArmIsRefusedWhileAScopeRuns(t *testing.T) {
	mm := startSet(t, false)
	mm.SetScopeMocks([]*models.Mock{inFlightMock("own-1", 1010, 1020)}, nil, true)
	if err := mm.RearmAppStart(); !errors.Is(err, ErrSetStarted) {
		t.Fatalf("a re-arm while a scope runs: %v, want ErrSetStarted", err)
	}
}

// A re-arm decided in one test set does not re-arm another's: once the
// manager moved to the next set, it says there is no set's start to serve
// again for it.
func TestAReArmDecidedInAnotherSetIsRefused(t *testing.T) {
	mm := startSet(t, false)
	set := mm.setGen.Load()
	if err := mm.rearmAppStart(nil, set+1); !errors.Is(err, ErrNoSetStart) {
		t.Fatalf("a re-arm decided in another set: %v, want ErrNoSetStart", err)
	}
	if err := mm.rearmAppStart(nil, set); err != nil {
		t.Fatalf("a re-arm decided in the manager's set: %v", err)
	}
}

// The build may decide while the proxy moves on (it waits on another
// connection's re-arm): a re-arm decided about one manager's set does not
// re-arm another's. Once the next set began, it re-arms nothing of it; once
// the worker that served the connection ended, it says so; once the run's
// manager was replaced, it re-arms nothing either.
func TestAReArmDecidedBeforeTheProxyMovedOnReArmsNothing(t *testing.T) {
	newProxy := func(t *testing.T) *Proxy {
		cfg := config.New()
		cfg.Agent.Mode = models.MODE_TEST
		p := New(zap.NewNop(), fakeDestInfo{}, cfg)
		p.nsswitchData = []byte("preseeded-by-test")
		if err := p.Mock(context.Background(), models.OutgoingOptions{Mocking: true}); err != nil {
			t.Fatal(err)
		}
		if err := p.SetMocksWithWindow(context.Background(), []*models.Mock{inFlightMock("boot-1", 300, 310)}, nil, models.BaseTime, time.Now()); err != nil {
			t.Fatal(err)
		}
		return p
	}
	decideAfter := func(t *testing.T, p *Proxy, inst uint32, meanwhile func()) error {
		var err error
		BeforeServing = func(_ Process, _ uint32, _ uint64, rearm func() error) { meanwhile(); err = rearm() }
		t.Cleanup(func() { BeforeServing = nil })
		p.beforeServing(Process{Pid: 1}, inst)
		return err
	}
	t.Run("the next set began", func(t *testing.T) {
		p := newProxy(t)
		var epoch uint64
		err := decideAfter(t, p, 0, func() {
			if err := p.Mock(context.Background(), models.OutgoingOptions{Mocking: true, StartsTestSet: true}); err != nil {
				t.Fatal(err)
			}
			if err := p.SetMocksWithWindow(context.Background(), []*models.Mock{inFlightMock("boot-1", 300, 310)}, nil, models.BaseTime, time.Now()); err != nil {
				t.Fatal(err)
			}
			epoch = p.getMockManager().StagingGeneration()
		})
		if !errors.Is(err, ErrNoSetStart) {
			t.Fatalf("a re-arm decided in the set before: %v, want ErrNoSetStart", err)
		}
		if p.getMockManager().StagingGeneration() != epoch {
			t.Fatal("a re-arm decided in the set before re-staged the next set's start")
		}
	})
	t.Run("the worker ended", func(t *testing.T) {
		p := newProxy(t)
		if _, err := p.ServeInstance(1); err != nil {
			t.Fatal(err)
		}
		err := decideAfter(t, p, 1, func() {
			if err := p.StopServingInstance(1); err != nil {
				t.Fatal(err)
			}
		})
		if err == nil || !strings.Contains(err.Error(), "is not served") {
			t.Fatalf("a re-arm of a worker that ended: %v, want the worker not served", err)
		}
	})
	t.Run("the worker was served again", func(t *testing.T) {
		p := newProxy(t)
		if _, err := p.ServeInstance(1); err != nil {
			t.Fatal(err)
		}
		var again *MockManager
		var epoch uint64
		err := decideAfter(t, p, 1, func() {
			if err := p.StopServingInstance(1); err != nil {
				t.Fatal(err)
			}
			if _, err := p.ServeInstance(1); err != nil {
				t.Fatal(err)
			}
			again = p.instanceManager(1)
			epoch = again.StagingGeneration()
		})
		if !errors.Is(err, ErrNoSetStart) {
			t.Fatalf("a re-arm decided about the worker's earlier manager: %v, want ErrNoSetStart", err)
		}
		if again.StagingGeneration() != epoch {
			t.Fatal("the worker's new manager was re-staged for a decision about its earlier one")
		}
	})
	t.Run("the run's manager was replaced", func(t *testing.T) {
		p := newProxy(t)
		var replacement *MockManager
		var epoch uint64
		err := decideAfter(t, p, 0, func() {
			replacement = p.shareWindows(NewMockManager(NewTreeDb(customComparator), NewTreeDb(customComparator), zap.NewNop()))
			p.setMockManager(replacement)
			if err := p.SetMocksWithWindow(context.Background(), []*models.Mock{inFlightMock("boot-1", 300, 310)}, nil, models.BaseTime, time.Now()); err != nil {
				t.Fatal(err)
			}
			epoch = replacement.StagingGeneration()
		})
		if !errors.Is(err, ErrNoSetStart) {
			t.Fatalf("a re-arm of a manager replaced meanwhile: %v, want ErrNoSetStart", err)
		}
		if replacement.StagingGeneration() != epoch {
			t.Fatal("the replacement was re-staged for a decision about the manager it replaced")
		}
	})
	t.Run("a worker began to be served", func(t *testing.T) {
		p := newProxy(t)
		run := p.getMockManager()
		epoch := run.StagingGeneration()
		err := decideAfter(t, p, 0, func() {
			if _, err := p.ServeInstance(1); err != nil {
				t.Fatal(err)
			}
		})
		if !errors.Is(err, errNoWorkerInstance) {
			t.Fatalf("a re-arm decided before a worker was served: %v, want no worker instance", err)
		}
		if run.StagingGeneration() != epoch {
			t.Fatal("the run's manager was re-staged for a connection it no longer serves")
		}
	})
}

// A connection with no manager to serve it (no worker instance while workers
// are served) is handed set 0, and its rearm says why, whatever the proxy
// does meanwhile.
func TestAConnectionWithNoManagerIsHandedNoSet(t *testing.T) {
	cfg := config.New()
	cfg.Agent.Mode = models.MODE_TEST
	p := New(zap.NewNop(), fakeDestInfo{}, cfg)
	p.nsswitchData = []byte("preseeded-by-test")
	if err := p.Mock(context.Background(), models.OutgoingOptions{Mocking: true}); err != nil {
		t.Fatal(err)
	}
	if err := p.SetMocksWithWindow(context.Background(), []*models.Mock{inFlightMock("boot-1", 300, 310)}, nil, models.BaseTime, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := p.ServeInstance(1); err != nil {
		t.Fatal(err)
	}
	var set uint64
	var err error
	BeforeServing = func(_ Process, _ uint32, s uint64, rearm func() error) {
		set = s
		p.endInstances()
		err = rearm()
	}
	t.Cleanup(func() { BeforeServing = nil })
	p.beforeServing(Process{Pid: 1}, 0)
	if set != 0 || !errors.Is(err, errNoWorkerInstance) {
		t.Fatalf("a connection no manager serves is handed set %d, re-arm %v; want 0 and no worker instance", set, err)
	}
}
