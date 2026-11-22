package manager

import (
	"sync"
	"time"
)

// HoldExchanges notes that a parser holds exchanges it made, requested from
// at on, instead of handing them on, until it can tell what they are (the
// Generic recorder's first exchange of a connection that may lie wholly
// inside one test, or an exchange whose answer may go on). stopping is closed
// once the stop of the recording it is made in begins (it asks what is
// pending, BacklogBefore): the parser decides them then, with what it has,
// and hands them on. Until it calls handedOn, after handing them on, they are
// pending for the stop: its streams do not end without them. Nil-safe.
func (m *SyncMockManager) HoldExchanges(at time.Time) (stopping <-chan struct{}, handedOn func()) {
	if m == nil {
		return nil, func() {}
	}
	m.holdsMu.Lock()
	if m.holdsStopping == nil {
		m.holdsStopping = make(chan struct{})
	}
	if m.exchangeHolds == nil {
		m.exchangeHolds = make(map[uint64]int64)
	}
	m.exchangeHoldsNext++
	id := m.exchangeHoldsNext
	m.exchangeHolds[id] = at.UnixNano()
	ch := m.holdsStopping
	m.holdsMu.Unlock()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			m.holdsMu.Lock()
			delete(m.exchangeHolds, id)
			m.holdsMu.Unlock()
		})
	}
}

// stopHolds tells every parser holding exchanges (HoldExchanges) that the
// recording's stop has begun, once.
func (m *SyncMockManager) stopHolds() {
	m.holdsMu.Lock()
	defer m.holdsMu.Unlock()
	if m.holdsStopped {
		return
	}
	m.holdsStopped = true
	if m.holdsStopping == nil {
		m.holdsStopping = make(chan struct{})
	}
	close(m.holdsStopping)
}

// newHoldsSession starts the hold signal of a new recording (SetOutputChannel:
// a re-record): its holds are told of its own stop. Holds made before keep
// the channel they took, closed by the last stop or by none.
func (m *SyncMockManager) newHoldsSession() {
	m.holdsMu.Lock()
	defer m.holdsMu.Unlock()
	if m.holdsStopped {
		m.holdsStopped = false
		m.holdsStopping = make(chan struct{})
	}
}

// HoldsSession is the signal the holds of the current recording are told its
// stop by (HoldExchanges' stopping), nil before the first hold or stop
// (stopHolds makes it too): a new one once a re-record follows a stop
// (newHoldsSession). A parser that decided a hold as a stop began compares it
// with the signal that told it, to know whether it is still in that
// recording. Nil-safe.
func (m *SyncMockManager) HoldsSession() <-chan struct{} {
	if m == nil {
		return nil
	}
	m.holdsMu.Lock()
	defer m.holdsMu.Unlock()
	return m.holdsStopping
}

// holdsPendingBefore says why exchanges requested at or before `before` are
// still to be handed on, "" when none are: a parser holds them
// (HoldExchanges).
func (m *SyncMockManager) holdsPendingBefore(before time.Time) string {
	m.holdsMu.Lock()
	defer m.holdsMu.Unlock()
	n := before.UnixNano()
	for _, at := range m.exchangeHolds {
		if at <= n {
			return "a parser holds exchanges from before it until it has decided them (whether a connection's first exchange is one test's, or whether an answer has ended)"
		}
	}
	return ""
}
