package integrations

import "go.keploy.io/server/v3/pkg/models"

// OnceChains is an optional MockMemDb capability under the proven switch:
// where a recording stands in the once series keploy mock replay's scope
// table names (models.ScopeTableReq.Series and Once), in placement order. A
// declared rule's once series is its chain, the calls the application made
// just before the covered one on that call's connection, in the order it
// made them, and then the covered call, so the recording placed after one
// is the call the application made next on that connection. A parser that
// answers a connection's calls in turn follows a chain the connection began
// (OnceChainOnConn).
type OnceChains interface {
	OnceLink(mock string) (OnceLink, bool)
}

// OnceLink is where a recording of a once series stands in it.
type OnceLink struct {
	// Series is the series' id (a declared rule's "<rule>/-/<n>").
	Series string
	// First says it is the series' first-placed recording: its chain's
	// first call.
	First bool
	// Next is the recording placed after it; "" for the series' last.
	Next string
}

// OnceLinkOf is db's OnceLink of the recording named mock (OnceChains);
// false for a recording of no once series, and from a db that cannot tell.
func OnceLinkOf(db MockMemDb, mock string) (OnceLink, bool) {
	c, ok := db.(OnceChains)
	if !ok {
		return OnceLink{}, false
	}
	return c.OnceLink(mock)
}

// DeclaredBackground reports whether m, a recording of db, is one of the
// background's that no learn step decided for the open test, so a replay
// checks each field of it a clock rule of keploy.yml declares by that rule
// (learn.ReplayClocks' background):
//   - one of db's background recordings (InBackground): of a set the
//     background rules serve, or, in a test under any rules, one of the
//     worker scope the run reached that the view holds as no test's own;
//   - a recording of a once series (OnceLinkOf), wherever its call lands, in
//     a test or between tests, unless the open test owns it
//     (OwnedByOpenTest, which a store that cannot tell answers yes): a
//     declared background rule placed it in worker:, and it answers one
//     call in the run wherever the application makes it.
//
// Without the second, in a set the background rules do not serve (its
// mappings.yaml carries no recordedWith mark), a once series' recording
// answering a call between tests compared its declared clock fields as
// recorded, so a credential's last-use touch made after its test's window
// ended missed on the time it stamps. Any other worker: recording between
// tests of such a set still compares as recorded. false for a nil m.
func DeclaredBackground(db MockMemDb, m *models.Mock) bool {
	if m == nil {
		return false
	}
	if InBackground(db, m.Name) {
		return true
	}
	_, once := OnceLinkOf(db, m.Name)
	return once && !OwnedByOpenTest(db, m)
}

// OnceChainOnConn follows, on one connection, the once chain that
// connection began: once a once series' first recording answered a call on
// it, the connection's next call is the series' next recording's to answer
// (Take, OnceFollow.For), and so on through the series. Any other answer on
// the connection, a call answered by nothing, a call the parser did not
// match, and a view change of the connection's store (a test's begin or
// end, a set staged: InOneView) end it, so a chain answers ahead of nothing
// but the calls that follow it on the connection that made it, in the view
// it was carried on in. For one connection's calls, in turn: it holds no
// lock.
type OnceChainOnConn struct {
	next   string
	series string
	// at is the round of the call whose answer carried the chain on
	// (Answered's call).
	at CallRound
}

// OnceFollow is what Take found of a connection's chain: the recording the
// chain names next (Next, "" when no chain is in progress), the series it
// is of, and the round of the call that carried the chain on, which only a
// call of the same view follows (For).
type OnceFollow struct {
	Next, Series string
	at           CallRound
}

// For is the recording f names for a call marked call (MarkCallRound) to be
// answered with ahead of any other: f.Next when call was marked in the view
// the call that carried the chain on was (InOneView), "" otherwise and for
// no chain. So a chain begun in one test's window never answers a call made
// after another test opened (or the window ended) ahead of that test's own
// recording: a credential's touch that straddles a test's end reads, past
// it, as any call does.
func (f OnceFollow) For(call CallRound) string {
	if !InOneView(f.at, call) {
		return ""
	}
	return f.Next
}

// InOneView reports whether the calls marked a and b (MarkCallRound) were
// marked in one view of their store: both at the same staging and boundary
// count of its round (RoundAt.Epoch, RoundAt.Bounds), so no set was staged,
// no view installed and no boundary of the learn tracker landed between
// the two marks; or neither marked (a store that marks no round, where no
// view moves). A mark of a store that keeps no boundary count (Round's
// known false) is in one view with no other mark (fail closed).
func InOneView(a, b CallRound) bool {
	if !a.Read() || !b.Read() {
		return !a.Read() && !b.Read()
	}
	x, xKnown := a.Round()
	y, yKnown := b.Round()
	return xKnown && yKnown && x.Epoch == y.Epoch && x.Bounds == y.Bounds
}

// Take is what the connection's chain names for its call (OnceFollow): the
// zero OnceFollow when no chain is in progress on the connection. It ends
// the chain: only Answered, told the call's answer, carries it on.
func (c *OnceChainOnConn) Take() OnceFollow {
	f := OnceFollow{Next: c.next, Series: c.series, at: c.at}
	*c = OnceChainOnConn{}
	return f
}

// Answered tells c the recording that answered the connection's call,
// marked call (MarkCallRound), followed being what OnceFollow.For gave that
// call: a once series' first recording begins its chain, and the recording
// followed names carries it on; either leaves the chain's next recording to
// the connection's next call of call's view, none after the series' last.
// Any other answer leaves the chain ended (a recording of no once series
// has the zero OnceLink: no next).
func (c *OnceChainOnConn) Answered(db MockMemDb, answered, followed string, call CallRound) {
	*c = OnceChainOnConn{}
	if l, _ := OnceLinkOf(db, answered); l.First || answered == followed {
		c.next, c.series, c.at = l.Next, l.Series, call
	}
}
