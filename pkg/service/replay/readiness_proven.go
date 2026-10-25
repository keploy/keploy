package replay

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
)

// Readiness under the proven switch (design v14 §8.4, row W6). keploy test
// fires a set's first test when the application is ready, never after a time:
//
//   - an application keploy started (a native command, docker run, compose):
//     ready once it listens, in the namespace keploy reaches it in, on the port
//     the set's first test dials, mapped through the ports a container
//     publishes, and while that listen's own socket still listens (the
//     kernel's listen, not its bind: a port bound and not yet listening
//     refuses a connect; in Docker mode not on loopback only, which a
//     published port cannot reach). For a set keploy started the
//     application anew for, only the listens since (an earlier set's
//     application, or what outlived it, is not this one). Or, with
//     --health-url or --health-path, once that URL
//     answers (a 2xx for --health-url, redirects followed; any HTTP answer for
//     --health-path, on the scheme the tests dial);
//   - one keploy did not start (--base-path, no command): the first test's own
//     connect is the event, so nothing is waited for, but an explicit
//     --health-url's answer;
//   - a set whose first test says nowhere it dials: the same.
//
// No ceiling: what ends a wait that never sees its event is the watchdog. It
// wakes once a second and counts the wake-ups in a row at which the
// application made no progress: no listen, connect, process fork or exec by
// its processes (in a container, its main process and what that starts; a
// docker exec or a healthcheck is not the application), and, until it
// listens, no CPU time used (a start that only computes goes on; an
// application up and idle, on another port, uses CPU time now and then and
// is no start going on). At readyStallLimit the set is not judged, saying
// what the application does listen on; a kept-alive application's later
// set, nothing moved since, at its first read. The costs, each not judged,
// never a false pass: an application that listens early on a debug port
// (JDWP's 5005) and then only computes for longer than the count; one its
// launcher daemonizes before it listens (no longer seen as the
// application). It counts its own wake-ups, not seconds: a starved keploy
// wakes as seldom as it runs, so load alone cannot trip it. --delay is added
// after the event.

// readyStallLimit is the watchdog's count of wake-ups in a row with no
// progress (design v14's lead decision: 600).
var readyStallLimit = 600

// readyWake is how the watchdog wakes; a test replaces it.
var readyWake = func(ctx context.Context) (<-chan struct{}, func()) {
	t := time.NewTicker(time.Second)
	ch := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(ch)
		for {
			select {
			case <-t.C:
				select {
				case ch <- struct{}{}:
				case <-ctx.Done():
					return
				case <-done:
					return
				}
			case <-ctx.Done():
				return
			case <-done:
				return
			}
		}
	}()
	return ch, func() { t.Stop(); close(done) }
}

// readinessReader is what the wait asks keploy's agent
// (pkg/platform/http AgentClient.AppReadiness).
type readinessReader interface {
	// AppReadiness reads the listens from since on (provenReadiness.since).
	AppReadiness(ctx context.Context, since uint64) (models.AppReadiness, error)
}

// readinessMarker is the agent's mark for a set whose application keploy
// starts anew (pkg/platform/http AgentClient.MarkReadiness): what the last
// set's processes did is forgotten, so the new start's first connects,
// forks and execs are progress again; its ListenCount is where the set's
// listens begin.
type readinessMarker interface {
	MarkReadiness(ctx context.Context) (models.AppReadiness, error)
}

// publishedPortReader says which container port a host port is published to
// (pkg/platform/http AgentClient.PublishedContainerPort, from the keploy
// agent container, which publishes the application's ports).
type publishedPortReader interface {
	PublishedContainerPort(ctx context.Context, hostPort uint16) (uint16, bool)
}

// provenReadiness is one set's wait for its application.
type provenReadiness struct {
	logger *zap.Logger
	cfg    *config.Config
	// agent is asked for the application's listens and progress; nil when
	// keploy did not start the application (none of its processes are
	// keploy's to watch).
	agent readinessReader
	// scheme, host and port are where the set's first test dials; port is
	// "" when no test says.
	scheme, host, port string
	// listen is the port the application listens on for it (port, mapped
	// through a container's published ports).
	listen uint16
	// docker: the application runs in a container; a listen on loopback
	// only is not where a published port reaches it.
	docker bool
	// since is where this set's listens begin: the agent's count of listens
	// (and of those the kernel had no room to report) read before keploy
	// started the set's application (Replayer.markReadiness); zero for one
	// kept alive, or one that came up with its agent (compose), whose every
	// listen is its own. An earlier set's application, or what outlived it,
	// is not this one.
	since readyMark
	// stalledAt is where an earlier set's wait called this application, kept
	// alive, stalled: read again with nothing moved since, not a count, it is
	// the same stall, and the set is not judged at once rather than after
	// another watchdog's count. Anything moved, or any count lower (another
	// agent), and the wait counts afresh. Only when no health URL is waited
	// on, whose answer is its own evidence.
	stalledAt *progressMark
	// onStall, when set, is told the progress a stall ended the wait at.
	onStall func(progressMark)
	// warm: the application is one kept alive from an earlier set; ready at
	// the first look, no --delay is added.
	warm bool
	// dial: keploy's agent cannot tell when the application listens, and no
	// health URL is given: the first test's address taking a connect is the
	// event (what keploy test's own first request needs).
	dial bool
	// resolve, when set, says the port the application listens on for the
	// first test's (p.listen), from docker: until it answers, no listen
	// matches.
	resolve func(context.Context) uint16
	// tally, when set, counts the reads of the agent started and taken: a
	// test that drives the watchdog by hand sends a wake-up once the read
	// before it was weighed. Nil outside tests.
	tally *readTally
}

