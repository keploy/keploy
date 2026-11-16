package app

import (
	"context"
	"errors"
	"io"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/system"
	"github.com/docker/docker/errdefs"
	"go.keploy.io/server/v3/pkg/platform/docker"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// The app's watch reads the daemon's time before it begins
// (composeWatchSince), so each fake daemon a watch runs on says its time: a
// clock that is keploy's.
func (d *composeDocker) Info(context.Context) (system.Info, error) {
	return system.Info{SystemTime: time.Now().Format(time.RFC3339Nano)}, nil
}

func (d *daemonModel) Info(context.Context) (system.Info, error) {
	return system.Info{SystemTime: time.Now().Format(time.RFC3339Nano)}, nil
}

func (r *inspectRecorder) Info(context.Context) (system.Info, error) {
	return system.Info{SystemTime: time.Now().Format(time.RFC3339Nano)}, nil
}

func (d *stoppedAgentDocker) Info(context.Context) (system.Info, error) {
	return system.Info{SystemTime: time.Now().Format(time.RFC3339Nano)}, nil
}

// skewedDaemon is a docker daemon whose clock is off keploy's by skew, as a
// VM's daemon (Docker Desktop's) or a remote one can be. It says its time
// (Info), stamps each event it logs by it and keeps it; a stream it is asked
// for gets, as dockerd's does, the events it logged before that are stamped
// at or after the stream's since, then every event it logs after, whatever
// their stamp. The app's container is app (nil: the daemon has none).
type skewedDaemon struct {
	docker.Client
	skew time.Duration
	mu   sync.Mutex
	app  *container.State
	// logged is every event, in the order logged; subs the open streams.
	logged []events.Message
	subs   []chan events.Message
	// asks counts the streams asked for, asked gets one value per ask.
	asks  int
	asked chan struct{}
	// onFirstAsk, set, runs as the first stream is asked for, and that
	// stream then breaks before it gives any event (a daemon restarting,
	// a socket proxy dropping it).
	onFirstAsk func()
	// infoErr, set, fails every read of the daemon's time; infoTime, set, is
	// the time it says instead of its clock's. infos counts the reads.
	infoErr  error
	infoTime string
	infos    int
}

func newSkewedDaemon(skew time.Duration) *skewedDaemon {
	return &skewedDaemon{skew: skew, asked: make(chan struct{}, 16)}
}

func (d *skewedDaemon) now() time.Time { return time.Now().Add(d.skew) }

func (d *skewedDaemon) Info(context.Context) (system.Info, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.infos++
	if d.infoErr != nil {
		return system.Info{}, d.infoErr
	}
	if d.infoTime != "" {
		return system.Info{SystemTime: d.infoTime}, nil
	}
	return system.Info{SystemTime: d.now().Format(time.RFC3339Nano)}, nil
}

func (d *skewedDaemon) ContainerInspect(_ context.Context, ref string) (container.InspectResponse, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.app == nil {
		return container.InspectResponse{}, errdefs.NotFound(errors.New("no such container: " + ref))
	}
	st := *d.app
	return container.InspectResponse{ContainerJSONBase: &container.ContainerJSONBase{ID: ref, State: &st}}, nil
}

func (d *skewedDaemon) Events(ctx context.Context, o events.ListOptions) (<-chan events.Message, <-chan error) {
	msgs, errs := make(chan events.Message, 64), make(chan error, 1)
	d.mu.Lock()
	d.asks++
	first, onFirst := d.asks == 1, d.onFirstAsk
	d.mu.Unlock()
	if first && onFirst != nil {
		onFirst()
		errs <- io.ErrUnexpectedEOF
		d.asked <- struct{}{}
		return msgs, errs
	}
	since := sinceNanos(o.Since)
	d.mu.Lock()
	for _, m := range d.logged {
		if m.TimeNano >= since {
			msgs <- m
		}
	}
	d.subs = append(d.subs, msgs)
	d.mu.Unlock()
	go func() {
		<-ctx.Done()
		errs <- ctx.Err()
	}()
	d.asked <- struct{}{}
	return msgs, errs
}

// sinceNanos is a stream's since ("<seconds>.<nanoseconds>"), in Unix
// nanoseconds; 0 when it gives none.
func sinceNanos(s string) int64 {
	if s == "" {
		return 0
	}
	sec, frac, _ := strings.Cut(s, ".")
	secs, err := strconv.ParseInt(sec, 10, 64)
	if err != nil {
		return 0
	}
	nanos, _ := strconv.ParseInt(frac, 10, 64)
	return secs*int64(time.Second) + nanos
}

// die makes the app's container exit with code (it is gone when gone is
// set) and logs its die, stamped ago before now on the daemon's clock.
func (d *skewedDaemon) die(code int, gone bool, ago time.Duration) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if gone {
		d.app = nil
	} else {
		d.app = &container.State{Status: "exited", ExitCode: code, StartedAt: d.now().Add(-time.Second).UTC().Format(time.RFC3339Nano)}
	}
	m := events.Message{Type: events.ContainerEventType, Action: events.ActionDie, TimeNano: d.now().Add(-ago).UnixNano(),
		Actor: events.Actor{ID: "app-id", Attributes: map[string]string{"name": "app-1", "exitCode": strconv.Itoa(code)}}}
	d.logged = append(d.logged, m)
	for _, s := range d.subs {
		s <- m
	}
}

