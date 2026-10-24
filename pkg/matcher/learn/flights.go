package learn

// Flight is one request of the application's on one of its connections to
// keploy: aloft from the first bytes of it keploy read until keploy starts
// to write the answer back, or the connection closes (Land). A serve F made
// while the request was the only one aloft is that request's (ServedMockAt).
//
// Flights tell which serves the application can have seen in the order F
// made them. An application that sends a request only once it read the
// answer to the one before lifts the second after the first landed: F's
// order is the order it saw. Two requests aloft at one instant are in
// flight on parallel connections (a consumer's long poll, two start-up
// reads at once): whichever answer the application holds first is not told
// by the order F claimed their recordings in, so a serve in either is in
// an order keploy cannot tell (Order.Of: 0, 0), which the offset rule
// fails closed on. That holds whichever of the two the scheduler let F
// claim first, and whether F claimed the second before the first's answer
// went out or after it: an overlap is read from the requests, never from
// the instants, so it is no wall-clock budget.
//
// What flights cannot tell: two requests on one connection at once (HTTP/2
// streams, a pipeline) are one flight, ordered as F claimed them; and an
// application that sent its second request before it read the first's
// answer, which reached keploy only after that answer went out, is read as
// one that waited for it.
type Flight struct {
	t *Tracker
	// crossed: another request was aloft at an instant this one was.
	crossed bool
	// landed is the tracker's count of landings once this one landed (0
	// while aloft): a read of the order (Placed) reads it only if it landed
	// before the read, so later requests change no read's verdict.
	landed uint64
}

// Lift is a request that started to arrive on a connection: its flight,
// aloft until Land. Every request aloft at this instant, and this one, is
// crossed. The first Lift makes the tracker read F's order by flights from
// then on (Order.Of): a serve made in no request, or in one of several
// aloft, is in an order keploy cannot tell.
func (t *Tracker) Lift() *Flight {
	if t == nil {
		return nil
	}
	f := &Flight{t: t}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.flying = true
	if t.aloft == nil {
		t.aloft = map[*Flight]struct{}{}
	}
	for g := range t.aloft {
		g.crossed, f.crossed = true, true
	}
	t.aloft[f] = struct{}{}
	return f
}

// Land is the end of f: keploy starts to write its answer back, or its
// connection closed. Landed once; a nil or landed flight is nothing.
func (f *Flight) Land() {
	if f == nil {
		return
	}
	t := f.t
	t.mu.Lock()
	defer t.mu.Unlock()
	if f.landed != 0 {
		return
	}
	delete(t.aloft, f)
	t.landings++
	f.landed = t.landings
}

// flightOfLocked is the request the serve numbered now is made in: the one
// request aloft, nil with none or several aloft or for a protocol phase
// (a greeting the server sends first is no request's). t.mu is held.
func (t *Tracker) flightOfLocked(protocol bool) *Flight {
	if protocol || len(t.aloft) != 1 {
		return nil
	}
	for f := range t.aloft {
		return f
	}
	return nil
}

// told reports whether the serve numbered s is in an order keploy can tell
// for the read o: always without flights (no connection was ever lifted:
// F's claims are the only order there is); with them, only a serve made in
// one request that landed before the read and crossed no other. o.t.mu is
// held.
func (o Order) told(s int) bool {
	if !o.flying {
		return true
	}
	f := o.flights[s]
	return f != nil && f.landed != 0 && f.landed <= o.landings && !f.crossed
}
