package agent

// Tests for what UpdateMockParams hands the proxy for server-push pacing: the
// recorded windows of the set (O1) and the carry-over lookahead load (C1).

import (
	"context"
	"sync"
	"testing"
	"time"

	coreAgent "go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// pacingProxy is a WindowedProxy that records what the agent handed it.
type pacingProxy struct {
	coreAgent.Proxy // nil: any other call panics loudly

	mu      sync.Mutex
	seeded  [][]models.TestWindow
	sets    int
	lastF   []*models.Mock
	lastU   []*models.Mock
	lastWin [2]time.Time
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
