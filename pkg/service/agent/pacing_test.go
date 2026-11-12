package agent

// Tests for what UpdateMockParams hands the proxy for server-push pacing: the
// recorded windows of the set (O1) and the carry-over lookahead load (C1).

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg"
	coreAgent "go.keploy.io/server/v3/pkg/agent"
	proxyPkg "go.keploy.io/server/v3/pkg/agent/proxy"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// pacingProxy is a WindowedProxy that records what the agent handed it.
type pacingProxy struct {
	coreAgent.Proxy // nil: any other call panics loudly

	mu      sync.Mutex
	seeded  [][]models.TestWindow
	sets    int
	resets  int
	lastF   []*models.Mock
	lastU   []*models.Mock
	lastWin [2]time.Time
}

func (p *pacingProxy) ResetStatefulCursors() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.resets++
}

func (p *pacingProxy) SetMocksWithWindow(_ context.Context, f, u []*models.Mock, start, end time.Time) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sets++
	p.lastF, p.lastU, p.lastWin = f, u, [2]time.Time{start, end}
	return nil
}

func (p *pacingProxy) SeedRecordedWindows(ws []models.TestWindow) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seeded = append(p.seeded, append([]models.TestWindow(nil), ws...))
}

// residentAgent returns an agent holding the given mocks resident (the legacy,
// no-disk path), which is enough for UpdateMockParams to run end to end.
func residentAgent(p coreAgent.Proxy, filtered, unfiltered []*models.Mock) *Agent {
	a := &Agent{logger: zap.NewNop(), Proxy: p}
	a.clientMocks.Store(uint64(0), &ClientMockStorage{filtered: filtered, unfiltered: unfiltered})
	return a
}

func TestUpdateMockParamsSeedsRecordedWindows(t *testing.T) {
	base := time.Date(2026, 9, 30, 9, 0, 0, 0, time.UTC)
	ws := []models.TestWindow{
		{TestCase: "test-1", Start: base, End: base.Add(time.Second)},
		{TestCase: "test-2", Start: base.Add(2 * time.Second), End: base.Add(3 * time.Second)},
	}
	p := &pacingProxy{}
	a := residentAgent(p, nil, nil)

	// The staging call carries the windows.
	if err := a.UpdateMockParams(context.Background(), models.MockFilterParams{
		AfterTime: models.BaseTime, BeforeTime: time.Now(), RecordedWindows: ws,
	}); err != nil {
		t.Fatalf("staging UpdateMockParams: %v", err)
	}
	// A per-test call carries none, and must not re-seed (or clear) them.
	if err := a.UpdateMockParams(context.Background(), models.MockFilterParams{
		AfterTime: ws[0].Start, BeforeTime: ws[0].End,
	}); err != nil {
		t.Fatalf("per-test UpdateMockParams: %v", err)
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.seeded) != 1 {
		t.Fatalf("SeedRecordedWindows called %d times, want exactly once (the staging call)", len(p.seeded))
	}
	if got := p.seeded[0]; len(got) != 2 || got[1].TestCase != "test-2" || !got[1].Start.Equal(ws[1].Start) {
		t.Fatalf("seeded %+v, want the two recorded windows", got)
	}
	if p.sets != 2 {
		t.Fatalf("SetMocksWithWindow called %d times, want 2", p.sets)
	}
}