// readTally is provenReadiness.tally: reads of the agent started, answered
// (their answer sent to the wait) and taken; docker's answers (resolve) sent
// and taken.
type readTally struct{ started, answered, taken, portsSent, ports atomic.Int64 }

// readyMark is the agent's count of the listens it was told, of those the
// kernel had no room to report, and of the application's restarts, at a
// point in the run.
type readyMark struct{ listens, lost, restarts uint64 }

// errNotReadable is the agent unable to say whether the application is
// ready: unknown, never "not ready" (the set is not judged).
var errNotReadable = errors.New("keploy could not tell whether the application is ready")

// progressMark is what the watchdog compares between wake-ups.
type progressMark struct {
	p   models.ReadyProgress
	cpu time.Duration
}

// moved says the application made progress since was.
func (m progressMark) moved(was progressMark) bool {
	return m.p.Listens > was.p.Listens || m.p.Connects > was.p.Connects || m.p.Forks > was.p.Forks ||
		m.p.Execs > was.p.Execs || m.cpu > was.cpu
}

// readyStatusEvery is how many wake-ups of a wait that goes on apart keploy
// says what it waits on, and what moved: an endless wait (a start that
// loops, re-exec'ing before it ever listens) is said, not silent.
var readyStatusEvery = 60

// wait blocks until the application is ready (true), ctx ends (false, ""),
// or the application stalls (false and why, for the set's not-judged
// reason). The agent is read, and the health URL asked, one request of each
// in flight at a time, neither cut short by a timer: a starved agent or
// application answers late, and a late answer is an answer. Each wake-up
// reads the agent and is weighed by that read's answer; one whose read has
// not answered by the next wake-up is weighed as no progress seen, so a
// wedged agent is a stall, said, not a wait for ever.
func (p provenReadiness) wait(ctx context.Context) (bool, string) {
	health, healthAny := p.healthTarget()
	wake, stop := readyWake(ctx)
	defer stop()
	// What is still in flight when the wait ends ends with it.
	pctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// The port the application listens on for the first test's, when it
	// takes a look at docker to say (resolve): none matches until it
	// answers, and one that never does is a stall, said.
	resolved := p.resolve == nil
	ports := make(chan uint16, 1)
	if !resolved {
		tally := p.tally
		go func() {
			ports <- p.resolve(pctx)
			if tally != nil {
				tally.portsSent.Add(1)
			}
		}()
	}

	type probeResult struct {
		ok   bool
		what string
	}
	probes := make(chan probeResult, 1)
	probing := false
	lastProbe := ""
	startProbe := func() {
		if probing || (health == "" && !p.dial) {
			return
		}
		probing = true
		go func() {
			if health != "" {
				ok, what := answers(pctx, health, healthAny)
				probes <- probeResult{ok, what}
				return
			}
			ok, what := takesConnect(pctx, net.JoinHostPort(p.host, p.port))
			probes <- probeResult{ok, what}
		}()
	}
	type readResult struct {
		st  models.AppReadiness
		err error
	}
	reads := make(chan readResult, 1)
	reading, readWakes := false, 0
	startRead := func() {
		if p.agent == nil || reading {
			return
		}
		reading, readWakes = true, 0
		if p.tally != nil {
			p.tally.started.Add(1)
		}
		agent, tally := p.agent, p.tally
		go func() {
			st, err := agent.AppReadiness(pctx, p.since.listens)
			reads <- readResult{st, err}
			if tally != nil {
				tally.answered.Add(1)
			}
		}()
	}

	var last, mark, said progressMark
	var st models.AppReadiness
	read, first := false, true
	restarts0 := p.since.restarts
	lastErr := ""
	stalls, wakes := 0, 0
	// pending: the last wake-up waits on its read's answer to be weighed.
	pending := false
	// weigh counts a wake-up as progress or not, by the latest read's
	// answer, and says, every readyStatusEvery, what moved; true at the
	// stall.
	counted := 0
	weigh := func() bool {
		if read && mark.moved(last) {
			stalls = 0
		} else {
			stalls++
		}
		last = mark
		if counted++; counted%readyStatusEvery == 0 && stalls < readyStallLimit {
			p.sayStillWaiting(health, lastProbe, lastErr, st, said, mark, stalls, st.Progress.Restarts-min(st.Progress.Restarts, restarts0))
			said = mark
		}
		return stalls >= readyStallLimit
	}
	stalled := func() (bool, string) {
		if p.onStall != nil && read && lastErr == "" && !reading {
			p.onStall(mark)
		}
		why := lastErr
		if reading {
			why = fmt.Sprintf("keploy's agent has not answered its last read, in flight across %d wake-ups", readWakes)
		}
		if !resolved {
			why = strings.TrimPrefix(why+"; docker has not answered keploy's look at the ports the agent container publishes", "; ")
		}
		return false, p.stalled(health, lastProbe, why, st, st.Progress.Restarts-min(st.Progress.Restarts, restarts0))
	}
	// onRead weighs a read's answer; done when the wait ends with it.
	onRead := func(r readResult) (bool, string, bool) {
		reading = false
		if p.tally != nil {
			p.tally.taken.Add(1)
		}
		if r.err == nil && r.st.ListenCount < p.since.listens {
			r.err = fmt.Errorf("the agent was told %d listens, fewer than the %d it had when this set's application started: another agent, which cannot tell this set's listens from an earlier one's", r.st.ListenCount, p.since.listens)
		}
		switch {
		case r.err != nil && ctx.Err() != nil:
			return false, "", true
		case errors.Is(r.err, models.ErrReadinessUnsupported) && !read:
			// An agent that cannot watch (another platform's, an
			// older one): a health URL decides alone, else the first
			// test's address taking a connect.
			p.agent = nil
			if health == "" {
				p.dial = true
				p.logger.Warn("keploy's agent cannot tell when the application listens; the first test's address taking a connect decides", zap.Error(r.err), zap.String("address", net.JoinHostPort(p.host, p.port)))
				startProbe()
			} else {
				p.logger.Warn("keploy's agent cannot tell when the application listens; the health URL alone decides", zap.Error(r.err), zap.String("healthUrl", sanitizeHealthURL(health)))
			}
		case r.err != nil:
			// No progress seen (a transient failure, the agent
			// replaced mid-wait); one that never ends is a stall, and
			// says what.
			lastErr = r.err.Error()
		default:
			st, lastErr, read = r.st, "", true
			if health == "" && resolved {
				if port, ok := p.listened(st); ok {
					ready, why := p.afterEvent(ctx, fmt.Sprintf("the application listens on :%d", port), first && wakes == 0)
					return ready, why, true
				}
			}
			mark = p.progressOf(st, restarts0)
			if first {
				first = false
				if p.stalledAt != nil && health == "" && mark == *p.stalledAt {
					// The same stall as an earlier set's: nothing moved
					// since.
					ready, why := stalled()
					return ready, why, true
				}
				last, said = mark, mark
			}
		}
		if pending {
			pending = false
			if done := weigh(); done {
				ready, why := stalled()
				return ready, why, true
			}
		}
		return false, "", false
	}
	// takePort takes docker's answer: the port the first test's is in the
	// container, and ready (done) if the application listens there already.
	takePort := func(port uint16) (ready bool, why string, done bool) {
		resolved, p.listen = true, port
		if p.tally != nil {
			p.tally.ports.Add(1)
		}
		// Judged on the latest read only: a read's answer already in, newer
		// than st (its listen closed since, a probe), is weighed now, with
		// the port, before any wake-up is.
		if len(reads) > 0 {
			return onRead(<-reads)
		}
		if health == "" && !p.dial && read {
			if port, ok := p.listened(st); ok {
				ready, why = p.afterEvent(ctx, fmt.Sprintf("the application listens on :%d", port), wakes == 0)
				return ready, why, true
			}
		}
		return false, "", false
	}
	startProbe()
	startRead()
	for {
		if !resolved {
			// docker's answer, once in, is taken before anything else: a
			// read or a wake-up weighed with it waiting would count against
			// the application a port docker had given.
			select {
			case port := <-ports:
				if ready, why, done := takePort(port); done {
					return ready, why
				}
			default:
			}
		}
		select {
		case <-ctx.Done():
			return false, ""
		case port := <-ports:
			if ready, why, done := takePort(port); done {
				return ready, why
			}
		case r := <-probes:
			probing = false
			if r.ok {
				return p.afterEvent(ctx, r.what, wakes == 0)
			}
			lastProbe = r.what // the next probe at the next wake-up
		case r := <-reads:
			if ready, why, done := onRead(r); done {
				return ready, why
			}
		case _, ok := <-wake:
			if !ok {
				return false, ""
			}
			// A read's answer already in is weighed first: the last wake-up
			// is weighed by it, not as one whose read never answered.
			select {
			case r := <-reads:
				if ready, why, done := onRead(r); done {
					return ready, why
				}
			default:
			}
			wakes++
			if reading {
				readWakes++
			}
			if pending && weigh() {
				// The last wake-up's read never answered: no progress seen.
				return stalled()
			}
			startProbe()
			startRead()
			pending = p.agent != nil
			if !pending && weigh() {
				return stalled()
			}
		}
	}
}

