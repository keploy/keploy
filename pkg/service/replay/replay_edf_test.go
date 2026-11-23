package replay

import (
	"slices"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

func httpWindowTC(name string, start, end time.Time) *models.TestCase {
	return &models.TestCase{
		Name:     name,
		Kind:     models.HTTP,
		HTTPReq:  models.HTTPReq{Timestamp: start},
		HTTPResp: models.HTTPResp{Timestamp: end},
	}
}

func grpcWindowTC(name string, start, end time.Time) *models.TestCase {
	return &models.TestCase{
		Name:     name,
		Kind:     models.GRPC_EXPORT,
		GrpcReq:  models.GrpcReq{Timestamp: start},
		GrpcResp: models.GrpcResp{Timestamp: end},
	}
}

func tcNames(tcs []*models.TestCase) []string {
	out := make([]string, len(tcs))
	for i, tc := range tcs {
		out[i] = tc.Name
	}
	return out
}

func TestOrderActiveTestCasesForWindowReplay(t *testing.T) {
	base := time.Date(2026, 11, 23, 7, 0, 0, 0, time.UTC)
	ms := func(n int) time.Time { return base.Add(time.Duration(n) * time.Millisecond) }

	t.Run("non-overlapping and partially-overlapping preserve Start order", func(t *testing.T) {
		tcs := []*models.TestCase{
			httpWindowTC("t1-causal", ms(0), ms(5)),
			httpWindowTC("t2-partial-a", ms(10), ms(25)),
			httpWindowTC("t3-partial-b", ms(15), ms(30)),
			httpWindowTC("t4-after", ms(40), ms(50)),
		}
		orderActiveTestCasesForWindowReplay(tcs, false, true)
		want := []string{"t1-causal", "t2-partial-a", "t3-partial-b", "t4-after"}
		if got := tcNames(tcs); !slices.Equal(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	})

	t.Run("strictly nested tests replay inner before outer", func(t *testing.T) {
		// Mimics get-orders-by-id-304 enclosing 303 and 3-level deep nesting.
		tcs := []*models.TestCase{
			httpWindowTC("outer", ms(0), ms(100)),
			httpWindowTC("mid", ms(10), ms(60)),
			httpWindowTC("inner", ms(20), ms(40)),
			httpWindowTC("right-overlap", ms(30), ms(120)),
		}
		orderActiveTestCasesForWindowReplay(tcs, false, true)
		want := []string{"inner", "mid", "outer", "right-overlap"}
		if got := tcNames(tcs); !slices.Equal(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	})

	t.Run("equal End tie-breaks by Start then preserves original order", func(t *testing.T) {
		tcs := []*models.TestCase{
			httpWindowTC("same-a", ms(10), ms(50)),
			httpWindowTC("earlier-start", ms(5), ms(50)),
			httpWindowTC("same-b", ms(10), ms(50)),
		}
		orderActiveTestCasesForWindowReplay(tcs, false, true)
		want := []string{"earlier-start", "same-a", "same-b"}
		if got := tcNames(tcs); !slices.Equal(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	})

	t.Run("missing or skewed response timestamp follows recorded window end", func(t *testing.T) {
		// recordReqResTimestamps gives a zero HTTPResp.Timestamp a 1s window
		// [20ms, 1020ms] (ending after outer [0, 100ms]), while a skewed
		// response timestamp (End < Start) clamps to Start (30ms).
		tcs := []*models.TestCase{
			httpWindowTC("outer", ms(0), ms(100)),
			httpWindowTC("zero-resp", ms(20), time.Time{}),
			httpWindowTC("skewed-resp", ms(30), ms(10)),
			grpcWindowTC("grpc-inner", ms(25), ms(40)),
		}
		orderActiveTestCasesForWindowReplay(tcs, false, true)
		want := []string{"skewed-resp", "grpc-inner", "outer", "zero-resp"}
		if got := tcNames(tcs); !slices.Equal(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	})

	t.Run("disabled under exact serving or when strictMockWindow is off", func(t *testing.T) {
		for _, tc := range []struct {
			name        string
			exactActive bool
			strictWin   bool
		}{
			{"exact serving on", true, true},
			{"strictMockWindow off", false, false},
			{"exact on and strict off", true, false},
		} {
			t.Run(tc.name, func(t *testing.T) {
				tcs := []*models.TestCase{
					httpWindowTC("outer", ms(0), ms(100)),
					httpWindowTC("inner", ms(20), ms(40)),
				}
				orderActiveTestCasesForWindowReplay(tcs, tc.exactActive, tc.strictWin)
				want := []string{"outer", "inner"}
				if got := tcNames(tcs); !slices.Equal(got, want) {
					t.Fatalf("got %v, want %v", got, want)
				}
			})
		}
	})

	t.Run("EDF with FIFO solves both nested and right-overlapping bursts without starvation", func(t *testing.T) {
		// Combine both failure geometries under 96-VU noise:
		//  - outer [0, 100] with mock at 80ms enclosing inner [20, 50] with mock at 30ms
		//    plus unrelated concurrent requests starting between 30ms and 80ms;
		//  - right-overlapping [40, 120] with mock at 90ms.
		type reqSpec struct {
			tc     *models.TestCase
			mockAt time.Time
		}
		specs := []reqSpec{
			{httpWindowTC("outer", ms(0), ms(100)), ms(80)},
			{httpWindowTC("inner", ms(20), ms(50)), ms(30)},
			{httpWindowTC("right", ms(40), ms(120)), ms(90)},
		}
		tcs := []*models.TestCase{specs[0].tc, specs[1].tc, specs[2].tc}
		mockTimes := []time.Time{ms(30), ms(80), ms(90)} // recorded in timestamp order

		// Verify Start order + FIFO starves "inner".
		used := make([]bool, len(mockTimes))
		startMisses := 0
		for _, tc := range tcs {
			s, e := recordReqResTimestamps(tc)
			matched := false
			for i, mt := range mockTimes {
				if !used[i] && !mt.Before(s) && !mt.After(e) {
					used[i] = true
					matched = true
					break
				}
			}
			if !matched {
				startMisses++
			}
		}
		if startMisses == 0 {
			t.Fatalf("expected Start order + FIFO to starve inner, got 0 misses")
		}

		// Under EDF + FIFO, every test matches with 0 misses.
		orderActiveTestCasesForWindowReplay(tcs, false, true)
		used = make([]bool, len(mockTimes))
		for _, tc := range tcs {
			s, e := recordReqResTimestamps(tc)
			matched := false
			for i, mt := range mockTimes {
				if !used[i] && !mt.Before(s) && !mt.After(e) {
					used[i] = true
					matched = true
					break
				}
			}
			if !matched {
				t.Fatalf("EDF + FIFO missed test %s [%v..%v]", tc.Name, s, e)
			}
		}
	})
}
