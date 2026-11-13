package proxy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// untilTrue waits until cond holds. The conditions below are states the drain
// goroutine reaches by itself (it took an error off the channel, a marker is
// queued); waiting on them is waiting on an event, so no clock bounds it. It
// sleeps between looks rather than spinning, so a starved runner's other
// goroutines get the CPU; go test's -timeout bounds a state never reached.
func untilTrue(t *testing.T, cond func() bool) {
	t.Helper()
	for !cond() {
		time.Sleep(100 * time.Microsecond)
	}
}

// holdDrain parks the running drain on captureMu, the lock it takes to route
// every error, and returns the release. Nothing the drain needs from a sender
// is held: this is the drain falling behind, as it does on a starved machine.
func holdDrain(t *testing.T, p *Proxy) func() {
	t.Helper()
	p.captureMu.Lock()
	released := false
	release := func() {
		if !released {
			released = true
			p.captureMu.Unlock()
		}
	}
	t.Cleanup(release)
	return release
}

func startedDrainProxy(t *testing.T, capacity int) *Proxy {
	t.Helper()
	return startedDrainProxyLogging(t, capacity, zap.NewNop())
}

// startedDrainProxyLogging is startedDrainProxy with the logger the test
// reads. The logger is set before the drain starts, since the drain's
// goroutine reads it: swapping it afterwards is a data race.
func startedDrainProxyLogging(t *testing.T, capacity int, logger *zap.Logger) *Proxy {
	t.Helper()
	p := &Proxy{logger: logger, errChannel: make(chan error, capacity)}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	p.StartErrorDrain(ctx)
	return p
}

// A burst of misses larger than the error channel while the drain is behind.
// Every miss waits for the drain and reaches the test's list, with the proven
// switch on or off. With it off the overflow was dropped with a log line and
// the test's list came back short with no error: a silent pass, and a test
// auto-noise could then learn from.
func TestSendError_NeverDropsAMiss(t *testing.T) {
	const capacity, burst = 2, 6
	eitherSwitch(t, func(t *testing.T) {
		p := startedDrainProxy(t, capacity)
		p.BeginTestErrorCapture()
		release := holdDrain(t, p)

		sent := make(chan struct{})
		go func() {
			defer close(sent)
			for i := 0; i < burst; i++ {
				p.SendError(mockMiss(fmt.Sprintf("GET /%d", i)))
			}
		}()
		// The drain holds one error and the channel is full: the rest of the
		// burst is waiting, not dropped. A burst that returned while the drain
		// is held dropped what did not fit.
		untilTrue(t, func() bool {
			select {
			case <-sent:
				return true
			default:
				return len(p.errChannel) == capacity
			}
		})
		select {
		case <-sent:
			t.Fatal("every SendError returned while the drain was held: what did not fit was dropped")
		default:
		}
		release()
		<-sent

		got, err := p.GetMockErrors(context.Background())
		if err != nil {
			t.Fatalf("GetMockErrors: %v", err)
		}
		if len(got) != burst {
			t.Fatalf("got %d of %d misses: a miss was dropped for want of room", len(got), burst)
		}
	})
}

// eitherSwitch runs f with the proven switch on and with it off: a miss is
// reported to its test alike either way.
func eitherSwitch(t *testing.T, f func(t *testing.T)) {
	for _, on := range []bool{true, false} {
		name := "switch off"
		v := ""
		if on {
			name, v = "switch on", "1"
		}
		t.Run(name, func(t *testing.T) {
			t.Setenv(provenswitch.Env, v)
			f(t)
		})
	}
}

// The read waits for the drain on events only, with the proven switch on or
// off. A read its
// caller gives up on before the drain routed every miss is incomplete, and says
// so. The drain here is stalled for good (it never takes the miss queued ahead
// of the read's marker), so the caller giving up is the only event the read can
// end on: the outcome cannot depend on which of two ready events it sees first.
func TestGetMockErrors_ReportsAnIncompleteRead(t *testing.T) {
	eitherSwitch(t, func(t *testing.T) {
		p := &Proxy{logger: zap.NewNop(), errChannel: make(chan error, 2)}
		p.errDrainActive.Store(true) // a drain that stalled: active, never taking
		p.errDrainDone = make(chan struct{})
		p.BeginTestErrorCapture()
		p.errChannel <- mockMiss("GET /a")

		ctx, cancel := context.WithCancel(context.Background())
		type read struct {
			got []models.UnmatchedCall
			err error
		}
		done := make(chan read, 1)
		go func() {
			got, err := p.GetMockErrors(ctx)
			done <- read{got, err}
		}()
		untilTrue(t, func() bool { return len(p.errChannel) == 2 }) // the flush marker is queued behind the miss
		cancel()

		r := <-done
		if !errors.Is(r.err, models.ErrUnmatchedCallsUnread) {
			t.Fatalf("GetMockErrors = %v; a read given up before the drain routed every miss must say it is incomplete", r.err)
		}
	})
}

