package proxy

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"

	"github.com/miekg/dns"
	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/agent/containerkey"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/agent/realdeps"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// SetRealDepsErrNoSwitch refuses mock.real to an agent without the switch.
var SetRealDepsErrNoSwitch = errors.New("mock.real is the proven model's, and this keploy agent runs without " + provenswitch.Env)

// realState is a run's mock.real as the proxy decides connections by it: the
// list, and the run's name book.
type realState struct {
	list *realdeps.List
	book *realdeps.Book
	// search is the resolver's search list: a question for a listed name
	// that the app's resolver expanded with one of these is the listed
	// name's.
	search []string
	// containersUnread: the run said that its container: entries cannot
	// match, Docker's containers being unreadable.
	containersUnread atomic.Bool
}

// listedName is the mock.real name a DNS question asks for: the question's
// own name, or the listed name a search-list expansion of it came from
// (postgres.default.svc.cluster.local. for an entry postgres:5432).
func (st *realState) listedName(qname string) (string, bool) {
	if st.list.IsName(qname) {
		return qname, true
	}
	q := strings.TrimSuffix(strings.ToLower(qname), ".")
	for _, s := range st.search {
		s = strings.Trim(strings.ToLower(s), ".")
		if s == "" {
			continue
		}
		if short, ok := strings.CutSuffix(q, "."+s); ok && st.list.IsName(short) {
			return short, true
		}
	}
	return "", false
}

// SetRealDeps installs a run's mock.real (design v14 §4), under the proven
// switch, and starts the run's REAL bookkeeping afresh: what the last run's
// connections did, and the names its DNS gave, go. Each name entry is looked
// up with the system resolver now, so an address the hosts file or the app's
// own resolver gives it is a candidate too (a lookup that fails gives none:
// a name nothing resolves has no address to match). An empty list installs
// none: nothing is real.
//
// A list keploy cannot read is an error, and nothing is installed.
func (p *Proxy) SetRealDeps(ctx context.Context, raw []string) error {
	if !provenswitch.On() {
		return SetRealDepsErrNoSwitch
	}
	if p.GlobalPassthrough {
		// Every connection would be live: a call mock.real does not list
		// would reach its destination instead of being recorded or failing
		// as a miss (design v14 U18).
		return errors.New("this keploy agent was started with global passthrough, which mock.real replaces: start it without --global-passthrough (keploy.yml globalPassthrough), and list the destinations it reaches for real in mock.real")
	}
	list, err := realdeps.Parse(raw)
	if err != nil {
		return err
	}
	p.realTracker.Reset()
	if list.Empty() {
		p.real.Store(nil)
		return nil
	}
	book := &realdeps.Book{}
	for _, name := range list.Names() {
		addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", name)
		if err != nil {
			p.logger.Debug("the system resolver gave a mock.real name no address; only keploy's DNS answers will name its connections",
				zap.String("name", name), zap.Error(err))
			continue
		}
		book.Resolved(name, addrs)
	}
	p.real.Store(&realState{list: list, book: book, search: append([]string(nil), p.dnsSearch...)})
	agent.LogRealEgress(p.logger, "mock.real", "keploy relays these destinations to the real thing, never recording or mocking them",
		zap.Strings("real", list.Labels()))
	// A replay's session holds REAL connects only while a list is installed:
	// one set up before this list arrived starts holding now.
	p.ensureHandshakeHold(p.getSession())
	return nil
}

// KeepRealDeps installs raw for a later part of the run that installed it
// (keploy test's next test set): a proxy that already holds this list keeps
// what the run saw of it, its bookkeeping and its names; one that holds
// another list, or none, installs raw afresh (SetRealDeps).
func (p *Proxy) KeepRealDeps(ctx context.Context, raw []string) error {
	if !provenswitch.On() {
		return SetRealDepsErrNoSwitch
	}
	list, err := realdeps.Parse(raw)
	if err != nil {
		return err
	}
	if cur := p.real.Load(); !p.GlobalPassthrough && (cur == nil && list.Empty() || cur != nil && slices.Equal(cur.list.Labels(), list.Labels())) {
		return nil
	}
	return p.SetRealDeps(ctx, raw)
}

// RealTracker is what the run's REAL connections did, for the agent's tests
// and the run's verdict.
func (p *Proxy) RealTracker() *realdeps.Tracker { return &p.realTracker }

// realList is the run's list when one is installed and the switch is on.
func (p *Proxy) realList() *realState {
	if !provenswitch.On() {
		return nil
	}
	return p.real.Load()
}