// takesConnect dials addr once, with no deadline but ctx's: true once it
// takes the connect (an application that cannot be watched, and has no
// health URL: what keploy test's own first request would need).
func takesConnect(ctx context.Context, addr string) (bool, string) {
	var d net.Dialer
	c, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return false, "last: " + err.Error()
	}
	_ = c.Close()
	return true, addr + " takes a connect"
}

// progressOf is st's progress, as the watchdog weighs it: before the
// application listens (a listen of this set's still listening, anywhere in
// the agent's network namespace), its listens, connects, process forks and
// execs, and its CPU time; after, its listens alone. Up and listening, an
// application's pool reconnects, heartbeats, schedulers and children (a
// database's autovacuum), and its runtime's CPU time, are no start going on:
// one that listens on a port the set's first test does not dial is not
// judged once it stops listening anew, however busy it is.
//
// A listen is progress the first time its socket address (port, address,
// namespace) is listened on in the set: a start that loops, listening on a
// side port (a debug port, JMX, metrics) before each crash, listens there
// again, and that is no progress. A listen the kernel had no room to report
// is counted as one.
//
// A restart (the kernel's: a command line run again once every earlier run
// of it in the set exited, a supervisor re-exec'ing a crashing application)
// is no progress either, nor a connect, fork or exec seen before (the kernel
// counts each the first time only; a command line run again while an
// earlier run lives, a second worker, and one exec'd again in place by the
// process running it, are neither progress nor a restart), nor, once the
// start restarted (restarts since the set's mark, restarts0), anyone's CPU
// time: a supervisor's own and the restarted process's, a little at each
// round, are a loop going round, not a start going on. A start that loops is
// not judged, saying how often it restarted. The cost, not judged, never a
// false pass: a start that runs one command line again after it exited (a
// probe run again) and then only computes, for longer than the count.
func (p provenReadiness) progressOf(st models.AppReadiness, restarts0 uint64) progressMark {
	listens := p.listenedAddresses(st)
	if p.listenedAnywhere(st) {
		return progressMark{p: models.ReadyProgress{Listens: listens}}
	}
	m := progressMark{p: st.Progress}
	m.p.Listens = listens
	m.p.Restarts = 0
	if st.CPUKnown && st.Progress.Restarts <= restarts0 {
		m.cpu = st.CPU
	}
	return m
}

