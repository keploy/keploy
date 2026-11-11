package integrations

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

// OnceChainOnConn follows, on one connection, the once chain that
// connection began: once a once series' first recording answered a call on
// it, the connection's next call is the series' next recording's to answer
// (Take), and so on through the series. Any other answer on the connection,
// a call answered by nothing, and a call the parser did not match end it,
// so a chain answers ahead of nothing but the calls that follow it on the
// connection that made it. For one connection's calls, in turn: it holds no
// lock.
type OnceChainOnConn struct {
	next   string
	series string
}

// Take is the recording the connection's call is to be answered with
// ahead of any other (the chain's next), and the series it is of; "" when
// no chain is in progress on the connection. It ends the chain: only
// Answered, told the call's answer, carries it on.
func (c *OnceChainOnConn) Take() (next, series string) {
	next, series = c.next, c.series
	c.next, c.series = "", ""
	return next, series
}

// Answered tells c the recording that answered the connection's call,
// followed being what Take returned for that call: a once series' first
// recording begins its chain, and the recording followed names carries it
// on; either leaves the chain's next recording to the connection's next
// call, none after the series' last. Any other answer leaves the chain
// ended (a recording of no once series has the zero OnceLink: no next).
func (c *OnceChainOnConn) Answered(db MockMemDb, answered, followed string) {
	c.next, c.series = "", ""
	if l, _ := OnceLinkOf(db, answered); l.First || answered == followed {
		c.next, c.series = l.Next, l.Series
	}
}