// TestUpdateMockParamsResetsStatefulCursorsOncePerTestSet pins the stateful
// cursor's reset boundary: the staging call (which carries recordedSetShape —
// FirstRecordedTestStart and/or RecordedWindows) marks a new test-set and must
// reset the cursors, while the per-test-case calls (empty shape) must NOT, so a
// stateful sequence spans the whole test-set but does not carry across sets.
func TestUpdateMockParamsResetsStatefulCursorsOncePerTestSet(t *testing.T) {
	base := time.Date(2026, 9, 30, 9, 0, 0, 0, time.UTC)
	p := &pacingProxy{}
	a := residentAgent(p, nil, nil)

	// Staging call for a test-set: FirstRecordedTestStart set → reset.
	if err := a.UpdateMockParams(context.Background(), models.MockFilterParams{
		AfterTime: models.BaseTime, BeforeTime: time.Now(), FirstRecordedTestStart: base,
	}); err != nil {
		t.Fatalf("staging UpdateMockParams: %v", err)
	}
	// Two per-test-case calls (empty shape) → must NOT reset.
	for i := 0; i < 2; i++ {
		if err := a.UpdateMockParams(context.Background(), models.MockFilterParams{
			AfterTime: base, BeforeTime: base.Add(time.Second),
		}); err != nil {
			t.Fatalf("per-test UpdateMockParams: %v", err)
		}
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.resets != 1 {
		t.Fatalf("ResetStatefulCursors called %d times, want exactly once (the staging call) — a sequence must span the whole test-set, not reset per test case", p.resets)
	}
}

// A proxy without the capability keeps working: the windows are simply not
// seeded, which is the pre-existing behaviour.
func TestUpdateMockParamsWithoutTheSeederStillStages(t *testing.T) {
	p := &plainWindowedProxy{}
	a := residentAgent(p, nil, nil)
	if err := a.UpdateMockParams(context.Background(), models.MockFilterParams{
		AfterTime: models.BaseTime, BeforeTime: time.Now(),
		RecordedWindows: []models.TestWindow{{TestCase: "t", Start: time.Now(), End: time.Now()}},
	}); err != nil {
		t.Fatalf("UpdateMockParams: %v", err)
	}
	if p.sets != 1 {
		t.Fatalf("SetMocksWithWindow called %d times, want 1", p.sets)
	}
}

type plainWindowedProxy struct {
	coreAgent.Proxy
	sets int
}

func (p *plainWindowedProxy) SetMocksWithWindow(context.Context, []*models.Mock, []*models.Mock, time.Time, time.Time) error {
	p.sets++
	return nil
}

// mgrProxy puts a real MockManager behind the agent, so the lookahead range the
// manager plans, the disk load the agent does and the tier the manager files
// are exercised together.
type mgrProxy struct {
	coreAgent.Proxy
	mm *proxyPkg.MockManager
}

func (p *mgrProxy) SetMocksWithWindow(_ context.Context, f, u []*models.Mock, start, end time.Time) error {
	p.mm.SetMocksWithWindow(f, u, start, end)
	return nil
}
func (p *mgrProxy) SeedStartupCutoff(t time.Time)              { p.mm.SeedStartupCutoff(t) }
func (p *mgrProxy) SeedRecordedWindows(ws []models.TestWindow) { p.mm.SeedRecordedWindows(ws) }
func (p *mgrProxy) FirstTestWindowStart() time.Time            { return p.mm.FirstTestWindowStart() }
func (p *mgrProxy) GetPersistentConsumed() map[string]models.MockState {
	return p.mm.GetPersistentConsumed()
}
func (p *mgrProxy) CarryOverLoadRange(start time.Time) (time.Time, time.Time, bool) {
	return p.mm.CarryOverLoadRange(start)
}

const agentPulsar models.Kind = "AgentTestPulsar"

func agentBroker(name string, kind models.Kind, cmd string, at time.Time) *models.Mock {
	m := &models.Mock{Name: name, Kind: kind, Spec: models.MockSpec{
		Metadata:         map[string]string{"type": "mocks", "commandType": cmd},
		ReqTimestampMock: at, ResTimestampMock: at.Add(time.Millisecond),
	}}
	m.DeriveLifetime()
	m.TestModeInfo.Lifetime = models.LifetimePerTest
	return m
}

// diskAgent returns an agent whose per-test mocks live in an on-disk store, the
// strict-mode residency the lookahead load reads from.
func diskAgent(t *testing.T, p coreAgent.Proxy, mocks ...*models.Mock) *Agent {
	t.Helper()
	d, err := proxyPkg.NewDiskMocks(zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	for _, m := range mocks {
		if err := d.Add(m); err != nil {
			t.Fatal(err)
		}
	}
	d.Finalize()
	a := &Agent{logger: zap.NewNop(), Proxy: p}
	a.clientMocks.Store(uint64(0), &ClientMockStorage{diskMocks: d})
	return a
}

func carryNamesOf(mm *proxyPkg.MockManager) []string {
	ms, _ := mm.GetCarryOverMocksByKind(agentPulsar)
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.Name)
	}
	return out
}