// serveReal decides, before anything else reads it (the MySQL probe, the TLS
// handshake, a parser), whether a connection to dstAddr reaches a mock.real
// destination, and relays it when it does (design v14 §4). containerRepo is
// the image repository of the container dstAddr reaches, "" when it is none
// (containerkey). handled is false
// for a dependency: src is then what the rest of handleConnection reads the
// connection from, holding any bytes keploy read to name it.
//
// A connection to an address keploy's DNS did not name exactly once waits
// here for its client's first bytes, with no clock: it ends when the client
// sends them, closes the connection, or the run ends. A connection whose name
// keploy cannot read is closed, as capture_incomplete: it is never guessed,
// passed through or mocked.
func (p *Proxy) serveReal(ctx context.Context, src net.Conn, dstAddr, containerRepo string, connID int64, predialed *util.Predialed) (handled bool, out net.Conn, err error) {
	st := p.realList()
	if st == nil {
		return false, src, nil
	}
	dst, perr := containerkey.ParseAddrPort(dstAddr)
	if perr != nil {
		return false, src, nil
	}
	if containerRepo == "" {
		containerRepo = p.containerRepoOfConn(ctx, st, dstAddr, connID)
	}
	d := realdeps.Decider{List: st.list, Book: st.book}
	dec := d.AtAccept(dst, containerRepo)
	if dec.Verdict == realdeps.Dependency {
		p.dropRealPredial(predialed)
		return false, src, nil
	}
	br := bufio.NewReaderSize(src, realdeps.ReaderSize)
	wrapped := &util.Conn{Conn: src, Reader: br, Logger: p.logger}
	if dec.Verdict == realdeps.NeedName {
		done := p.realTracker.Pending(connID, dstAddr, dec.Candidates)
		stop := context.AfterFunc(ctx, func() { _ = src.Close() })
		name, rerr := realdeps.ReadName(br)
		stop()
		// The tracker charges a connection closed unnamed to the open test,
		// whatever earlier tests' ends charged it (Tracker.Pending).
		switch {
		case errors.Is(rerr, realdeps.ErrNoName):
			why := realdeps.UnreadableWhy(dstAddr, dec.Candidates, rerr.Error())
			done(why)
			p.logger.Warn("keploy closed a connection it could not name: capture_incomplete", zap.String("destination", dstAddr), zap.String("why", why))
			return true, wrapped, nil
		case rerr != nil:
			if ctx.Err() == nil {
				done(realdeps.UnreadableWhy(dstAddr, dec.Candidates, "its client closed it before naming it"))
			} else {
				done("")
			}
			return true, wrapped, nil
		}
		done("")
		dec = d.ByName(name, dst.Port())
		if dec.Verdict == realdeps.Dependency {
			p.logger.Debug("a connection to a mock.real name's address is for another name: a dependency",
				zap.String("destination", dstAddr), zap.String("name", name.Host), zap.String("via", name.Via))
			p.dropRealPredial(predialed)
			return false, wrapped, nil
		}
	}
	return true, wrapped, p.relayReal(ctx, src, br, dec, dstAddr, connID, predialed)
}

// realLabelAt is the mock.real entry (proven switch) that a connection to
// dstAddr reaches by its address alone (realdeps.Decider.AtAccept), and
// whether there is one: not for an address several name entries share, which
// only a client's bytes would name.
func (p *Proxy) realLabelAt(dstAddr string) (string, bool) {
	st := p.realList()
	if st == nil {
		return "", false
	}
	dst, err := containerkey.ParseAddrPort(dstAddr)
	if err != nil {
		return "", false
	}
	dec := (&realdeps.Decider{List: st.list, Book: st.book}).AtAccept(dst, "")
	if dec.Verdict != realdeps.Real {
		return "", false
	}
	return dec.Label, true
}

// treeRealConn is a caller's connection to a mock.real destination that a
// listener of the run's own tree serves (serveTreeListener): it is relayed to
// that listener, live, and counted as mock.real counts a relay to the
// destination itself (relayReal). The caller's bytes use the destination
// (Tracker.Sent), and the listener's answer to it reaches it
// (Tracker.Answered).
type treeRealConn struct {
	net.Conn
	p     *Proxy
	label string
	id    int64
	// live: the live handler serves it, and may capture it as a test case
	// (Tracker.SentLive).
	live bool
}

func (c *treeRealConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if n > 0 {
		if c.live {
			c.p.realTracker.SentLive(c.id, c.label)
		} else {
			c.p.realTracker.Sent(c.id, c.label)
		}
	}
	return n, err
}

