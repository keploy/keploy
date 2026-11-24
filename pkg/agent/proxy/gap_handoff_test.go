package proxy

import (
	"sort"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

func gapStampedMock(name string, reqMs, resMs int) *models.Mock {
	mk := inFlightMock(name, reqMs, resMs)
	if mk.Spec.Metadata == nil {
		mk.Spec.Metadata = make(map[string]string, 1)
	}
	mk.Spec.Metadata[models.MetadataInterTestGapKey] = "true"
	return mk
}

func TestWindowScheduleGapAfterAndInGap(t *testing.T) {
	w1 := models.TestWindow{TestCase: "t1", Start: pms(1000), End: pms(1100)}
	w2 := models.TestWindow{TestCase: "t2", Start: pms(2000), End: pms(2100)}
	w3 := models.TestWindow{TestCase: "t3", Start: pms(2050), End: pms(2200)} // overlaps w2
	sched := models.NewWindowSchedule([]models.TestWindow{w1, w2, w3})

	from, to, ok := sched.GapAfter(w1.Start)
	if !ok || !from.Equal(pms(1100)) || !to.Equal(pms(2000)) {
		t.Fatalf("GapAfter(t1) = (%v, %v, %v); want (1100, 2000, true)", from, to, ok)
	}
	if _, _, ok := sched.GapAfter(w2.Start); ok {
		t.Fatal("GapAfter(t2) = true; want false when t3 overlaps t2")
	}
	if _, _, ok := sched.GapAfter(w3.Start); ok {
		t.Fatal("GapAfter(last window) = true; want false so post-last-test traffic is never pulled into last window")
	}

	// Disjointness from SentInGapAnsweredBy:
	if !models.InGap(pms(1500), pms(1600), pms(1100), pms(2000)) {
		t.Fatal("strictly in-gap call must satisfy InGap")
	}
	if models.InGap(pms(1500), pms(2000), pms(1100), pms(2000)) {
		t.Fatal("call answered at/after nextStart belongs to SentInGapAnsweredBy, not InGap")
	}
	if models.InGap(pms(1100), pms(1200), pms(1100), pms(2000)) {
		t.Fatal("call requested at from (inside previous window) must not satisfy InGap")
	}
	if models.InGap(pms(2000), pms(2010), pms(1100), pms(2000)) {
		t.Fatal("call requested at to (inside next window) must not satisfy InGap")
	}
}

func TestDiskMocksLoadInGap(t *testing.T) {
	d, err := NewDiskMocks(zap.NewNop())
	if err != nil {
		t.Fatalf("NewDiskMocks: %v", err)
	}
	defer func() { _ = d.Close() }()

	failure := gapStampedMock("conn-fail-gap", 1500, 1600)
	failure.Kind = models.ConnectionFailure

	for _, mk := range []*models.Mock{
		inFlightMock("in-t1", 1050, 1060),
		gapStampedMock("gap-1", 1200, 1300),
		inFlightMock("unstamped-gap", 1400, 1450),
		gapStampedMock("gap-2", 1700, 1800),
		gapStampedMock("in-flight-into-t2", 1500, 2050),
		inFlightMock("in-t2", 2010, 2020),
		failure,
	} {
		if err := d.Add(mk); err != nil {
			t.Fatalf("Add(%s): %v", mk.Name, err)
		}
	}
	d.Finalize()

	got, err := d.LoadInGap(pms(1100), pms(2000))
	if err != nil {
		t.Fatalf("LoadInGap: %v", err)
	}
	var names []string
	for _, m := range got {
		names = append(names, m.Name)
	}
	sort.Strings(names)
	if !equalStrings(names, []string{"gap-1", "gap-2"}) {
		t.Fatalf("LoadInGap = %v; want [gap-1 gap-2]", names)
	}
}

func TestMockManagerPostResponseGapAndSingleStepHandoff(t *testing.T) {
	windows := []models.TestWindow{
		{TestCase: "t1", Start: pms(1000), End: pms(1100)},
		{TestCase: "t2", Start: pms(2000), End: pms(2100)},
		{TestCase: "t3", Start: pms(3000), End: pms(3100)},
	}
	perTestInWindowNames := func(mm *MockManager) []string {
		got, err := mm.GetPerTestMocksInWindow()
		if err != nil {
			t.Fatalf("GetPerTestMocksInWindow: %v", err)
		}
		var names []string
		for _, mk := range got {
			names = append(names, mk.Name)
		}
		sort.Strings(names)
		return names
	}

	t.Run("consumed during predecessor window is not handed off to successor; unstamped gap mock is never served", func(t *testing.T) {
		mm := NewMockManager(nil, nil, zap.NewNop())
		defer mm.Close()
		mm.ResetForReplaySession()
		mm.SeedStartupCutoff(pms(1000))
		mm.SeedRecordedWindows(windows)
		mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())

		t1Own := inFlightMock("t1-own", 1010, 1020)
		gapCall := gapStampedMock("gap-http", 1500, 1600)
		unstampedGap := inFlightMock("unstamped-gap", 1400, 1450)
		mm.SetMocksWithWindow([]*models.Mock{t1Own, gapCall, unstampedGap}, nil, pms(1000), pms(1100))

		if got := perTestInWindowNames(mm); !equalStrings(got, []string{"gap-http", "t1-own"}) {
			t.Fatalf("t1 sees %v; want [gap-http t1-own]", got)
		}

		// Consume gap-http during t1.
		for _, mk := range mustPerTest(t, mm) {
			if mk.Name == "gap-http" {
				if !mm.DeleteFilteredMock(*mk) {
					t.Fatal("DeleteFilteredMock(gap-http) failed")
				}
			}
		}

		// Advance to t2: gap-http was already consumed; t1-own and unstamped-gap must never leak.
		t2Own := inFlightMock("t2-own", 2010, 2020)
		mm.SetMocksWithWindow([]*models.Mock{t2Own}, nil, pms(2000), pms(2100))
		if got := perTestInWindowNames(mm); !equalStrings(got, []string{"t2-own"}) {
			t.Fatalf("t2 sees %v; want [t2-own] (consumed gap mock must not reappear)", got)
		}
	})

	httpIx := &integrations.MockIndex{
		Keys: func(mk *models.Mock) []string {
			if mk == nil || mk.Kind != models.HTTP {
				return nil
			}
			return []string{"http-get"}
		},
	}
	keyedPerTestNames := func(t *testing.T, mm *MockManager) []string {
		t.Helper()
		var names []string
		if err := mm.RangePerTestMocksWithKey(httpIx, "http-get", func(mk *models.Mock) bool {
			names = append(names, mk.Name)
			return true
		}); err != nil {
			t.Fatalf("RangePerTestMocksWithKey: %v", err)
		}
		sort.Strings(names)
		return names
	}

	t.Run("unconsumed during predecessor window hands off once to immediate successor and never to t3", func(t *testing.T) {
		mm := NewMockManager(nil, nil, zap.NewNop())
		defer mm.Close()
		mm.ResetForReplaySession()
		mm.SeedStartupCutoff(pms(1000))
		mm.SeedRecordedWindows(windows)
		mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())

		t1Own := inFlightMock("t1-own", 1010, 1020)
		gapCall := gapStampedMock("gap-http", 1500, 1600)
		mm.SetMocksWithWindow([]*models.Mock{t1Own, gapCall}, nil, pms(1000), pms(1100))
		if got := keyedPerTestNames(t, mm); !equalStrings(got, []string{"gap-http", "t1-own"}) {
			t.Fatalf("t1 keyed walk sees %v; want [gap-http t1-own]", got)
		}
		for _, mk := range mustPerTest(t, mm) {
			if mk.Name == "gap-http" && !mm.OwnedByOpenTest(mk) {
				t.Fatal("t1 OwnedByOpenTest(gap-http) = false; want true")
			}
		}

		// Advance to t2 WITHOUT consuming gap-http or t1-own:
		// gap-http hands off once; t1-own (in-window) MUST NOT hand off!
		t2Own := inFlightMock("t2-own", 2010, 2020)
		mm.SetMocksWithWindow([]*models.Mock{t2Own}, nil, pms(2000), pms(2100))
		if got := perTestInWindowNames(mm); !equalStrings(got, []string{"gap-http", "t2-own"}) {
			t.Fatalf("t2 sees %v; want [gap-http t2-own]", got)
		}
		if got := keyedPerTestNames(t, mm); !equalStrings(got, []string{"gap-http", "t2-own"}) {
			t.Fatalf("t2 keyed walk sees %v; want [gap-http t2-own]", got)
		}
		for _, mk := range mustPerTest(t, mm) {
			if mk.Name == "gap-http" && !mm.OwnedByOpenTest(mk) {
				t.Fatal("t2 OwnedByOpenTest(gap-http) = false; want true")
			}
		}

		// Advance to t3 without consuming gap-http: must NOT leak past t2.
		t3Own := inFlightMock("t3-own", 3010, 3020)
		mm.SetMocksWithWindow([]*models.Mock{t3Own}, nil, pms(3000), pms(3100))
		if got := perTestInWindowNames(mm); !equalStrings(got, []string{"t3-own"}) {
			t.Fatalf("t3 sees %v; want [t3-own] (gap mock must not leak beyond immediate successor)", got)
		}
		if got := keyedPerTestNames(t, mm); !equalStrings(got, []string{"t3-own"}) {
			t.Fatalf("t3 keyed walk sees %v; want [t3-own]", got)
		}
	})

	t.Run("MarkMockAsUsed-consumed gap mock is not handed off to successor", func(t *testing.T) {
		mm := NewMockManager(nil, nil, zap.NewNop())
		defer mm.Close()
		mm.ResetForReplaySession()
		mm.SeedStartupCutoff(pms(1000))
		mm.SeedRecordedWindows(windows)
		mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())

		t1Own := inFlightMock("t1-own", 1010, 1020)
		gapCall := gapStampedMock("gap-used", 1500, 1600)
		mm.SetMocksWithWindow([]*models.Mock{t1Own, gapCall}, nil, pms(1000), pms(1100))

		// MarkMockAsUsed records persistent consumption without removing the
		// entry from the tree; handoff must still respect it.
		if !mm.MarkMockAsUsed(*gapCall) {
			t.Fatal("MarkMockAsUsed(gap-used) returned false")
		}

		t2Own := inFlightMock("t2-own", 2010, 2020)
		mm.SetMocksWithWindow([]*models.Mock{t2Own}, nil, pms(2000), pms(2100))
		if got := perTestInWindowNames(mm); !equalStrings(got, []string{"t2-own"}) {
			t.Fatalf("t2 sees %v; want [t2-own] (MarkMockAsUsed gap mock must not hand off)", got)
		}
	})

	t.Run("SetCurrentTestWindow preserves handedOffGap for same start and clears on move", func(t *testing.T) {
		mm := NewMockManager(nil, nil, zap.NewNop())
		defer mm.Close()
		mm.ResetForReplaySession()
		mm.SeedStartupCutoff(pms(1000))
		mm.SeedRecordedWindows(windows)
		mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())

		gapCall := gapStampedMock("gap-http", 1500, 1600)
		mm.SetMocksWithWindow([]*models.Mock{gapCall}, nil, pms(1000), pms(1100))
		t2Own := inFlightMock("t2-own", 2010, 2020)
		mm.SetMocksWithWindow([]*models.Mock{t2Own}, nil, pms(2000), pms(2100))
		if got := perTestInWindowNames(mm); !equalStrings(got, []string{"gap-http", "t2-own"}) {
			t.Fatalf("t2 sees %v; want [gap-http t2-own]", got)
		}

		// Same start preserves handedOffGap.
		mm.SetCurrentTestWindow(pms(2000), pms(2150))
		if got := perTestInWindowNames(mm); !equalStrings(got, []string{"gap-http", "t2-own"}) {
			t.Fatalf("after same-start SetCurrentTestWindow: sees %v; want [gap-http t2-own]", got)
		}

		// Moving window via SetCurrentTestWindow must clear handedOffGap.
		mm.SetCurrentTestWindow(pms(3000), pms(3100))
		if got := perTestInWindowNames(mm); len(got) != 0 {
			t.Fatalf("after SetCurrentTestWindow(t3): sees %v; want empty", got)
		}
	})

	t.Run("concurrent ClaimForRepeats and OwnedByOpenTest with SetMocksWithWindow never deadlocks", func(t *testing.T) {
		mm := NewMockManager(nil, nil, zap.NewNop())
		defer mm.Close()
		mm.ResetForReplaySession()
		mm.SeedStartupCutoff(pms(1000))
		mm.SeedRecordedWindows(windows)
		mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())

		gapCall := gapStampedMock("gap-race", 1500, 1600)
		t2Own := inFlightMock("t2-own", 2010, 2020)

		done := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			for i := 0; i < 60; i++ {
				mm.SetMocksWithWindow([]*models.Mock{gapCall.ShallowCopy()}, nil, pms(1000), pms(1100))
				mm.SetMocksWithWindow([]*models.Mock{t2Own.ShallowCopy()}, nil, pms(2000), pms(2100))
			}
			close(done)
		}()
		go func() {
			defer wg.Done()
			for {
				select {
				case <-done:
					return
				default:
					if mocks, err := mm.GetPerTestMocksInWindow(); err == nil {
						for _, mk := range mocks {
							_ = mm.OwnedByOpenTest(mk)
							_ = mm.ClaimForRepeats(*mk)
						}
					}
					_ = mm.consumedOutOfWindow(*gapCall)
				}
			}
		}()

		finished := make(chan struct{})
		go func() {
			wg.Wait()
			close(finished)
		}()
		select {
		case <-finished:
		case <-time.After(10 * time.Second):
			t.Fatal("deadlock detected between ClaimForRepeats/OwnedByOpenTest and SetMocksWithWindow")
		}
	})
}

func mustPerTest(t *testing.T, mm *MockManager) []*models.Mock {
	t.Helper()
	got, err := mm.GetPerTestMocksInWindow()
	if err != nil {
		t.Fatalf("GetPerTestMocksInWindow: %v", err)
	}
	return got
}