// The same read, not given up: it waits for the drain to catch up, on the
// drain reaching its marker, and is complete.
func TestGetMockErrors_ReadIsCompleteOnceTheDrainCatchesUp(t *testing.T) {
	eitherSwitch(t, func(t *testing.T) {
		p := startedDrainProxy(t, 4)
		p.BeginTestErrorCapture()
		release := holdDrain(t, p)

		p.SendError(mockMiss("GET /a"))
		untilTrue(t, func() bool { return len(p.errChannel) == 0 })
		done := make(chan error, 1)
		var got []models.UnmatchedCall
		go func() {
			var err error
			got, err = p.GetMockErrors(context.Background())
			done <- err
		}()
		untilTrue(t, func() bool { return len(p.errChannel) == 1 })
		release()
		if err := <-done; err != nil {
			t.Fatalf("GetMockErrors: %v", err)
		}
		if len(got) != 1 || got[0].ActualSummary != "GET /a" {
			t.Fatalf("got %+v, want the one miss", got)
		}
	})
}

// A miss that cannot be queued at all (no drain, channel full) is lost; the
// next read says it is incomplete instead of coming back short.
func TestGetMockErrors_ReportsALostMiss(t *testing.T) {
	eitherSwitch(t, func(t *testing.T) {
		p := &Proxy{logger: zap.NewNop(), errChannel: make(chan error, 1)} // no drain
		p.SendError(mockMiss("GET /kept"))
		p.SendError(mockMiss("GET /lost"))
		got, err := p.GetMockErrors(context.Background())
		if !errors.Is(err, models.ErrUnmatchedCallsUnread) || !strings.Contains(err.Error(), missLostNoDrainChanFull) {
			t.Fatalf("GetMockErrors = %v with a miss lost; want ErrUnmatchedCallsUnread naming the full channel", err)
		}
		if len(got) != 1 || got[0].ActualSummary != "GET /kept" {
			t.Fatalf("got %+v; the read must still return what it read", got)
		}
		// Reported once: the next read starts clean.
		if _, err := p.GetMockErrors(context.Background()); err != nil {
			t.Fatalf("the loss was reported again on the next read: %v", err)
		}
	})
}

// A miss the bounded list of its test's window has no room for
// (maxPendingMockErrors), alike to none it holds (missAlikeKey), is kept on
// the agent's disk (missSpill) and read back: the read lists every miss, and
// is complete.
func TestGetMockErrors_KeepsAMissPastTheListOnDisk(t *testing.T) {
	eitherSwitch(t, func(t *testing.T) {
		p := startedDrainProxy(t, maxPendingMockErrors+2)
		p.BeginTestErrorCapture()
		for i := 0; i <= maxPendingMockErrors; i++ {
			p.SendError(distinctMiss(i))
		}
		got, err := p.GetMockErrors(context.Background())
		if err != nil || len(got) != maxPendingMockErrors+1 {
			t.Fatalf("read %d misses, err %v; want all %d, complete", len(got), err, maxPendingMockErrors+1)
		}
		if last := got[maxPendingMockErrors]; last.ClosestMock != fmt.Sprintf("mock-%d", maxPendingMockErrors) {
			t.Fatalf("the miss kept on the disk was read back as %+v", last)
		}
	})
}

