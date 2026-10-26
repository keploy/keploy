package learn

// Flight is one request of the application's on one of its connections to
// keploy: aloft from the first bytes of it keploy read until keploy starts
// to write the answer back (Land), or until keploy knows the application is
// not given the answer: the connection closed or half-closed before the
// answer started, or a write of it failed (EndUntold). A request
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
// every request aloft then counts as a serve (fail closed). A serve the
// parser says was made in no request (NoRequest) makes none a serve. A serve
// a request caused (LiftCaused: the push its PUBLISH let go) is ordered
// after that request's serves, as F claimed them, as one request's are,
// although the application may read it on another connection before the
// cause's own answer.
//
// A handshake, a protocol phase, has a place in F's order only where its
// parser says which request it answers, and chose the connection's recorded
// handshake in that same request (Tracker.ServedHandshakeIn: a Postgres
// start-up answers the StartupMessage its client opened the connection with,
// and is picked as that request arrives): told once that request landed, as
// a serve made in it is, and in an order keploy cannot tell where its request
// was aloft beside one that was a serve or got a handshake, as the
// application may hold either answer first, and two connections opened at
// once may get each other's start-ups. One chosen in an earlier request (an
// SSLRequest's, before the StartupMessage it answers) has no place: two
// connections whose SSLRequests were aloft together may have swapped
// start-ups although their StartupMessages came one after the other. A
// handshake makes no other request's serve untold: a pool's handshake on its
// own connection beside a statement leaves the statement its place, and the
// handshake none.
type Flight struct {
	t *Tracker
	// overlaps are the requests aloft at an instant this one was: those
	// aloft when it lifted, and those lifted while it was aloft.
	overlaps []*Flight
	// set and lift are where it lifted: the set's epoch (Tracker.setEpoch)
	// and its number in that set's lifts (1 the first; Tracker.lifts).
	set  uint64
	lift int
	// serving is the number of the serve that made this request a serve (0
	// while it is none): the first data-plane serve made in it, or one made
	// while it was aloft that no parser said whose request it was.
	serving int
	// answering is the number of the first handshake served in this
	// request (0 with none; Tracker.ServedHandshakeIn), placed or not: the
	// answer a server gives the request a client opens its connection with.
	// It makes no other request's serve one keploy cannot tell, but another
	// handshake aloft beside it is (two connections opened at once).
	// servingSet and answeringSet are the set epochs (Tracker.setEpoch)
	// serving and answering were numbered in: a request aloft across a
	// NewSet is numbered in the set it was served in, and a read compares
	// only its own set's numbers (Order.byRead).
	answering                int
	servingSet, answeringSet uint64
	// landed is the tracker's count of landings once this one landed (0
	// while aloft): a read of the order (Placed) reads it only if it landed
	// before the read, so later requests change no read's verdict.
	landed uint64
	// untold is the tracker's count of landings once this one ended untold
	// (EndUntold; 0 while it has not): a read made since reads none of its
	// serves, and a read made before keeps the verdict it gave.
	untold uint64
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
	for g := range t.aloft {
		g.overlaps = append(g.overlaps, f)
		f.overlaps = append(f.overlaps, g)
	}
	t.aloftLocked(f)
	return f
}

// aloftLocked puts f aloft, the set's latest lift: F's order is read by
// flights from now on. t.mu is held.
func (t *Tracker) aloftLocked(f *Flight) {
	t.flying = true
	if t.aloft == nil {
		t.aloft = map[*Flight]struct{}{}
	}
	t.aloft[f] = struct{}{}
	t.lifts = append(t.lifts, f)
	f.set, f.lift = t.setEpoch, len(t.lifts)
}

// noRequest is NoRequest's: no tracker's, never aloft.
var noRequest = &Flight{}

// NoRequest is the request a serve made in no request of the application's
// is told in (Tracker.ServedMockIn, integrations.ServeTeller.ServingFor): a
// server push nothing the application sent caused (another client's
// PUBLISH, a keyspace notification). The serve makes no request aloft a
// serve, so a heartbeat or a test's own command aloft beside it keeps its
// place in F's order; and, once a connection lifted (Lift), it is itself in
// an order keploy cannot tell (Order.Of: 0, 0, which the offset rule fails
// closed on): the application holds it whenever it reads it, which no
// request of its tells. It never lifts or lands: Land on it is nothing, and
// LiftCaused of it is it.
func NoRequest() *Flight { return noRequest }

// LiftCaused is the request of a serve f caused, made once f's own serve
// was (a server push f's PUBLISH let go, consumed on the subscriber's
// connection, whether f landed by then or not): aloft from f's lift until
// its own Land, which its caller calls as it starts to write the serve's
// answer back. It overlaps every request aloft at an instant of that span
// but f: those aloft when f lifted, those lifted since, landed or not (the
// set's lifts; with f lifted in an earlier set, every lift of this one), and
// those that lift before it lands. A serve told in it (Tracker.ServedMockIn)
// is in F's order once it landed, if none of those was a serve by the read;
// and after f's serves, as F claimed them, as a serve made in f's own
// request is. f itself is left out of those it overlaps: f is always a
// serve, so with it every serve f caused would be in no order keploy can
// tell (Order.Of: 0, 0), which is what keploy read of a push before it could
// be told in its cause. A nil f, or NoRequest, is a serve in no request:
// NoRequest.
func (f *Flight) LiftCaused() *Flight {
	if f == nil || f.t == nil {
		return noRequest
	}
	t := f.t
	p := &Flight{t: t}
	t.mu.Lock()
	defer t.mu.Unlock()
	crossed := map[*Flight]struct{}{f: {}}
	cross := func(g *Flight) {
		if _, ok := crossed[g]; ok {
			return
		}
		crossed[g] = struct{}{}
		g.overlaps = append(g.overlaps, p)
		p.overlaps = append(p.overlaps, g)
	}
	for _, g := range f.overlaps {
		cross(g)
	}
	since := 0
	if f.set == t.setEpoch {
		since = f.lift
	}
	for _, g := range t.lifts[since:] {
		cross(g)
	}
	for g := range t.aloft {
		cross(g)
	}
	t.aloftLocked(p)
	return p
}