func (c *treeRealConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	if n > 0 {
		c.p.realTracker.Answered(c.label)
	}
	return n, err
}

// CloseWrite forwards a half-close to the connection underneath, as
// liveReader does: an embedded net.Conn does not promote it.
func (c *treeRealConn) CloseWrite() error {
	return util.CloseWriteIfPossible(c.Conn)
}

// relayReal dials a REAL connection's destination and relays every byte to
// it (realdeps.Relay). A dial that fails is the app's to see, as it would be
// without keploy: its connection is reset, and the test's REAL row counts the
// failure.
func (p *Proxy) relayReal(ctx context.Context, client net.Conn, br *bufio.Reader, dec realdeps.Decision, dstAddr string, connID int64, predialed *util.Predialed) error {
	logger := p.logger.With(zap.Int64("Client ConnectionID", connID), zap.String("destination", dstAddr), zap.String("real", dec.Label))
	// Exactly the address the app dialed, with no loopback-family fallback
	// and no budget of keploy's: a fallback could reach an address mock.real
	// does not list, count it reached under the listed label, and let a
	// clock decide connect versus reset. The app makes its own fallback: the
	// handshake hold answers its connect as the real destination did
	// (decideHandshake).
	dest, err := util.DialRaw(ctx, nil, "tcp", dstAddr, predialed)
	if err != nil && predialed == nil {
		// Accepted before the dial (no handshake hold on this host), the
		// app can no longer fall back itself: a localhost entry lists both
		// loopback families, so its other one is tried, once, on a refusal.
		if other, ok := p.otherLoopback(dec, dstAddr, err); ok {
			if alt, aerr := util.DialRaw(ctx, nil, "tcp", other, nil); aerr == nil {
				logger.Debug("a mock.real localhost destination refused one loopback family; relaying to the other", zap.String("dialed", other))
				dest, err = alt, nil
			}
		}
	}
	p.realTracker.Dialed(connID, dec.Label, err == nil)
	if err != nil {
		logger.Debug("a mock.real destination refused or could not be reached; the app sees its connection reset", zap.Error(err))
		if tc, ok := tcpConnOf(client); ok {
			_ = tc.SetLinger(0)
		}
		return nil
	}
	defer func() { _ = dest.Close() }()
	logger.Debug("relaying a connection to a mock.real destination")
	r := &realdeps.Relay{
		Client: client, In: br, Dest: dest, Check: dec.Check,
		Sent:     func(int) { p.realTracker.Sent(connID, dec.Label) },
		Answered: func() { p.realTracker.Answered(dec.Label) },
		H2:       func() { p.realTracker.H2(dec.Label) },
		Refused: func(why string) {
			p.realTracker.Incomplete(dstAddr, why)
			logger.Warn("keploy ended a mock.real connection: capture_incomplete", zap.String("why", why))
		},
		// The app's side over, the connection waits on the real server's
		// answer: no work of keploy's, which a test's end, and a set's
		// end, do not wait on (integrations.MarkIdle), as a record waits
		// on no real server. Unmarked, an app that ended its side (stopped
		// at the run's last set, or giving up on a call) held each end's
		// read until the watchdog gave up, while the destination stayed
		// silent.
		AwaitsAnswer: func() func() { return integrations.MarkIdle(ctx) },
	}
	if err := r.Run(ctx); err != nil && !isNetworkClosedErr(err) && !errors.Is(err, context.Canceled) {
		logger.Debug("a mock.real connection ended with an error", zap.Error(err))
	}
	return nil
}

// realDNS answers a question for a mock.real name: live, from the upstream
// resolver, never from a recording, never recorded, and never from the
// answer cache (design v14 §4). An upstream that cannot be asked is answered
// SERVFAIL, so the app's resolver sees the failure as it is and is never
// steered to keploy's own address.
func (p *Proxy) realDNS(question dns.Question) dnsCacheEntry {
	fwd, err := p.forwardDNSUpstream(question)
	if err != nil || fwd == nil {
		utils.LogError(p.logger, err, "keploy could not resolve a mock.real name with the upstream resolver; the app is answered SERVFAIL",
			append(dnsQuestionFields(question), zap.String("next_step", "check the upstream DNS resolver keploy's agent forwards to"))...)
		return dnsCacheEntry{Msg: &dns.Msg{MsgHdr: dns.MsgHdr{Rcode: dns.RcodeServerFailure, RecursionAvailable: true}}}
	}
	return dnsCacheEntry{Msg: fwd, FromUpstream: true}
}

