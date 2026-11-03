package record

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/utils"
	"go.keploy.io/server/v3/utils/stall"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"golang.org/x/sync/errgroup"
)

// stopDrainTest readies a test of a stop's drains and the user's interrupts:
// short drain notices, the interrupts put back at the end.
func stopDrainTest(t *testing.T) {
	t.Helper()
	utils.ClearInterrupted()
	utils.ResetInterruptedAgain()
	restore := utils.SetDrainNoticeForTest(5 * time.Millisecond)
	t.Cleanup(func() { restore(); utils.ClearInterrupted(); utils.ResetInterruptedAgain() })
}

// A frame drain given up, at a stall or at the user's interrupt after its
// offer, may hold its stream to a compose agent open, and the agent's stop
// waits for that stream: the recording tells the agent its drain gave up, so
// it goes. A drain that joined lets the agent go by its streams. The reads
// come first, while the agent is up.
func TestAComposeAgentIsToldItsFrameDrainGaveUp(t *testing.T) {
	for _, tc := range []struct {
		end     utils.DrainEnd
		release bool
	}{{utils.DrainJoined, false}, {utils.DrainStalled, true}, {utils.DrainStopped, true}} {
		agent := &stoppingComposeAgent{endingAgent: newEndingAgent(), stackDown: make(chan struct{}), ending: make(chan struct{})}
		close(agent.stackDown)
		r := &Recorder{logger: zap.NewNop(), instrumentation: agent}
		var readBeforeEnd bool
		r.drainBeforeComposeAgentEnds(func() utils.DrainEnd { return tc.end }, func() {
			select {
			case <-agent.ending:
			default:
				readBeforeEnd = true
			}
		})
		select {
		case <-agent.ending:
		default:
			t.Fatalf("drain %v: the agent was not seen to its end", tc.end)
		}
		if !readBeforeEnd {
			t.Fatalf("drain %v: the reads came after the agent's end", tc.end)
		}
		if got := agent.released.Load(); got != tc.release {
			t.Fatalf("drain %v: the agent was told the drain gave up: %v, want %v", tc.end, got, tc.release)
		}
	}
}

// The recording's frame drain runs inside its progress reporter, whose offer
// to end at the next Ctrl+C is the drain's too: the drain says nothing while
// the reporter runs, so one offer stands at a time. Here the drain's notices
// come every 5 ms and the reporter's every 5 s: a drain that spoke for
// itself would have said so many times over.
func TestTheFrameDrainLetsItsReporterMakeItsOffer(t *testing.T) {
	stopDrainTest(t)
	utils.MarkInterrupted()
	core, logs := observer.New(zap.InfoLevel)
	r := &Recorder{logger: zap.New(core), drainClock: &stall.ManualClock{}}
	var g errgroup.Group
	release := make(chan struct{})
	g.Go(func() error { <-release; return nil })
	ended := make(chan utils.DrainEnd, 1)
	go func() { ended <- r.drainFrameStreams(&g) }()
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if n := logs.FilterMessageSnippet("made no progress").Len(); n != 0 {
			close(release)
			t.Fatalf("the frame drain spoke while its reporter ran: %v", logs.All())
		}
	}
	close(release)
	select {
	case end := <-ended:
		if end != utils.DrainJoined {
			t.Fatalf("the frame drain ended %v, want its group joined", end)
		}
	case <-time.After(time.Minute): // only turns a hang into a failure
		t.Fatal("the frame drain did not end at its group's end")
	}
}

// hookRan notes whether the end-of-recording hook ran.
type hookRan struct {
	BaseRecordHooks
	ran atomic.Bool
}

func (h *hookRan) AfterRecordingComplete(context.Context, *RecordingCompleteContext) error {
	h.ran.Store(true)
	return nil
}

// A record drain given up, at a stall or at the user's interrupt after its
// offer, leaves a mock writer that may still append to the mock file, which
// the end-of-recording hook rewrites whole: the hook is skipped, and the
// recording says why, naming the cause. A press is no stall: the line said
// the drain "timed out".
func TestARecordDrainGivenUpSkipsTheEndOfRecordingHook(t *testing.T) {
	for _, tc := range []struct {
		name, why string
		press     bool
	}{
		{"at the stall", "the record drain made no progress", false},
		{"at the press", "the record drain was given up at the user's interrupt", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stopDrainTest(t)
			wedge := make(chan struct{})
			t.Cleanup(func() { close(wedge) })
			core, logs := observer.New(zap.DebugLevel)
			clk := &armedClock{armed: make(chan struct{}, 1)}
			hooks := &hookRan{}
			run := startRecording(t, &recMockDB{unencodable: map[string]bool{}, gate: map[string]chan struct{}{"mock-1": wedge}},
				hooks, zap.New(core), func(r *Recorder) { r.drainClock = clk })
			run.test(t, "test-1")
			run.mock(t, "mock-1")
			run.mapping(t, "test-1", "mock-1")
			run.until(t, "test-1's entry held", heldIs(1))
			if tc.press {
				// The stop's first signal, and the user, who answers each
				// offer to end at the next Ctrl+C once it is said.
				utils.MarkInterrupted()
				pressed := make(chan struct{})
				defer close(pressed)
				go func() {
					answered := 0
					for {
						select {
						case <-pressed:
							return
						case <-time.After(time.Millisecond):
						}
						if n := logs.FilterMessageSnippet("press Ctrl+C again to stop now").Len(); n > answered {
							answered = n
							utils.MarkInterruptedAgain()
						}
					}
				}()
				run.stop(t)
			} else {
				run.stopGivenUp(t, clk)
			}
			if hooks.ran.Load() {
				t.Fatal("the end-of-recording hook ran over a mock file a given-up writer may still append to")
			}
			skipped := logs.FilterMessageSnippet("skipping the end-of-recording hook").All()
			if len(skipped) != 1 || !strings.Contains(skipped[0].Message, tc.why) {
				t.Fatalf("the hook's skip said %v, want one line naming %q", skipped, tc.why)
			}
		})
	}
}