// The agent's windowed load also loads registered mocks recorded up to 1 s past
// the current window's release span, and only those: not the other kinds, not
// the registered kind's other commands, and nothing in lax mode.
func TestUpdateMockParamsLoadsCarryOverAhead(t *testing.T) {
	unregister := models.RegisterCarryOver(agentPulsar, func(m *models.Mock) bool {
		return m.Spec.Metadata["commandType"] == "SEND"
	})
	t.Cleanup(unregister)

	base := time.Date(2026, 9, 30, 9, 0, 0, 0, time.UTC)
	ms := func(n int) time.Time { return base.Add(time.Duration(n) * time.Millisecond) }
	ws := []models.TestWindow{
		{TestCase: "test-1", Start: ms(1000), End: ms(1100)},
		{TestCase: "test-2", Start: ms(2000), End: ms(2100)},
		{TestCase: "test-3", Start: ms(3000), End: ms(3100)},
	}
	recording := []*models.Mock{
		agentBroker("s-ahead", agentPulsar, "SEND", ms(2500)), // 1500 -> W1: reachable at W1
		agentBroker("s-later", agentPulsar, "SEND", ms(3050)), // 2050 -> W2: not at W1
		agentBroker("flow", agentPulsar, "FLOW", ms(2600)),    // registered kind, not carry-over
		agentBroker("http", models.HTTP, "", ms(2700)),        // other kind
		agentBroker("own", agentPulsar, "SEND", ms(1050)),     // W1's own
	}

	for _, strict := range []bool{true, false} {
		t.Run(map[bool]string{true: "strict", false: "lax"}[strict], func(t *testing.T) {
			mm := proxyPkg.NewMockManager(nil, nil, zap.NewNop())
			defer mm.Close()
			mm.ResetForReplaySession()
			p := &mgrProxy{mm: mm}
			a := diskAgent(t, p, recording...)
			if err := a.UpdateMockParams(context.Background(), models.MockFilterParams{
				AfterTime: models.BaseTime, BeforeTime: time.Now(), StrictMockWindow: strict,
				FirstRecordedTestStart: ws[0].Start, RecordedWindows: ws,
			}); err != nil {
				t.Fatal(err)
			}
			if err := a.UpdateMockParams(context.Background(), models.MockFilterParams{
				AfterTime: ws[0].Start, BeforeTime: ws[0].End, StrictMockWindow: strict,
			}); err != nil {
				t.Fatal(err)
			}
			got := carryNamesOf(mm)
			if pkg.IsStrictMockWindow(strict) {
				if len(got) != 1 || got[0] != "s-ahead" {
					t.Fatalf("W1 carry-over = %v, want [s-ahead]: loaded 1 s past W1's release span, "+
						"nothing that is not a registered SEND, nothing released later", got)
				}
				// W2: s-later becomes reachable; s-ahead stays until consumed, and
				// W1 closed with its own SEND unconsumed, so that is carried too.
				if err := a.UpdateMockParams(context.Background(), models.MockFilterParams{
					AfterTime: ws[1].Start, BeforeTime: ws[1].End, StrictMockWindow: strict,
				}); err != nil {
					t.Fatal(err)
				}
				if got := carryNamesOf(mm); len(got) != 3 || got[0] != "own" || got[1] != "s-ahead" || got[2] != "s-later" {
					t.Fatalf("W2 carry-over = %v, want [own s-ahead s-later]", got)
				}
				return
			}
			if len(got) != 0 {
				t.Fatalf("lax mode loaded a lookahead: %v", got)
			}
		})
	}
}

