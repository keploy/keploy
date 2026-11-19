package agent

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"

	"go.uber.org/zap"

	coreAgent "go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/models"
)

// CaptureLosses counts every kind of capture loss the agent can see, each
// only ever growing for its life (models.CaptureLosses): the processes the
// kernel refused a place in the PID maps (the hooks), the calls the proxy
// intercepted and could not record or mock (a TLS client that rejected
// keploy's certificate, a TLS upgrade it could not make), what the kernel
// dropped at the proxy's listener that the tests' ends charged them
// (settledSince), and a downstream build's own
// (coreAgent.RegisterCaptureLossReporter). Each later source
// (design v14 §3.2) adds its kind here. With neither source able to
// count it gives models.ErrCaptureLossesUnsupported, "unknown"; a count that
// could not be read is its own error.
//
// This is what GET /agent/capturelosses answers, the run's own verdict's
// read: the counts (captureCounts), and every loss the wire made the run's
// (runsOwn), which no count holds -- a close counted in a test process's
// gap between tests, charged to no test, still fails the replay it was
// counted in, or leaves it not judged. The tests' own charges read the
// counts alone.
func (a *Agent) CaptureLosses(ctx context.Context) (models.CaptureLosses, error) {
	out, err := a.captureCounts(ctx)
	if err != nil {
		return nil, err
	}
	a.provenMu.Lock()
	run, at := a.runsOwn, maps.Clone(a.proven.runsAt)
	a.provenMu.Unlock()
	return withCounted(out, run, at), nil
}

// withCounted is counts with each of run's losses added to the count of
// its kind (a kind none of counts has, appended): one count per kind, as
// models.CaptureLosses.Since reads them. A count is named by its newest
// loss: run's words name it only when this session made a loss of its kind
// that names what it lost the run's (at, provenWire.runsAt) and the count
// has not grown past what was read with it; a count otherwise keeps its own
// words, and a kind none of counts has keeps none of an earlier session's.
func withCounted(counts, run models.CaptureLosses, at map[models.CaptureLossKind]uint64) models.CaptureLosses {
	if len(run) == 0 {
		return counts
	}
	out := append(models.CaptureLosses(nil), counts...)
	for _, r := range run {
		read, newest := at[r.Kind]
		i := slices.IndexFunc(out, func(c models.CaptureLoss) bool { return c.Kind == r.Kind })
		if i < 0 {
			if !newest {
				r = models.CaptureLoss{Kind: r.Kind, Count: r.Count, Err: r.Err}
			}
			out = append(out, r)
			continue
		}
		newest = newest && out[i].Count <= read
		out[i].Count += r.Count
		if newest && r.What != "" {
			out[i].SetWhatOf(r)
		}
	}
	return out
}

