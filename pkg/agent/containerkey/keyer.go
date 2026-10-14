package containerkey

import (
	"context"
	"errors"
	"maps"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"sync/atomic"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// Default is the agent's keyer: the proxy names each connection with it, the
// scope routes open and close its test scopes, and the sync manager stamps
// recorded mocks with it.
var Default = NewKeyer(newDockerIndex())

// resolver finds the container behind a destination from its index, without
// waiting on anything. err says the daemon cannot be read.
type resolver interface {
	Lookup(dst netip.AddrPort) (c Container, ok bool, err error)
	// Bind makes ctx, the agent's own, the bound of the index's reading of
	// Docker, so it stops with the agent.
	Bind(ctx context.Context)
	// Watch starts keeping the index, if it is not kept already, and returns
	// once it holds what was running (or ctx ends).
	Watch(ctx context.Context, logger *zap.Logger)
	// Endpoint names the Docker endpoint read, for messages.
	Endpoint() string
}

// Keyer numbers container destinations and holds each open container
// connection's key.
//
// A key's number counts the image's containers by their first connection
// within the scope (design §3.1): the open test, or, outside tests, the
// instance, which is the test process that claimed the run (one at a time in
// this release). Each instance starts from the numbers given before the run's
// first claim, so a test process numbers the same whether the processes
// before it ran or not. A test scope starts from the numbers its instance
// gave before the test began, so a container the suite set up keeps its
// number inside every test, and one the test starts itself is numbered after
// it: the same in a run of that one test as in a run of all of them.
//
// A connection is named once, when the proxy accepts it, from the index, in
// the scope open then; the key holds for the connection's life and goes when
// it closes (Forget). Nothing waits: a connection to a container the index
// has not read yet (one Docker started a moment before) is named by its
// address, as without the switch.
type Keyer struct {
	mu sync.Mutex
	// res is nil where keploy cannot read Docker at all (Disable).
	res resolver
	// run numbers the containers outside tests: the instance's, or the
	// run's before its first claim; test, while a test is open, numbers that
	// test's. unclaimed is the run's numbers at its first claim, which each
	// instance starts from; nil before it.
	run, test, unclaimed *numbering
	// conns holds each open container connection's key, by the connection ID
	// its mocks carry.
	conns map[string]string
	// warned: this session already said Docker cannot be read.
	warned bool
}

// NewKeyer is a keyer that asks res which destinations are containers.
func NewKeyer(res resolver) *Keyer {
	return &Keyer{res: res, run: newNumbering(), conns: map[string]string{}}
}

// Disable stops k reading Docker: every destination is named by its address,
// as without the switch. keploy's agent container has no Docker socket, so a
// docker-mode agent disables its keyer at start.
func (k *Keyer) Disable() {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.res = nil
}

// Watch starts reading Docker for a session, if k can, and returns once the
// index holds what was already running, or ctx (the session's) ends: called
// when a session starts under the proven switch, before the app runs. Only a
// session start does: a daemon that cannot be read stays unread for the rest
// of the session.
func (k *Keyer) Watch(ctx context.Context, logger *zap.Logger) {
	k.mu.Lock()
	res := k.res
	k.mu.Unlock()
	if res != nil {
		res.Watch(ctx, logger)
	}
}

// Bind makes ctx, the agent's own, the bound of k's reading of Docker, so it
// stops with the agent: called when the agent starts, under the proven
// switch.
func (k *Keyer) Bind(ctx context.Context) {
	k.mu.Lock()
	res := k.res
	k.mu.Unlock()
	if res != nil {
		res.Bind(ctx)
	}
}

// Reset starts a new session (a record or a replay of one test set): every
// number goes, and so does every connection the last session left open.
func (k *Keyer) Reset() {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.run, k.test, k.unclaimed, k.conns, k.warned = newNumbering(), nil, nil, map[string]string{}, false
}

// BeginInstance starts a test process's numbering outside its tests, when it
// claims the run: from the numbers the run gave before its first claim, not
// from what the processes before it numbered.
func (k *Keyer) BeginInstance() {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.unclaimed == nil {
		k.unclaimed = k.run.clone()
	}
	k.run, k.test = k.unclaimed.clone(), nil
}

// BeginTest opens a test's scope; a scope already open is replaced, since a
// run has one open test at a time.
func (k *Keyer) BeginTest() {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.test = k.run.clone()
}

// EndTest closes the open test's scope: connections from here on are the
// run's again.
func (k *Keyer) EndTest() {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.test = nil
}

// Name names the connection connID, accepted for the address the proxy dials
// (ip:port), in the scope open now, when the index knows its destination as
// a container, and returns the container and key. It never waits, and logs
// only after letting go of k.
//
// When Docker cannot be read, the connection is named by its address and the
// session says so once: its container destinations are then compared by
// address, so one on a random port misses in replay instead of matching
// another container. A host with no Docker daemon socket has no containers
// to name: the agent says so once, at Info, and at Debug after.
func (k *Keyer) Name(logger *zap.Logger, connID, dst string) (Container, string, bool) {
	ap, err := parseAddrPort(dst)
	if err != nil {
		return Container{}, "", false
	}
	got, key, ok, warn := k.name(connID, ap)
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
		logger.Debug("named a container destination", zap.String("destination", ap.String()), zap.String("containerKey", key))
	}
	return got, key, ok
}