// A carry-over SEND whose recorder stated no lifetime, as enterprise's Pulsar
// and Redis recorders leave theirs, recorded in the gap after test-7: under
// the default strict window over the on-disk store, the agent's carry-over
// lookahead hands it to the carry-over tier at test-7's window, not before,
// and neither it nor an unstated FLOW in the same gap reaches the session
// pool in any window: no filter sees an out-of-window per-test mock to
// promote. The agent's one serve decision, asked on the window the replayer's
// mock prune holds an unanswered test-7 by (strict, the lookahead run),
// releases it there, so the prune keeps it for that test.
func TestUpdateMockParamsServesAnUnstatedCarryOverFromTheCarryTier(t *testing.T) {
	t.Cleanup(models.RegisterCarryOver(agentPulsar, func(m *models.Mock) bool {
		return m.Spec.Metadata["commandType"] == "SEND"
	}))
	base := time.Date(2026, 9, 30, 9, 0, 0, 0, time.UTC)
	at := func(s float64) time.Time { return base.Add(time.Duration(s * float64(time.Second))) }
	var ws []models.TestWindow
	for i := 1; i <= 8; i++ {
		ws = append(ws, models.TestWindow{TestCase: fmt.Sprintf("test-%d", i), Start: at(float64(10 * i)), End: at(float64(10*i + 2))})
	}
	send := agentBroker("send-gap-7", agentPulsar, "SEND", at(73))
	flow := agentBroker("flow-gap-7", agentPulsar, "FLOW", at(73.2))
	if send.StatesLifetime() || flow.StatesLifetime() {
		t.Fatal("precondition: the mocks state no lifetime")
	}
	if !models.IsCarryOver(send) || models.IsCarryOver(flow) {
		t.Fatal("precondition: the SEND, and not the FLOW, is a carry-over mock")
	}

	mm := proxyPkg.NewMockManager(nil, nil, zap.NewNop())
	defer mm.Close()
	mm.ResetForReplaySession()
	a := diskAgent(t, &mgrProxy{mm: mm}, send, flow)
	call := func(p models.MockFilterParams) {
		t.Helper()
		p.StrictMockWindow = true
		if err := a.UpdateMockParams(context.Background(), p); err != nil {
			t.Fatal(err)
		}
	}
	has := func(read func() ([]*models.Mock, error), name string) bool {
		t.Helper()
		got, err := read()
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range got {
			if m != nil && m.Name == name {
				return true
			}
		}
		return false
	}
	call(models.MockFilterParams{AfterTime: models.BaseTime, BeforeTime: time.Now(), FirstRecordedTestStart: ws[0].Start, RecordedWindows: ws})
	for i := 0; i <= 6; i++ {
		call(models.MockFilterParams{AfterTime: ws[i].Start, BeforeTime: ws[i].End})
		for _, n := range []string{send.Name, flow.Name} {
			if has(mm.GetUnFilteredMocks, n) {
				t.Errorf("%s: the session pool holds %s", ws[i].TestCase, n)
			}
		}
		carried := has(mm.GetCarryOverMocks, send.Name)
		switch {
		case i < 6 && carried:
			t.Errorf("%s: the carry-over tier holds the SEND before test-7's window", ws[i].TestCase)
		case i == 6 && !carried:
			t.Errorf("test-7: the carry-over tier does not hold the SEND")
		}
	}

	sched := models.NewWindowSchedule(ws)
	sw := pkg.ServeWindow{Start: ws[6].Start, End: ws[6].End, FirstStart: ws[0].Start, StrictWindow: true, Recorded: sched, Lookahead: true}
	if from, ok := sched.GapBefore(sw.Start); ok {
		sw.InFlightFrom = from
	}
	if got := pkg.ServeInWindow(send, false, sw); got != pkg.ServedReleased {
		t.Errorf("pkg.ServeInWindow on the unanswered test-7's window: %d, want %d (released there)", got, pkg.ServedReleased)
	}
}