// captureCounts is CaptureLosses's counts: the hooks', the proxy's, the
// listener's and a downstream build's. Each test's charge, and the run's
// mark, count from them.
func (a *Agent) captureCounts(ctx context.Context) (models.CaptureLosses, error) {
	// Every report the kernel made before this read is counted first, so a
	// call the application made before it is in it (a test's end, the run's
	// last read), however late the agent's goroutines run.
	if d, ok := a.Hooks.(coreAgent.CaptureLossDrainer); ok && a.Hooks != nil {
		if err := d.DrainCaptureLosses(ctx); err != nil {
			return nil, err
		}
	}
	extra := coreAgent.CaptureLossReporters()
	for _, r := range extra {
		if d, ok := r.(coreAgent.CaptureLossDrainer); ok {
			if err := d.DrainCaptureLosses(ctx); err != nil {
				return nil, err
			}
		}
	}
	var out models.CaptureLosses
	proxy, proxyCounts := a.Proxy.(coreAgent.CaptureLossReporter)
	if proxyCounts && a.Proxy != nil {
		out = append(out, proxy.CaptureLosses()...)
	}
	// What the kernel dropped at the proxy's listener in the windows the
	// tests' ends closed (settledSince), each test's own.
	out = append(out, a.listenerDrops.Snapshot()...)
	// The hooks' own: the application's connections lost before the proxy
	// got them, and its AF_UNIX connects to sockets no member of it owns
	// (under the proven switch; none otherwise).
	if hooks, ok := a.Hooks.(coreAgent.CaptureLossReporter); ok && a.Hooks != nil {
		out = append(out, hooks.CaptureLosses()...)
	}
	// A downstream build's own capture's (coreAgent.RegisterCaptureLossReporter),
	// unknown, never 0, while one of them cannot count.
	for _, r := range extra {
		out = append(out, r.CaptureLosses()...)
		// Asked after the read, as the hooks are: a read that failed is
		// this read's.
		if blind, ok := r.(coreAgent.CaptureLossErrReporter); ok {
			if err := blind.CaptureLossesErr(); err != nil {
				return nil, err
			}
		}
	}
	// A count of theirs that could not start is unknown, never 0: not
	// models.ErrCaptureLossesUnsupported (an agent that counts nothing),
	// which leaves every test as it was.
	if blind, ok := a.Hooks.(coreAgent.CaptureLossErrReporter); ok && a.Hooks != nil {
		if err := blind.CaptureLossesErr(); err != nil {
			return nil, err
		}
	}
	refusals, err := a.PIDMapRefusals(ctx)
	switch {
	case errors.Is(err, models.ErrPIDMapRefusalsUnsupported):
		// A downstream build's reporter counts too: with one, the agent
		// counts something, whatever its hooks can.
		if !proxyCounts && len(extra) == 0 {
			return nil, fmt.Errorf("%w: %v", models.ErrCaptureLossesUnsupported, err)
		}
	case err != nil:
		return nil, fmt.Errorf("read the PID map refusals: %w", err)
	default:
		// One kind per map, so what grew since a mark names its own map.
		if refusals.Target > 0 {
			out = append(out, models.CaptureLoss{Kind: models.CaptureLossPIDMap, Count: refusals.Target, What: pidMapCause("target_namespace_pids", refusals.LastTargetErr), Err: refusals.LastTargetErr})
		}
		if refusals.Excluded > 0 {
			out = append(out, models.CaptureLoss{Kind: models.CaptureLossPIDMapExcluded, Count: refusals.Excluded, What: pidMapCause("excluded_pids", refusals.LastExcludedErr), Err: refusals.LastExcludedErr})
		}
	}
	return mergeKinds(out), nil
}

// countsCaptureLosses says the agent's capture losses can be read: CaptureLosses
// gives no models.ErrCaptureLossesUnsupported, as its proxy counts them, a
// downstream build's capture does, or its hooks count the PID maps' refusals.
func (a *Agent) countsCaptureLosses(ctx context.Context) bool {
	if _, ok := a.Proxy.(coreAgent.CaptureLossReporter); ok && a.Proxy != nil {
		return true
	}
	if len(coreAgent.CaptureLossReporters()) > 0 {
		return true
	}
	_, err := a.PIDMapRefusals(ctx)
	return !errors.Is(err, models.ErrPIDMapRefusalsUnsupported)
}

// mergeKinds is l with one entry per kind, in the order each kind first comes:
// two sources may count one kind (the hooks' kernel_skipped and a downstream
// build's, the proxy's capture_dropped and its ring's), and a mark is read
// by kind (models.CaptureLosses.Since keeps one count per kind), so the
// counts are added, which only grow as each does, the Whats joined (with the
// hosts each names), and the first Err any of them said kept.
func mergeKinds(l models.CaptureLosses) models.CaptureLosses {
	at := make(map[models.CaptureLossKind]int, len(l))
	var out models.CaptureLosses
	for _, c := range l {
		i, ok := at[c.Kind]
		if !ok {
			at[c.Kind] = len(out)
			out = append(out, c)
			continue
		}
		out[i].Count += c.Count
		if out[i].Err == 0 {
			out[i].Err = c.Err
		}
		// Each What with the hosts it names (CaptureLoss.Hosts).
		if c.What != out[i].What {
			out[i].JoinWhat(c)
		}
	}
	return out
}

// pidMapCause names a PID map's latest refusal by the map and its errno, never
// a value the app sent.
func pidMapCause(m string, err int32) string {
	if c := models.PIDMapRefusalCause(err); c != "" {
		return m + ", " + c
	}
	return m
}

