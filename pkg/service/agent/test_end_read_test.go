package agent

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	proxyPkg "go.keploy.io/server/v3/pkg/agent/proxy"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// settlingTestEndProxy is settlingSetEndProxy noting, in order, the run's
// test ends and begins it is told too.
type settlingTestEndProxy struct {
	settlingSetEndProxy
}

func (p settlingTestEndProxy) SeedRunPlaceWindows([]models.TestWindow) {}

func (p settlingTestEndProxy) ReachRunTest(test string, open bool) {
	if open {
		*p.did = append(*p.did, "begin "+test)
		return
	}
	*p.did = append(*p.did, "end "+test)
}

// A test's end read ends its test before the read settles the application's
// calls, as the set's end read ends the set, with the proven switch on or
// off: an answer held for the gap after the test is written, and the settle
// waits for the calls it makes. A read that names no test ends none.
func TestGetMockErrors_ATestsEndReadEndsItsTestBeforeTheSettle(t *testing.T) {
	for _, tc := range []struct {
		name   string
		proven string
		ctx    context.Context
		want   []string
	}{
		{"switch on, test's end", "1", models.WithTestEndRead(context.Background(), "test-1"), []string{"end test-1", "settle", "read"}},
		{"switch on, unmarked", "1", context.Background(), []string{"settle", "read"}},
		{"switch off, test's end", "", models.WithTestEndRead(context.Background(), "test-1"), []string{"end test-1", "read"}},
		{"switch off, unmarked", "", context.Background(), []string{"read"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(provenswitch.Env, tc.proven)
			a, h, p := settleAgent()
			a.config = config.New()
			a.config.Agent.Mode = models.MODE_TEST
			h.established = 1
			p.set(1, 0)
			var did []string
			a.Proxy = settlingTestEndProxy{settlingSetEndProxy{settleProxy: p, once: &sync.Once{}, did: &did}}
			if _, err := a.GetMockErrors(tc.ctx); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(did, tc.want) {
				t.Fatalf("did %v, want %v", did, tc.want)
			}
		})
	}
}

// consumedSetEndProxy notes, in order, the run's set end and the read of
// what was consumed.
type consumedSetEndProxy struct {
	*stageProxy
	did *[]string
}

func (p consumedSetEndProxy) EndRunSet() {
	*p.did = append(*p.did, "set end")
	p.stageProxy.EndRunSet()
}

func (p consumedSetEndProxy) SeedRecordedWindows(ws []models.TestWindow) {
	p.mm.SeedRecordedWindows(ws)
}

func (p consumedSetEndProxy) GetConsumedMocks(context.Context) ([]models.MockState, error) {
	*p.did = append(*p.did, "consumed")
	return nil, nil
}

// The read of what was consumed after a set's last test (keploy test with
// the proven switch off) ends the run's set before it reads, once marked as
// the set's end; unmarked, as every other such read is, it ends nothing.
func TestGetConsumedMocks_ASetEndReadEndsTheSetFirst(t *testing.T) {
	for _, tc := range []struct {
		name string
		ctx  context.Context
		want []string
	}{
		{"unmarked", context.Background(), []string{"consumed"}},
		{"the set's end", models.WithSetEndRead(context.Background()), []string{"set end", "consumed"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var did []string
			a := &Agent{logger: zap.NewNop(), Proxy: consumedSetEndProxy{stageProxy: &stageProxy{mm: proxyPkg.NewMockManager(nil, nil, zap.NewNop())}, did: &did}}
			if _, err := a.GetConsumedMocks(tc.ctx); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(did, tc.want) {
				t.Fatalf("did %v, want %v", did, tc.want)
			}
		})
	}
}

// keploy test with the proven switch off, against the real manager and HTTP
// parser: the set's recorded windows are seeded at its staging, each test's
// window opens at its mocks' window (SetMocksWithWindow), and its end is the
// read of its misses marked with its name. An answer recorded in the gap
// after Test1 is written at Test1's end read: not while Test1 runs, not at
// a read that names no test, nor one that names a test the windows do not
// hold. One recorded in the window of Test3, which the run set aside, is
// written at the read of what was consumed after the set's last test,
// marked as the set's end: not at Test2's end read, the last test that ran.
func TestKeployTestWritesAnAnswerAtItsTestsEndReadWithTheSwitchOff(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	t.Cleanup(models.RegisterAnswerRelease(models.HTTP, placeRelease))
	gap := stageMock("mock-gap", "/gap", stageT0.Add(500*time.Millisecond))
	gap.Spec.ResTimestampMock = stageT0.Add(3 * time.Second)
	late := stageMock("mock-late", "/late", stageT0.Add(600*time.Millisecond))
	late.Spec.ResTimestampMock = stageT0.Add(9 * time.Second)
	mocks := []*models.Mock{gap, late}
	ws := []models.TestWindow{
		{TestCase: "Test1", Start: stageT0, End: stageT0.Add(time.Second)},
		{TestCase: "Test2", Start: stageT0.Add(4 * time.Second), End: stageT0.Add(6 * time.Second)},
		{TestCase: "Test3", Start: stageT0.Add(8 * time.Second), End: stageT0.Add(10 * time.Second)},
	}
	mm := proxyPkg.NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	var did []string
	p := consumedSetEndProxy{stageProxy: &stageProxy{mm: mm}, did: &did}
	cfg := &config.Config{}
	cfg.Agent.Mode = models.MODE_TEST
	a := New(zap.NewNop(), nil, p, nil, nil, cfg)
	ctx := context.Background()
	mm.SeedRecordedWindows(ws)
	mm.SetMocksWithWindow(mocks, nil, models.BaseTime, time.Now())

	mm.SetMocksWithWindow(mocks, nil, ws[0].Start, ws[0].End)
	gapGot, lateGot := heldCall(t, mm, "/gap"), heldCall(t, mm, "/late")
	notYet(t, gapGot, "the answer recorded after Test1, while Test1 runs")
	for _, read := range []context.Context{ctx, models.WithTestEndRead(ctx, "no-such-test")} {
		if _, err := a.GetMockErrors(read); err != nil {
			t.Fatal(err)
		}
	}
	notYet(t, gapGot, "the answer recorded after Test1, at reads that end no test of the set")
	if _, err := a.GetMockErrors(models.WithTestEndRead(ctx, "Test1")); err != nil {
		t.Fatal(err)
	}
	written(t, gapGot, "mock-gap", "at Test1's end read")

	mm.SetMocksWithWindow(mocks, nil, ws[1].Start, ws[1].End)
	if _, err := a.GetMockErrors(models.WithTestEndRead(ctx, "Test2")); err != nil {
		t.Fatal(err)
	}
	notYet(t, lateGot, "Test3's answer at the end read of Test2, the last test that ran")
	if _, err := a.GetConsumedMocks(models.WithSetEndRead(ctx)); err != nil {
		t.Fatal(err)
	}
	written(t, lateGot, "mock-late", "at the set's end read of what was consumed")
	if want := []string{"set end", "consumed"}; !slices.Equal(did, want) {
		t.Fatalf("did %v, want %v", did, want)
	}
}
