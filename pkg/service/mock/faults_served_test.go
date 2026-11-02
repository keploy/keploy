package mock

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// partialConsumed is an agent whose read of the mocks it served came back
// cut short: the part it read, and the error.
type partialConsumed struct{ outcomeInstr }

func (p partialConsumed) GetConsumedMocks(context.Context) ([]models.MockState, error) {
	return p.consumed, errors.New("read cut short")
}

// The replay summary names each fault mock the agent served, in the order it
// first served them, as the application met it ("mock-12: upstream reset after
// 0 bytes, as recorded"); a mock with a whole answer, or a fault mock no call
// consumed, is not named. A read of what was served that came back cut short
// still names the faults it holds: each was served.
func TestTheReplaySummaryNamesEachFaultServed(t *testing.T) {
	fault := func(name string, f *models.Fault) *models.Mock {
		return &models.Mock{Name: name, Kind: models.HTTP, Spec: models.MockSpec{Fault: f}}
	}
	whole := fault("mock-1", nil)
	reset := fault("mock-12", &models.Fault{Kind: models.FaultReset})
	truncate := fault("mock-3", &models.Fault{Kind: models.FaultTruncate, BodyBytes: 10})
	unserved := fault("mock-5", &models.Fault{Kind: models.FaultTimeout})
	faults := faultMocks([]*models.Mock{whole, truncate, nil}, []*models.Mock{reset, unserved})
	consumed := []models.MockState{{Name: "mock-1", Timestamp: 1}, {Name: "mock-3", Timestamp: 3}, {Name: "mock-12", Timestamp: 2}}
	want := []string{
		"mock-12: upstream reset after 0 bytes, as recorded",
		"mock-3: upstream sent the head and 10 body bytes, then closed, as recorded",
	}
	for name, inst := range map[string]Instrumentation{
		"the whole read":     outcomeInstr{consumed: consumed},
		"a read cut short":   partialConsumed{outcomeInstr{consumed: consumed}},
		"nothing was served": outcomeInstr{},
	} {
		t.Run(name, func(t *testing.T) {
			core, logs := observer.New(zap.InfoLevel)
			m := &mockService{config: &config.Config{}, logger: zap.New(core), instrumentation: inst}
			m.reportOutcome(context.Background(), 4, replayDetail{set: "s", faults: faults}, func() bool { return false })
			var got []string
			for _, e := range logs.FilterMessage("served a mock's fault").All() {
				got = append(got, e.ContextMap()["fault"].(string))
			}
			exp := want
			if name == "nothing was served" {
				exp = nil
			}
			if !reflect.DeepEqual(got, exp) {
				t.Fatalf("named %q, want %q", got, exp)
			}
		})
	}
}

// faultSetDB is a set of one whole answer and a fault mock in each pool.
type faultSetDB struct{ stubMockDB }

func (faultSetDB) GetFilteredMocks(context.Context, string, time.Time, time.Time, map[string]bool, map[string]bool) ([]*models.Mock, error) {
	return []*models.Mock{
		{Name: "mock-0", Kind: models.HTTP},
		{Name: "mock-1", Kind: models.HTTP, Spec: models.MockSpec{Fault: &models.Fault{Kind: models.FaultEOF}}},
	}, nil
}

func (faultSetDB) GetUnFilteredMocks(context.Context, string, time.Time, time.Time, map[string]bool, map[string]bool) ([]*models.Mock, error) {
	return []*models.Mock{{Name: "mock-2", Kind: models.HTTP, Spec: models.MockSpec{Fault: &models.Fault{Kind: models.FaultReset, AfterHead: true, BodyBytes: 4}}}}, nil
}

// keploy mock replay names the faults the agent served out of the set it
// loaded, from either pool.
func TestAReplayNamesTheFaultsItServedFromTheSetItLoaded(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)
	instr := newInstr(t, agentUpFromSetup, false, models.AppError{AppErrorType: models.ErrAppStopped})
	instr.consumedMocks = []models.MockState{{Name: "mock-0", Timestamp: 1}, {Name: "mock-1", Timestamp: 2}, {Name: "mock-2", Timestamp: 3}}
	cfg := instrConfig(instr, utils.Native, "pytest -q")
	utils.ErrCode = 0
	t.Cleanup(func() { utils.ErrCode = 0 })
	if err := New(zap.New(core), instr, faultSetDB{}, nil, nil, nil, cfg).Replay(context.Background()); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	var got []string
	for _, e := range logs.FilterMessage("served a mock's fault").All() {
		got = append(got, e.ContextMap()["fault"].(string))
	}
	want := []string{
		"mock-1: upstream closed with no answer, as recorded",
		"mock-2: upstream reset after the head and 4 body bytes, as recorded",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("named %q, want %q", got, want)
	}
}
