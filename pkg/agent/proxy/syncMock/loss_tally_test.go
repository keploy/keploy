package manager

import (
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// givenUpWindow opens a window on mgr and has the hold's budget give it up,
// its verdict not made yet.
func givenUpWindow(t *testing.T, mgr *SyncMockManager) *Window {
	t.Helper()
	start := time.Now().Add(-30 * time.Second)
	w := mgr.OpenWindow(start, nil)
	for i := 0; i < maxHeldMocks+5; i++ {
		mgr.AddMock(boundMockAt(start.Add(time.Second + time.Duration(i)*time.Microsecond)))
	}
	n := time.Now()
	mgr.ResolveRange(n, n, "test-n", true, true)
	if !givenUp(w) {
		t.Fatal("fixture: the window was not given up")
	}
	return w
}

// leftOutWindow is a kept request left out of the recording: its window given
// up, then kept, so its loss is told.
func leftOutWindow(t *testing.T, mgr *SyncMockManager) *Window {
	t.Helper()
	w := givenUpWindow(t, mgr)
	if w.Keep() {
		t.Fatal("fixture: a given-up window was kept")
	}
	w.Close()
	return w
}

// A tally counts the losses told while it is open, less those of them taken
// back, and nothing else: not a loss told before it opened, nor that loss's
// take-back, which a deduper that outlives a session can make in a later one;
// not a loss told after it closed. Tallies open side by side each count
// exactly their own.
func TestLossTallyCountsTheLossesOfItsOwnSession(t *testing.T) {
	t.Parallel()
	mgr := openWindowManager(make(chan *models.Mock, 16), make(chan models.TestMockMapping, 16))
	check := func(step string, tally *LossTally, want uint64) {
		t.Helper()
		if got := tally.LeftOut(); got != want {
			t.Fatalf("%s: LeftOut = %d, want %d", step, got, want)
		}
	}

	before := leftOutWindow(t, mgr) // told before any session
	a := mgr.OpenLossTally()
	check("a opened after a loss", a, 0)
	inA := leftOutWindow(t, mgr)
	b := mgr.OpenLossTally() // a second session beside the first
	inBoth := leftOutWindow(t, mgr)
	check("a after two losses", a, 2)
	check("b after one", b, 1)
	both := leftOutWindow(t, mgr)
	both.Replaced() // told and taken back with both open: off both
	check("a after a loss told and taken back beside b", a, 2)
	check("b after a loss told and taken back beside a", b, 1)

	before.Replaced() // an earlier session's loss, taken back now: on neither
	check("a after an earlier loss's take-back", a, 2)
	check("b after an earlier loss's take-back", b, 1)

	inA.Replaced()
	inA.Replaced() // taken back once
	check("a after its first loss's take-back", a, 1)
	check("b after a loss told before it opened was taken back", b, 1)

	a.Close()
	afterA := leftOutWindow(t, mgr)
	check("a after it closed", a, 1)
	check("b after a loss told with a closed", b, 2)
	inBoth.Replaced()
	check("a, closed, after a loss it counted was taken back", a, 1)
	check("b after a loss it counted with a was taken back", b, 1)
	afterA.Replaced()
	check("b after every loss it counted was taken back", b, 0)

	b.Close()
	b.Close()
	if n := mgr.OpenLossTallies(); n != 0 {
		t.Fatalf("%d tallies open after both closed", n)
	}
	if n := mgr.KeptRequestsLeftOut(); n != 0 {
		t.Fatalf("KeptRequestsLeftOut = %d after every loss was taken back", n)
	}

	var none *LossTally
	none.Close()
	var noMgr *SyncMockManager
	if none.LeftOut() != 0 || noMgr.OpenLossTally() != nil || noMgr.OpenLossTallies() != 0 {
		t.Fatal("a nil tally or manager counted something")
	}
}

// Losses told and taken back while tallies open and close beside them: a tally
// open throughout counts every loss and every take-back, one that opens and
// closes on the way never reads more losses than were told, and no count
// wraps around.
func TestLossTallyUnderConcurrentLossesAndSessions(t *testing.T) {
	t.Parallel()
	mgr := openWindowManager(make(chan *models.Mock, 16), make(chan models.TestMockMapping, 16))
	const losses = 8
	var ws []*Window
	for i := 0; i < losses; i++ {
		ws = append(ws, givenUpWindow(t, mgr))
	}
	whole := mgr.OpenLossTally()

	stop := make(chan struct{})
	var sessions sync.WaitGroup
	var bad sync.Once
	var badRead uint64
	for g := 0; g < 4; g++ {
		sessions.Add(1)
		go func() {
			defer sessions.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				s := mgr.OpenLossTally()
				if n := s.LeftOut(); n > losses {
					bad.Do(func() { badRead = n })
				}
				s.Close()
				if n := s.LeftOut(); n > losses {
					bad.Do(func() { badRead = n })
				}
			}
		}()
	}
	var told sync.WaitGroup
	for i, w := range ws {
		told.Add(1)
		go func() {
			defer told.Done()
			if w.Keep() {
				t.Error("fixture: a given-up window was kept")
				return
			}
			w.Close()
			if i%2 == 0 {
				w.Replaced()
			}
		}()
	}
	told.Wait()
	close(stop)
	sessions.Wait()

	if badRead != 0 {
		t.Fatalf("a tally read %d losses, more than the %d told", badRead, losses)
	}
	if got := whole.LeftOut(); got != losses/2 {
		t.Fatalf("a tally open throughout reads %d, want %d: %d told, %d taken back", got, losses/2, losses, losses/2)
	}
	whole.Close()
	if n := mgr.OpenLossTallies(); n != 0 {
		t.Fatalf("%d tallies open after every session closed", n)
	}
}
