package learn

// Flight is one request of the application's on one of its connections to
// keploy: aloft from the first bytes of it keploy read until keploy starts
// to write the answer back, or the connection closes (Land). A request
// counts as a serve once F served a recording in it (ServedMockIn: the
// parser says whose request it serves; ServedMockAt: the one request
// aloft). A request that reads no recording (a driver's monitor heartbeat,
// a long poll no recording answers, a health check, a protocol phase) is
// never one, and crosses no serve, whenever it happens to be aloft.
//
// Flights tell which serves the application can have seen in the order F
// made them. An application that sends a request only once it read the
// answer to the one before lifts the second after the first landed: F's
// order is the order it saw. Two serves whose requests were aloft at one
// instant are in flight on parallel connections (a consumer's long poll
// answered by a recording, two start-up reads at once): whichever answer
// the application holds first is not told by the order F claimed their
// recordings in, so both are in an order keploy cannot tell (Order.Of: 0,
// 0), which the offset rule fails closed on. That holds whichever of the
// two the scheduler let F claim first, and whether F claimed the second
// before the first's answer went out or after it: an overlap is read from
// the requests, never from the instants, so it is no wall-clock budget. A
// read of the order sees a crossing once the second serve was numbered
// (Flight.serving): before it, the application cannot have read the second
// answer, and the read keeps its verdict after.
//
// What flights cannot tell: two requests on one connection at once (HTTP/2
// streams, a pipeline) are one flight, ordered as F claimed them; an
// application that sent its second request before it read the first's
// answer, which reached keploy only after that answer went out, is read as
// one that waited for it; and a serve made by a parser that does not say
// whose request it serves (integrations.ServingIn) while several requests
// are aloft is no one request's: it is in an order keploy cannot tell, and
// every request aloft then counts as a serve (fail closed).
type Flight struct {
	t *Tracker
	// overlaps are the requests aloft at an instant this one was: those
	// aloft when it lifted, and those lifted while it was aloft.
	overlaps []*Flight
	// serving is the number of the serve that made this request a serve (0
	// while it is none): the first data-plane serve made in it, or one made
	// while it was aloft that no parser said whose request it was.
	serving int
	// landed is the tracker's count of landings once this one landed (0
	// while aloft): a read of the order (Placed) reads it only if it landed
	// before the read, so later requests change no read's verdict.
	landed uint64
}

// Lift is a request that started to arrive on a connection: its flight,
// aloft until Land. It overlaps every request aloft at this instant. The
// first Lift makes the tracker read F's order by flights from then on
// (Order.Of): a serve made in no request, or in one keploy cannot tell from
// another aloft, is in an order keploy cannot tell.
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
		g.overlaps = append(g.overlaps, f)
		f.overlaps = append(f.overlaps, g)
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

// flightOfLocked is the request the serve numbered now (t.seq) is made in,
// which it makes a serve (Flight.serving): in, the request its parser said
// it serves, while that one is aloft; with none said, the one request
// aloft. nil for a protocol phase (a greeting the server sends first is no
// request's, and makes none a serve), for a serve in no request, in one
// already landed, or with several aloft and none said, when every one aloft
// becomes a serve. t.mu is held.
func (t *Tracker) flightOfLocked(protocol bool, in *Flight) *Flight {
	if protocol {
		return nil
	}
	serves := func(f *Flight) {
		if f.serving == 0 {
			f.serving = t.seq
		}
	}
	if in != nil && in.t == t {
		serves(in)
		if in.landed != 0 {
			return nil
		}
		return in
	}
	for f := range t.aloft {
		serves(f)
	}
	if len(t.aloft) != 1 {
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
// one request that landed before the read, and overlapped no request that
// was a serve by then. o.t.mu is held.
func (o Order) told(s int) bool {
	if !o.flying {
		return true
	}
	f := o.flights[s]
	if f == nil || f.landed == 0 || f.landed > o.landings {
		return false
	}
	for _, g := range f.overlaps {
		if g.serving != 0 && g.serving <= o.upTo {
			return false
		}
	}
	return true
}
