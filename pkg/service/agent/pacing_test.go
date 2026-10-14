package agent

// Tests for what UpdateMockParams hands the proxy for server-push pacing: the
// recorded windows of the set (O1) and the carry-over lookahead load (C1).

import (
	"context"
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
