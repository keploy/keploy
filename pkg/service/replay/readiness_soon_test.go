package replay

import (
	"context"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// timedAgent answers AppReadiness with nothing until up has passed since
// begin, then with a live listen on port 8080, counting the reads.
type timedAgent struct {
	begin time.Time
	up    time.Duration
	reads atomic.Int64
}

func (a *timedAgent) AppReadiness(context.Context, uint64) (models.AppReadiness, error) {
	a.reads.Add(1)
	if time.Since(a.begin) < a.up {
		return models.AppReadiness{}, nil
	}
	return models.AppReadiness{Listens: []models.AppListen{{Port: 8080, Here: true, Live: true}}}, nil
}

// heldAgent holds its first read until release is closed, then answers it,
// and every read after it at once, with answer.
type heldAgent struct {
	answer  models.AppReadiness
	release chan struct{}
	calls   atomic.Int64
}

func (a *heldAgent) AppReadiness(ctx context.Context, _ uint64) (models.AppReadiness, error) {
	if a.calls.Add(1) == 1 {
		select {
		case <-a.release:
		case <-ctx.Done():
			return models.AppReadiness{}, ctx.Err()
		}
	}
	return a.answer, nil
}

// waitedFor runs a set's wait to its end and says whether it was ready, how
// long it took, and the --delay its ready line said.
func waitedFor(t *testing.T, p provenReadiness, during func(res chan waitResult)) (bool, time.Duration, any) {
	t.Helper()
	core, logs := observer.New(zapcore.InfoLevel)
	p.logger = zap.New(core)
	start := time.Now()
	res := startWait(p)
	if during != nil {
		during(res)
	}
	var r waitResult
	select {
	case r = <-res:
	case <-time.After(30 * time.Second):
		t.Fatal("the wait did not end")
	}
	took := time.Since(start)
	ready := logs.FilterMessage("the application is ready; firing the first test").All()
	if len(ready) != 1 {
		t.Fatalf("the ready line was said %d times, want once (wait %+v)", len(ready), r)
	}
	return r.ready, took, ready[0].ContextMap()["then --delay"]
}

// sendWakes sends n wake-ups, each taken by the wait.
func sendWakes(t *testing.T, wake chan struct{}, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		select {
		case wake <- struct{}{}:
		case <-time.After(30 * time.Second):
			t.Fatalf("the wait did not take wake-up %d", i)
		}
	}
}

// An application that listens a few tens of milliseconds after keploy started
// it is found ready then, not at the watchdog's first whole second: its first
// wake-up comes 10 ms in, the next ones readyWakeEvery apart. The reads before
// it listens are a handful, no busy loop.
func TestUnderTheSwitchAnAppListeningSoonIsFoundSoon(t *testing.T) {
	cfg := testCfg()
	a := &timedAgent{up: 150 * time.Millisecond}
	a.begin = time.Now()
	ready, took, delay := waitedFor(t, provenReadiness{cfg: cfg, agent: a, host: "localhost", port: "8080", listen: 8080}, nil)
	if !ready || took >= a.up+700*time.Millisecond || delay != time.Duration(0) {
		t.Fatalf("ready %v after %v (--delay %v), want ready within 700 ms of its listen at %v: the watchdog first wakes a whole second in", ready, took, delay, a.up)
	}
	if n := a.reads.Load(); n > 8 {
		t.Fatalf("the agent was read %d times before a listen 150 ms in: no longer pause after each wake-up", n)
	}
}

