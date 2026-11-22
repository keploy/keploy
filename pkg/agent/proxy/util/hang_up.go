package util

import (
	"net"
	"sync"
)

// HangUpTeller is a connection the proxy keeps that a test's end waits on
// (its accepted ones), whose client's hang-up a watch tells on to the app the
// call is for (WatchPeerEnd's callers). HoldHangUp has it hold work, from the
// watch's start until release, whenever its client shows gone and the
// hang-up is not yet told: a watch that has not run since the client hung up
// (on a loaded host, as long as the scheduler makes it) is waited for, so the
// app is told in the scope its client went in. HangUpTold says the hang-up
// was told: what the connection holds from then on, its client's end and an
// answer no one is there to take, is no test's work.
type HangUpTeller interface {
	HoldHangUp() (release func())
	HangUpTold()
}

// HoldHangUp holds conn's client's hang-up (HangUpTeller) on the first
// connection of conn's chain (conn, then each NetConn, at most 8 deep) that
// tells one. release ends the hold, once; told says first that the hang-up
// was told (HangUpTold). A conn with none in its chain gets a release that
// does nothing.
func HoldHangUp(conn net.Conn) (release func(told bool)) {
	h, ok := hangUpTellerOf(conn)
	if !ok {
		return func(bool) {}
	}
	held := h.HoldHangUp()
	var once sync.Once
	return func(told bool) {
		once.Do(func() {
			if told {
				h.HangUpTold()
			}
			held()
		})
	}
}

// hangUpTellerOf is the first HangUpTeller of conn's chain (HoldHangUp).
func hangUpTellerOf(conn net.Conn) (HangUpTeller, bool) {
	for i := 0; conn != nil; i++ {
		if h, ok := conn.(HangUpTeller); ok {
			return h, true
		}
		u, ok := conn.(interface{ NetConn() net.Conn })
		if !ok || i == 8 {
			return nil, false
		}
		conn = u.NetConn()
	}
	return nil, false
}