// Past the disk's bound too, a miss is dropped, and not silently: the read
// says it is incomplete, naming the full list and why the disk took no more,
// with what it read, and the agent says the loss at Warn, once, with what to
// do.
func TestGetMockErrors_ReportsAMissPastTheDiskBound(t *testing.T) {
	eitherSwitch(t, func(t *testing.T) {
		noSpillRoom(t)
		core, logs := observer.New(zap.WarnLevel)
		p := startedDrainProxyLogging(t, maxPendingMockErrors+4, zap.New(core))
		p.BeginTestErrorCapture()
		for i := 0; i <= maxPendingMockErrors+1; i++ {
			p.SendError(distinctMiss(i))
		}
		got, err := p.GetMockErrors(context.Background())
		// The cause is the full list, not a stopped drain: the drain ran.
		if !errors.Is(err, models.ErrUnmatchedCallsUnread) || !strings.Contains(err.Error(), missLostListFull+": they reached the 0 MiB") || strings.Contains(err.Error(), "drain") {
			t.Fatalf("GetMockErrors = %v with misses past the disk's bound; want ErrUnmatchedCallsUnread naming the full list and the disk's bound", err)
		}
		if len(got) != maxPendingMockErrors {
			t.Fatalf("got %d misses, want the %d the window held", len(got), maxPendingMockErrors)
		}
		said := logs.FilterMessageSnippet("a miss was lost").All()
		if len(said) != 1 || said[0].ContextMap()["next_step"] != missLostNextStep {
			t.Fatalf("the loss was said %d times (%+v); want once at Warn, with what to do", len(said), said)
		}
	})
}

// flushDrain waits until the drain has routed every error queued before now:
// it takes errors in order, so once it reaches a marker queued after them it
// has routed each.
func flushDrain(p *Proxy) {
	m := &flushMarker{done: make(chan struct{})}
	p.errChannel <- m
	<-m.done
}

// overflowPending makes one more miss than keploy keeps while no test's
// window is open, each alike to no other, with the drain routing them, and
// returns once it has.
func overflowPending(p *Proxy) {
	for i := 0; i <= maxPendingMockErrors; i++ {
		p.SendError(distinctMiss(i))
	}
	flushDrain(p)
}

// Misses made while no test's window is open, past the list's room, are kept
// on the agent's disk and are the set's first test's as the rest are: with
// the proven switch off they are discarded, said, and no read is made
// incomplete by them; under the switch the first test reads every one, marked
// as made before the set, complete. Past the disk's bound they are dropped:
// with the switch off the drop is said; under it the first test's read says
// it is short of them.
func TestBeginTestErrorCapture_SaysAPendingOverflowAndJudgesTheTest(t *testing.T) {
	for _, full := range []bool{false, true} {
		t.Run(fmt.Sprintf("disk full %v", full), func(t *testing.T) {
			eitherSwitch(t, func(t *testing.T) {
				if full {
					noSpillRoom(t)
				}
				// The logger is the proxy's before its drain starts: the drain reads it.
				core, logs := observer.New(zap.WarnLevel)
				p := &Proxy{logger: zap.New(core), errChannel: make(chan error, 8)}
				ctx, cancel := context.WithCancel(context.Background())
				t.Cleanup(cancel)
				p.StartErrorDrain(ctx)
				overflowPending(p)
				p.BeginTestErrorCapture()
				got, err := p.GetMockErrors(context.Background())
				kept := maxPendingMockErrors + 1
				if full {
					kept = maxPendingMockErrors
				}
				if provenswitch.On() {
					if full != errors.Is(err, models.ErrUnmatchedCallsUnread) || (full && !strings.Contains(err.Error(), missLostBeforeSetFull)) {
						t.Fatalf("the set's first test's read err %v; want it short of the misses made before the set only past the disk's bound", err)
					}
					if len(got) != kept || !got[0].BeforeSet || !got[len(got)-1].BeforeSet {
						t.Fatalf("the set's first test read %d misses (first %+v); want the %d kept, marked as the set's", len(got), got[0], kept)
					}
					return
				}
				if err != nil || len(got) != 0 {
					t.Fatalf("the set's first test read %d misses, err %v; want none, complete", len(got), err)
				}
				if n := len(logs.FilterMessageSnippet("than keploy keeps; the rest were dropped").All()); n != map[bool]int{false: 0, true: 1}[full] {
					t.Fatalf("the drop was said %d times; want it said once, past the disk's bound only", n)
				}
			})
		})
	}
}