// The SQS shape: an async consumer's poll that returned messages (an HTTP
// ReceiveMessage the recorder stated per-test), declared a StagingRelease
// need, is delivered at its place in the recording, once:
//   - one recorded before the first test (the app's bootstrap poll) is in
//     the startup tier from the set's staging on, and once consumed is not
//     staged again for the next test, though no drain has told the agent
//     yet (the replay's map is sent with each call, as by default);
//   - one recorded in the gap after test-1 is reachable from test-1 on (its
//     release window), until consumed;
//   - an HTTP mock the need does not declare is neither carried nor, consumed
//     outside its window, marked CarryOver.
func TestAReleaseNeedDeliversEachRecordedPollOnceAtItsPlace(t *testing.T) {
	isPoll := func(m *models.Mock) bool { return m.Spec.Metadata["sqs_messages"] == "1" }
	defer models.RegisterStagingNeed(models.HTTP, models.StagingNeed{Use: models.StagingRelease, Match: isPoll})()
	base := time.Date(2026, 9, 30, 9, 0, 0, 0, time.UTC)
	ms := func(n int) time.Time { return base.Add(time.Duration(n) * time.Millisecond) }
	ws := []models.TestWindow{
		{TestCase: "test-1", Start: ms(1000), End: ms(1100)},
		{TestCase: "test-2", Start: ms(3000), End: ms(3100)},
		{TestCase: "test-3", Start: ms(5000), End: ms(5100)},
	}
	poll := func(name string, at time.Time) *models.Mock {
		m := stageMock(name, "/receive", at)
		m.Spec.Metadata["sqs_messages"] = "1"
		return m
	}
	pollBoot, pollGap := poll("poll-boot", ms(500)), poll("poll-gap", ms(2000))
	plainGap := stageMock("plain-gap", "/other", ms(2100))
	recording := []*models.Mock{pollBoot, pollGap, plainGap, stageMock("own-1", "/q", ms(1050)), stageMock("own-3", "/q", ms(5050))}

	mm := proxyPkg.NewMockManager(nil, nil, zap.NewNop())
	defer mm.Close()
	mm.ResetForReplaySession()
	a := diskAgent(t, &mgrProxy{mm: mm}, recording...)
	call := func(after, before time.Time, shape bool) {
		t.Helper()
		// No consumed history at all: the replay's map, sent with each call
		// by default, holds a consume only once a drain after it took it.
		params := models.MockFilterParams{AfterTime: after, BeforeTime: before, StrictMockWindow: true}
		if shape {
			params.FirstRecordedTestStart, params.RecordedWindows = ws[0].Start, ws
		}
		if err := a.UpdateMockParams(context.Background(), params); err != nil {
			t.Fatal(err)
		}
	}
	startupNames := func() []string {
		t.Helper()
		ms, err := mm.GetStartupMocks()
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, m := range ms {
			out = append(out, m.Name)
		}
		sort.Strings(out)
		return out
	}

	call(models.BaseTime, time.Now(), true)
	if got := startupNames(); !reflect.DeepEqual(got, []string{"poll-boot"}) {
		t.Fatalf("the app's startup is served %v; want its bootstrap poll [poll-boot]", got)
	}
	if !mm.DeleteFilteredMock(*pollBoot) {
		t.Fatal("the bootstrap poll was not consumed")
	}
	if st := mm.GetPersistentConsumed()["poll-boot"]; st.Usage != models.Deleted || !st.CarryOver {
		t.Fatalf("the consumed bootstrap poll is %+v; want Deleted and CarryOver, consumed once", st)
	}

	carriedHTTP := func(step string) []*models.Mock {
		t.Helper()
		carried, err := mm.GetCarryOverMocksByKind(models.HTTP)
		if err != nil {
			t.Fatal(err)
		}
		if len(carried) != 1 || carried[0].Name != "poll-gap" {
			t.Fatalf("%s's carry-over tier holds %v; want the gap poll after test-1, and no undeclared HTTP mock", step, namesOf(carried))
		}
		return carried
	}
	call(ws[0].Start, ws[0].End, false)
	if got := startupNames(); len(got) != 0 {
		t.Fatalf("test-1's startup tier holds %v; the bootstrap poll was consumed, and is not staged again", got)
	}
	carriedHTTP("test-1") // its release window
	call(ws[1].Start, ws[1].End, false)
	carriedHTTP("test-2") // until consumed
	if !mm.DeleteFilteredMock(*pollGap) {
		t.Fatal("the gap poll was not consumed from the carry-over tier")
	}
	if st := mm.GetPersistentConsumed()["poll-gap"]; st.Usage != models.Deleted || !st.CarryOver {
		t.Fatalf("the consumed gap poll is %+v; want Deleted and CarryOver", st)
	}
	mm.MarkMockAsUsed(*plainGap)
	if st := mm.GetPersistentConsumed()["plain-gap"]; st.CarryOver {
		t.Fatalf("an undeclared HTTP mock consumed outside its window was marked CarryOver: %+v", st)
	}

	call(ws[2].Start, ws[2].End, false)
	if carried, _ := mm.GetCarryOverMocksByKind(models.HTTP); len(carried) != 0 {
		t.Fatalf("test-3's carry-over tier holds %v; the gap poll was consumed once", namesOf(carried))
	}
}

