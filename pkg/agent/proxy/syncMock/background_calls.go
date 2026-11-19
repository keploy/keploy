package manager

import (
	"crypto/sha256"
	"sync"
	"sync/atomic"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// Background calls: the calls an application makes on its own, not for one
// test (a timer's write, a worker's poll, a token refresh), which the user
// names with worker-mock rules (models.WorkerMockRule, config
// record.workerMocks). The recorder adds what a rule matches to the set's
// worker scope, which a replay serves to every test; it hands the rules to
// the agent (models.OutgoingOptions.WorkerMockRules), and this manager keeps
// those calls from its cleanups:
//
//   - AddMock tags each mock a rule of its recording names
//     (TestModeInfo.Background, markBackground).
//   - A test whose window holds such a mock owns it, as any other: a kept
//     resolve, the retroactive bin and the hold hand it to that test.
//   - Where a cleanup would drop it (inside a static-dedup duplicate's
//     window, or before its prune horizon; past the stale cutoff with no
//     window to own it; held for a request in flight that ended without it),
//     it is kept instead (keepBackgroundLocked) and sent with no owner
//     (handBackgroundLocked). A flush sends one no request resolves over once
//     no request may claim it any more (backgroundDueLocked), so a call made
//     between requests reaches the recording while the application is idle.
//   - Of those it sends only the first copy of each call
//     (models.WorkerMockCallOf, backgroundCalls.repeat): a timer's call made
//     every second would otherwise grow the recording by a mock a second, and
//     the worker scope serves one copy to every test. A copy whose send is
//     dropped is forgotten, so the call's next copy is sent. A call keploy
//     cannot name is sent at every copy.
//
// Without rules nothing changes: no mock is tagged. Under the proven switch
// the recorder applies none (its window mappings place the worker scope), and
// neither does this manager.

// maxBackgroundCalls bounds the calls a recording remembers it sent
// (backgroundCalls.sent), as the recorder bounds the calls it marks: past
// it, a call's copies are all sent.
const maxBackgroundCalls = 10000

// backgroundCalls is a manager's worker-mock rules, per recording, and the
// calls it sent with no owner. AddMock reads the rules on every mock, before
// it takes the manager's lock, so they are a snapshot read with no lock
// (rules); the calls sent have a lock of their own, taken as a send checks a
// call after the manager's is let go.
type backgroundCalls struct {
	// any is set while some recording has a rule: AddMock reads no rule
	// otherwise, and a settled prefix is not scanned for background calls
	// (keepSettledLocked).
	any atomic.Bool
	// rules is each recording's rules, by its destination key (destKey) and
	// then by the kind they name: most mocks are of a kind no rule names,
	// and are matched against none. Replaced whole, never written in place.
	rules atomic.Pointer[map[string]map[string][]models.WorkerMockRule]
	// mu guards sent and full, and orders the writers of rules.
	mu sync.Mutex
	// sent is, per recording, the calls sent with no owner, as digests (a
	// call's key can hold a whole Redis value, or an AUTH password).
	sent map[string]map[[sha256.Size]byte]struct{}
	// full is the recordings that reached maxBackgroundCalls, said once.
	full map[string]bool
	// kept counts the background calls sent; repeats the copies let go.
	kept, repeats atomic.Uint64
}

// backgroundCall is a call sent with no owner: the recording and the digest
// of the call. The zero value is none.
type backgroundCall struct {
	dest string
	sum  [sha256.Size]byte
	ok   bool
}

// SetBackgroundRules sets the worker-mock rules of dest's recording ("" for
// a manager whose mocks go to one recording, SetDestinationOf), and forgets
// the calls sent for it: called as its recording starts, before its first
// mock. Nil rules clear them. Ignored under the proven switch, which applies
// no worker-mock rule at record.
func (m *SyncMockManager) SetBackgroundRules(dest string, rules []models.WorkerMockRule) {
	if m == nil {
		return
	}
	key := m.destKey(dest)
	b := &m.bg
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.sent, key)
	delete(b.full, key)
	next := map[string]map[string][]models.WorkerMockRule{}
	if cur := b.rules.Load(); cur != nil {
		for k, v := range *cur {
			if k != key {
				next[k] = v
			}
		}
	}
	if len(rules) > 0 && !provenswitch.On() {
		byKind := map[string][]models.WorkerMockRule{}
		for _, r := range rules {
			byKind[r.Kind] = append(byKind[r.Kind], r)
		}
		next[key] = byKind
	}
	b.rules.Store(&next)
	b.any.Store(len(next) > 0)
}

