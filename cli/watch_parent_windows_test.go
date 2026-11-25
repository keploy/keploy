//go:build windows

package cli

import (
	"context"
	"errors"
	"math"
	"os"
	"os/exec"
	"testing"
	"time"

	"go.uber.org/zap"
	"golang.org/x/sys/windows"
)

// watchEnds replaces the watchdog's end of the agent with a channel it
// closes, for the test's length.
func watchEnds(t *testing.T) <-chan struct{} {
	t.Helper()
	ended := make(chan struct{})
	prev := endForGoneClient
	endForGoneClient = func(*zap.Logger) {
		select {
		case <-ended:
		default:
			close(ended)
		}
	}
	t.Cleanup(func() { endForGoneClient = prev })
	return ended
}

// startStandIn starts a process that runs until it is killed, and kills it
// at the test's end.
func startStandIn(t *testing.T) (*exec.Cmd, <-chan error) {
	t.Helper()
	return startWatchHelper(t, watchStandInEnv+"=1")
}

// agentStartedAt has the watchdog take its own start as the stand-in's, as
// for an agent its client started in the same tick of the clock.
func agentStartedAt(t *testing.T, standIn *exec.Cmd) {
	t.Helper()
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(standIn.Process.Pid))
	if err != nil {
		t.Fatalf("opening the stand-in: %v", err)
	}
	defer func() { _ = windows.CloseHandle(h) }()
	at, err := processStarted(h)
	if err != nil {
		t.Fatalf("reading the stand-in's start: %v", err)
	}
	prev := processStarted
	processStarted = func(h windows.Handle) (int64, error) {
		if h == windows.CurrentProcess() {
			return at, nil
		}
		return prev(h)
	}
	t.Cleanup(func() { processStarted = prev })
}

func ended(ch <-chan struct{}, within time.Duration) bool {
	select {
	case <-ch:
		return true
	case <-time.After(within):
		return false
	}
}

func killStandIn(t *testing.T, standIn *exec.Cmd, exited <-chan error) {
	t.Helper()
	if err := standIn.Process.Kill(); err != nil {
		t.Fatalf("killing the stand-in: %v", err)
	}
	select {
	case <-exited:
	case <-time.After(10 * time.Second):
		t.Fatal("the killed stand-in did not exit within 10s")
	}
}

// A client that started in the same tick of the clock as the agent is its
// client: the agent runs while it runs and ends when it is killed.
func TestOnWindowsAClientStartedInTheAgentsClockTickIsItsClient(t *testing.T) {
	ends := watchEnds(t)
	standIn, exited := startStandIn(t)
	agentStartedAt(t, standIn)
	watchParentProcess(context.Background(), zap.NewNop(), standIn.Process.Pid)
	if ended(ends, 3*watchParentInterval) {
		t.Fatal("the agent ended while its client still ran")
	}
	killStandIn(t, standIn, exited)
	if !ended(ends, 5*time.Second) {
		t.Fatal("the agent did not end within 5s of its client being killed")
	}
}

// The client's pid reused by a process that started after the agent: the
// client, which started the agent, is gone, and the agent ends at once,
// though the process with that pid runs on.
func TestOnWindowsAPidReusedAfterTheAgentStartedIsNotItsClient(t *testing.T) {
	ends := watchEnds(t)
	standIn, exited := startStandIn(t) // started after this process, the agent
	watchParentProcess(context.Background(), zap.NewNop(), standIn.Process.Pid)
	if !ended(ends, 2*time.Second) {
		t.Fatal("the agent watched a process that started after it as its client")
	}
	select {
	case err := <-exited:
		t.Fatalf("the stand-in ended (%v): the row did not see a running process with the pid", err)
	default:
	}
}

// A client that exited before the watchdog opened it, as Windows really has
// it: no process has its pid, or another handle on the exited client keeps
// its process, which started after the agent; either way the agent ends.
// Which of the two a run sees is Windows', so the next row pins the first.
func TestOnWindowsAClientGoneBeforeTheWatchEndsTheAgent(t *testing.T) {
	ends := watchEnds(t)
	standIn, exited := startStandIn(t)
	killStandIn(t, standIn, exited)
	watchParentProcess(context.Background(), zap.NewNop(), standIn.Process.Pid)
	if !ended(ends, 2*time.Second) {
		t.Fatal("the agent did not end for a client that had exited")
	}
}

