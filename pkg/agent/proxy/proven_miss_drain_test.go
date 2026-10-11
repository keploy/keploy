package proxy

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// untilTrue yields until cond holds. The conditions below are states the drain
// goroutine reaches by itself (it took an error off the channel, a marker is
// queued); waiting on them is waiting on an event, so no clock is involved.
func untilTrue(t *testing.T, cond func() bool) {
	t.Helper()
	for !cond() {
		runtime.Gosched()
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
	p := &Proxy{logger: zap.NewNop(), errChannel: make(chan error, capacity)}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	p.StartErrorDrain(ctx)
	return p
}

// A burst of misses larger than the error channel while the drain is behind.
// Without the switch the overflow is dropped with a log line and the test's
// list comes back short with no error: a silent pass. Under the switch every
// miss waits for the drain and reaches the test's list.
func TestSendError_ProvenSwitchNeverDropsAMiss(t *testing.T) {
	const capacity, burst = 2, 6
	for _, on := range []bool{false, true} {
		t.Run(fmt.Sprintf("switch=%v", on), func(t *testing.T) {
			if on {
				t.Setenv(provenswitch.Env, "1")
			} else {
				t.Setenv(provenswitch.Env, "")
			}
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
			if !on {
				<-sent // nothing waits: what did not fit was dropped
			} else {
				// The drain holds one error and the channel is full: the rest of
				// the burst is waiting, not dropped.
				untilTrue(t, func() bool { return len(p.errChannel) == capacity })
			}
			release()
			<-sent

			got, err := p.GetMockErrors(context.Background())
			if !on {
				if err != nil || len(got) >= burst {
					t.Fatalf("control: got %d misses, err %v; the switch-off path is expected to drop the overflow silently (that is the hole)", len(got), err)
				}
				return
			}
			if err != nil {
				t.Fatalf("GetMockErrors: %v", err)
			}
			if len(got) != burst {
				t.Fatalf("got %d of %d misses: a miss was dropped for want of room", len(got), burst)
			}
		})
	}
}

// Under the switch the read waits for the drain on events only. A read its
// caller gives up on before the drain routed every miss is incomplete, and says
// so, with what was read.
func TestGetMockErrors_ProvenSwitchReportsAnIncompleteRead(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	p := startedDrainProxy(t, 4)
	p.BeginTestErrorCapture()
	release := holdDrain(t, p)

	p.SendError(mockMiss("GET /a"))
	untilTrue(t, func() bool { return len(p.errChannel) == 0 }) // the drain took it, and is parked

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
	untilTrue(t, func() bool { return len(p.errChannel) == 1 }) // the flush marker is queued behind it
	cancel()
	release()

	r := <-done
	if !errors.Is(r.err, models.ErrUnmatchedCallsUnread) {
		t.Fatalf("GetMockErrors = %v; a read given up before the drain routed every miss must say it is incomplete", r.err)
	}
}

// The same read, not given up: it waits for the drain however long the drain
// takes, and is complete.
func TestGetMockErrors_ProvenSwitchWaitsForTheDrain(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
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
}

// A miss that cannot be queued at all (no drain, channel full) is lost; under
// the switch the next read says it is incomplete instead of coming back short.
func TestGetMockErrors_ProvenSwitchReportsALostMiss(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	p := &Proxy{logger: zap.NewNop(), errChannel: make(chan error, 1)} // no drain
	p.SendError(mockMiss("GET /kept"))
	p.SendError(mockMiss("GET /lost"))
	got, err := p.GetMockErrors(context.Background())
	if !errors.Is(err, models.ErrUnmatchedCallsUnread) {
		t.Fatalf("GetMockErrors = %v with a miss lost; want ErrUnmatchedCallsUnread", err)
	}
	if len(got) != 1 || got[0].ActualSummary != "GET /kept" {
		t.Fatalf("got %+v; the read must still return what it read", got)
	}
	// Reported once: the next read starts clean.
	if _, err := p.GetMockErrors(context.Background()); err != nil {
		t.Fatalf("the loss was reported again on the next read: %v", err)
	}
}