// The health URL is asked again at each wake-up: one that answers 2xx a few
// tens of milliseconds into the wait is found then, not a whole second in.
func TestUnderTheSwitchAHealthURLAnsweringSoonIsFoundSoon(t *testing.T) {
	begin := time.Now()
	up := 150 * time.Millisecond
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if time.Since(begin) < up {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer srv.Close()
	cfg := testCfg()
	cfg.Test.HealthURL = srv.URL
	begin = time.Now()
	ready, took, _ := waitedFor(t, provenReadiness{cfg: cfg, agent: &fakeAgent{answers: []models.AppReadiness{{}}}, host: "localhost", port: "8080", listen: 8080}, nil)
	if !ready || took >= up+700*time.Millisecond {
		t.Fatalf("ready %v after %v, want ready within 700 ms of the health URL's 2xx at %v", ready, took, up)
	}
}

// readyWake wakes 10 ms after the wait begins, then every readyWakeEvery:
// never sooner (no busy loop), and never growing back toward a second.
func TestReadyWakeSoonThenOnceEachInterval(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	wake, stop := readyWake(ctx)
	defer stop()
	var gaps []time.Duration
	last := time.Now()
	for i := 0; i < 12; i++ {
		select {
		case <-wake:
		case <-time.After(5 * time.Second):
			t.Fatalf("no wake-up %d within 5 s (gaps %v)", i, gaps)
		}
		now := time.Now()
		gaps = append(gaps, now.Sub(last))
		last = now
	}
	if gaps[0] >= 40*time.Millisecond {
		t.Fatalf("the first wake-up came %v in, want about 10 ms (gaps %v)", gaps[0], gaps)
	}
	later := slices.Clone(gaps[1:])
	slices.Sort(later)
	if later[0] < readyWakeEvery*9/10 {
		t.Fatalf("two wake-ups came %v apart, want %v at least (gaps %v)", later[0], readyWakeEvery, gaps)
	}
	if median := later[len(later)/2]; median >= readyWakeEvery*3/2 {
		t.Fatalf("the wake-ups came %v apart, want about %v (gaps %v)", median, readyWakeEvery, gaps)
	}
}

// An application that listens long after keploy started it is found ready
// within about one wake-up of its listen: not at the next whole second (a 1 s
// watchdog found one listening 1.4 s in at 2 s), nor at the next wake-up of a
// schedule whose pauses double up to a second (one listening 320 ms in at
// 630 ms, one listening 1.4 s in at 2.27 s).
func TestUnderTheSwitchAnAppListeningLateIsFoundWithinAWakeUp(t *testing.T) {
	for _, up := range []time.Duration{320 * time.Millisecond, 1400 * time.Millisecond} {
		a := &timedAgent{up: up}
		a.begin = time.Now()
		ready, took, _ := waitedFor(t, provenReadiness{cfg: testCfg(), agent: a, host: "localhost", port: "8080", listen: 8080}, nil)
		if !ready || took >= up+2*readyWakeEvery {
			t.Fatalf("ready %v after %v, want ready within about %v of its listen at %v", ready, took, readyWakeEvery, up)
		}
	}
}

// The watchdog's limits are times, whatever its pace: the stall limit is ten
// minutes of wake-ups (design v14's 600 once a second), and a wait that goes
// on says what it waits on once a minute.
func TestReadyWatchdogLimitsAreTheirTimes(t *testing.T) {
	if got := time.Duration(readyStallLimit) * readyWakeEvery; got != readyStallFor || readyStallFor != 10*time.Minute {
		t.Fatalf("the stall limit is %d wake-ups %v apart, %v, want %v", readyStallLimit, readyWakeEvery, got, 10*time.Minute)
	}
	if got := time.Duration(readyStatusEvery) * readyWakeEvery; got != readyStatusFor || readyStatusFor != time.Minute {
		t.Fatalf("the status line comes every %d wake-ups %v apart, %v, want %v", readyStatusEvery, readyWakeEvery, got, time.Minute)
	}
}

// The stall limit falls at its time, at the watchdog's own pace: an
// application that makes no progress is not judged once the limit's time has
// passed, not sooner, and not long after.
func TestReadyStallLimitFallsAtItsTime(t *testing.T) {
	limit := 500 * time.Millisecond
	stallLimit(t, wakeUpsIn(limit))
	start := time.Now()
	res := startWait(provenReadiness{cfg: testCfg(), logger: zap.NewNop(), agent: &fakeAgent{answers: []models.AppReadiness{{}}}, host: "localhost", port: "8080", listen: 8080})
	var r waitResult
	select {
	case r = <-res:
	case <-time.After(30 * time.Second):
		t.Fatal("the wait did not end")
	}
	took := time.Since(start)
	if r.ready || r.why == "" {
		t.Fatalf("wait = %+v, want not judged: the application made no progress", r)
	}
	if took < limit-readyWakeEvery || took >= limit+time.Second {
		t.Fatalf("not judged after %v, want at about %v: the limit counts wake-ups, %v apart", took, limit, readyWakeEvery)
	}
}

// A kept-alive application found listening by the wait's first read pays no
// --delay, however many wake-ups came while that read was in flight: it was
// ready at the first look.
func TestUnderTheSwitchAWarmAppFoundByASlowFirstReadPaysNoDelay(t *testing.T) {
	wake := manualWake(t)
	stallLimit(t, 1000)
	cfg := testCfg()
	cfg.Test.Delay = 2
	a := &heldAgent{answer: models.AppReadiness{Listens: []models.AppListen{{Port: 8080, Here: true, Live: true}}}, release: make(chan struct{})}
	ready, took, delay := waitedFor(t, provenReadiness{cfg: cfg, agent: a, host: "localhost", port: "8080", listen: 8080, warm: true}, func(chan waitResult) {
		sendWakes(t, wake, 3)
		close(a.release)
	})
	if !ready || delay != time.Duration(0) || took >= 2*time.Second {
		t.Fatalf("ready %v after %v, --delay %v; want ready at once, no --delay", ready, took, delay)
	}
}

// The same for a kept-alive application whose health URL answers the wait's
// first probe after wake-ups came.
func TestUnderTheSwitchAWarmAppsSlowFirstHealthAnswerPaysNoDelay(t *testing.T) {
	wake := manualWake(t)
	stallLimit(t, 1000)
	release, asked := make(chan struct{}), make(chan struct{})
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		once.Do(func() { close(asked) })
		<-release
	}))
	defer srv.Close()
	defer close(release)
	cfg := testCfg()
	cfg.Test.Delay = 2
	cfg.Test.HealthURL = srv.URL
	ready, took, delay := waitedFor(t, provenReadiness{cfg: cfg, agent: &fakeAgent{answers: []models.AppReadiness{{}}}, host: "localhost", port: "8080", listen: 8080, warm: true}, func(chan waitResult) {
		<-asked
		sendWakes(t, wake, 3)
		release <- struct{}{}
	})
	if !ready || delay != time.Duration(0) || took >= 2*time.Second {
		t.Fatalf("ready %v after %v, --delay %v; want ready at once, no --delay", ready, took, delay)
	}
}