// sayStillWaiting says, every readyStatusEvery wake-ups of a wait, what it
// waits on and what moved since it last said: an endless wait is told.
func (p provenReadiness) sayStillWaiting(health, lastProbe, lastErr string, st models.AppReadiness, was, now progressMark, stalls int, restarts uint64) {
	waits := fmt.Sprintf("a listen on :%d, where the set's first test dials %s", p.listen, net.JoinHostPort(p.host, p.port))
	switch {
	case p.resolve != nil && p.listen == 0 && health == "" && !p.dial:
		waits = fmt.Sprintf("a listen where the set's first test dials %s (docker has not said which container port that is)", net.JoinHostPort(p.host, p.port))
	case health != "":
		waits = sanitizeHealthURL(health) + "'s answer"
	case p.dial:
		waits = net.JoinHostPort(p.host, p.port) + " taking a connect (keploy's agent cannot tell when the application listens)"
	}
	fields := []zap.Field{zap.String("waitingFor", waits), zap.Int("wakeUpsWithNoProgress", stalls), zap.Int("notJudgedAt", readyStallLimit)}
	if p.agent != nil {
		fields = append(fields, zap.String("listening", p.listeningOn(st)),
			zap.Uint64("newListens", now.p.Listens-min(now.p.Listens, was.p.Listens)),
			zap.Uint64("newConnects", now.p.Connects-min(now.p.Connects, was.p.Connects)),
			zap.Uint64("newForks", now.p.Forks-min(now.p.Forks, was.p.Forks)),
			zap.Uint64("newExecs", now.p.Execs-min(now.p.Execs, was.p.Execs)),
			zap.Duration("newCPU", now.cpu-min(now.cpu, was.cpu)),
			zap.Uint64("restarts", restarts))
		if restarts > 0 && st.LastRestart != "" {
			fields = append(fields, zap.String("lastRestart", st.LastRestart))
		}
	}
	if lastProbe != "" {
		fields = append(fields, zap.String("lastProbe", lastProbe))
	}
	if lastErr != "" {
		fields = append(fields, zap.String("lastReadError", lastErr))
	}
	p.logger.Info("still waiting for the application before the set's first test (KEPLOY_PROVEN=1)", fields...)
}

// reachable says a listen of the application's is where keploy's requests
// reach it: in the agent's network namespace, and, in Docker mode, not on a
// loopback address only (docker forwards a published port to the
// container's own address).
func (p provenReadiness) reachable(l models.AppListen) bool {
	if !l.Here {
		return false
	}
	if p.docker {
		if a, err := netip.ParseAddr(l.Addr); err == nil && a.Unmap().IsLoopback() {
			return false
		}
	}
	return true
}

// listened says the application listens, where keploy's requests reach it,
// on p.listen: a listen of its the kernel reported since the set began
// (p.since) whose own socket, by cookie, still listens now. A listen whose
// socket has since closed (a start that probes a port by listening on it,
// one that died and is being restarted), an earlier set's application's, and
// a listen the kernel had no room to report (keploy cannot vouch for whose
// it was) are no readiness.
func (p provenReadiness) listened(st models.AppReadiness) (uint16, bool) {
	for _, l := range p.setListens(st) {
		if l.Port == p.listen && p.reachable(l) && l.Live {
			return p.listen, true
		}
	}
	return 0, false
}

// setListens are the listens the kernel reported since the set began.
func (p provenReadiness) setListens(st models.AppReadiness) []models.AppListen {
	var out []models.AppListen
	for _, l := range st.Listens {
		if l.Seq >= p.since.listens {
			out = append(out, l)
		}
	}
	return out
}

// listenedAnywhere says the application listens now, in the agent's network
// namespace, on any port: a listen since the set began whose socket still
// listens (a port probe it closed, an earlier set's application, is not).
func (p provenReadiness) listenedAnywhere(st models.AppReadiness) bool {
	for _, l := range p.setListens(st) {
		if l.Here && l.Live {
			return true
		}
	}
	return false
}

// listenedAddresses counts the socket addresses (port, address, namespace)
// the application listened on in the set, live or since closed, and the
// listens the kernel had no room to report.
func (p provenReadiness) listenedAddresses(st models.AppReadiness) uint64 {
	type address struct {
		port uint16
		addr string
		here bool
	}
	seen := map[address]bool{}
	for _, l := range p.setListens(st) {
		seen[address{l.Port, l.Addr, l.Here}] = true
	}
	return uint64(len(seen)) + st.ListensLost
}

// afterEvent logs the event and adds --delay after it, but for a kept-alive
// application ready at the first look (warm: --delay is for a start, and
// there was none).
func (p provenReadiness) afterEvent(ctx context.Context, what string, atOnce bool) (bool, string) {
	delay := time.Duration(p.cfg.Test.Delay) * time.Second
	if p.warm && atOnce {
		delay = 0
	}
	p.logger.Info("the application is ready; firing the first test", zap.String("ready", what), zap.Duration("then --delay", delay))
	if delay <= 0 {
		return ctx.Err() == nil, ""
	}
	t := time.NewTimer(delay)
	defer t.Stop()
	select {
	case <-t.C:
		return true, ""
	case <-ctx.Done():
		return false, ""
	}
}