// A set's strict-window staging, before its app starts, also loads the
// registered mocks the app's startup reaches: those recorded within
// models.CarryOverLookahead after the set's first test began (a publish the
// app makes as it starts, recorded after the first test began), the
// lookahead's end excluded. They are startup traffic while the app starts,
// and the first test refiles them: out of the startup tier, into its window
// or the carry-over tier. A registered mock at or past the lookahead's end,
// another command of the kind, and lax mode load none.
func TestStagingLoadsTheCarryOverMocksTheStartupReaches(t *testing.T) {
	unregister := models.RegisterCarryOver(agentPulsar, func(m *models.Mock) bool {
		return m.Spec.Metadata["commandType"] == "SEND"
	})
	t.Cleanup(unregister)
	base := time.Date(2026, 9, 30, 9, 0, 0, 0, time.UTC)
	ms := func(n int) time.Time { return base.Add(time.Duration(n) * time.Millisecond) }
	ws := []models.TestWindow{
		{TestCase: "test-1", Start: ms(1000), End: ms(1100)},
		{TestCase: "test-2", Start: ms(3000), End: ms(3100)},
	}
	recording := []*models.Mock{
		agentBroker("s-boot", agentPulsar, "SEND", ms(500)),        // the startup band
		agentBroker("s-crossed", agentPulsar, "SEND", ms(1600)),    // within 1 s of test-1's start
		agentBroker("s-edge", agentPulsar, "SEND", ms(2000)),       // at the lookahead's end
		agentBroker("s-later", agentPulsar, "SEND", ms(2100)),      // past it
		agentBroker("flow-crossed", agentPulsar, "FLOW", ms(1500)), // not carry-over
	}
	for _, strict := range []bool{true, false} {
		t.Run(map[bool]string{true: "strict", false: "lax"}[strict], func(t *testing.T) {
			mm := proxyPkg.NewMockManager(nil, nil, zap.NewNop())
			defer mm.Close()
			mm.ResetForReplaySession()
			p := &mgrProxy{mm: mm}
			a := diskAgent(t, p, recording...)
			if err := a.UpdateMockParams(context.Background(), models.MockFilterParams{
				AfterTime: models.BaseTime, BeforeTime: time.Now(), StrictMockWindow: strict,
				FirstRecordedTestStart: ws[0].Start, RecordedWindows: ws,
			}); err != nil {
				t.Fatal(err)
			}
			startup, err := mm.GetStartupMocks()
			if err != nil {
				t.Fatal(err)
			}
			var names []string
			for _, m := range startup {
				if m.Kind == agentPulsar {
					names = append(names, m.Name)
				}
			}
			sort.Strings(names)
			want := []string{"s-boot", "s-crossed"}
			if !pkg.IsStrictMockWindow(strict) {
				// Lax mode stages the whole recording, as it always did.
				want = []string{"flow-crossed", "s-boot", "s-crossed", "s-edge", "s-later"}
			}
			if !reflect.DeepEqual(names, want) {
				t.Fatalf("the staging filed %v as startup traffic; want %v", names, want)
			}
			if !pkg.IsStrictMockWindow(strict) {
				return
			}
			// The first test refiles them: s-crossed is outside test-1's
			// window, so it goes to the carry-over tier, out of the startup
			// tier, which keeps only what was recorded before the first test.
			if err := a.UpdateMockParams(context.Background(), models.MockFilterParams{
				AfterTime: ws[0].Start, BeforeTime: ws[0].End, StrictMockWindow: strict,
			}); err != nil {
				t.Fatal(err)
			}
			startup, _ = mm.GetStartupMocks()
			names = names[:0]
			for _, m := range startup {
				if m.Kind == agentPulsar {
					names = append(names, m.Name)
				}
			}
			if !reflect.DeepEqual(names, []string{"s-boot"}) {
				t.Fatalf("after the first test the startup tier holds %v; want [s-boot]", names)
			}
			if got := carryNamesOf(mm); !reflect.DeepEqual(got, []string{"s-crossed", "s-edge", "s-later"}) {
				t.Fatalf("after the first test the carry-over tier holds %v; want [s-crossed s-edge s-later], each reachable from test-1", got)
			}
		})
	}
}

func (p *mgrProxy) InFlightFrom(start time.Time) (time.Time, bool) { return p.mm.InFlightFrom(start) }