// Misses made between two tests, past the list's room, are kept on the
// agent's disk and are the next test's; past the disk's bound, its read says
// it is incomplete, and why.
func TestContinueTestErrorCapture_ReportsAPendingOverflow(t *testing.T) {
	for _, full := range []bool{false, true} {
		t.Run(fmt.Sprintf("disk full %v", full), func(t *testing.T) {
			eitherSwitch(t, func(t *testing.T) {
				if full {
					noSpillRoom(t)
				}
				p := startedDrainProxy(t, 8)
				overflowPending(p)
				p.ContinueTestErrorCapture()
				got, err := p.GetMockErrors(context.Background())
				if !full {
					if err != nil || len(got) != maxPendingMockErrors+1 {
						t.Fatalf("read %d misses, err %v; want all %d, complete", len(got), err, maxPendingMockErrors+1)
					}
					return
				}
				if !errors.Is(err, models.ErrUnmatchedCallsUnread) || !strings.Contains(err.Error(), missLostBetweenFull) {
					t.Fatalf("GetMockErrors = %v; want ErrUnmatchedCallsUnread naming the full between-tests list", err)
				}
				if len(got) != maxPendingMockErrors {
					t.Fatalf("got %d misses, want the %d kept", len(got), maxPendingMockErrors)
				}
				if _, err := p.GetMockErrors(context.Background()); err != nil {
					t.Fatalf("the loss was reported again on the next read: %v", err)
				}
			})
		})
	}
}

// With no window ever opened, a read takes the kept misses, those kept on the
// agent's disk too; past the disk's bound, it says it is incomplete.
func TestGetMockErrors_ReportsAPendingOverflowWithNoWindow(t *testing.T) {
	for _, full := range []bool{false, true} {
		t.Run(fmt.Sprintf("disk full %v", full), func(t *testing.T) {
			eitherSwitch(t, func(t *testing.T) {
				if full {
					noSpillRoom(t)
				}
				p := startedDrainProxy(t, 8)
				overflowPending(p)
				got, err := p.GetMockErrors(context.Background())
				if !full {
					if err != nil || len(got) != maxPendingMockErrors+1 {
						t.Fatalf("read %d misses, err %v; want all %d, complete", len(got), err, maxPendingMockErrors+1)
					}
					return
				}
				if !errors.Is(err, models.ErrUnmatchedCallsUnread) || !strings.Contains(err.Error(), missLostBetweenFull) {
					t.Fatalf("GetMockErrors = %v; want ErrUnmatchedCallsUnread naming the full list", err)
				}
				if len(got) != maxPendingMockErrors {
					t.Fatalf("got %d misses, want the %d kept", len(got), maxPendingMockErrors)
				}
			})
		})
	}
}

// A dropped miss that counts against no test is no loss: a DNS lookup's, which
// the proxy answers itself. `keploy mock` opens no test window with the switch
// off, so every miss of its run waits as one made between tests; a run with
// more DNS misses than keploy keeps would otherwise read as incomplete, and
// fail --strict for misses that never count. Past the list's room they are
// kept on the agent's disk; past its bound too, dropped, with no loss.
func TestAPendingOverflowOfDNSMissesIsNoLoss(t *testing.T) {
	for _, full := range []bool{false, true} {
		t.Run(fmt.Sprintf("disk full %v", full), func(t *testing.T) {
			eitherSwitch(t, func(t *testing.T) {
				if full {
					noSpillRoom(t)
				}
				p := startedDrainProxy(t, 8)
				for i := 0; i <= maxPendingMockErrors; i++ {
					p.SendError(dnsMiss(i))
				}
				flushDrain(p)
				got, err := p.GetMockErrors(context.Background())
				if err != nil {
					t.Fatalf("GetMockErrors = %v; dropped DNS misses count against no test, so the read is not short of one", err)
				}
				if want := map[bool]int{false: maxPendingMockErrors + 1, true: maxPendingMockErrors}[full]; len(got) != want {
					t.Fatalf("got %d misses, want the %d kept", len(got), want)
				}
			})
		})
	}
}

// dnsMiss is a DNS lookup's miss, alike to no other i's.
func dnsMiss(i int) models.ParserError {
	return models.ParserError{
		ParserErrorType: models.ErrMockNotFound,
		MismatchReport:  &models.MockMismatchReport{Protocol: "DNS", ActualSummary: fmt.Sprintf("A host%d.example", i), ClosestMock: fmt.Sprintf("dns-%d", i)},
	}
}