// healthTarget is the URL to wait on, and whether any HTTP answer will do
// (--health-path) rather than a 2xx (--health-url); "" for none. An invalid
// --health-url is none (appReady says so, once a run), and the listen is the
// event.
func (p provenReadiness) healthTarget() (string, bool) {
	if u := strings.TrimSpace(p.cfg.Test.HealthURL); u != "" {
		if _, ok := validateHealthURL(u); ok {
			return u, false
		}
	}
	if path := strings.TrimSpace(p.cfg.Test.HealthPath); path != "" && p.port != "" {
		if !strings.HasPrefix(path, "/") {
			path = "/" + path
		}
		scheme := p.scheme
		if scheme == "" {
			scheme = "http"
		}
		if forced := strings.ToLower(strings.TrimSpace(p.cfg.Test.HealthScheme)); forced == "http" || forced == "https" {
			scheme = forced
		}
		return fmt.Sprintf("%s://%s%s", scheme, net.JoinHostPort(p.host, p.port), path), true
	}
	return "", false
}

// stalled is the not-judged reason of a wait that saw no progress across the
// watchdog's count.
func (p provenReadiness) stalled(health, lastProbe, lastErr string, st models.AppReadiness, restarts uint64) string {
	idle := fmt.Sprintf("it made no progress (no new listen, and before it listened no new connect, process fork or exec, or CPU time used) across %d wake-ups of keploy's watchdog", readyStallLimit)
	if !p.listenedAnywhere(st) && !st.CPUKnown {
		idle = fmt.Sprintf("it made no progress (no new listen, connect, process fork or exec; its CPU time keploy could not read) across %d wake-ups of keploy's watchdog", readyStallLimit)
	}
	if p.agent == nil {
		idle = fmt.Sprintf("across %d wake-ups of keploy's watchdog (keploy's agent cannot tell when the application listens)", readyStallLimit)
	}
	var what string
	switch {
	case health != "":
		what = fmt.Sprintf("the application never answered %s", sanitizeHealthURL(health))
	case p.dial:
		what = fmt.Sprintf("%s, where the set's first test dials, never took a connect", net.JoinHostPort(p.host, p.port))
	case p.resolve != nil && p.listen == 0:
		// docker has not said which container port the first test's is.
		what = fmt.Sprintf("the application never listened where the set's first test dials %s", net.JoinHostPort(p.host, p.port))
	default:
		what = fmt.Sprintf("the application never listened on :%d, where the set's first test dials %s", p.listen, net.JoinHostPort(p.host, p.port))
	}
	if lastProbe != "" {
		what += " (" + lastProbe + ")"
	}
	if p.agent != nil {
		what += "; " + p.listeningOn(st)
		if restarts > 0 {
			what += fmt.Sprintf("; it restarted %d time(s)", restarts)
			if !p.listenedAnywhere(st) {
				what += " without listening"
			}
			if st.LastRestart != "" {
				what += " (the last: " + st.LastRestart + ")"
			}
		}
	}
	if lastErr != "" {
		what += fmt.Sprintf("; %v at the last read: %s", errNotReadable, lastErr)
	}
	return what + "; " + idle + ", so keploy never fired its tests"
}

// listeningOn says which ports the application listens on: its reported
// listens where keploy's requests reach it whose port is still listened on,
// those since closed, those on loopback only in a container, and those in
// another network namespace.
func (p provenReadiness) listeningOn(st models.AppReadiness) string {
	// A socket address listened on again is listening: its live listen
	// stands for the ones before it, closed.
	key := func(l models.AppListen) string {
		return strconv.Itoa(int(l.Port)) + strconv.FormatBool(l.Here) + l.Addr
	}
	live := map[string]bool{}
	for _, l := range p.setListens(st) {
		if l.Live {
			live[key(l)] = true
		}
	}
	seen := map[string]bool{}
	var here, closed, loopback, elsewhere []string
	for _, l := range p.setListens(st) {
		s := ":" + strconv.Itoa(int(l.Port))
		if seen[key(l)] || (!l.Live && live[key(l)]) {
			continue
		}
		seen[key(l)] = true
		switch {
		case !l.Here:
			elsewhere = append(elsewhere, s)
		case !p.reachable(l):
			loopback = append(loopback, net.JoinHostPort(l.Addr, strconv.Itoa(int(l.Port))))
		case l.Live:
			here = append(here, s)
		default:
			closed = append(closed, s)
		}
	}
	for _, list := range [][]string{here, closed, loopback, elsewhere} {
		sort.Strings(list)
	}
	out := "it listens on no port"
	if len(here) > 0 {
		out = "it listens on " + strings.Join(here, ", ")
	}
	if len(closed) > 0 {
		out += " (it listened on " + strings.Join(closed, ", ") + " and closed it)"
	}
	if len(loopback) > 0 {
		out += " (and on " + strings.Join(loopback, ", ") + " only, which no published port reaches: listen on 0.0.0.0 in the container)"
	}
	if len(elsewhere) > 0 {
		out += " (and on " + strings.Join(elsewhere, ", ") + " in another network namespace, where keploy's requests cannot reach it)"
	}
	if st.ListensLost > p.since.lost {
		out += fmt.Sprintf(" (and the kernel had no room to report %d listen(s), whose keploy cannot vouch for)", st.ListensLost-p.since.lost)
	}
	return out
}