func skewedApp(d *skewedDaemon, logger *zap.Logger) *App {
	return &App{logger: logger, docker: d, container: "app-1", kind: "docker-compose"}
}

// waitEnd waits a bounded while for the watch's end; false: it did not end.
func waitEnd(ended <-chan struct{}, within time.Duration) bool {
	select {
	case <-ended:
		return true
	case <-time.After(within):
		return false
	}
}

// THE APP'S WATCH READS THE DAEMON'S EVENTS ON THE DAEMON'S CLOCK. A daemon
// behind keploy's clock: the app's container dies while the watch's first
// stream is broken, and the stream asked for again replays that die, so the
// run ends on it with its exit code. From keploy's clock, the die (stamped
// an hour earlier on the daemon's) was dropped, and compose's attached up
// never returned.
func TestTheAppWatchSeesAnExitOnADaemonClockBehindKeploys(t *testing.T) {
	for _, proven := range []string{"", "1"} {
		t.Setenv(provenswitch.Env, proven)
		d := newSkewedDaemon(-time.Hour)
		d.app = &container.State{Running: true, Status: "running", StartedAt: d.now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)}
		d.onFirstAsk = func() { d.die(3, false, 0) }
		a := skewedApp(d, zap.NewNop())
		ended := make(chan struct{}, 1)
		stop := a.watchComposeApp(context.Background(), func() { ended <- struct{}{} })
		if !waitEnd(ended, 20*time.Second) {
			stop()
			t.Fatalf("%s=%q: the app's exit, logged while the watch's stream was broken on a daemon an hour behind keploy, did not end the run", provenswitch.Env, proven)
		}
		stop()
		if h := a.composeAppHalt(); h == nil || h.exitCode != 3 || h.agent != "" {
			t.Fatalf("%s=%q: halt %+v; want the app's, code 3", provenswitch.Env, proven, h)
		}
	}
}