// The same in a test's window: a DNS miss its full list, and the disk past
// it, drop counts against no test, so the read is not short of one
// (noteMissLost's missCounts), with the proven switch on or off.
func TestAWindowOverflowOfDNSMissesIsNoLoss(t *testing.T) {
	for _, full := range []bool{false, true} {
		t.Run(fmt.Sprintf("disk full %v", full), func(t *testing.T) {
			eitherSwitch(t, func(t *testing.T) {
				if full {
					noSpillRoom(t)
				}
				p := startedDrainProxy(t, maxPendingMockErrors+2)
				p.BeginTestErrorCapture()
				for i := 0; i <= maxPendingMockErrors; i++ {
					p.SendError(dnsMiss(i))
				}
				got, err := p.GetMockErrors(context.Background())
				if err != nil {
					t.Fatalf("GetMockErrors = %v; a dropped DNS miss counts against no test, so the read is not short of one", err)
				}
				if want := map[bool]int{false: maxPendingMockErrors + 1, true: maxPendingMockErrors}[full]; len(got) != want {
					t.Fatalf("got %d misses, want the %d kept", len(got), want)
				}
			})
		})
	}
}

// A miss that carries no report (a protocol whose matcher emits none) says
// nothing of what it was, so it counts: one its test's full list, and the
// disk past it, drop is a loss, and the read says so.
func TestAWindowOverflowOfUnreportedMissesIsALoss(t *testing.T) {
	eitherSwitch(t, func(t *testing.T) {
		noSpillRoom(t)
		p := startedDrainProxy(t, maxPendingMockErrors+2)
		p.BeginTestErrorCapture()
		for i := 0; i <= maxPendingMockErrors; i++ {
			p.SendError(models.ParserError{ParserErrorType: models.ErrMockNotFound, Err: fmt.Errorf("call %d: %w", i, models.ErrNoMockMatched)})
		}
		got, err := p.GetMockErrors(context.Background())
		if !errors.Is(err, models.ErrUnmatchedCallsUnread) || !strings.Contains(err.Error(), missLostListFull) {
			t.Fatalf("GetMockErrors = %v with an unreported miss past the bound; want ErrUnmatchedCallsUnread naming the full list", err)
		}
		if len(got) != maxPendingMockErrors {
			t.Fatalf("got %d misses, want the %d the window held", len(got), maxPendingMockErrors)
		}
	})
}

// A miss SendError was holding for the drain when the drain stopped is still
// queued if the channel has room (a later read's sweep takes it), never lost:
// a select picks either ready case, so without the second try about half of
// these would be.
func TestSendError_QueuesAMissWithRoomAfterTheDrainStops(t *testing.T) {
	const n = 64
	p := &Proxy{logger: zap.NewNop(), errChannel: make(chan error, n)}
	p.errDrainDone = make(chan struct{})
	close(p.errDrainDone)
	p.errDrainActive.Store(true)
	for i := 0; i < n; i++ {
		p.SendError(mockMiss(fmt.Sprintf("GET /%d", i)))
		if got := len(p.errChannel); got != i+1 {
			t.Fatalf("after %d misses the channel holds %d: a miss with room was not queued", i+1, got)
		}
	}
	if cause := p.missLoss.Load(); cause != nil {
		t.Fatalf("a miss with room in the channel was recorded as lost: %s", *cause)
	}
}

// The command a parser names its call by (MockMismatchReport.Command) is
// kept on the miss the proxy files for it, so the miss's shape is that
// command (models.MissShape), with the proven switch on or off.
func TestAMissKeepsTheCommandItsParserNamed(t *testing.T) {
	eitherSwitch(t, func(t *testing.T) {
		p := startedDrainProxy(t, 4)
		p.BeginTestErrorCapture()
		p.SendError(models.ParserError{
			ParserErrorType: models.ErrMockNotFound,
			MismatchReport:  &models.MockMismatchReport{Protocol: "Redis", ActualSummary: "GET", Command: "GET"},
		})
		got, err := p.GetMockErrors(context.Background())
		if err != nil {
			t.Fatalf("GetMockErrors = %v", err)
		}
		if len(got) != 1 || got[0].Command != "GET" {
			t.Fatalf("got %+v, want one miss with Command GET", got)
		}
		if s := got[0].Sanitized(); s.ActualSummary != "GET" || s.Command != "GET" {
			t.Fatalf("sanitized miss %+v, want it named GET", s)
		}
	})
}