// answers sends one GET to target, with no deadline but ctx's: true on a 2xx
// (any HTTP answer when anyAnswer), with what it saw otherwise. A 2xx is
// looked for across redirects, as the health poll always followed them; any
// answer is the first one.
func answers(ctx context.Context, target string, anyAnswer bool) (bool, string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return false, err.Error()
	}
	client := &http.Client{
		Transport: &http.Transport{DisableKeepAlives: true, Proxy: nil, TLSClientConfig: &tls.Config{}},
	}
	if anyAnswer {
		client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	}
	defer client.CloseIdleConnections()
	resp, err := client.Do(req)
	if err != nil {
		if anyAnswer && isTLSVerificationError(err) {
			// It completed a TLS handshake: it serves.
			return true, sanitizeHealthURL(target) + " answered (a certificate keploy does not trust)"
		}
		// Its cause only: the URL, which may carry a secret, is said
		// sanitized where the reason is written.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return false, "last: " + err.Error()
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if anyAnswer || (resp.StatusCode >= 200 && resp.StatusCode < 300) {
		return true, fmt.Sprintf("%s answered %d", sanitizeHealthURL(target), resp.StatusCode)
	}
	return false, fmt.Sprintf("last answer: %d", resp.StatusCode)
}

// firstDialTarget is where the set's first test that says dials: an HTTP
// test's resolved URL (with its scheme), a gRPC test's host and port.
func firstDialTarget(ctx context.Context, testCfg config.Test, testCases []*models.TestCase, testSetID string, logger *zap.Logger, published func(context.Context, string, uint16) uint16) (scheme, host, port string, ok bool) {
	for _, tc := range testCases {
		if tc == nil {
			continue
		}
		switch tc.Kind {
		case models.HTTP:
			if sc, h, p, ok := resolveProbeTarget(ctx, testCfg, tc, testSetID, logger, published); ok {
				return sc, h, p, true
			}
		case models.GRPC_EXPORT:
			// Where the test is sent (pkg.SimulateGRPC, as the replay hooks
			// configure it): test.host for the authority's host, its app
			// port, test.grpcPort, replaceWith, and the one host port its
			// app port is published on.
			if h, p, ok := resolveGRPCDialTarget(ctx, testCfg, tc, testSetID, logger, published); ok {
				return "", h, p, true
			}
		}
	}
	return "", "", "", false
}

// portMapping is one published port: the host's, and the container's it
// forwards to.
type portMapping struct{ host, container uint16 }

// listenPortFor is the port the application listens on for a test dialing
// hostPort: in Docker mode the container port the keploy agent container
// (which publishes the application's ports) maps it to, else what the docker
// run's -p or the compose service's ports say; hostPort itself when nothing
// maps it.
func listenPortFor(ctx context.Context, pub publishedPortReader, cmd, containerName string, hostPort uint16) uint16 {
	if pub != nil {
		if c, ok := pub.PublishedContainerPort(ctx, hostPort); ok {
			return c
		}
	}
	maps := dockerRunMappings(cmd)
	if len(maps) == 0 {
		maps = composeMappings(cmd, containerName)
	}
	for _, m := range maps {
		if m.host == hostPort {
			return m.container
		}
	}
	return hostPort
}

// dockerRunMappings reads every -p/--publish of a docker run command.
func dockerRunMappings(cmd string) []portMapping {
	fields := strings.Fields(cmd)
	var out []portMapping
	for i := 0; i+1 < len(fields); i++ {
		spec := ""
		switch {
		case fields[i] == "-p" || fields[i] == "--publish":
			spec = fields[i+1]
		case strings.HasPrefix(fields[i], "--publish="):
			spec = strings.TrimPrefix(fields[i], "--publish=")
		default:
			continue
		}
		if m, ok := parsePortMapping(spec, os.LookupEnv); ok {
			out = append(out, m)
		}
	}
	return out
}

// composeVar is a compose file's $$ (a literal $), ${VAR}, ${VAR:-d},
// ${VAR-d}, ${VAR:?e}, ${VAR?e}, ${VAR:+r}, ${VAR+r}, or $VAR.
var composeVar = regexp.MustCompile(`\$\$|\$\{([A-Za-z_][A-Za-z0-9_]*)(?:(:?[-?+])([^}]*))?\}|\$([A-Za-z_][A-Za-z0-9_]*)`)

// envLookup is a variable's value, and whether it is set.
type envLookup func(string) (string, bool)

// interpolate expands a compose file's variables as compose does, from env:
// ${VAR:-d} is d when VAR is unset or empty, ${VAR-d} when it is unset;
// ${VAR:+r} is r when VAR is set and not empty, ${VAR+r} when it is set;
// ${VAR:?e} and ${VAR?e} are VAR (compose refuses the file when it is not
// set, and then nothing here runs).
func interpolate(s string, env envLookup) string {
	return composeVar.ReplaceAllStringFunc(s, func(m string) string {
		if m == "$$" {
			return "$"
		}
		g := composeVar.FindStringSubmatch(m)
		if g[4] != "" {
			v, _ := env(g[4])
			return v
		}
		v, set := env(g[1])
		switch g[2] {
		case ":-":
			if v == "" {
				return g[3]
			}
		case "-":
			if !set {
				return g[3]
			}
		case ":+":
			if v != "" {
				return g[3]
			}
			return ""
		case "+":
			if set {
				return g[3]
			}
			return ""
		}
		return v
	})
}