// A daemon ahead of keploy's clock logged, a moment before the watch began,
// the die of the run before's app container (since removed). The watch,
// reading from the daemon's own time, does not replay it: the run ends on
// this run's die, with its code. From keploy's clock, an hour earlier on the
// daemon's, the old die was replayed and read as this run's halt.
func TestTheAppWatchSkipsAnEarlierRunsExitOnADaemonClockAheadOfKeploys(t *testing.T) {
	for _, proven := range []string{"", "1"} {
		t.Setenv(provenswitch.Env, proven)
		d := newSkewedDaemon(time.Hour)
		d.die(9, true, time.Second)
		a := skewedApp(d, zap.NewNop())
		ended := make(chan struct{}, 1)
		stop := a.watchComposeApp(context.Background(), func() { ended <- struct{}{} })
		<-d.asked
		d.die(5, false, 0)
		if !waitEnd(ended, 20*time.Second) {
			stop()
			t.Fatalf("%s=%q: this run's exit did not end the run", provenswitch.Env, proven)
		}
		stop()
		if h := a.composeAppHalt(); h == nil || h.exitCode != 5 {
			t.Fatalf("%s=%q: halt %+v; want this run's, code 5, not the run before's (9)", provenswitch.Env, proven, h)
		}
	}
}

// A daemon that will not say its time never holds the watch's start: one
// that refuses the read is read once, one that fails it (a 500) is read
// composeWatchSinceReads times, one that gives no time it can parse is read
// once. Each leaves keploy's clock, said at Warn, and the watch still ends
// the run on the app's exit. A watch whose context has ended reads once and
// says nothing.
func TestAWatchWhoseDaemonWillNotSayItsTimeTakesKeploysClock(t *testing.T) {
	cases := []struct {
		name      string
		infoErr   error
		infoTime  string
		wantReads int
	}{
		{"refused", errdefs.Forbidden(errors.New("a socket proxy forbids /info")), "", 1},
		{"not implemented", errdefs.NotImplemented(errors.New("no /info here")), "", 1},
		{"failing", errdefs.System(errors.New("500: the daemon failed")), "", composeWatchSinceReads},
		{"no time", nil, "not a time", 1},
		{"zero time", nil, "0001-01-01T00:00:00Z", 1},
	}
	for _, proven := range []string{"", "1"} {
		t.Setenv(provenswitch.Env, proven)
		for _, c := range cases {
			d := newSkewedDaemon(0)
			d.infoErr, d.infoTime = c.infoErr, c.infoTime
			d.app = &container.State{Running: true, Status: "running"}
			core, logs := observer.New(zapcore.WarnLevel)
			a := skewedApp(d, zap.New(core))
			ended := make(chan struct{}, 1)
			began := time.Now()
			stop := a.watchComposeApp(context.Background(), func() { ended <- struct{}{} })
			if took := time.Since(began); took > 5*time.Second {
				t.Fatalf("%s=%q %s: the watch took %v to begin", provenswitch.Env, proven, c.name, took)
			}
			d.mu.Lock()
			reads := d.infos
			d.mu.Unlock()
			if reads != c.wantReads {
				t.Fatalf("%s=%q %s: %d reads of the daemon's time; want %d", provenswitch.Env, proven, c.name, reads, c.wantReads)
			}
			if w := logs.FilterMessageSnippet("could not read the docker daemon's time").Len(); w != 1 {
				t.Fatalf("%s=%q %s: %d warnings that keploy takes its own clock; want 1", provenswitch.Env, proven, c.name, w)
			}
			<-d.asked
			d.die(4, false, 0)
			if !waitEnd(ended, 20*time.Second) {
				stop()
				t.Fatalf("%s=%q %s: the app's exit did not end the run", provenswitch.Env, proven, c.name)
			}
			stop()
			if h := a.composeAppHalt(); h == nil || h.exitCode != 4 {
				t.Fatalf("%s=%q %s: halt %+v; want code 4", provenswitch.Env, proven, c.name, h)
			}
		}
	}

	d := newSkewedDaemon(0)
	d.infoErr = errdefs.System(errors.New("500"))
	core, logs := observer.New(zapcore.WarnLevel)
	a := skewedApp(d, zap.New(core))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if since := a.composeWatchSince(ctx); since.IsZero() {
		t.Fatal("an ended watch's since is zero; want keploy's clock")
	}
	if d.infos != 1 || logs.Len() != 0 {
		t.Fatalf("an ended watch read the daemon's time %d times and warned %d times; want 1 and 0", d.infos, logs.Len())
	}
}