// A pid no process has (OpenProcess says ERROR_INVALID_PARAMETER) is a client
// that has exited: the agent ends at once, and waits on nothing.
func TestOnWindowsAPidNoProcessHasEndsTheAgent(t *testing.T) {
	ends := watchEnds(t)
	prev := openClient
	openClient = func(uint32) (windows.Handle, error) { return 0, windows.ERROR_INVALID_PARAMETER }
	t.Cleanup(func() { openClient = prev })
	prevWait := waitClient
	waited := make(chan struct{}, 1)
	waitClient = func(windows.Handle, uint32) (uint32, error) {
		select {
		case waited <- struct{}{}:
		default:
		}
		return uint32(windows.WAIT_TIMEOUT), nil
	}
	t.Cleanup(func() { waitClient = prevWait })
	watchParentProcess(context.Background(), zap.NewNop(), os.Getppid())
	if !ended(ends, 2*time.Second) {
		t.Fatal("the agent did not end for a client pid no process has")
	}
	select {
	case <-waited:
		t.Fatal("the agent waited on a client pid no process has")
	default:
	}
}

// No pid named no client: an agent nothing started runs on.
func TestOnWindowsAnAgentWithNoClientPidIsNotEnded(t *testing.T) {
	ends := watchEnds(t)
	watchParentProcess(context.Background(), zap.NewNop(), 0)
	if ended(ends, 2*time.Second) {
		t.Fatal("an agent with no --client-pid was ended")
	}
}

// A client the agent may not open (anything but "no such process") is not
// taken for a gone one: the agent is never stopped on uncertainty.
func TestOnWindowsAClientTheAgentCannotOpenIsNotTakenForGone(t *testing.T) {
	ends := watchEnds(t)
	prev := openClient
	openClient = func(uint32) (windows.Handle, error) { return 0, windows.ERROR_ACCESS_DENIED }
	t.Cleanup(func() { openClient = prev })
	prevWait := waitClient
	waited := make(chan struct{}, 1)
	waitClient = func(windows.Handle, uint32) (uint32, error) {
		select {
		case waited <- struct{}{}:
		default:
		}
		return windows.WAIT_OBJECT_0, nil
	}
	t.Cleanup(func() { waitClient = prevWait })
	watchParentProcess(context.Background(), zap.NewNop(), os.Getppid())
	if ended(ends, 2*time.Second) {
		t.Fatal("the agent was ended for a client it could not open")
	}
	select {
	case <-waited:
		t.Fatal("the agent waited on a client it could not open")
	default:
	}
}

// A wait on the client that fails stops the watch; it never ends the agent.
func TestOnWindowsAFailedWaitDoesNotEndTheAgent(t *testing.T) {
	ends := watchEnds(t)
	standIn, _ := startStandIn(t)
	agentStartedAt(t, standIn)
	prev := waitClient
	// What a caller reads in spite of the error is the event that would end
	// the agent.
	waitClient = func(windows.Handle, uint32) (uint32, error) {
		return windows.WAIT_OBJECT_0, windows.ERROR_INVALID_HANDLE
	}
	t.Cleanup(func() { waitClient = prev })
	watchParentProcess(context.Background(), zap.NewNop(), standIn.Process.Pid)
	if ended(ends, 2*time.Second) {
		t.Fatal("a failed wait on the client ended the agent")
	}
}

// Start times it cannot read do not stop the watch: the process with the
// pid is watched as the client, neither taken for a reused pid nor left
// unwatched, whichever of the two times fails.
func TestOnWindowsUnreadableStartTimesStillWatchTheClient(t *testing.T) {
	for _, failing := range []string{"the agent's", "the client's"} {
		t.Run(failing, func(t *testing.T) {
			ends := watchEnds(t)
			standIn, exited := startStandIn(t)
			agentStartedAt(t, standIn)
			prev := processStarted
			processStarted = func(h windows.Handle) (int64, error) {
				// What a caller reads in spite of the error is the time
				// that would take the client for a reused pid.
				if h == windows.CurrentProcess() && failing == "the agent's" {
					return math.MinInt64, errors.New("unreadable")
				}
				if h != windows.CurrentProcess() && failing == "the client's" {
					return math.MaxInt64, errors.New("unreadable")
				}
				return prev(h)
			}
			t.Cleanup(func() { processStarted = prev })
			watchParentProcess(context.Background(), zap.NewNop(), standIn.Process.Pid)
			if ended(ends, 2*watchParentInterval) {
				t.Fatal("the agent ended while its client still ran")
			}
			killStandIn(t, standIn, exited)
			if !ended(ends, 5*time.Second) {
				t.Fatal("the agent did not end within 5s of its client being killed")
			}
		})
	}
}

// An agent that is stopping stops watching: its client's exit after that
// does not stop it again.
func TestOnWindowsAStoppedAgentStopsWatchingItsClient(t *testing.T) {
	ends := watchEnds(t)
	standIn, exited := startStandIn(t)
	agentStartedAt(t, standIn)
	ctx, cancel := context.WithCancel(context.Background())
	watchParentProcess(ctx, zap.NewNop(), standIn.Process.Pid)
	cancel()
	time.Sleep(2 * watchParentInterval)
	killStandIn(t, standIn, exited)
	if ended(ends, 3*watchParentInterval) {
		t.Fatal("a stopped agent's watchdog still ended it when its client exited")
	}
}