// composeEnv is what compose interpolates a project's files from: the
// environment, then the project's .env file (each --env-file given, else
// .env in the --project-directory, else in the first file's directory).
func composeEnv(cmd string, files []string) envLookup {
	fields := strings.Fields(cmd)
	var envFiles []string
	dir := ""
	if len(files) > 0 {
		dir = filepath.Dir(files[0])
	}
	for i, f := range fields {
		switch {
		case f == "--env-file" && i+1 < len(fields):
			envFiles = append(envFiles, fields[i+1])
		case strings.HasPrefix(f, "--env-file="):
			envFiles = append(envFiles, strings.TrimPrefix(f, "--env-file="))
		case f == "--project-directory" && i+1 < len(fields):
			dir = fields[i+1]
		case strings.HasPrefix(f, "--project-directory="):
			dir = strings.TrimPrefix(f, "--project-directory=")
		}
	}
	if len(envFiles) == 0 {
		envFiles = []string{filepath.Join(dir, ".env")}
	}
	dotenv := map[string]string{}
	for _, path := range envFiles {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "export "))
			k, v, ok := strings.Cut(line, "=")
			if !ok || strings.HasPrefix(line, "#") {
				continue
			}
			v = strings.TrimSpace(v)
			if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
				v = v[1 : len(v)-1]
			} else if i := strings.Index(v, " #"); i >= 0 {
				v = strings.TrimSpace(v[:i])
			}
			dotenv[strings.TrimSpace(k)] = v
		}
	}
	return func(name string) (string, bool) {
		if v, ok := os.LookupEnv(name); ok {
			return v, true
		}
		v, ok := dotenv[name]
		return v, ok
	}
}

// parsePortMapping reads docker's short syntax: [ip:]host:container[/proto];
// ranges and a bare container port (a random host port) map nothing.
func parsePortMapping(spec string, env envLookup) (portMapping, bool) {
	spec = strings.SplitN(strings.TrimSpace(interpolate(spec, env)), "/", 2)[0]
	segs := strings.Split(spec, ":")
	if len(segs) < 2 {
		return portMapping{}, false
	}
	h, err1 := strconv.ParseUint(segs[len(segs)-2], 10, 16)
	c, err2 := strconv.ParseUint(segs[len(segs)-1], 10, 16)
	if err1 != nil || err2 != nil || h == 0 || c == 0 {
		return portMapping{}, false
	}
	return portMapping{uint16(h), uint16(c)}, true
}

// composeFiles are the compose files cmd runs: every -f/--file, else what
// docker compose itself looks for in the working directory.
func composeFiles(cmd string) []string {
	fields := strings.Fields(cmd)
	var files []string
	for i, f := range fields {
		switch {
		case (f == "-f" || f == "--file") && i+1 < len(fields):
			files = append(files, fields[i+1])
		case strings.HasPrefix(f, "--file="):
			files = append(files, strings.TrimPrefix(f, "--file="))
		}
	}
	if len(files) > 0 {
		return files
	}
	if f, ok := composeFileFromCommand(cmd); ok {
		return []string{f}
	}
	return nil
}

// composeMappings reads the application service's ports in the compose files
// cmd runs: the service named containerName (by its container_name or its
// own name), or the only one publishing.
func composeMappings(cmd, containerName string) []portMapping {
	byService := map[string][]portMapping{}
	named := map[string]bool{}
	files := composeFiles(cmd)
	env := composeEnv(cmd, files)
	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var doc struct {
			Services map[string]struct {
				ContainerName string      `yaml:"container_name"`
				Ports         []yaml.Node `yaml:"ports"`
			} `yaml:"services"`
		}
		if err := yaml.Unmarshal(data, &doc); err != nil {
			continue
		}
		for name, svc := range doc.Services {
			if containerName != "" && (interpolate(svc.ContainerName, env) == containerName || name == containerName) {
				named[name] = true
			}
			for i := range svc.Ports {
				n := &svc.Ports[i]
				switch n.Kind {
				case yaml.ScalarNode:
					if m, ok := parsePortMapping(n.Value, env); ok {
						byService[name] = append(byService[name], m)
					}
				case yaml.MappingNode:
					var long struct {
						Published interface{} `yaml:"published"`
						Target    interface{} `yaml:"target"`
					}
					if err := n.Decode(&long); err != nil || long.Published == nil || long.Target == nil {
						continue
					}
					h, err1 := strconv.ParseUint(strings.TrimSpace(interpolate(fmt.Sprint(long.Published), env)), 10, 16)
					c, err2 := strconv.ParseUint(strings.TrimSpace(interpolate(fmt.Sprint(long.Target), env)), 10, 16)
					if err1 == nil && err2 == nil && h > 0 && c > 0 {
						byService[name] = append(byService[name], portMapping{uint16(h), uint16(c)})
					}
				}
			}
		}
	}
	if len(named) == 1 {
		for name := range named {
			return byService[name]
		}
	}
	if len(byService) == 1 {
		for _, ms := range byService {
			return ms
		}
	}
	return nil
}