// name is Name under k.mu; warn is the error to tell, once per session.
func (k *Keyer) name(connID string, ap netip.AddrPort) (got Container, key string, ok bool, warn error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.res == nil {
		return Container{}, "", false, nil
	}
	got, ok, err := k.res.Lookup(ap)
	if err != nil {
		if !k.warned && !errors.Is(err, context.Canceled) {
			k.warned = true
			warn = err
		}
		return Container{}, "", false, warn
	}
	if !ok {
		return Container{}, "", false, nil
	}
	s := k.run
	if k.test != nil {
		s = k.test
	}
	key = Key{Image: got.Image, Port: got.Port, N: s.number(got)}.String()
	if connID != "" {
		k.conns[connID] = key
	}
	return got, key, true, nil
}

// endpoint names the Docker endpoint k reads, for messages.
func (k *Keyer) endpoint() string {
	k.mu.Lock()
	res := k.res
	k.mu.Unlock()
	if res == nil {
		return ""
	}
	return res.Endpoint()
}

// Forget drops a connection that closed.
func (k *Keyer) Forget(connID string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	delete(k.conns, connID)
}

// key is the key of the open connection connID, "" for one that is not a
// container.
func (k *Keyer) key(connID string) string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.conns[connID]
}

// Stamp writes the key of the connection a recorded mock came from into its
// metadata (MetadataKey). The sync manager stamps each mock as a parser hands
// it over, while its connection is open. A mock of any other connection is
// left as it is.
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
	key := k.key(id)
	if key == "" {
		return
	}
	// A copy: a parser may hand several mocks one metadata map, and the
	// outgoing stream may be encoding an earlier one while this one is
	// stamped.
	md := maps.Clone(m.Spec.Metadata)
	if md == nil {
		md = map[string]string{}
	}
	md[MetadataKey] = key
	m.Spec.Metadata = md
}

// numbering is one scope's numbers: per image, each container's number, and
// how many the image has.
type numbering struct {
	byImage map[string]*imageNumbers
}

type imageNumbers struct {
	of    map[string]int // container ID -> its number
	count int
}

func newNumbering() *numbering { return &numbering{byImage: map[string]*imageNumbers{}} }

// number is c's number in this scope, giving it the image's next one on its
// first connection here.
func (s *numbering) number(c Container) int {
	im := s.byImage[c.Image]
	if im == nil {
		im = &imageNumbers{of: map[string]int{}}
		s.byImage[c.Image] = im
	}
	if n, ok := im.of[c.ID]; ok {
		return n
	}
	im.count++
	im.of[c.ID] = im.count
	return im.count
}

func (s *numbering) clone() *numbering {
	out := newNumbering()
	for image, im := range s.byImage {
		of := make(map[string]int, len(im.of))
		for id, n := range im.of {
			of[id] = n
		}
		out.byImage[image] = &imageNumbers{of: of, count: im.count}
	}
	return out
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

// FromContext is the container key of the connection a parser serves, if its
// destination is a container and the proven switch is on.
func FromContext(ctx context.Context) (string, bool) {
	ref, ok := ctx.Value(ctxKey{}).(connRef)
	if !ok || ref.k == nil {
		return "", false
	}
	key := ref.k.key(ref.id)
	return key, key != ""
}

// parseAddrPort reads ip:port, IPv6 in brackets, an IPv4-mapped IPv6 address
// read as IPv4.
func parseAddrPort(s string) (netip.AddrPort, error) {
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
