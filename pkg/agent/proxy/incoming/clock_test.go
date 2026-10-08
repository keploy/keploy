package proxy

import (
	"sync"
	"testing"
	"time"

	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.uber.org/zap"
)

// waitPastYield returns once the clock reads later than the yield, waiting as
// long as it takes the clock to get there and no longer: at once on a clock
// past it already, or for a window that claims nothing; without a sleep on a
// clock of microseconds (macOS's wall clock), which passes it in a few reads;
// through as many ticks as a clock that moves only at a timer's tick (Windows')
// is behind it. A wall clock set back behind the yield does not get there
// soon: past clockWaitMax it gives up rather than hold the lock. Its sleeps
// start at clockPoll and double up to clockPollMax, so a wait of a whole tick
// wakes a few dozen times at most.
//
// Every stub here moves on by windowsTick as waitPastYield sleeps. That it
// waits for both of a reading's clocks, the wall one and the monotonic one,
// is not tested: a stubbed time carries no monotonic reading.
func TestWaitPastYield(t *testing.T) {
	yield := time.Now().Round(0)
	for _, tc := range []struct {
		name    string
		claims  bool          // a window, else nil
		behind  time.Duration // how far the clock reads behind the yield
		res     time.Duration // the clock's resolution: what its readings are cut down to (0: none)
		perRead time.Duration // how far time moves on between two readings
		sleeps  int           // the sleeps it takes
		past    bool          // the clock reads past the yield once it returns
	}{
		{name: "a window that claims nothing", claims: false, sleeps: 0, past: false},
		{name: "a clock past the yield already", claims: true, behind: -time.Nanosecond, sleeps: 0, past: true},
		{name: "a clock at the yield", claims: true, sleeps: 1, past: true},
		{name: "a clock a tick and a half behind it", claims: true, behind: windowsTick + windowsTick/2, sleeps: 2, past: true},
		{name: "a clock of microseconds read every 30 ns", claims: true, res: time.Microsecond, perRead: 30 * time.Nanosecond, sleeps: 0, past: true},
		{name: "a wall clock set back an hour", claims: true, behind: time.Hour, sleeps: int((clockWaitMax + windowsTick - 1) / windowsTick), past: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			at := yield
			if tc.res > 0 {
				at = at.Truncate(tc.res) // read off that clock
			}
			// Guarded: an earlier test's read-ahead may still be stamping
			// its connection's close.
			var mu sync.Mutex
			current, last, sleeps := at.Add(-tc.behind), time.Time{}, 0
			var naps []time.Duration
			reading := func() time.Time {
				if tc.res > 0 {
					return current.Truncate(tc.res)
				}
				return current
			}
			prev := stubbedClock.Swap(&ingressClock{
				now: func() time.Time {
					mu.Lock()
					defer mu.Unlock()
					last = reading()
					current = current.Add(tc.perRead)
					return last
				},
				sleep: func(d time.Duration) {
					mu.Lock()
					defer mu.Unlock()
					naps = append(naps, d)
					sleeps++
					current = current.Add(windowsTick)
				},
			})
			t.Cleanup(func() { stubbedClock.Store(prev) })
			var win *syncMock.Window
			if tc.claims {
				win = syncMock.New(zap.NewNop()).OpenWindow(at.Add(-time.Second), nil)
				t.Cleanup(win.Close)
			}

			waitPastYield(win, at)

			mu.Lock()
			defer mu.Unlock()
			if sleeps != tc.sleeps {
				t.Errorf("it slept %d times, want %d", sleeps, tc.sleeps)
			}
			for i, want := 0, clockPoll; i < len(naps); i, want = i+1, min(2*want, clockPollMax) {
				if naps[i] != want {
					t.Errorf("its sleeps were %v, want them to start at %v and double up to %v", naps, clockPoll, clockPollMax)
					break
				}
			}
			if !tc.claims {
				last = reading()
			}
			if got := last.After(at); got != tc.past {
				t.Errorf("the clock it last read is past the yield: %v, want %v", got, tc.past)
			}
		})
	}
}