// A kept-alive application in a container, listening at the wait's first read
// on the port docker names only after wake-ups came, pays no --delay: it was
// ready at the first look, though later reads came in before docker's answer.
func TestUnderTheSwitchAWarmDockerSetPaysNoDelayForDockersLateAnswer(t *testing.T) {
	wake := manualWake(t)
	stallLimit(t, 1000)
	cfg := testCfg()
	cfg.Test.Delay = 2
	up := models.AppReadiness{Listens: []models.AppListen{{Port: 3000, Addr: "0.0.0.0", Here: true, Live: true}}}
	answer := make(chan uint16)
	ready, took, delay := waitedFor(t, provenReadiness{cfg: cfg, agent: &fakeAgent{answers: []models.AppReadiness{up}}, host: "localhost", port: "8098",
		docker: true, warm: true, resolve: func(context.Context) uint16 { return <-answer }}, func(res chan waitResult) {
		for i := 0; i < 3; i++ {
			readsWeighed(t, res, i)
			sendWakes(t, wake, 1)
		}
		readsWeighed(t, res, 3)
		answer <- 3000
	})
	if !ready || delay != time.Duration(0) || took >= 2*time.Second {
		t.Fatalf("ready %v after %v, --delay %v; want ready on docker's answer, no --delay", ready, took, delay)
	}
}

// A kept-alive application not ready at the first look is a start: found by a
// later probe, or by a read after a first one that failed, it pays --delay.
func TestUnderTheSwitchAWarmAppFoundAfterTheFirstLookPaysDelay(t *testing.T) {
	t.Run("health URL", func(t *testing.T) {
		wake := manualWake(t)
		// The wake-ups below come as fast as the wait takes them, until the
		// second probe is asked: no count of them may end the wait while
		// the first probe is in flight, however long a loaded host keeps it.
		stallLimit(t, math.MaxInt)
		var asks atomic.Int64
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if asks.Add(1) == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
			}
		}))
		defer srv.Close()
		cfg := testCfg()
		cfg.Test.Delay = 1
		cfg.Test.HealthURL = srv.URL
		ready, took, delay := waitedFor(t, provenReadiness{cfg: cfg, agent: &fakeAgent{answers: []models.AppReadiness{{}}}, host: "localhost", port: "8080", listen: 8080, warm: true}, func(chan waitResult) {
			// Wake-ups until the second probe is asked: one sent while
			// the first is in flight starts none.
			for deadline := time.Now().Add(30 * time.Second); asks.Load() < 2; {
				if time.Now().After(deadline) {
					t.Fatal("the health URL was never asked again")
				}
				select {
				case wake <- struct{}{}:
				case <-time.After(10 * time.Millisecond):
				}
			}
		})
		if !ready || delay != time.Second || took < time.Second {
			t.Fatalf("ready %v after %v, --delay %v; want ready after the second probe, then --delay (1s)", ready, took, delay)
		}
	})
	t.Run("first read failed", func(t *testing.T) {
		wake := manualWake(t)
		stallLimit(t, 1000)
		cfg := testCfg()
		cfg.Test.Delay = 1
		up := models.AppReadiness{Listens: []models.AppListen{{Port: 8080, Here: true, Live: true}}}
		a := &fakeAgent{answers: []models.AppReadiness{{}, up}, errs: []error{errors.New("the agent is being replaced")}}
		ready, took, delay := waitedFor(t, provenReadiness{cfg: cfg, agent: a, host: "localhost", port: "8080", listen: 8080, warm: true}, func(res chan waitResult) {
			readsWeighed(t, res, 0)
			sendWakes(t, wake, 1)
		})
		if !ready || delay != time.Second || took < time.Second {
			t.Fatalf("ready %v after %v, --delay %v; want ready at the second read, then --delay (1s)", ready, took, delay)
		}
	})
}
