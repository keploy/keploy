package manager

import (
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// settledPrefix is how many of the buffer's first mocks the last pass over it
// left settled: a per-test mock (not one any test may reuse,
// attributedReusable, nor a startup one, isStartupMock) whose request no
// window in the ring holds. A call a worker-mock rule names is settled too: a
// flush walks the prefix for the ones no request may claim any more only once
// the oldest waiting is (sendDueSettledBackgroundLocked), so a recording with
// a rule keeps the prefix a resolve steps over. Such a mock waits for a window to be resolved over
// its request, or for the stale cutoff: nothing else moves it.
//
//   - ResolveRange decides it in full only when its window holds the request,
//     or the request is past the cutoff. Any other mock, the walk of every
//     window in the ring found none over it, and kept it in the buffer,
//     unchanged (the retention at the end of the walk).
//   - FlushOwnedWindows keeps it, as its flushTakeLocked says (flushKeep).
//
// It stays settled while it stays in the buffer: the ring changes only as
// ResolveRange adds the window it resolves, after deciding in full every mock
// that window holds, and lets go of its oldest, which owns less. So a pass
// decides only the mocks added since the last one, those its window holds and
// those the cutoff reaches, and steps over the rest.
//
// The prefix is trusted only while the buffer still has the last of it where
// the pass left it, the ring was not rebuilt (windowIndex.gen: a row that
// writes the ring) and the proven switch, which says which mocks any test may
// reuse, reads as it did. The passes that take mocks out of the buffer keep
// it (keepSettledLocked); one that does not is caught by the first check,
// since the mock it finds there then is another. AddMock adds after it.
//
// at is the prefix's mocks' request times, in buffer order: a pass steps over
// a settled mock by reading its request time from here, one array, not from
// the mock. A buffered mock's request time does not change (the hold and the
// sent claims are sorted by it too). Guarded by mu.
type settledPrefix struct {
	n      int
	last   *models.Mock
	gen    uint64
	proven bool
	at     []time.Time
}

// settledLocked is how many of the buffer's first mocks are settled, as the
// last pass left them. Caller holds mu.
func (m *SyncMockManager) settledLocked() int {
	s := m.settled
	if s.n <= 0 || s.n > len(m.buffer) || s.n != len(s.at) || m.buffer[s.n-1] != s.last || s.gen != m.windowsLocked().gen || s.proven != provenswitch.On() {
		return 0
	}
	return s.n
}

// settledTimes is the request times of the settled prefix, as settledLocked
// trusts it, to be compacted by a pass in step with the buffer and handed
// back to keepSettledLocked. It has room for the prefix's growth. Caller
// holds mu.
func (m *SyncMockManager) settledTimes(trusted int) []time.Time {
	if trusted == 0 {
		return m.settled.at[:0]
	}
	return m.settled.at
}

// keepAt sets at[i] to t, at holding i entries or more.
func keepAt(at []time.Time, i int, t time.Time) []time.Time {
	if i < len(at) {
		at[i] = t
		return at
	}
	return append(at, t)
}

// keepSettledLocked notes that the buffer's first n mocks are settled, at
// holding their request times (n of them or more). Caller holds mu.
func (m *SyncMockManager) keepSettledLocked(n int, at []time.Time) {
	if n <= 0 || n > len(m.buffer) || n > len(at) {
		m.settled = settledPrefix{at: at[:0]}
		return
	}
	clear(at[n:])
	m.settled = settledPrefix{n: n, last: m.buffer[n-1], gen: m.windowsLocked().gen, proven: provenswitch.On(), at: at[:n]}
}