// A consumer keeps a fetch in flight between tests (a Kafka fetch, an SQS long
// poll): the one sent in the gap after test-1 and answered inside test-2 is
// test-2's. The agent loads it with test-2's window from its on-disk store,
// which loads by request time, its filter keeps it per-test in the strict
// window (the default), whether its recorder stated its lifetime (a MySQL or
// Postgres v3 recorder) or not (a Kafka or Mongo recorder), and the mock
// manager files it in test-2's per-test tier, not the session pool: a parser
// that reads only the per-test tier (Mongo's data matcher) is served it, once.
// One sent and answered in the gap is loaded for no test.
func TestTheAgentServesACallInFlightWhenItsTestBegins(t *testing.T) {
	base := time.Date(2026, 10, 17, 6, 56, 0, 0, time.UTC)
	ms := func(n int) time.Time { return base.Add(time.Duration(n) * time.Millisecond) }
	for _, stated := range []bool{true, false} {
		mk := func(name string, req, res int) *models.Mock {
			m := stageMock(name, "/"+name, ms(req))
			m.Spec.ResTimestampMock = ms(res)
			if stated {
				m.Spec.Metadata[models.MetadataLifetimeKey] = models.MetadataLifetimePerTest
			} else {
				m.Kind = models.Mongo // a recorder that states no lifetime: per-test by its tag
			}
			m.TestModeInfo = models.TestModeInfo{}
			m.DeriveLifetime()
			return m
		}
		ws := []models.TestWindow{
			{TestCase: "post-order-1", Start: ms(1000), End: ms(1100)},
			{TestCase: "post-order-2", Start: ms(3344), End: ms(3505)},
		}
		recording := []*models.Mock{
			mk("own-1", 1050, 1051),
			mk("fetch-o2", 2986, 3495), // sent in the gap, answered inside post-order-2
			mk("gap-only", 2000, 2100),
			mk("own-2", 3400, 3401),
		}
		if lt := recording[1].TestModeInfo.Lifetime; lt != models.LifetimePerTest {
			t.Fatalf("stated=%v: the fixture's fetch is %v, want per-test", stated, lt)
		}
		mm := proxyPkg.NewMockManager(nil, nil, zap.NewNop())
		mm.ResetForReplaySession()
		a := diskAgent(t, &mgrProxy{mm: mm}, recording...)
		call := func(after, before time.Time, shape bool) {
			t.Helper()
			params := models.MockFilterParams{AfterTime: after, BeforeTime: before, StrictMockWindow: true}
			if shape {
				params.FirstRecordedTestStart, params.RecordedWindows = ws[0].Start, ws
			}
			if err := a.UpdateMockParams(context.Background(), params); err != nil {
				t.Fatal(err)
			}
		}
		call(models.BaseTime, time.Now(), true)
		call(ws[0].Start, ws[0].End, false)
		call(ws[1].Start, ws[1].End, false)
		namesOf := func(got []*models.Mock) []string {
			var names []string
			for _, m := range got {
				names = append(names, m.Name)
			}
			sort.Strings(names)
			return names
		}
		perTest, _ := mm.GetFilteredMocks()
		if got := namesOf(perTest); !reflect.DeepEqual(got, []string{"fetch-o2", "own-2"}) {
			t.Fatalf("stated=%v: post-order-2's per-test tier holds %v; want its own mock and the fetch in flight when it began", stated, got)
		}
		session, _ := mm.GetUnFilteredMocks()
		if got := namesOf(session); slices.Contains(got, "fetch-o2") {
			t.Fatalf("stated=%v: the fetch in flight is in the session pool %v, served to any test and more than once", stated, got)
		}
		mm.Close()
	}
}