// captureLossMark is the run's capture-loss counts at its start, which each
// test's end counts from. known is false when they could not be read; err
// says why.
type captureLossMark struct {
	at    models.CaptureLosses
	known bool
	err   error
}

// markCaptureLosses reads the counts the run's tests are counted from. Called
// as a session starts.
func (a *Agent) markCaptureLosses(ctx context.Context) captureLossMark {
	at, err := a.captureCounts(ctx)
	if err != nil {
		return captureLossMark{err: err}
	}
	return captureLossMark{at: at, known: true}
}

// UnchargedCaptureLosses is what the agent could not capture since the run
// began that no test was charged with: every test's end charges what was lost
// up to it (chargeCaptureLosses: the open test and every later one for a
// taint, the test it happened in for a test's own kind), so this is what was
// lost after the last test ended, or before a test process's claim. The CLI
// reads it once the run's last test has ended; a record's judging fails the
// run on it (design v14 §7.5: evidence charged to no test). A standing state
// (unix_unseen) is kept by every mark and is in it whatever was charged.
func (a *Agent) UnchargedCaptureLosses(ctx context.Context) (models.CaptureLosses, error) {
	// First, the proxy counts the close of every connection the run's stop
	// ended (DrainCaptureLosses): a loss of one is in this read, the
	// session's last, never only the log line of a later session's reset.
	// One it could not finish fails the read, below, once the one-process
	// switch is off.
	drainErr := a.DrainCaptureLosses(ctx)
	// What the listener dropped after the last test is counted first
	// (closeDropsGap), as every loss after it is.
	a.closeDropsGap(ctx)
	// The session's last read: no test of a run of one test process is
	// begun after it, so a close of a connection of no worker's is counted
	// again (coreAgent.PairedBytesBeginner), as the session's reset counts
	// it -- one counted during the session's teardown is in the counts
	// before the next session's mark, said as the proxy counts it, and no
	// later session's. Told before the take below, so one the proxy stamped
	// before it is taken here: the run's.
	if pb, ok := a.Proxy.(coreAgent.PairedBytesBeginner); ok {
		pb.PairedBytesOneProcess(false)
	}
	if drainErr != nil {
		return nil, drainErr
	}
	// Taken first: a loss the proxy counted before the counts are read
	// is in this answer whichever way it was counted.
	var runs models.CaptureLosses
	for _, o := range a.takeOwnedLosses() {
		runs = append(runs, o.Loss)
	}
	a.provenMu.Lock()
	// Kept for a worker's open test whose end never charged them.
	for id, kept := range a.proven.ownedOpen {
		runs = append(runs, kept...)
		delete(a.proven.ownedOpen, id)
	}
	mark, charged, outside := a.proven.losses, a.proven.lossesCharged, a.proven.unreadOutside
	a.provenMu.Unlock()
	_, now, err := a.captureLossesSince(ctx, mark, nil)
	// The run's, with the counts read with them (none, when the read
	// failed), and kept whatever it gave.
	a.provenMu.Lock()
	a.runsLocked(runs, now, err == nil)
	owned := a.proven.ownedRun
	a.provenMu.Unlock()
	if err != nil {
		return nil, err
	}
	// What cut-short ends owned that was counted after their charge: in
	// no count, so in no test's charge, and the run's.
	uncharged := sinceRun(now, mark).Since(charged).Plus(owned)
	if !a.replaying() {
		// A record charges a container read the daemon did not answer by
		// the mocks its stamps marked (unreadMock): the run's are those of
		// the time outside tests, whatever the count.
		uncharged = uncharged.Without(models.CaptureLossContainerUnread).Plus(outside)
	}
	return uncharged, nil
}

// DrainCaptureLosses waits until the proxy has counted the close of every
// connection whose application end has ended, or whose close it has begun
// (coreAgent.PairedBytesDrainer): keploy test asks at a test set's end, once
// the set's application has stopped and before it reads the set's losses,
// and the session's last read (UnchargedCaptureLosses) drains first. A loss
// of a close the application's stop made is then in that read, never in a
// later set's or session's. nil from a proxy that does not drain.
func (a *Agent) DrainCaptureLosses(ctx context.Context) error {
	d, ok := a.Proxy.(coreAgent.PairedBytesDrainer)
	if !ok {
		return nil
	}
	if err := d.PairedBytesDrain(ctx); err != nil {
		return fmt.Errorf("keploy could not count the close of every connection the application's stop ended: %w", err)
	}
	return nil
}

