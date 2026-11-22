package record

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// unreachablePortWarner warns while recording, once per app port, that the test
// cases recorded on that port will not reach the app on a replay that sends
// them where it does by default: replay sends each test from the host to the
// port it was recorded on, and the instrumentation says the host cannot reach
// the app there (in Docker mode, a port the docker command does not publish as
// itself, nor on one other host port only, where replay sends its tests
// instead: pkg.PublishedTarget; the app calling its own in-container server is
// recorded as ingress on such a port). A replay with a port map in keploy.yml (test.replaceWith) can
// still send those tests to a host port their port is published on, and the
// reason says so where there is one. Without it the first sign is a replay
// refusing every one of those tests.
//
// It asks about the address replay will use by default: test.host and the
// recorded port. A keploy.yml that sends replay elsewhere (test.port, a
// protocol port, replaceWith) makes that the wrong address to ask about, so
// then it asks nothing rather than warn about a port replay will not use.
type unreachablePortWarner struct {
	logger *zap.Logger
	reach  pkg.AppPortReachability
	pub    pkg.AppPortPublishing // nil when the instrumentation cannot tell
	host   string

	mu      sync.Mutex
	checked map[uint16]bool
	// asking is each app port whose question is still out; idle is closed
	// while none is (wait).
	asking map[uint16]bool
	idle   chan struct{}
	// sealed: no question is asked any more; the stop's wait seals the
	// warner as it begins (seal).
	sealed bool
	// parked, when set (a test's), is called each time wait is about to
	// wait for the questions still out.
	parked func()
}

func newUnreachablePortWarner(logger *zap.Logger, instrumentation Instrumentation, cfg *config.Config) *unreachablePortWarner {
	reach, ok := instrumentation.(pkg.AppPortReachability)
	if !ok || cfg == nil || replayTargetRedirected(cfg.Test) {
		return &unreachablePortWarner{}
	}
	host := cfg.Test.Host
	if host == "" {
		host = "localhost"
	}
	pub, _ := instrumentation.(pkg.AppPortPublishing)
	return &unreachablePortWarner{logger: logger, reach: reach, pub: pub, host: host, checked: map[uint16]bool{}, asking: map[uint16]bool{}}
}

// check asks about tc's app port the first time a test case is recorded on it,
// on a goroutine of its own: a docker daemon slow to answer the questions
// (which wait for it by keploy's runnable time) holds up no recording. The
// stop's end does not cut a question short, but the interrupt that gives the
// stop's waits up (utils.StopNow) does. The stop waits for those still out
// after its drains (wait), so keploy does not exit under a question that
// has its answer before it says it. What a question asks of is the agent's
// container (in Docker mode), which the stop has stopped by then: one that
// had not read it yet reads a container that is not running, and says
// nothing (it cannot tell).
func (w *unreachablePortWarner) check(ctx context.Context, tc *models.TestCase) {
	if w.reach == nil || tc == nil || tc.AppPort == 0 {
		return
	}
	w.mu.Lock()
	if w.checked[tc.AppPort] || w.sealed {
		w.mu.Unlock()
		return
	}
	w.checked[tc.AppPort] = true
	if len(w.asking) == 0 {
		w.idle = make(chan struct{})
	}
	w.asking[tc.AppPort] = true
	w.mu.Unlock()
	askCtx, cancelAsk := context.WithCancel(context.WithoutCancel(ctx))
	go func() {
		select {
		case <-utils.StopNow():
			cancelAsk()
		case <-askCtx.Done():
		}
	}()
	go func(appPort uint16, name string) {
		defer w.answered(appPort)
		defer cancelAsk()
		w.ask(askCtx, appPort, name)
	}(tc.AppPort, tc.Name)
}

// answered is appPort's question having said what it had to.
func (w *unreachablePortWarner) answered(appPort uint16) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.asking, appPort)
	if len(w.asking) == 0 {
		close(w.idle)
	}
}

// out is the app ports whose questions are still out, and a channel closed
// once none is.
func (w *unreachablePortWarner) out() ([]uint16, <-chan struct{}) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.asking) == 0 {
		return nil, closedChan
	}
	ports := make([]uint16, 0, len(w.asking))
	for p := range w.asking {
		ports = append(ports, p)
	}
	slices.Sort(ports)
	return ports, w.idle
}