// appReady waits for the set's application before its first test: with the
// proven switch off as keploy always has (waitForAppReady, for an application
// keploy started; none for one it did not), and under it for the event
// (provenReadiness): ready (true), the run stopped (false, ""), or why the
// application never got ready (false, why), and then the set is not judged.
// warm says the application is one kept alive from an earlier set
// (--keep-app-alive past the first), whose listens are all its own and whose
// stall, nothing moved since, is the same stall.
func (r *Replayer) appReady(ctx context.Context, testCases []*models.TestCase, testSetID string, started, warm bool) (bool, string) {
	if !provenswitch.On() {
		if !started {
			return true, ""
		}
		return waitForAppReady(ctx, r.logger, r.config, r.readinessProbeTarget(ctx, testCases, testSetID)), ""
	}
	p := provenReadiness{logger: r.logger, cfg: r.config, docker: utils.IsDockerCmd(utils.CmdType(r.config.CommandType))}
	scheme, host, port, ok := firstDialTarget(ctx, r.config.Test, testCases, testSetID, r.logger, publishedPortsOf(r.logger, r.instrumentation).of(testSetID))
	if ok {
		p.scheme, p.host, p.port = scheme, host, port
	}
	if u := strings.TrimSpace(r.config.Test.HealthURL); u != "" {
		if reason, valid := validateHealthURL(u); !valid {
			r.badHealthURLSaid.Do(func() {
				r.logger.Error("invalid --health-url; the application's listen is the event instead",
					zap.String("healthUrl", sanitizeHealthURL(u)), zap.String("reason", reason),
					zap.String("next_step", "--health-url must be a full URL with scheme (http:// or https://) and host"))
			})
		}
	}
	health, _ := p.healthTarget()
	if !started {
		// The first test's own connect is the event, but for an explicit
		// --health-url.
		if strings.TrimSpace(r.config.Test.HealthURL) == "" || health == "" {
			return true, ""
		}
		return p.wait(ctx)
	}
	if !ok && health == "" {
		// Nowhere the first test says it dials: its own connect is the event.
		return true, ""
	}
	p.warm = warm
	a, isReader := r.instrumentation.(readinessReader)
	if !isReader {
		if health != "" {
			r.logger.Warn("this keploy cannot ask its agent when the application listens; the health URL alone decides")
			return p.wait(ctx)
		}
		r.logger.Warn("this keploy cannot ask its agent when the application listens; the first test's address taking a connect decides", zap.String("address", net.JoinHostPort(p.host, p.port)))
		p.dial = true
		return p.wait(ctx)
	}
	p.agent = a
	if !warm {
		// A set whose application keploy started anew: only the listens
		// after it started it.
		if r.readySinceErr != "" {
			return false, fmt.Sprintf("%v: before it started the set's application keploy could not read which listens its agent had seen already, so none tells this application's from an earlier one's: %s", errNotReadable, r.readySinceErr)
		}
		if r.readySince != nil {
			// Its listens, and its restarts, begin at the mark before keploy
			// started it: a restart before the wait's first read is one.
			// With no mark (an application kept alive for the run, one that
			// came up with its agent, compose) both begin at 0: the agent's
			// every listen and restart is the application's.
			p.since = *r.readySince
		}
	} else if health == "" {
		p.stalledAt = r.readyStalledAt
	}
	p.onStall = func(m progressMark) { r.readyStalledAt = &m }
	p.tally = r.readyTally
	if n, err := strconv.ParseUint(p.port, 10, 16); err == nil {
		if p.docker {
			// The container port docker publishes it to: a look at docker,
			// under the watchdog.
			pub, _ := r.instrumentation.(publishedPortReader)
			cmd, name := r.config.Command, r.config.ContainerName
			p.resolve = func(ctx context.Context) uint16 { return listenPortFor(ctx, pub, cmd, name, uint16(n)) }
		} else {
			p.listen = uint16(n) // a native application listens on it
		}
	}
	ready, why := p.wait(ctx)
	if ready {
		r.readyStalledAt = nil
	}
	return ready, why
}

func (r *Replayer) readinessProbeTarget(ctx context.Context, testCases []*models.TestCase, testSetID string) httpProbeTarget {
	published := publishedPortsOf(r.logger, r.instrumentation).of(testSetID)
	probe := resolveTestSetProbeTarget(ctx, r.config.Test, testCases, testSetID, r.logger, appPortReachabilityOf(r.instrumentation), published)
	if probe.ok || utils.IsDockerCmd(utils.CmdType(r.config.CommandType)) {
		return probe
	}
	return resolveTestSetGRPCTarget(ctx, r.config.Test, testCases, testSetID, r.logger, published)
}

// markReadiness reads, under the proven switch, the agent's count of the
// listens it was told before keploy starts a set's application anew: that
// set's readiness looks only at those after (provenReadiness.since). An agent
// that cannot watch leaves nothing to mark (the wait warns, and the first
// test's own connect decides); one that cannot be read now leaves the set
// unable to tell its application's listens from an earlier one's.
func (r *Replayer) markReadiness(ctx context.Context) {
	r.readySince, r.readySinceErr = nil, ""
	if !provenswitch.On() {
		return
	}
	var st models.AppReadiness
	var err error
	if m, ok := r.instrumentation.(readinessMarker); ok {
		st, err = m.MarkReadiness(ctx)
	} else if a, ok := r.instrumentation.(readinessReader); ok {
		st, err = a.AppReadiness(ctx, math.MaxUint64)
	} else {
		return
	}
	switch {
	case errors.Is(err, models.ErrReadinessUnsupported):
	case err != nil && ctx.Err() != nil:
		// The run stopped: the set ends as a stop, not as not judged.
	case err != nil:
		r.readySinceErr = err.Error()
	default:
		r.readySince = &readyMark{listens: st.ListenCount, lost: st.ListensLost, restarts: st.Progress.Restarts}
	}
}

// waitsForApp says a set waits for its application before its first test:
// always under the proven switch, a kept-alive application's later sets too
// (one ready is ready at the first read; one that stalled, nothing moved
// since, is not judged at once); with it off, not a kept-alive application's
// after the first set (--keep-app-alive: the app is warm, and
// waitForAppReady's --delay would be dead time per set).
func waitsForApp(serveTest, firstSet bool) bool {
	return !serveTest || firstSet || provenswitch.On()
}