// markBackground tags mock (TestModeInfo.Background) when a worker-mock rule
// of the recording it goes to (dest, known false when the manager cannot tell
// it) names its call. Called by AddMock before it takes mu.
func (m *SyncMockManager) markBackground(mock *models.Mock, dest string, known bool) {
	b := &m.bg
	if !b.any.Load() || mock == nil || !known || provenswitch.On() {
		return
	}
	all := b.rules.Load()
	if all == nil {
		return
	}
	rules := (*all)[m.destKey(dest)][string(mock.Kind)]
	if len(rules) == 0 {
		return
	}
	if _, ok := models.MatchWorkerMockRules(rules, mock); ok {
		mock.TestModeInfo.Background = true
	}
}

// keepBackgroundLocked keeps mk, a mock a cleanup was about to drop, when a
// worker-mock rule names its call: to be sent with no owner
// (handBackgroundLocked). Counted in t (if not nil). Reports whether it kept
// it. Caller holds mu.
func (m *SyncMockManager) keepBackgroundLocked(mk *models.Mock, t *dropTally) bool {
	if mk == nil || !mk.TestModeInfo.Background {
		return false
	}
	m.background = append(m.background, mk)
	if t != nil {
		t.background++
	}
	return true
}

// handBackgroundLocked moves the background calls kept from a cleanup into
// send, with no owner, no name and no mapping: no test owns one. The send
// lets go of a copy of a call sent already (sendTaken). Caller holds mu, with
// the output wired.
func (m *SyncMockManager) handBackgroundLocked(send *[]ownedMock) {
	for _, mk := range m.background {
		*send = append(*send, ownedMock{mock: mk, background: true})
	}
	clear(m.background)
	m.background = m.background[:0]
}

// backgroundDueLocked reports whether a flush sends mock, a call a worker-mock
// rule names that no resolved window owns, with no owner now: no request in
// flight may claim it (it was requested before the earliest one open
// started), and no request opened late may either (it is past the stale
// cutoff, as the cutoff would find it), or the recording is stopping. A mock
// with no request time no window can own. Caller holds mu.
func (m *SyncMockManager) backgroundDueLocked(mock *models.Mock) bool {
	at := mock.Spec.ReqTimestampMock
	if at.IsZero() {
		return true
	}
	if from, open := m.openFromLocked(); open && !at.Before(from) {
		return false
	}
	return !m.stopAt.IsZero() || at.Before(time.Now().Add(-StaleHorizon))
}

// backgroundRepeat reports whether mock, a background call about to be sent
// with no owner, is a copy of a call its recording sent already; if not, it
// returns the call, now remembered as sent, for a dropped send to forget
// (backgroundCalls.forget). A call keploy cannot name
// (models.WorkerMockCallOf), and any once the recording remembers
// maxBackgroundCalls, is never a repeat. Called without mu: it may call the
// destination's namer.
func (m *SyncMockManager) backgroundRepeat(mock *models.Mock) (backgroundCall, bool) {
	key, named := models.WorkerMockCallOf(mock)
	if !named {
		m.bg.kept.Add(1)
		return backgroundCall{}, false
	}
	dest, _ := m.mockDest(mock)
	call := backgroundCall{dest: m.destKey(dest), sum: sha256.Sum256([]byte(key)), ok: true}
	b := &m.bg
	b.mu.Lock()
	sent := b.sent[call.dest]
	if _, done := sent[call.sum]; done {
		b.mu.Unlock()
		b.repeats.Add(1)
		return backgroundCall{}, true
	}
	if len(sent) >= maxBackgroundCalls {
		said := b.full[call.dest]
		if b.full == nil {
			b.full = map[string]bool{}
		}
		b.full[call.dest] = true
		b.mu.Unlock()
		b.kept.Add(1)
		if !said {
			if logger := m.dropLogger(); logger != nil {
				logger.Warn("worker-mock rules matched more distinct background calls than a recording remembers; every copy of a later call is recorded",
					zap.Int("calls", maxBackgroundCalls),
					zap.String("next_step", "a worker-mock rule names a call the application makes on its own: narrow a rule that matches a call per request"))
			}
		}
		return backgroundCall{}, false
	}
	if sent == nil {
		sent = map[[sha256.Size]byte]struct{}{}
		if b.sent == nil {
			b.sent = map[string]map[[sha256.Size]byte]struct{}{}
		}
		b.sent[call.dest] = sent
	}
	sent[call.sum] = struct{}{}
	b.mu.Unlock()
	b.kept.Add(1)
	return call, false
}

// forget forgets call, sent and then dropped on its way out, so the call's
// next copy is sent. The zero call is none.
func (b *backgroundCalls) forget(call backgroundCall) {
	if !call.ok {
		return
	}
	b.mu.Lock()
	delete(b.sent[call.dest], call.sum)
	b.mu.Unlock()
	b.kept.Add(^uint64(0))
}

// BackgroundCalls is how many background calls the manager sent with no
// owner, and how many copies of a call it sent already it let go.
func (m *SyncMockManager) BackgroundCalls() (sent, repeats uint64) {
	if m == nil {
		return 0, 0
	}
	return m.bg.kept.Load(), m.bg.repeats.Load()
}