// A consumer's poll placed at its answer (models.RegisterAnswerPlaced) is
// staged for the test its answer is in, by the agent and its mock manager:
// one sent inside post-order-1 and answered in post-order-2 is
// post-order-2's, and one sent as the app boots and answered in post-order-1
// is post-order-1's, not start-up traffic. So in a strict-window replay, and
// in a mapping replay whose mappings.yaml lists the first poll under
// post-order-1, by request time, as an older keploy wrote it: the mapping's
// names are loaded, and so is every poll placed in the window, whatever the
// mapping lists.
func TestTheAgentStagesAPollForTheTestItsAnswerIsIn(t *testing.T) {
	t.Cleanup(models.RegisterAnswerPlaced(models.HTTP, func(m *models.Mock) bool { return m.Spec.Metadata["poll"] == "yes" }))
	base := time.Date(2026, 10, 18, 6, 56, 0, 0, time.UTC)
	ms := func(n int) time.Time { return base.Add(time.Duration(n) * time.Millisecond) }
	mk := func(name string, req, res int, poll bool) *models.Mock {
		m := stageMock(name, "/"+name, ms(req))
		m.Spec.ResTimestampMock = ms(res)
		if poll {
			m.Spec.Metadata["poll"] = "yes"
		}
		return m
	}
	ws := []models.TestWindow{
		{TestCase: "post-order-1", Start: ms(1000), End: ms(1100)},
		{TestCase: "post-order-2", Start: ms(3344), End: ms(3505)},
	}
	recording := func() []*models.Mock {
		return []*models.Mock{
			mk("boot-poll", 500, 1060, true),
			mk("own-1", 1050, 1051, false),
			mk("poll-b", 1080, 3450, true),
			mk("own-2", 3400, 3401, false),
		}
	}
	namesOf := func(got []*models.Mock) []string {
		names := []string{}
		for _, m := range got {
			names = append(names, m.Name)
		}
		sort.Strings(names)
		return names
	}
	for _, mapped := range []bool{false, true} {
		mm := proxyPkg.NewMockManager(nil, nil, zap.NewNop())
		mm.ResetForReplaySession()
		a := diskAgent(t, &mgrProxy{mm: mm}, recording()...)
		call := func(after, before time.Time, shape bool, mapping []string) {
			t.Helper()
			params := models.MockFilterParams{AfterTime: after, BeforeTime: before, StrictMockWindow: true}
			if shape {
				params.FirstRecordedTestStart, params.RecordedWindows = ws[0].Start, ws
			}
			if mapped && mapping != nil {
				params.UseMappingBased, params.MockMapping = true, mapping
			}
			if err := a.UpdateMockParams(context.Background(), params); err != nil {
				t.Fatal(err)
			}
		}
		call(models.BaseTime, time.Now(), true, nil)
		if startup, _ := mm.GetStartupMocks(); len(namesOf(startup)) != 0 {
			t.Fatalf("mapped=%v: the start-up tier holds %v; want none: the boot poll is post-order-1's", mapped, namesOf(startup))
		}
		call(ws[0].Start, ws[0].End, false, []string{"own-1", "poll-b"})
		if got, _ := mm.GetFilteredMocks(); !reflect.DeepEqual(namesOf(got), []string{"boot-poll", "own-1"}) {
			t.Fatalf("mapped=%v: post-order-1's per-test tier holds %v; want its own mock and the boot poll", mapped, namesOf(got))
		}
		call(ws[1].Start, ws[1].End, false, []string{"own-2"})
		if got, _ := mm.GetFilteredMocks(); !reflect.DeepEqual(namesOf(got), []string{"own-2", "poll-b"}) {
			t.Fatalf("mapped=%v: post-order-2's per-test tier holds %v; want its own mock and the poll answered in it", mapped, namesOf(got))
		}
		mm.Close()
	}
}

// A staging call's window (models.BaseTime to now) is no test's: the polls
// placed at their answer are not added to its mapping for it. A scope's
// staging (stageScope, keploy mock replay with the switch off) begins a test
// so, its mapping the test's whole recording (ExactMapping), and was served
// every declared poll of the recording as the test's own, the later tests'
// among them.
func TestAStagingCallsMappingIsServedWithNoPollPlacedInItsWindow(t *testing.T) {
	t.Cleanup(models.RegisterAnswerPlaced(models.HTTP, func(m *models.Mock) bool { return m.Spec.Metadata["poll"] == "yes" }))
	ms := func(n int) time.Time { return stageT0.Add(time.Duration(n) * time.Millisecond) }
	poll := func(name string, req, res int) *models.Mock {
		m := stageMock(name, "/"+name, ms(req))
		m.Spec.Metadata["poll"] = "yes"
		m.Spec.ResTimestampMock = ms(res)
		return m
	}
	mm := proxyPkg.NewMockManager(nil, nil, zap.NewNop())
	defer mm.Close()
	a := diskAgent(t, &mgrProxy{mm: mm}, stageMock("own-1", "/q1", ms(50)), poll("poll-2", 60, 2050), stageMock("own-3", "/q3", ms(3010)), poll("poll-4", 3020, 4050))
	params := models.MockFilterParams{AfterTime: models.BaseTime, BeforeTime: time.Now(), MockMapping: []string{"own-1"}, UseMappingBased: true, ExactMapping: true, StrictMockWindow: true}
	filtered, _, err := a.mocksForParams(context.Background(), params)
	if err != nil {
		t.Fatal(err)
	}
	if got := namesOf(filtered); !reflect.DeepEqual(got, []string{"own-1"}) {
		t.Fatalf("a staging call whose mapping is [own-1] is served %v; want its mapping alone", got)
	}
	// Nor read from disk for it: the load reads what the mapping names.
	st, _ := a.clientMocks.Load(uint64(0))
	loaded, err := a.loadPerTestMocks(nil, st.(*ClientMockStorage).diskMocks, params, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if got := namesOf(loaded); !reflect.DeepEqual(got, []string{"own-1"}) {
		t.Fatalf("a staging call whose mapping is [own-1] reads %v from disk; want its mapping alone", got)
	}
}