// bookDNSAnswer notes, for the run's mock.real names, which addresses keploy's
// DNS answered question with: under the listed name for a search-list
// expansion of one, so the address has the one name the app asked for.
func (st *realState) bookDNSAnswer(question dns.Question, answer []dns.RR) {
	var addrs []netip.Addr
	for _, rr := range answer {
		switch v := rr.(type) {
		case *dns.A:
			if a, ok := netip.AddrFromSlice(v.A); ok {
				addrs = append(addrs, a.Unmap())
			}
		case *dns.AAAA:
			if a, ok := netip.AddrFromSlice(v.AAAA); ok {
				addrs = append(addrs, a.Unmap())
			}
		}
	}
	name := question.Name
	if listed, ok := st.listedName(name); ok {
		name = listed
	}
	st.book.Answered(name, addrs)
}

// RealRun is what the run saw of mock.real beyond its tests.
func (p *Proxy) RealRun() models.RealRun { return p.realTracker.Run() }

// listedNameOf is listedName on a run that may have no list.
func (st *realState) listedNameOf(qname string) (string, bool) {
	if st == nil {
		return "", false
	}
	return st.listedName(qname)
}

// realAtHandshake is the mock.real destination a held handshake's
// destination is, when its address alone decides it (an address entry, a
// container entry, or the one name keploy's DNS gave the address): known
// before the connection is accepted, so its connect can answer as the real
// destination's does. A destination whose name is read from its client's
// bytes is not known yet, and is accepted as before.
func (p *Proxy) realAtHandshake(dest *agent.NetworkAddress) (string, bool) {
	st := p.realList()
	if st == nil || dest == nil {
		return "", false
	}
	addr := destAddr(dest)
	dst, err := containerkey.ParseAddrPort(addr)
	if err != nil {
		return "", false
	}
	dec := realdeps.Decider{List: st.list, Book: st.book}.AtAccept(dst, p.containerRepoAt(st, addr))
	if dec.Verdict != realdeps.Real {
		return "", false
	}
	return dec.Label, true
}

// containerRepoOfConn is the image repository of the container accepted
// connection connID reaches, for a list with container: entries: the one the
// keyer placed it at, waiting, if the index did not place it at its accept,
// for the containers the index was placing then and, placing none there, for
// a list of the daemon asked then (containerkey.Keyer.Placed, never a
// configuration's read), so a container Docker started a moment before is a
// mock.real destination, not a dependency recorded under its key. Else what
// the index says of addr now (containerRepoAt).
func (p *Proxy) containerRepoOfConn(ctx context.Context, st *realState, addr string, connID int64) string {
	if !st.list.HasContainers() {
		return ""
	}
	if c, ok := containerkey.Default.Placed(ctx, fmt.Sprint(connID)); ok {
		return containerkey.Repository(c.Image)
	}
	return p.containerRepoAt(st, addr)
}

// containerRepoAt is the image repository of the container addr reaches, ""
// for none, read for a list with container: entries from the index without
// naming anything (containerkey.Keyer.Lookup): the handshake hold asks
// before the connection is accepted, and naming it there would hold the
// connection in the keyer before it is one and spend the session's one
// warning that Docker cannot be read. It never waits: a container the index
// is still placing is no container's yet to the hold, which dials it ahead,
// if at all, as a dependency (its accept waits, containerRepoOfConn, and
// relays it).
// While the index cannot be read, the run's container: entries match
// nothing, and the run says so once.
func (p *Proxy) containerRepoAt(st *realState, addr string) string {
	if !st.list.HasContainers() {
		return ""
	}
	c, ok, err := containerkey.Default.Lookup(addr)
	if err != nil {
		if !errors.Is(err, context.Canceled) && st.containersUnread.CompareAndSwap(false, true) {
			p.logger.Warn("mock.real lists container: entries, and keploy cannot read the containers of the Docker daemon: while it cannot, those entries match nothing, and connections to their containers are recorded and mocked as dependencies",
				zap.Strings("real", st.list.Labels()), zap.Error(err),
				zap.String("next_step", "let keploy's agent read the local Docker daemon the tests use (keploy passes it DOCKER_HOST, else the docker CLI's current context, else the default socket), or list those dependencies by address"))
		}
		return ""
	}
	if !ok {
		return ""
	}
	return containerkey.Repository(c.Image)
}

// dropRealPredial closes, in a replay, a connection the handshake hold
// dialled to a destination it took for REAL when the accept decides the
// connection is a dependency (keploy's DNS gave its address a second name in
// between): the replay serves it from mocks and never takes the dial, and,
// left open, the real server closing it would close the app's end
// (WatchUntilTaken). A record dials its dependencies, and takes it.
func (p *Proxy) dropRealPredial(predialed *util.Predialed) {
	if predialed == nil {
		return
	}
	if rule := p.getSession(); rule != nil && !p.dialsUpstream(rule) {
		predialed.CloseIfUnused()
	}
}