var closedChan = func() chan struct{} { c := make(chan struct{}); close(c); return c }()

// seal says no question is asked any more; one asked after it is not.
// Idempotent.
func (w *unreachablePortWarner) seal() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.sealed = true
}

// wait seals the warner (no question is asked from then on: the stop has
// joined, or given up, the test-case consumer that asks them), and is until
// every one asked has said what it had to, as the stop waits for its other
// work (closeMockStore): every `every` it says which app ports it still asks
// about, and, once it has, that one more Ctrl+C stops it now
// (utils.StopNowNotice). That press, or the one that gave an earlier wait up
// (utils.StopNow), ends the wait and the questions with it: what they would
// have said is then not said.
func (w *unreachablePortWarner) wait(every time.Duration) {
	if w.reach == nil {
		return // asks nothing
	}
	// Sealed first: a question is asked (under mu) before the seal or not at
	// all, so out() then sees every question there will be.
	w.seal()
	if _, idle := w.out(); isClosed(idle) {
		return
	}
	notice := utils.NewStopNowNotice(every)
	defer notice.Withdraw()
	tick := time.NewTicker(every)
	defer tick.Stop()
	said, offering := false, false
	for {
		ports, idle := w.out()
		if w.parked != nil {
			w.parked()
		}
		select {
		case <-idle:
			return
		case <-notice.Interrupt():
			return
		case <-utils.StopNow():
			return
		case <-tick.C:
		}
		wasOffering := offering
		offering = notice.Next()
		if !said || offering != wasOffering {
			msg := "Still checking whether the test cases recorded on the app's port(s) can be replayed from the host"
			if offering {
				msg += "; press Ctrl+C again to stop now: what the check finds is then not said"
			}
			w.logger.Info(msg, zap.Uint16s("appPorts", ports))
			said = true
		}
	}
}

func isClosed(c <-chan struct{}) bool {
	select {
	case <-c:
		return true
	default:
		return false
	}
}

// ask says, at WARN, why test cases recorded on the app's port appPort (the
// first of them name) cannot be replayed from the host, if they cannot.
func (w *unreachablePortWarner) ask(ctx context.Context, appPort uint16, name string) {
	tc := &models.TestCase{Name: name, AppPort: appPort}
	// A port published on one other host port only is where replay sends
	// these tests (pkg.PublishedTarget): whether they can be replayed is
	// whether that host port reaches the app (it does not when the app
	// listens there only on loopback).
	sentTo := tc.AppPort
	if w.pub != nil {
		if port, known := w.pub.PublishedHostPort(ctx, w.host, tc.AppPort); known && port != 0 {
			w.logger.Debug("the app's port is published on one other host port, where replay sends its test cases",
				zap.Uint16("appPort", tc.AppPort), zap.Uint16("hostPort", port))
			sentTo = port
		}
	}
	reason := w.reach.UnreachableAppPort(ctx, w.host, sentTo, tc.AppPort)
	if reason == "" {
		return
	}
	if sentTo != tc.AppPort {
		w.logger.Warn(fmt.Sprintf("test cases recorded on the app's port %d cannot be replayed from the host at host port %d, where it is published: %s", tc.AppPort, sentTo, reason),
			zap.String("first testcase on the port", tc.Name))
		return
	}
	w.logger.Warn(fmt.Sprintf("test cases recorded on the app's port %d cannot be replayed at that port from the host: %s", tc.AppPort, reason),
		zap.String("first testcase on the port", tc.Name))
}

// replayTargetRedirected reports whether the test config sends replay somewhere
// other than test.host and each test's recorded port.
func replayTargetRedirected(t config.Test) bool {
	if t.Port != 0 || t.GRPCPort != 0 || t.SSEPort != 0 {
		return true
	}
	for _, p := range t.Protocol {
		if p.Port != 0 {
			return true
		}
	}
	rw := t.ReplaceWith
	return len(rw.Global.URL) > 0 || len(rw.Global.Port) > 0 || len(rw.TestSets) > 0
}
