package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// storeParser reports the mock store each connection it is handed is
// served from.
type storeParser struct {
	saw chan integrations.MockMemDb
}

func (storeParser) MatchType(context.Context, []byte) bool { return true }
func (storeParser) RecordOutgoing(context.Context, *integrations.RecordSession) error {
	return nil
}
func (s storeParser) MockOutgoing(_ context.Context, _ net.Conn, _ *models.ConditionalDstCfg, db integrations.MockMemDb, _ models.OutgoingOptions) error {
	s.saw <- db
	return nil
}

// storeProxy is instanceProxy whose parser reports the store it is handed.
func storeProxy(t *testing.T, dest *agent.NetworkAddress) (*Proxy, storeParser) {
	t.Helper()
	p, _ := instanceProxy(t, dest, nil)
	sp := storeParser{saw: make(chan integrations.MockMemDb, 4)}
	p.Integrations = map[integrations.IntegrationType]integrations.Integrations{integrations.HTTP: sp}
	t.Cleanup(p.endInstances)
	return p, sp
}

// handle sends one connection of the app through p's handleConnection and
// returns what it returned.
func handle(t *testing.T, p *Proxy) error {
	t.Helper()
	app, src := acceptedConn(t)
	done := make(chan error, 1)
	go func() { done <- p.handleConnection(context.Background(), p.inflight.track(src)) }()
	_, _ = io.WriteString(app, "hello\n")
	err := awaitEvent(t, done, "handleConnection returning")
	_ = app.Close()
	return err
}

func instanceMgrOf(t *testing.T, p *Proxy, inst uint32) *MockManager {
	t.Helper()
	m := p.instanceManager(inst)
	if m == nil {
		t.Fatalf("test worker instance %d has no manager", inst)
	}
	return m
}

