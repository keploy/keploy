package containerkey

import (
	"context"
	"errors"
	"maps"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// Default is the agent's keyer: the proxy names each connection with it, the
// scope routes open and close its test scopes, the sync manager stamps
// recorded mocks with it, and the agent tells it, from each test set's mocks,
// which recorded keys are ambiguous.
var Default = NewKeyer(newDockerIndex())

// fingerprintsResolver is a resolver that also reads a container's legacy
// fingerprint (legacyFingerprint), with its current one: Docker's index, and
// the list a docker-mode agent is given. One that does not reads no legacy
// recording's keys.
type fingerprintsResolver interface {
	Fingerprints(ctx context.Context, id string) (fp, legacy string, ok bool)
}

// resolver finds the container behind a destination from its index, without
// waiting on anything. err says the daemon cannot be read.
type resolver interface {
	Lookup(dst netip.AddrPort) (c Container, ok bool, err error)
	// Fingerprint is container id's fingerprint, waiting while the index is
	// reading it; ok is false when the index does not hold it, or ctx ends.
	Fingerprint(ctx context.Context, id string) (fp string, ok bool)
	// Pending says whether a container could be at dst by the index (none
	// is at a docker-mode agent's loopback, the app container's own), and
	// which containers it is placing now: ones an event started or moved,
	// whose addresses it does not hold yet. It never waits.
	Pending(dst netip.AddrPort) (placing []string, could bool)
	// AwaitReadsOf waits until none of the containers ids is still being
	// placed, or ctx ends: a container Docker started a moment before a
	// connection to it was accepted is then placed. Placing a container
	// never waits for its configuration's read.
	AwaitReadsOf(ctx context.Context, ids []string)
	// Bind makes ctx, the agent's own, the bound of the index's reading of
	// Docker, so it stops with the agent.
	Bind(ctx context.Context)
	// Watch starts keeping the index, if it is not kept already, and returns
	// once it holds what was running (or ctx ends).
	Watch(ctx context.Context, logger *zap.Logger)
	// Endpoint names the Docker endpoint read, for messages.
	Endpoint() string
}

// State is what the keyer says of a connection's destination when a call on
// it is matched.
type State int

const (
	// NotContainer: the destination is no container the index holds.
	NotContainer State = iota
	// Keyed: the destination is a container named by its key.
	Keyed
	// Withheld: the destination is a container, but its key names it for no
	// recording: another container configured the same (the same key) is in
	// use in the scope, or its configuration could not be read. Its calls
	// match no recording of a container, by key or by address.
	Withheld
)

// Keyer names container destinations and holds each open container
// connection's container.
//
// A key names a container by its repository, port and configuration (Key),
// which do not depend on the order the tests start or use containers in.
// Two containers configured the same have one key, and cannot be told apart:
// where both are used in one scope (the open test, or, outside tests, the
// instance: the test process that claimed the run), the keyer withholds the
// key from every use of either from then on, and a recording whose scope used
// two such containers is ambiguous for the matcher (RecordedAmbiguous), so a
// call to either misses rather than takes the other's answer. A scope starts
// with no container used: a container the suite set up and one a test starts
// configured the same are told apart only where one scope uses both.
//
// A connection is named when the proxy accepts it, from the index, which
// never waits on Docker; its key is taken at its first use (a call matched,
// a mock stamped), waiting, if it must, for the index's read of its
// container's configuration.
type Keyer struct {
	mu sync.Mutex
	// res is the index: the local daemon's, or, in a docker-mode agent, the
	// list its CLI gives it (UsePushed).
	res resolver
	// run is the scope outside tests: the instance's, or the run's before
	// its first claim; test, while a test is open, that test's.
	run, test *scope
	// scopes counts the scopes this session began, for their tokens.
	scopes int
	// begun counts the scopes this keyer ever began, never reset (a
	// session's Reset restarts scopes): each scope's id (Scope).
	begun uint64
	// spans are the session's scopes, in the order they began, with when
	// they began and ended: Stamp files a recorded mock under the scope its
	// request was sent in, which may have ended by the time it is stamped.
	spans []span
	// conns holds each connection's container, by the connection ID its
	// mocks carry, while it is open and for the closed ones a late mock may
	// still be stamped for (closed, oldest first, at most keptClosed).
	conns  map[string]Container
	closed []string
	// unplaced holds the open connections to a destination where a
	// container could be (a loopback, private, link-local or unspecified
	// address, couldBeContainer, by the index: Pending) that the index did
	// not place when they were accepted: each is looked up again once, at
	// its first use (or at its accept, in a run whose mock.real lists
	// container: entries: Placed), after the containers the index was
	// placing at its accept are placed, so a container Docker started a
	// moment before is still named.
	unplaced map[string]*pendingConn
	// opened counts each ID's Names not yet forgotten. A capture can name
	// one ID twice before either is forgotten: a connection it routes afresh
	// under its own ID (a JSSE stream recorded again from its next request),
	// or an ID reused while the earlier connection's parser still drains.
	// Only the ID's last Forget closes it, so the first's close neither drops
	// the look-up again of the one still open nor puts it among the closed,
	// where keptClosed later closes would take its container while it is
	// open.
	opened map[string]int
	// recorded is, for the test set the agent was given, the recorded
	// groups (a key in one scope) whose mocks came from two or more
	// containers: SetRecorded.
	recorded map[recordedGroup]bool
	// recordedNamed: that test set was recorded naming containers
	// (RecordedTally.named).
	recordedNamed bool
	// warned: this session already said Docker cannot be read.
	warned bool
	// session bounds what waits for a container's configuration outside a
	// call (Stamp): the keyer's own, from one Watch (a session's start) to
	// the next, or Reset, within the agent's (bound, Bind's); nil before a
	// session. endSession ends it.
	session    context.Context
	endSession context.CancelFunc
	bound      context.Context
}

// pendingConn is an open connection the index did not place at its accept
// (Keyer.unplaced).
type pendingConn struct {
	ap netip.AddrPort
	// placingAtAccept is the containers the index was placing at the
	// accept: its first use waits for those only.
	placingAtAccept []string
	// lookedUp closes when its one look-up again is over; nil until its
	// first use begins one.
	lookedUp chan struct{}
	// renamed: its ID was named again while it waited (dropLocked), for
	// another connection or for this one routed afresh. Its look-up then
	// names the uses that waited on it (got, placed), not the ID, whose
	// container is what the later Name gave.
	renamed bool
	got     Container
	placed  bool
}

// keptClosed bounds how many closed connections a keyer keeps for mocks
// stamped after their connection closed.
const keptClosed = 4096

// scope is one scope: its token, its id (Scope), and the containers used in
// it, by key.
type scope struct {
	token string
	id    uint64
	used  map[string]map[string]bool
}

// span is when a scope was open; to is zero while it is. test says whether it
// is a test's scope, which wins over the run's it began in.
type span struct {
	token    string
	from, to time.Time
	test     bool
}

// recordedGroup is the mocks of one key in one recorded scope.
type recordedGroup struct{ key, scope string }

// NewKeyer is a keyer that asks res, which is never nil, which destinations
// are containers.
func NewKeyer(res resolver) *Keyer {
	k := &Keyer{res: res, conns: map[string]Container{}, unplaced: map[string]*pendingConn{}, opened: map[string]int{}}
	k.run = k.beginLocked(false, time.Now())
	return k
}

// beginLocked starts a scope at, as a test's when test; k.mu is held.
func (k *Keyer) beginLocked(test bool, at time.Time) *scope {
	k.scopes++
	k.begun++
	s := &scope{token: "s" + strconv.Itoa(k.scopes), id: k.begun, used: map[string]map[string]bool{}}
	k.spans = append(k.spans, span{token: s.token, from: at, test: test})
	return s
}

// endLocked ends scope s's span at; k.mu is held.
func (k *Keyer) endLocked(s *scope, at time.Time) {
	if s == nil {
		return
	}
	for i := len(k.spans) - 1; i >= 0; i-- {
		if k.spans[i].token == s.token {
			if k.spans[i].to.IsZero() {
				k.spans[i].to = at
			}
			return
		}
	}
}

// Watch starts reading Docker for a session, if k can, and returns once the
// index holds what was already running, or ctx (the session's) ends: called
// when a session starts under the proven switch, before the app runs. Only a
// session start does: a daemon that cannot be read stays unread for the rest
// of the session.
func (k *Keyer) Watch(ctx context.Context, logger *zap.Logger) {
	k.mu.Lock()
	res := k.res
	k.endSessionLocked()
	parent := k.bound
	if parent == nil {
		parent = context.Background()
	}
	// The session's own context, not ctx: ctx may be the request that
	// started the session (the replay's), which ends as soon as it is
	// answered.
	k.session, k.endSession = context.WithCancel(parent)
	k.mu.Unlock()
	res.Watch(ctx, logger)
}

// endSessionLocked ends the session's context, if one is open: what waits
// in it for a container's configuration (Stamp) stops. k.mu is held.
func (k *Keyer) endSessionLocked() {
	if k.endSession != nil {
		k.endSession()
		k.endSession = nil
	}
}

// sessionCtx is the context of the session open now, which bounds a wait
// for a container's configuration that no call bounds (Stamp): a read that
// never ends, or a docker-mode agent whose list never comes, holds a parser
// no longer than its session (to the next session's start, or the agent's
// end).
func (k *Keyer) sessionCtx() context.Context {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.session == nil {
		return context.Background()
	}
	return k.session
}

// Bind makes ctx, the agent's own, the bound of k's reading of Docker, so it
// stops with the agent: called when the agent starts, under the proven
// switch.
func (k *Keyer) Bind(ctx context.Context) {
	k.mu.Lock()
	res := k.res
	k.bound = ctx
	k.mu.Unlock()
	res.Bind(ctx)
}

// Reset starts a new session (a record or a replay of one test set): every
// scope goes, and so does every connection the last session left open. What
// SetRecorded said of the test set's mocks stays: the agent is given the
// set's mocks around the session's start, in either order.
func (k *Keyer) Reset() {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.scopes, k.spans, k.test = 0, nil, nil
	k.run = k.beginLocked(false, time.Now())
	k.conns, k.closed, k.unplaced, k.warned = map[string]Container{}, nil, map[string]*pendingConn{}, false
	k.opened = map[string]int{}
	k.endSessionLocked()
}

// BeginInstance starts a test process's scope outside its tests, when it
// claims the run.
func (k *Keyer) BeginInstance() {
	k.mu.Lock()
	defer k.mu.Unlock()
	now := time.Now()
	k.endLocked(k.test, now)
	k.endLocked(k.run, now)
	k.run, k.test = k.beginLocked(false, now), nil
}

// BeginTest opens a test's scope; a scope already open is replaced, since a
// run has one open test at a time.
func (k *Keyer) BeginTest() {
	k.mu.Lock()
	defer k.mu.Unlock()
	now := time.Now()
	k.endLocked(k.test, now)
	k.test = k.beginLocked(true, now)
}

// EndTest closes the open test's scope: connections from here on are the
// run's again, in a scope of their own until the next test (the setup
// between two tests is a scope, not the run's whole time outside tests: a
// suite that starts one container per test, configured the same, does not
// make them ambiguous by the traffic of its fixtures).
func (k *Keyer) EndTest() {
	k.mu.Lock()
	defer k.mu.Unlock()
	now := time.Now()
	k.endLocked(k.test, now)
	k.test = nil
	k.endLocked(k.run, now)
	k.run = k.beginLocked(false, now)
}

// Scope is the scope in effect now -- the open test's, or the run's outside
// tests (the run's start, the setup between two tests, after the last
// test) -- by an id no other scope of this keyer's has had, a session's Reset
// included: what a connection's events compare to tell whether they happened
// within one scope.
func (k *Keyer) Scope() uint64 {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.test != nil {
		return k.test.id
	}
	return k.run.id
}

// Name names the connection connID, accepted for the address the proxy dials
// (ip:port), when the index knows its destination as a container, and
// returns the container and, when its configuration is in, its key. It never
// waits, and logs only after letting go of k.
//
// When Docker cannot be read, the connection is named by its address and the
// session says so once: its container destinations are then compared by
// address, so one on a random port misses in replay instead of matching
// another container. A host with no Docker daemon socket has no containers
// to name: the agent says so once, at Info, and at Debug after.
//
// Name begins connection connID. What the keyer still holds under that ID is
// an earlier connection's that carried it, kept closed for its late mocks: a
// capture that names a connection by something reused (a decrypted TLS
// stream by its SSL*, freed and allocated again) hands the keyer one ID for
// both. It is not this connection's, so it goes first: else a connection the
// index does not place would take, at its first use, the container the
// earlier one reached. A dst Name cannot read (empty: a caller that does not
// know the connection's destination) names no container. A capture may also
// name an open connection again under its own ID (a JSSE stream it records
// afresh from its next request): it is named by dst again, and a look-up
// again its first use was waiting on still names that use (place).
func (k *Keyer) Name(logger *zap.Logger, connID, dst string) (Container, string, bool) {
	if connID != "" {
		k.mu.Lock()
		k.dropLocked(connID)
		k.opened[connID]++
		k.mu.Unlock()
	}
	ap, err := ParseAddrPort(dst)
	if err != nil {
		return Container{}, "", false
	}
	got, ok, warn := k.name(connID, ap)
	key := ""
	if kk, in := KeyOf(got); ok && in {
		key = kk.String()
	}
	if logger == nil {
		return got, key, ok
	}
	if warn != nil {
		fields := []zap.Field{zap.String("dockerEndpoint", k.endpoint()), zap.Error(warn),
			zap.String("next_step", "let keploy's agent read the local Docker daemon the tests use: keploy passes it DOCKER_HOST, else the docker CLI's current context, else it reads the default socket")}
		msg := "keploy cannot read the containers of the Docker daemon, so this run names container destinations by their address: a container on a random port, such as one Testcontainers starts, will not match its recording"
		switch {
		case !errors.Is(warn, errNoDaemon):
			logger.Warn(msg, fields...)
		case noDaemonSaid.CompareAndSwap(false, true):
			// A run with no Docker at all is said once per agent, at Info:
			// tests that reach a daemon through a socket keploy cannot see
			// lose their keys, and the user should be able to tell.
			logger.Info(msg, fields...)
		default:
			logger.Debug(msg, fields...)
		}
	}
	if ok {
		logger.Debug("named a container destination", zap.String("destination", ap.String()), zap.String("containerKey", key), zap.String("image", got.Image))
	}
	return got, key, ok
}

// Lookup is the container dst (ip:port) reaches by the index, read with no
// effect on k: no connection is held, no key used, and nothing said, so a caller
// that only asks, before the proxy accepts and names the connection (the
// handshake hold), changes nothing Name later gives or says. err says the
// daemon cannot be read.
func (k *Keyer) Lookup(dst string) (Container, bool, error) {
	ap, err := ParseAddrPort(dst)
	if err != nil {
		return Container{}, false, nil
	}
	k.mu.Lock()
	res := k.res
	k.mu.Unlock()
	return res.Lookup(ap)
}

// PlacedOrPlacing is the container dst reaches now (c, ok), or how to wait
// for one the index is still placing: wait, non-nil, when the index places
// none at dst now but is placing containers that could be there (a container
// Docker started a moment before). wait(fn) then waits, off the caller's
// path, for those placings only (never a configuration's read, nor a
// container started since), no longer than the session or the index's
// watch, and runs fn: ok is whether dst then reaches a container, resolved
// false when the wait ended first and that cannot be told. Neither (ok false,
// wait nil): no container is at dst and none could be, the index places
// nothing now, or it cannot be read (a container: entry then matches
// nothing, as at the accept). The placings are read before dst is looked up,
// so one that ends between the two is seen placed, never missed.
func (k *Keyer) PlacedOrPlacing(dst string) (c Container, ok bool, wait func(fn func(c Container, ok, resolved bool))) {
	ap, err := ParseAddrPort(dst)
	if err != nil || !couldBeContainer(ap.Addr()) {
		return Container{}, false, nil
	}
	k.mu.Lock()
	res := k.res
	k.mu.Unlock()
	placing, could := res.Pending(ap)
	if c, ok, err := res.Lookup(ap); err != nil || ok {
		return c, ok && err == nil, nil
	}
	if !could || len(placing) == 0 {
		return Container{}, false, nil
	}
	ctx := k.sessionCtx()
	return Container{}, false, func(fn func(c Container, ok, resolved bool)) {
		go func() {
			res.AwaitReadsOf(ctx, placing)
			c, ok, err := res.Lookup(ap)
			if err != nil || (!ok && ctx.Err() != nil) {
				// The index failed, or the session ended first: whether dst
				// is a container cannot be told.
				fn(Container{}, false, false)
				return
			}
			fn(c, ok, true)
		}()
	}
}

// Placed is the container connection connID reaches: the one Name gave it,
// or the one its look-up again finds (place), after the containers the index
// was placing at its accept are placed. It never waits for a container's
// configuration, nor for a container started since. ok is false when the
// connection reaches none, or ctx ends first.
func (k *Keyer) Placed(ctx context.Context, connID string) (Container, bool) {
	k.mu.Lock()
	c, ok := k.conns[connID]
	k.mu.Unlock()
	if ok {
		return c, true
	}
	return k.place(ctx, connID)
}

// name is Name under k.mu; warn is the error to tell, once per session.
func (k *Keyer) name(connID string, ap netip.AddrPort) (got Container, ok bool, warn error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	got, ok, err := k.res.Lookup(ap)
	if err != nil {
		if !k.warned && !errors.Is(err, context.Canceled) {
			k.warned = true
			warn = err
		}
		return Container{}, false, warn
	}
	if !ok {
		if connID != "" && couldBeContainer(ap.Addr()) {
			if placing, could := k.res.Pending(ap); could {
				k.unplaced[connID] = &pendingConn{ap: ap, placingAtAccept: placing}
			}
		}
		return Container{}, false, nil
	}
	if connID != "" {
		k.conns[connID] = got
	}
	return got, true, nil
}

// couldBeContainer reports whether a container could be at a: a loopback
// (a published port), private or link-local address (a container's own),
// or an unspecified one.
func couldBeContainer(a netip.Addr) bool {
	a = a.Unmap()
	return a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() || a.IsUnspecified()
}

// place looks up again an open connection the index did not place when it
// was accepted, once, at its first use (or Placed): after the containers the
// index was placing at its accept are placed (not those started since, which
// would hold every call while containers keep starting; nor any container's
// configuration's read, which a container Docker holds can wedge), and names
// it if the index places it now. A use while that look-up runs waits for it;
// a later use takes its outcome.
func (k *Keyer) place(ctx context.Context, connID string) (Container, bool) {
	k.mu.Lock()
	p := k.unplaced[connID]
	if p == nil {
		// Placed by another use since this one looked (key), or never
		// unplaced.
		c, ok := k.conns[connID]
		k.mu.Unlock()
		return c, ok
	}
	if p.lookedUp != nil {
		lookedUp := p.lookedUp
		k.mu.Unlock()
		select {
		case <-lookedUp:
		case <-ctx.Done():
			return Container{}, false
		}
		k.mu.Lock()
		defer k.mu.Unlock()
		if p.renamed {
			return p.got, p.placed
		}
		c, ok := k.conns[connID]
		return c, ok
	}
	p.lookedUp = make(chan struct{})
	res := k.res
	k.mu.Unlock()
	if len(p.placingAtAccept) > 0 {
		res.AwaitReadsOf(ctx, p.placingAtAccept)
	}
	got, ok, err := res.Lookup(p.ap)
	k.mu.Lock()
	defer k.mu.Unlock()
	defer close(p.lookedUp)
	if k.unplaced[connID] != p {
		if p.renamed && err == nil && ok {
			// Named again meanwhile: what it found is this connection's,
			// for its uses; the ID's is the later Name's.
			p.got, p.placed = got, true
			return got, true
		}
		return Container{}, false // closed, or the session reset, meanwhile
	}
	delete(k.unplaced, connID)
	if err != nil || !ok {
		return Container{}, false
	}
	k.conns[connID] = got
	return got, true
}

// endpoint names the Docker endpoint k reads, for messages.
func (k *Keyer) endpoint() string {
	k.mu.Lock()
	res := k.res
	k.mu.Unlock()
	return res.Endpoint()
}

// dropLocked forgets connection connID altogether: its container, its place
// among the closed connections and its look-up again. k.mu is held.
func (k *Keyer) dropLocked(connID string) {
	if p := k.unplaced[connID]; p != nil {
		p.renamed = true
		delete(k.unplaced, connID)
	}
	if _, held := k.conns[connID]; !held {
		return
	}
	delete(k.conns, connID)
	k.closed = slices.DeleteFunc(k.closed, func(id string) bool { return id == connID })
}

// Forget marks a connection closed, at the last Forget of its ID's Names
// (opened). Its container is kept for a mock stamped late, until keptClosed
// connections closed after it.
func (k *Keyer) Forget(connID string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if n := k.opened[connID]; n > 1 {
		k.opened[connID] = n - 1
		return // named again since, and that one is open
	}
	delete(k.opened, connID)
	delete(k.unplaced, connID)
	if _, ok := k.conns[connID]; !ok {
		return
	}
	k.closed = append(k.closed, connID)
	if len(k.closed) > keptClosed {
		delete(k.conns, k.closed[0])
		k.closed = k.closed[1:]
	}
}

// key is the key of connection connID's container, waiting, if it must, for
// the index's read of its configuration; ok is false for a connection that
// is no container's, and key is "" for a container whose configuration
// could not be read.
func (k *Keyer) key(ctx context.Context, connID string) (c Container, key string, ok bool) {
	k.mu.Lock()
	c, ok = k.conns[connID]
	res := k.res
	k.mu.Unlock()
	if !ok {
		if c, ok = k.place(ctx, connID); !ok {
			return Container{}, "", false
		}
	}
	if c.Fingerprint == "" {
		var fp, legacy string
		var in bool
		if fr, ok := res.(fingerprintsResolver); ok {
			fp, legacy, in = fr.Fingerprints(ctx, c.ID)
		} else {
			fp, in = res.Fingerprint(ctx, c.ID)
		}
		if !in {
			return c, "", true
		}
		c.Fingerprint, c.Legacy = fp, legacy
		k.mu.Lock()
		if held, still := k.conns[connID]; still && held.ID == c.ID {
			k.conns[connID] = c
		}
		k.mu.Unlock()
	}
	kk, _ := KeyOf(c)
	return c, kk.String(), true
}

// use is a use of connection connID's destination in the scope open now:
// its key, and whether the keyer gives it (Keyed) or withholds it.
func (k *Keyer) use(ctx context.Context, connID string) (string, State) {
	c, key, ok := k.key(ctx, connID)
	switch {
	case !ok:
		return "", NotContainer
	case key == "":
		return "", Withheld
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	s := k.run
	if k.test != nil {
		s = k.test
	}
	ids := s.used[key]
	if ids == nil {
		ids = map[string]bool{}
		s.used[key] = ids
	}
	ids[c.ID] = true
	if len(ids) > 1 {
		return key, Withheld
	}
	return key, Keyed
}

// Stamp writes, into a recorded mock's metadata, the key of the container its
// connection reached (MetadataKey), the container's instance (InstanceKey)
// and the scope its request was sent in (ScopeKey): the matcher tells from
// them whether two containers configured the same answered one scope's
// calls (RecordedAmbiguous). A container whose configuration could not be
// read is stamped with its instance and scope only: a container's, whose key
// no call has. A mock of any other connection is left as it is. The sync
// manager stamps each mock as a parser hands it over.
func (k *Keyer) Stamp(m *models.Mock) {
	if m == nil {
		return
	}
	id := m.ConnectionID
	if id == "" {
		id = m.Spec.Metadata["connID"]
	}
	if id == "" {
		return
	}
	k.stampConn(id, m)
}

// StampFromContext stamps m as Stamp does, for the connection ctx carries
// (WithConn): a mock no parser hands the sync manager, such as a call served
// from the real dependency on a miss and captured.
func StampFromContext(ctx context.Context, m *models.Mock) {
	ref, ok := ctx.Value(ctxKey{}).(connRef)
	if !ok || ref.k == nil || m == nil {
		return
	}
	ref.k.stampConn(ref.id, m)
}

// stampConn stamps m for connection id (Stamp).
func (k *Keyer) stampConn(id string, m *models.Mock) {
	c, key, ok := k.key(k.sessionCtx(), id)
	if !ok {
		return
	}
	at := m.Spec.ReqTimestampMock
	if at.IsZero() {
		at = time.Now()
	}
	k.mu.Lock()
	scopeToken := k.scopeAtLocked(at)
	k.mu.Unlock()
	// A copy: a parser may hand several mocks one metadata map, and the
	// outgoing stream may be encoding an earlier one while this one is
	// stamped.
	md := maps.Clone(m.Spec.Metadata)
	if md == nil {
		md = map[string]string{}
	}
	if key != "" {
		md[MetadataKey] = key
	}
	md[InstanceKey] = instanceToken(c.ID)
	md[ScopeKey] = scopeToken
	m.Spec.Metadata = md
}

// scopeAtLocked is the token of the scope open at: the test's open then, else
// the run's; k.mu is held. The spans are in the order they began, so it
// reads them from the newest: a mock is stamped soon after its request.
func (k *Keyer) scopeAtLocked(at time.Time) string {
	for i := len(k.spans) - 1; i >= 0; i-- {
		s := k.spans[i]
		if s.from.After(at) {
			continue
		}
		if s.test && !s.to.IsZero() && !at.Before(s.to) {
			continue // a test that had ended: the run's span after it, or before
		}
		return s.token
	}
	if len(k.spans) > 0 {
		return k.spans[0].token // sent before the session began
	}
	return ""
}

// noDaemonSaid: this agent said at Info that it found no Docker daemon.
var noDaemonSaid atomic.Bool

type ctxKey struct{}

type connRef struct {
	k  *Keyer
	id string
}

// WithConn is ctx carrying the connection connID of k, which the proxy hands
// its parsers so a matcher can compare the connection's key (FromContext)
// with a mock's (Recorded).
func WithConn(ctx context.Context, k *Keyer, connID string) context.Context {
	return context.WithValue(ctx, ctxKey{}, connRef{k: k, id: connID})
}

// StateFromContext is a use, in the scope open now, of the destination of
// the connection a parser serves: its container's key, and whether the keyer
// gives it. It may wait for the index's read of the container's
// configuration, at most as long as ctx.
func StateFromContext(ctx context.Context) (string, State) {
	ref, ok := ctx.Value(ctxKey{}).(connRef)
	if !ok || ref.k == nil {
		return "", NotContainer
	}
	return ref.k.use(ctx, ref.id)
}

// KeyFromContext is the container key of the connection ctx carries, as
// StateFromContext reads it, waiting at most as long as ctx for the index's
// read of the container's configuration, but without counting a use of it in
// the scope open now: it says what the destination is, as the MySQL detection
// probe asks before any parser serves the connection. keyed is false when ctx
// carries no connection of a keyer (the proven switch is off); isContainer is
// false for a connection that is no container's; key is "" for a container
// whose configuration could not be read.
func KeyFromContext(ctx context.Context) (key string, isContainer, keyed bool) {
	ref, ok := ctx.Value(ctxKey{}).(connRef)
	if !ok || ref.k == nil {
		return "", false, false
	}
	_, key, isContainer = ref.k.key(ctx, ref.id)
	return key, isContainer, true
}

// LegacyKeyFromContext is the legacy-form key (legacyFingerprint) of the
// container of the connection ctx carries, as KeyFromContext reads its key,
// without counting a use: what a mock recorded before secrets were left out
// of fingerprints names it by (SameKey). "" where there is none.
func LegacyKeyFromContext(ctx context.Context) string {
	ref, ok := ctx.Value(ctxKey{}).(connRef)
	if !ok || ref.k == nil {
		return ""
	}
	c, key, isContainer := ref.k.key(ctx, ref.id)
	if !isContainer || key == "" {
		return ""
	}
	lk, ok := legacyKeyOf(c)
	if !ok {
		return ""
	}
	return lk.String()
}

// SameKey reports whether a recorded key names the live container whose
// key is live and whose legacy-form key is legacy (LegacyKeyFromContext): a
// key of the current form is compared with live, one of the legacy form, a
// recording made before secrets were left out of fingerprints, with legacy.
// Each mock is compared in its own form, so a test set recorded across
// keploy versions (a recording refreshed with what a replay captured)
// compares each as it was written.
func SameKey(recorded, live, legacy string) bool {
	if recorded == "" {
		return false
	}
	if recorded == live {
		return true
	}
	k, err := Parse(recorded)
	return err == nil && isLegacyFingerprint(k.Fingerprint) && legacy != "" && recorded == legacy
}

// PlacedFromContext is KeyFromContext without the key: whether the connection
// ctx carries reaches a container (Placed), which never waits on a
// container's configuration.
func PlacedFromContext(ctx context.Context) (isContainer, keyed bool) {
	ref, ok := ctx.Value(ctxKey{}).(connRef)
	if !ok || ref.k == nil {
		return false, false
	}
	_, isContainer = ref.k.Placed(ctx, ref.id)
	return isContainer, true
}

// FromContext is the container key of the connection a parser serves, if its
// destination is a container the keyer gives a key (StateFromContext) and
// the proven switch is on.
func FromContext(ctx context.Context) (string, bool) {
	key, st := StateFromContext(ctx)
	return key, st == Keyed
}

// ParseAddrPort reads a connection's destination as keploy's proxy writes it:
// ip:port, IPv6 in brackets. An IPv4-mapped one is read as the IPv4 address
// it reaches, both as [::ffff:a.b.c.d]:p and as [a.b.c.d]:p, the form the
// proxy writes a dual-stack socket's connection to an IPv4 address in (an
// IPv6 socket to ::ffff:a.b.c.d, Java's default), which
// netip.ParseAddrPort refuses.
func ParseAddrPort(s string) (netip.AddrPort, error) {
	host, port, err := net.SplitHostPort(s)
	if err != nil {
		return netip.AddrPort{}, err
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return netip.AddrPort{}, err
	}
	p, err := strconv.ParseUint(port, 10, 16)
	if err != nil {
		return netip.AddrPort{}, err
	}
	return netip.AddrPortFrom(ip.Unmap(), uint16(p)), nil
}