// runsLocked makes l, losses this session counted, the run's: charged to no
// test (ownedRun, UnchargedCaptureLosses) and in the run's own count
// (runsOwn, CaptureLosses), which no count of the proxy's holds. at is the
// counts read with them, and read says that read worked: each kind's count
// read there is noted with the newest of l that names what it lost (runsAt),
// so a later count of the kind is named by its own words (withCounted). A
// read that failed notes nothing, and leaves the note an earlier read of this
// session made. With provenMu held.
func (a *Agent) runsLocked(l, at models.CaptureLosses, read bool) {
	a.proven.ownedRun = a.proven.ownedRun.Plus(l)
	a.runsOwn = a.runsOwn.Plus(l)
	for _, r := range l {
		if r.What == "" || !read {
			continue
		}
		if a.proven.runsAt == nil {
			a.proven.runsAt = map[models.CaptureLossKind]uint64{}
		}
		a.proven.runsAt[r.Kind] = kindCount(at, r.Kind)
	}
}

// takeOwnedLosses takes, once each, the losses the proxy counted that a test
// whose end was cut short owned (coreAgent.OwnedLossTaker), or that a close
// of a worker's connection charged (OwnedLoss.Open). None from a proxy that
// does not tell them apart, whose counts hold every loss. Only the open
// session's: one the proxy stamped with an earlier session
// (coreAgent.PairedBytesSessioner) was made in a session that is over, which
// took everything it could, and no test of this one caused it; it is logged,
// once, and charges no run or test.
func (a *Agent) takeOwnedLosses() []coreAgent.OwnedLoss {
	t, ok := a.Proxy.(coreAgent.OwnedLossTaker)
	if !ok || a.Proxy == nil {
		return nil
	}
	taken := t.TakeOwnedLosses()
	if _, stamps := a.Proxy.(coreAgent.PairedBytesSessioner); !stamps || len(taken) == 0 {
		return taken
	}
	a.provenMu.Lock()
	session := a.session
	a.provenMu.Unlock()
	mine := make([]coreAgent.OwnedLoss, 0, len(taken))
	for _, o := range taken {
		if o.Session == session {
			mine = append(mine, o)
			continue
		}
		a.logEarlierSessionsLoss(o.Session, o.Test, o.Loss, lateLoss)
	}
	return mine
}

// lateLoss and leftLoss are the two ways a loss of an earlier session reaches
// none of the open session's verdicts (logEarlierSessionsLoss): counted after
// its session was reset, from a connection that session used, or counted in
// it and never read by it.
const (
	lateLoss = "it was counted after that session ended, on a connection that session used"
	leftLoss = "the session counted it and never read it (it ended before its last read, or made none)"
)

// logEarlierSessionsLoss logs, at warning level, a loss l that session, an
// earlier session than the one open, made (in test, where keploy knows it),
// which charges no run or test of a later session, and why it reached none of
// that session's verdicts (lateLoss, leftLoss).
func (a *Agent) logEarlierSessionsLoss(session uint64, test string, l models.CaptureLoss, why string) {
	msg := "keploy could not capture a call on a connection of an earlier session's; it charges no run or test of a later session"
	next := "the earlier session's tests may have made calls keploy neither recorded nor mocked: run them again, and check the agent's log for why the connection ended"
	if l.Kind.NotJudged() {
		msg = "keploy could not tell whether a connection of an earlier session's carried a call it did not capture; it charges no run or test of a later session"
		next = models.AppBytesUntoldRemedy
	}
	f := []zap.Field{zap.Uint64("session", session), zap.String("kind", string(l.Kind)),
		zap.Uint64("count", l.Count), zap.String("what", l.LogWhat()), zap.String("why", why), zap.String("next_step", next)}
	if test != "" {
		f = append(f, zap.String("test", test))
	}
	a.logger.Warn(msg, f...)
}

