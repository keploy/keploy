package manager

import (
	"sync"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// ServedByCopy says how a kind's replay serves one of its mocks: ok when the
// replay answers every call mock answers from any mock of its kind with the
// same key, whichever connection or test recorded that one, so a recording
// that holds one such copy needs no other; key "" when the replay answers the
// call with no mock at all. A mock it does not answer ok for, or a mock of a
// kind that registers none, is needed in itself (fail closed): the replay may
// need that very mock. The key holds everything the replay compares or serves
// from the mock, and nothing a connection varies (its id, its scramble, its
// auth response): a key stricter than the replay only makes a lost mock count
// as needed.
type ServedByCopy func(mock *models.Mock) (key string, ok bool)

var (
	byCopyMu sync.RWMutex
	byCopy   = map[models.Kind]ServedByCopy{}
)

// RegisterServedByCopy registers how kind's replay serves its mocks by copy
// (ServedByCopy), as a parser does from its init, beside
// integrations.Register. A nil f unregisters kind's.
func RegisterServedByCopy(kind models.Kind, f ServedByCopy) {
	byCopyMu.Lock()
	defer byCopyMu.Unlock()
	if f == nil {
		delete(byCopy, kind)
		return
	}
	byCopy[kind] = f
}

// ServedByCopyOf is the ServedByCopy registered for kind's mocks; ok is
// false when none is.
func ServedByCopyOf(kind models.Kind) (f ServedByCopy, ok bool) {
	byCopyMu.RLock()
	defer byCopyMu.RUnlock()
	f, ok = byCopy[kind]
	return f, ok
}

// servedByCopy is the registered ServedByCopy's answer for mock: ok false
// for a kind that registered none.
func servedByCopy(mock *models.Mock) (key string, ok bool) {
	f, ok := ServedByCopyOf(mock.Kind)
	if !ok {
		return "", false
	}
	return f(mock)
}

// heldCopiesMax bounds the keys heldCopies keeps for one recording: a mock
// whose key is not kept counts as needed, so the bound costs leave-outs,
// never a test kept without a mock its replay needs.
const heldCopiesMax = 4096

type copyKey struct {
	kind models.Kind
	key  string
}

// heldCopies is, for the recording of the output channel's epoch, the
// earliest request time of a mock of each copy key (ServedByCopy) delivered
// to its consumer: a lost mock of that key requested no earlier is served at
// replay from that copy. Guarded by mu, a leaf lock.
type heldCopies struct {
	mu    sync.Mutex
	epoch uint64
	first map[copyKey]time.Time
}

// noteDelivered notes mock, delivered to the consumer of the channel of
// epoch, as a copy the recording holds of its key. An answer the client
// never read serves no call, and a mock with no request time cannot be shown
// requested before a loss: neither is noted. A delivery for an older
// recording's channel notes nothing for this one.
func (m *SyncMockManager) noteDelivered(mock *models.Mock, epoch uint64) {
	if mock.IsUndelivered() || mock.Spec.ReqTimestampMock.IsZero() {
		return
	}
	key, ok := servedByCopy(mock)
	if !ok || key == "" {
		return
	}
	k, req := copyKey{kind: mock.Kind, key: key}, mock.Spec.ReqTimestampMock
	h := &m.copies
	h.mu.Lock()
	defer h.mu.Unlock()
	switch {
	case epoch < h.epoch:
		return
	case epoch > h.epoch || h.first == nil:
		h.epoch, h.first = epoch, map[copyKey]time.Time{}
	}
	if t, have := h.first[k]; have {
		if req.Before(t) {
			h.first[k] = req
		}
		return
	}
	if len(h.first) < heldCopiesMax {
		h.first[k] = req
	}
}

// neededByNone reports whether no test of the recording of epoch needs mock
// in itself: its kind's replay answers its call with no mock, or from any
// copy of its key (ServedByCopy), and the recording's consumer took one
// requested no later than it (noteDelivered), which any replay that would
// serve mock holds. Its loss then leaves no test case out. False for a mock
// with no request time, as no copy noted has none, and for a recording's
// epoch other than the one whose
// copies are held: a recording whose consumer took none holds the last one's.
// A drop of an older recording's lane leaves none of this one's out anyway
// (leaveOutAfter), and queueBound asks for the current channel's.
func (m *SyncMockManager) neededByNone(mock *models.Mock, epoch uint64) bool {
	key, ok := servedByCopy(mock)
	if !ok {
		return false
	}
	if key == "" {
		return true
	}
	req := mock.Spec.ReqTimestampMock
	h := &m.copies
	h.mu.Lock()
	defer h.mu.Unlock()
	if epoch != h.epoch {
		return false
	}
	t, have := h.first[copyKey{kind: mock.Kind, key: key}]
	return have && !t.After(req)
}