// Two test workers that open a test with the same recording each answer it
// from their own manager: one worker's call uses up its own test's mock,
// never the other worker's, and neither touches the run's manager.
func TestEachTestWorkerIsServedFromAManagerOfItsOwn(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	dest := ipv4Dest(t, "127.0.0.1:50061")
	dest.KernelPid = uint32(os.Getpid())
	p, _ := storeProxy(t, dest)
	run := p.getMockManager()
	a := exactMock("a", 0, models.LifetimeSession)
	if err := p.SetMocksWithWindow(context.Background(), nil, []*models.Mock{a}, models.BaseTime, time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, inst := range []uint32{0, 4} {
		if m, err := p.connManager(inst); err != nil || m != run {
			t.Fatalf("before any worker is served, instance %d's connection is served from %p (%v), want the run's manager %p", inst, m, err, run)
		}
	}
	w1, err := p.ServeInstance(1)
	if err != nil {
		t.Fatal(err)
	}
	w2, err := p.ServeInstance(2)
	if err != nil {
		t.Fatal(err)
	}
	m1, m2 := instanceMgrOf(t, p, 1), instanceMgrOf(t, p, 2)
	if m1 == m2 || m1 == run || m2 == run {
		t.Fatalf("the two workers and the run share a manager: %p %p %p", m1, m2, run)
	}
	for inst, want := range map[uint32]*MockManager{1: m1, 2: m2} {
		if got, err := p.connManager(inst); err != nil || got != want {
			t.Fatalf("worker %d's connection is served from %p (%v), want its own manager %p", inst, got, err, want)
		}
	}
	for _, w := range []agent.ExactServer{w1, w2} {
		if err := w.ServeExactTest("T", []*models.Mock{a}); err != nil {
			t.Fatal(err)
		}
	}
	pt, _ := m1.GetPerTestMocksInWindow()
	if len(pt) != 1 || !m1.DeleteFilteredMock(*pt[0]) {
		t.Fatalf("worker 1 could not answer its test's call from its own mock: %v", exactNames(pt))
	}
	if test, consumed, unconsumed, ok := w2.ExactTestOutcome(); !ok || test != "T" || len(consumed) != 0 || len(unconsumed) != 1 {
		t.Fatalf("worker 2's test after worker 1's call: %q consumed %v unconsumed %v (%v), want its own mock unanswered", test, consumed, unconsumed, ok)
	}
	if _, consumed, _, _ := w1.ExactTestOutcome(); len(consumed) != 1 || consumed[0] != "a" {
		t.Fatalf("worker 1's test consumed %v, want its own a", consumed)
	}
	pt, _ = m2.GetPerTestMocksInWindow()
	if len(pt) != 1 || !m2.DeleteFilteredMock(*pt[0]) {
		t.Fatalf("worker 2's call found no mock of its own left: %v", exactNames(pt))
	}
	if _, _, _, on := run.ExactTestOutcome(); on {
		t.Fatal("the run's manager served a worker's test")
	}
	if _, err := p.ServeInstance(1); err == nil {
		t.Fatal("worker 1 was served twice")
	}
	if _, err := p.ServeInstance(0); err == nil {
		t.Fatal("instance 0, no worker, was served")
	}
	if _, err := (&Proxy{logger: zap.NewNop()}).ServeInstance(1); err == nil {
		t.Fatal("a worker was served by a proxy that serves no replay")
	}
	// A new session's manager: no worker is served on it until the agent
	// serves it again.
	fresh := NewMockManager(nil, nil, zap.NewNop())
	p.setMockManager(fresh)
	if got, err := p.connManager(1); err != nil || got != fresh || p.instanceManager(1) != nil {
		t.Fatalf("after a new session, worker 1's connection is served from %p (%v), want the new run's manager", got, err)
	}
	if _, _, _, on := w1.ExactTestOutcome(); on {
		t.Fatal("worker 1's test outlived the session that served it")
	}
}

// A worker's manager is staged as the run's is, whichever came first: what
// the run was staged with before the worker began, what it is staged with
// after, and a new session's reset, in the order they came. Each manager
// stages its own copies of the mocks.
func TestATestWorkersManagerIsStagedAsTheRunsIs(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	dest := ipv4Dest(t, "127.0.0.1:50062")
	p, _ := storeProxy(t, dest)
	ctx := context.Background()
	a, b := exactMock("a", 0, models.LifetimeSession), exactMock("b", time.Second, models.LifetimeSession)
	if err := p.SetMocks(ctx, nil, []*models.Mock{a}); err != nil {
		t.Fatal(err)
	}
	if err := p.SetPoolNames([]string{"a"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := p.ServeInstance(1); err != nil {
		t.Fatal(err)
	}
	m1 := instanceMgrOf(t, p, 1)
	got, _ := m1.GetUnFilteredMocks()
	if len(got) != 1 || got[0].Name != "a" || got[0] == a {
		t.Fatalf("worker 1 began with %v (the run's own mock: %v), want its own copy of the run's a", exactNames(got), len(got) == 1 && got[0] == a)
	}
	if names := m1.exact.poolNames.Load(); names == nil || len(*names) != 1 {
		t.Fatalf("worker 1 began without the run's pool names: %v", names)
	}
	if err := p.SetMocksWithWindow(ctx, nil, []*models.Mock{a, b}, models.BaseTime, time.Now()); err != nil {
		t.Fatal(err)
	}
	if got, _ = m1.GetUnFilteredMocks(); len(got) != 2 {
		t.Fatalf("worker 1 holds %v after the run was staged with a and b", exactNames(got))
	}
	// Another session of the run: its reset reaches the worker, and a
	// worker begun after it starts from it too, not from the pool names
	// the reset dropped.
	if err := p.Mock(ctx, models.OutgoingOptions{DisableMysqlAutoDetect: true, Mocking: true}); err != nil {
		t.Fatal(err)
	}
	if names := m1.exact.poolNames.Load(); names != nil {
		t.Fatalf("the run's reset did not reach worker 1: pool names %v", *names)
	}
	if _, err := p.ServeInstance(2); err != nil {
		t.Fatal(err)
	}
	m2 := instanceMgrOf(t, p, 2)
	if names := m2.exact.poolNames.Load(); names != nil {
		t.Fatalf("worker 2, begun after the reset, holds the pool names it dropped: %v", *names)
	}
	if got, _ = m2.GetUnFilteredMocks(); len(got) != 2 || got[0] == a || got[1] == b {
		t.Fatalf("worker 2 began with %v, want its own copies of the run's a and b", exactNames(got))
	}
	if err := p.SetPoolNames([]string{"b"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := p.ServeInstance(3); err != nil {
		t.Fatal(err)
	}
	if names := instanceMgrOf(t, p, 3).exact.poolNames.Load(); names == nil {
		t.Fatal("worker 3 began without the pool names staged after the reset")
	}
}

// Once a worker is served, a connection from no worker, from a worker
// keploy never served, or from one it stopped serving is refused before
// any parser is handed it, a MySQL one too; a served worker's goes to its
// own manager. A stopped worker's tests can no longer be served.
func TestACallNoServedWorkerMadeIsRefusedBeforeAnyMockAnswers(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	dest := ipv4Dest(t, "127.0.0.1:50063")
	dest.KernelPid = uint32(os.Getpid())
	p, sp := storeProxy(t, dest)
	p.Integrations[integrations.MYSQL] = sp
	served := func(what string, want *MockManager) {
		t.Helper()
		if err := handle(t, p); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if got := awaitEvent(t, sp.saw, "the parser being handed the connection"); got != integrations.MockMemDb(want) {
			t.Fatalf("%s is served from %p, want %p", what, got, want)
		}
	}
	served("with no worker served, a connection from no worker", p.getMockManager())
	if _, err := p.ServeInstance(5); err != nil {
		t.Fatal(err)
	}
	for _, port := range []uint32{50063, 3306} {
		dest.Port = port
		dest.Instance = 5
		served(fmt.Sprintf("worker 5's connection to port %d", port), instanceMgrOf(t, p, 5))
		for _, c := range []struct {
			inst uint32
			want string
		}{
			{0, "not part of any test worker's keploy instance"},
			{9, "instance 9 is not served"},
		} {
			dest.Instance = c.inst
			err := handle(t, p)
			if err == nil || !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), fmt.Sprintf("process %d", dest.KernelPid)) {
				t.Fatalf("instance %d's connection to port %d: %v, want it refused, naming its process: %q", c.inst, port, err, c.want)
			}
			select {
			case db := <-sp.saw:
				t.Fatalf("instance %d's refused connection to port %d was handed to a parser, served from %p", c.inst, port, db)
			default:
			}
		}
	}
	if err := p.StopServingInstance(5); err != nil {
		t.Fatal(err)
	}
	dest.Instance = 5
	if err := handle(t, p); err == nil || !strings.Contains(err.Error(), "instance 5 is not served") {
		t.Fatalf("a stopped worker's connection: %v, want it refused", err)
	}
	if err := p.StopServingInstance(5); err == nil {
		t.Fatal("worker 5 was stopped twice")
	}
	w, err := p.ServeInstance(6)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.StopServingInstance(6); err != nil {
		t.Fatal(err)
	}
	for what, call := range map[string]func() error{
		"ServeExactTest":           func() error { return w.ServeExactTest("T", nil) },
		"OpenExactTest":            func() error { return w.(agent.ExactTestOpener).OpenExactTest("T", nil, nil) },
		"ServeExactTestConcurrent": func() error { return w.(agent.ExactConcurrentServer).ServeExactTestConcurrent("T", nil, nil) },
		"OpenExactTestConcurrent":  func() error { return w.(agent.ExactConcurrentServer).OpenExactTestConcurrent("T", nil, nil, nil) },
		"GiveExactTest":            func() error { return w.(agent.ExactTestGiver).GiveExactTest("T", models.VariableBinding{}) },
		"ServeExactPool":           func() error { return w.ServeExactPool("k", nil) },
		"ServeExactPoolAhead":      func() error { return w.(agent.ExactAheadServer).ServeExactPoolAhead("k", nil, nil) },
		"ServeExactWorker":         func() error { return w.(agent.ExactWorkerServer).ServeExactWorker(nil) },
		"RestartExactKey":          func() error { return w.(agent.ExactKeyRestarter).RestartExactKey("k", nil) },
	} {
		if err := call(); err == nil || !strings.Contains(err.Error(), "instance 6 is not served") {
			t.Fatalf("%s on a stopped worker: %v, want it refused", what, err)
		}
	}
	if _, _, _, on := w.ExactTestOutcome(); on {
		t.Fatal("a stopped worker's test outcome was read")
	}
}

// Every staging of the set reaches each worker's manager and is kept, one
// of each kind in the order they last came; a test's own window (the run's
// test, not the set) is not kept.
func TestEveryStagingOfTheSetReachesEachTestWorker(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	p, _ := storeProxy(t, ipv4Dest(t, "127.0.0.1:50064"))
	if _, err := p.ServeInstance(1); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	a := exactMock("a", 0, models.LifetimeSession)
	for _, err := range []error{
		p.SetMocks(ctx, nil, []*models.Mock{a}),
		p.SetMocksWithWindow(ctx, nil, []*models.Mock{a}, models.BaseTime, time.Now()),
		p.OpenTestWithWindow(ctx, nil, []*models.Mock{a}, exactT0, exactT0.Add(time.Second), nil),
		p.SetPoolNames([]string{"a"}, nil, nil),
		p.StageExactBackground(models.RecordedWithS12Routing),
		p.ServeExactBackground(models.RecordedWithS12Routing, []string{"a"}, nil),
		p.SetRecordedOfKind(func(models.Kind, func(string, func() (*models.Mock, error)) bool) error { return nil }),
		p.SetRecordingTimes(func(func(string, time.Time, bool, string) bool) error { return nil }),
		p.SetRecordedBefore(func(time.Time, func(*models.Mock) bool) error { return nil }),
		p.SetRecordingSource(func(models.Kind, []string, func(string, func() (*models.Mock, error)) bool) error { return nil }),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	p.SeedStartupCutoff(exactT0)
	p.SeedStatementCatalog([]*models.Mock{a})
	p.SeedRecordedWindows(nil)
	var keys []string
	for _, st := range p.instances.staged {
		keys = append(keys, st.key)
	}
	want := "mocks mocksWindow poolNames bgRules bg recordedOfKind recordingTimes recordedBefore recordingSource startupCutoff catalog recordedWindows"
	if got := strings.Join(keys, " "); got != want {
		t.Fatalf("kept stagings: %s, want %s", got, want)
	}
	m1 := instanceMgrOf(t, p, 1)
	if m1.exact.times.Load() == nil || m1.exact.staging.Load() {
		t.Fatal("worker 1, served before the set's stagings, missed some")
	}
	if _, err := p.ServeInstance(2); err != nil {
		t.Fatal(err)
	}
	if m2 := instanceMgrOf(t, p, 2); m2.exact.times.Load() == nil || m2.exact.staging.Load() {
		t.Fatal("worker 2, served after the set's stagings, missed some")
	}
	// The same kind staged again is kept once, as the latest of them all,
	// and a worker served after it begins with it, not the one it replaced.
	if err := p.SetPoolNames([]string{"b"}, nil); err != nil {
		t.Fatal(err)
	}
	keys = keys[:0]
	for _, st := range p.instances.staged {
		keys = append(keys, st.key)
	}
	want = "mocks mocksWindow bgRules bg recordedOfKind recordingTimes recordedBefore recordingSource startupCutoff catalog recordedWindows poolNames"
	if got := strings.Join(keys, " "); got != want {
		t.Fatalf("kept stagings after pool names came again: %s, want %s", got, want)
	}
	if _, err := p.ServeInstance(3); err != nil {
		t.Fatal(err)
	}
	for inst := uint32(1); inst <= 3; inst++ {
		names := instanceMgrOf(t, p, inst).exact.poolNames.Load()
		if names == nil {
			t.Fatalf("worker %d holds no pool names, want the latest, b", inst)
		}
		if _, b := (*names)["b"]; len(*names) != 1 || !b {
			t.Fatalf("worker %d holds pool names %v, want only the latest, b", inst, *names)
		}
	}
}

// A staging of the set before the run has a manager (no replay yet) is
// refused, as it was before workers, and kept for no worker: a worker
// served later begins from the run's stagings only.
func TestASetStagingWithNoReplayIsRefusedAndKeptForNoWorker(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	p := &Proxy{logger: zap.NewNop()}
	if err := p.SetPoolNames([]string{"a"}, nil); !errors.Is(err, errNoMockManager) {
		t.Fatalf("SetPoolNames with no replay gave %v, want errNoMockManager", err)
	}
	if len(p.instances.staged) != 0 {
		t.Fatalf("a refused staging was kept for later workers: %d kept", len(p.instances.staged))
	}
}