// leftLossesLocked is what the session a reset ends counted that no read of
// it took (resetSession): taken, the proxy's last, each naming the test it
// was counted in where keploy knows it (a cut-short end's own, or the test
// of the worker its close was stamped with that was open at that epoch),
// then those kept for its tests still open (ownedOpen), by test. With
// provenMu held, before the reset clears the session's state.
func (a *Agent) leftLossesLocked(taken []coreAgent.OwnedLoss) []coreAgent.OwnedLoss {
	left := make([]coreAgent.OwnedLoss, 0, len(taken))
	for _, o := range taken {
		owner := o.Test
		if t := a.proven.open[o.Worker]; o.Open && t.epoch == o.Epoch {
			owner = t.id
		}
		left = append(left, coreAgent.OwnedLoss{Test: owner, Loss: o.Loss})
	}
	for _, id := range slices.Sorted(maps.Keys(a.proven.ownedOpen)) {
		for _, c := range a.proven.ownedOpen[id] {
			left = append(left, coreAgent.OwnedLoss{Test: id, Loss: c})
		}
	}
	return left
}

// TakeOwnedCaptureLosses takes, once each, the losses a test whose end was
// cut short owned, by test: keploy test's read, at each test case's end and
// at the set's end. keploy test reaches no end that charges them
// (chargeCaptureLosses), and they are in none of the counts it reads, so
// this is its only report of them. A test's own is "between test cases" for
// a pool call's (endingTestCase), "" for the run's.
func (a *Agent) TakeOwnedCaptureLosses(context.Context) ([]models.OwnedCaptureLoss, error) {
	var out []models.OwnedCaptureLoss
	for _, o := range a.takeOwnedLosses() {
		out = append(out, models.OwnedCaptureLoss{Test: o.Test, Loss: o.Loss})
	}
	return out, nil
}

// sinceRun is what was lost since the run began, read as now: what keploy
// cannot capture at all is counted from the hooks' load, not the session's
// start -- the application may open its Unix-socket connection before its
// first test claims the session, and every test after that rides on it
// (decision 20) -- and every other kind from the run's mark, a process that
// outlives the application (a refused exclusion) among them: one an earlier
// session of this agent counted is not this run's.
func sinceRun(now models.CaptureLosses, mark captureLossMark) models.CaptureLosses {
	return now.SinceWithin(runBases(mark))
}

// runBases are the bases of a session's losses (models.LossBases): its mark
// for each kind, but the hooks' load for what keploy cannot capture at all.
func runBases(mark captureLossMark) models.LossBases {
	return models.LossBases{Set: mark.at, App: mark.at, Run: mark.at}
}

// captureLossesSince is what a test is charged as it ends (models.Charged):
// what was lost since the run began (sinceRun), which taints the test open
// when it happened and every test after it (design v14 §3.2), so every test
// that ends after such a loss carries it; and what of a test's own kind
// (PerTest) was lost since the previous test ended (test; the run's mark for
// the first), which is that test's only. It returns the counts read now, the
// next test's mark (a read that failed leaves the next test to count its own
// kinds from the test before, so it is charged this one's too). A mark or a
// read that failed is an error: a test keploy cannot tell was fully captured
// is not judged as if it were. Hooks that cannot count at all (other
// platforms' hooks, which release 1 does not prove) leave every test as it
// was, as the PID-map check does for a session
// (models.ErrPIDMapRefusalsUnsupported). A kernel's count of skipped runs
// is left out (models.CaptureLosses.WithoutEvidence): it is evidence only,
// no verdict of a test's or of the run's; keploy record's own watch says
// it.
func (a *Agent) captureLossesSince(ctx context.Context, mark captureLossMark, test models.CaptureLosses) (lost, now models.CaptureLosses, err error) {
	if errors.Is(mark.err, models.ErrCaptureLossesUnsupported) {
		return nil, nil, nil
	}
	if !mark.known {
		if mark.err == nil {
			return nil, nil, errors.New("keploy did not read its capture-loss counts when the run began")
		}
		return nil, nil, fmt.Errorf("at the start of the run: %w", mark.err)
	}
	now, err = a.captureCounts(ctx)
	if err != nil {
		// Hooks that counted at the run's start and no longer can: the count
		// was lost, which is not "never counted".
		return nil, nil, err
	}
	now = now.WithoutEvidence()
	if test == nil {
		test = mark.at
	}
	return now.Charged(runBases(mark), test), now, nil
}