// otherLoopback is the other loopback family's address of a connection to a
// localhost entry (dec's label is localhost:P, which lists 127.0.0.1:P and
// [::1]:P) whose dial was refused: what the app would have tried next had its
// own connect been refused. Any other entry, destination or error has none.
func (p *Proxy) otherLoopback(dec realdeps.Decision, dstAddr string, err error) (string, bool) {
	if !errors.Is(err, syscall.ECONNREFUSED) || !strings.HasPrefix(dec.Label, "localhost:") {
		return "", false
	}
	ap, perr := containerkey.ParseAddrPort(dstAddr)
	if perr != nil || !ap.Addr().IsLoopback() {
		return "", false
	}
	other := netip.AddrPortFrom(netip.IPv6Loopback(), ap.Port())
	if !ap.Addr().Unmap().Is4() {
		other = netip.AddrPortFrom(netip.AddrFrom4([4]byte{127, 0, 0, 1}), ap.Port())
	}
	st := p.realList()
	if st == nil {
		return "", false
	}
	if again := (realdeps.Decider{List: st.list, Book: st.book}).AtAccept(other, ""); again.Verdict != realdeps.Real || again.Label != dec.Label {
		return "", false
	}
	return other.String(), true
}

// RealDestination is, under the proven switch, the mock.real entry a
// recorded mock's destination is (realdeps.List.Recorded): a call to it is
// reached for real, never served from a mock, so a test that owns the mock
// (a recording made before the destination was listed) is neither served it
// nor judged for it. ok is false for a mock whose recording does not say,
// and without a list.
func (p *Proxy) RealDestination(m *models.Mock) (string, bool) {
	st := p.realList()
	if st == nil {
		return "", false
	}
	return st.list.Recorded(m)
}

// tcpConnOf is the TCP connection under c, through the conns that wrap one
// and say so (NetConn, as *tls.Conn and the proxy's trackedConn do).
func tcpConnOf(c net.Conn) (*net.TCPConn, bool) {
	for i := 0; c != nil && i < 8; i++ {
		if tc, ok := c.(*net.TCPConn); ok {
			return tc, true
		}
		u, ok := c.(interface{ NetConn() net.Conn })
		if !ok {
			return nil, false
		}
		c = u.NetConn()
	}
	return nil, false
}

// realDialFailed counts, against the open test's REAL row, a held handshake's
// dial to a mock.real destination that failed (the app's connect fails the
// same way, answered at once), and returns the destination's label. label is
// what the hold knew of addr; "" when addr was not a mock.real destination by
// then, which for a list with container: entries may be only because the
// index was still placing a container Docker started a moment before. Placed
// since, the failure counts now. Still being placed, the failed connect is
// noted with the tracker as it happens (Tracker.FailedUndecided) and the
// placing is waited for off the handshake's path (Keyer.PlacedOrPlacing):
// told REAL while the test it failed in is open, it counts against that test;
// that test ending first is charged with it as capture_incomplete. An index
// that cannot be read, or places nothing that could be there, leaves it a
// dependency's failed connect, as at the accept.
func (p *Proxy) realDialFailed(addr, label string) (string, bool) {
	if label != "" {
		p.realTracker.Dialed(0, label, false)
		return label, false
	}
	st := p.realList()
	if st == nil || !st.list.HasContainers() {
		return "", false
	}
	dst, err := containerkey.ParseAddrPort(addr)
	if err != nil {
		return "", false
	}
	decide := func(repo string) string {
		if dec := (realdeps.Decider{List: st.list, Book: st.book}).AtAccept(dst, repo); dec.Verdict == realdeps.Real {
			return dec.Label
		}
		return ""
	}
	c, ok, wait := containerkey.Default.PlacedOrPlacing(addr)
	switch {
	case ok:
		if label = decide(containerkey.Repository(c.Image)); label != "" {
			p.realTracker.Dialed(0, label, false)
		}
		return label, false
	case wait != nil:
		resolve := p.realTracker.FailedUndecided(addr)
		wait(func(c containerkey.Container, ok, resolved bool) {
			label := ""
			if resolved && ok {
				label = decide(containerkey.Repository(c.Image))
			}
			resolve(label, !resolved)
			if p.realDialResolved != nil {
				p.realDialResolved()
			}
		})
		return "", true
	}
	return "", false
}