// Land is the end of f: keploy starts to write its answer back. Landed
// once; a nil or landed flight is nothing. One that ended untold
// (EndUntold) stays untold.
func (f *Flight) Land() {
	if f == nil || f.t == nil {
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

// EndUntold is the end of f whose answer the application is not given: no
// answer is written in f, or the write that started it failed. The proxy's
// kept connection ends f untold when it closes or half-closes with f aloft,
// and when the write that landed f fails; a later write that fails ends
// nothing, as the connection cannot tell the rest of an answer from a write
// after it, so a request whose answer is cut after its first write stays
// landed. A layer over the connection that writes as it closes (TLS's
// close_notify) lands f first, as an answer's first bytes would: the proxy
// ends its request aloft untold before it closes the layer it serves over. A
// parser that closes such a layer itself (the TLS it was handed, or a
// STARTTLS upgrade of its own) lands its request aloft so, unless it ends it
// untold first: one that will not answer it must. So must one that takes a
// server push it will not deliver as that consume's, or that lands a flight
// itself (LiftCaused) and then fails to write its push. Every serve made in f
// is then in an order keploy cannot tell (Order.Of: 0, 0) in each read made
// from now on, so it is never one the application read before a request of
// its: the application was not given it. Aloft, f is no longer in flight: a
// request lifted after it (Lift) overlaps it no more, while those that
// overlapped it keep the overlap (with f a serve, their serves stay untold,
// which fails closed), and a request a serve caused (LiftCaused) still
// crosses it as one of the set's lifts, which fails closed too. Landed, f
// stays out of flight, and a read made before this keeps the verdict it gave
// (Placed reads every flight as the read saw it), as a read made before a
// Land does. Once: a nil flight, NoRequest, or one that ended untold already
// is nothing, and a Land after it changes no read: its serves stay untold.
func (f *Flight) EndUntold() {
	if f == nil || f.t == nil {
		return
	}
	t := f.t
	t.mu.Lock()
	defer t.mu.Unlock()
	if f.untold != 0 {
		return
	}
	delete(t.aloft, f)
	t.landings++
	f.untold = t.landings
}

// flightOfLocked is the request the serve numbered now (t.seq) is made in,
// which it makes a serve (Flight.serving): in, the request its parser said
// it serves, while that one is aloft; with none said, the one request
// aloft. nil for a protocol phase (a greeting the server sends first is no
// request's, and makes none a serve), for a serve its parser said was made
// in no request (NoRequest), which makes none a serve either, for a serve in
// no request, in one already landed, or with several aloft and none said,
// when every one aloft becomes a serve. A handshake (k.handshake: its parser
// said it answers in, ServedHandshakeIn) makes in, a request of t's, one
// that got a handshake (Flight.answering), never a serve, and is in's while
// in is aloft and the handshake was chosen in it (k.placed); nil otherwise,
// and no request aloft becomes anything. t.mu is held. A serve made in one
// that ended untold is untold whatever this returns (Order.told).
func (t *Tracker) flightOfLocked(protocol bool, in *Flight, k serveKind) *Flight {
	if k.handshake {
		if in == nil || in.t != t {
			return nil
		}
		if in.answering == 0 {
			in.answering, in.answeringSet = t.seq, t.setEpoch
		}
		if !k.placed || in.landed != 0 {
			return nil
		}
		return in
	}
	if protocol || in == noRequest {
		return nil
	}
	serves := func(f *Flight) {
		if f.serving == 0 {
			f.serving, f.servingSet = t.seq, t.setEpoch
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
// one request that landed before the read and had not ended untold by then
// (EndUntold), and overlapped no request that was a serve by then; a
// handshake (Flight.answering), no request either that got one by then.
// o.t.mu is held.
func (o Order) told(s int) bool {
	if !o.flying {
		// A serve made in no request (NoRequest) is told here too. The
		// agent never reaches that: a push is consumed on a connection the
		// application subscribed on, and that SUBSCRIBE lifted it.
		return true
	}
	f := o.flights[s]
	if f == nil || f.landed == 0 || f.landed > o.landings {
		return false
	}
	if f.untold != 0 && f.untold <= o.landings {
		return false
	}
	handshake := o.handshakes[s]
	for _, g := range f.overlaps {
		if o.byRead(g.serving, g.servingSet) {
			return false
		}
		if handshake && o.byRead(g.answering, g.answeringSet) {
			return false
		}
	}
	return true
}

// byRead reports whether the serve numbered n in the set set was made by the
// read o: in an earlier set, or in o's at or before its last serve. A number
// of a later set, or 0 (none), was not. o.t.mu is held.
func (o Order) byRead(n int, set uint64) bool {
	return n != 0 && (set < o.set || set == o.set && n <= o.upTo)
}

// serveKind is how a serve's parser told it (Tracker.servedIn): handshake,
// a handshake that answers the request it was told in (ServedHandshakeIn);
// placed, chosen in that same request, so it can have that request's place.
type serveKind struct{ handshake, placed bool }
