package containerkey

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
)

// A docker-mode agent runs in keploy's container, which has no Docker socket
// (handing it one would hand it the host). The keploy CLI, which drives
// Docker for the run, keeps the index instead (Pusher) and gives it to the
// agent whole (Snapshot): before each session starts, when the agent
// restarts, and on every change. The agent names connections from the last
// list it was given (pushedIndex) and never waits for one: a list that has
// not come yet names nothing, which only costs keys. Nothing is refreshed on
// a timer.

// Snapshot is the index as the keploy CLI gives it to its docker-mode agent.
type Snapshot struct {
	// Source names the pusher and Seq orders its lists: an agent keeps the
	// newest list of a source, so a list that arrives late never replaces a
	// newer one.
	Source string `json:"source"`
	Seq    uint64 `json:"seq"`
	// Err says the CLI cannot read Docker; NoDaemon, that the host has no
	// Docker daemon socket at all.
	Err      string `json:"err,omitempty"`
	NoDaemon bool   `json:"noDaemon,omitempty"`
	// Endpoint is the daemon the CLI reads, for messages.
	Endpoint string `json:"endpoint,omitempty"`
	// Local is the host's addresses, the Docker bridges' gateways among
	// them: the app container reaches a port published on all addresses at
	// one of those. (Where Docker runs in a VM or rootless, the CLI's
	// addresses are not the containers' host: such ports are named by
	// address, which misses, never matches wrongly.)
	Local      []string            `json:"local,omitempty"`
	Containers []SnapshotContainer `json:"containers"`
	// Placing is the containers the CLI is placing: ones an event started
	// or moved, not yet in Containers where they now are. A connection the
	// agent accepts meanwhile waits for them, as the CLI's index would.
	Placing []string `json:"placing,omitempty"`
}

// SnapshotContainer is one running container of a Snapshot.
type SnapshotContainer struct {
	ID    string `json:"id"`
	Image string `json:"image"`
	// Fingerprint is the container's Fingerprint, once the CLI's read of its
	// configuration is in; Reading says that read is in flight, and a later
	// list will carry it.
	Fingerprint string `json:"fingerprint,omitempty"`
	Reading     bool   `json:"reading,omitempty"`
	// Name is the container's name, for messages.
	Name  string         `json:"name,omitempty"`
	IPs   []string       `json:"ips,omitempty"`
	Ports []SnapshotPort `json:"ports,omitempty"`
}

// SnapshotPort is a TCP port a container publishes on the host.
type SnapshotPort struct {
	HostIP   string `json:"hostIP"`
	HostPort uint16 `json:"hostPort"`
	Port     uint16 `json:"port"`
}

// snapshot is r's current index as a Snapshot, leaving out the container
// named exclude (keploy's own agent container, whose keys would change with
// every keploy release), and whether it has one to give (its first list is
// in, or it failed), with the count of the changes it covers. It takes in
// every change so far: what it gives covers them, and only a later change
// asks for another.
func (r *dockerIndex) snapshot(exclude string) (Snapshot, uint64, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.changed != nil {
		select {
		case <-r.changed:
		default:
		}
	}
	st := r.cur
	if st == nil {
		return Snapshot{}, 0, false
	}
	r.snapSeq++
	s := Snapshot{Seq: r.snapSeq, Endpoint: r.endpoint, Containers: []SnapshotContainer{}}
	if st.err != nil {
		s.Err, s.NoDaemon = st.err.Error(), errors.Is(st.err, errNoDaemon)
		return s, r.changeGen, true
	}
	if !st.listed {
		return Snapshot{}, 0, false
	}
	for a := range st.local {
		s.Local = append(s.Local, a.String())
	}
	s.Placing = st.placingIDs()
	at := map[string]int{}
	for id, image := range st.images {
		if exclude != "" && st.names[id] == "/"+exclude {
			continue
		}
		at[id] = len(s.Containers)
		s.Containers = append(s.Containers, SnapshotContainer{ID: id, Image: image, Name: strings.TrimPrefix(st.names[id], "/"),
			Fingerprint: st.fingerprints[id], Reading: st.fingerprints[id] == "" && st.reading[id]})
	}
	// Each IP goes to the container the index names at it (byIP): a
	// container that took an IP whose old holder's die never arrived holds
	// it alone, though the old one's own list still has it.
	for a, id := range st.byIP {
		if i, ok := at[id]; ok {
			s.Containers[i].IPs = append(s.Containers[i].IPs, a.String())
		}
	}
	for hostPort, bs := range st.published {
		for _, b := range bs {
			if i, ok := at[b.id]; ok {
				s.Containers[i].Ports = append(s.Containers[i].Ports, SnapshotPort{HostIP: b.ip.String(), HostPort: hostPort, Port: b.port})
			}
		}
	}
	return s, r.changeGen, true
}

// pushedIndex is a docker-mode agent's index: the newest Snapshot its keploy
// CLI gave it.
type pushedIndex struct {
	mu sync.Mutex
	st *watchState
	// applied closes, and is replaced, whenever a list is taken: what waits
	// for a container's fingerprint looks again then.
	applied chan struct{}
	// source and seq are the list held.
	source string
	seq    uint64
	// endpoint is the daemon the CLI reads, for messages.
	endpoint string
}

func newPushedIndex() *pushedIndex { return &pushedIndex{applied: make(chan struct{})} }

// apply takes a snapshot in place of the one held, unless it is an older
// list of the same source.
func (p *pushedIndex) apply(s Snapshot) {
	st := newWatchState()
	st.loopbackIsHost = false // the app container's loopback is its own
	st.listed = true
	switch {
	case s.NoDaemon:
		st.err = fmt.Errorf("%w (keploy read %s)", errNoDaemon, s.Endpoint)
	case s.Err != "":
		st.err = errors.New(s.Err)
	}
	for _, id := range s.Placing {
		st.placing[id] = true
	}
	st.local = map[netip.Addr]bool{}
	for _, a := range s.Local {
		if ip, err := netip.ParseAddr(a); err == nil {
			st.local[ip.Unmap()] = true
		}
	}
	for _, c := range s.Containers {
		e := entry{id: c.ID, image: c.Image}
		for _, a := range c.IPs {
			e.addIP(a)
		}
		for _, port := range c.Ports {
			e.publish(port.HostIP, port.HostPort, port.Port)
		}
		st.images[e.id] = e.image
		if c.Fingerprint != "" {
			st.fingerprints[e.id] = c.Fingerprint
		} else if c.Reading {
			st.reading[e.id] = true
		}
		for _, a := range e.ips {
			st.byIP[a] = e.id
		}
		for i, b := range e.bindings {
			st.published[e.hostPort[i]] = append(st.published[e.hostPort[i]], b)
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.st != nil && s.Source == p.source && s.Seq <= p.seq {
		return // an older list that arrived late
	}
	p.st, p.source, p.seq, p.endpoint = st, s.Source, s.Seq, s.Endpoint
	close(p.applied)
	p.applied = make(chan struct{})
}

// errNoList is a docker-mode agent's lookup before its CLI gave it a list:
// nothing is a container then, and the session says so once (Keyer).
var errNoList = errors.New("keploy's CLI has not given this agent the list of the Docker daemon's containers")

// Lookup reads the list held; before the first, nothing is a container, and
// it says why.
func (p *pushedIndex) Lookup(dst netip.AddrPort) (Container, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.st == nil {
		return Container{}, false, errNoList
	}
	if p.st.err != nil {
		return Container{}, false, p.st.err
	}
	c, ok := p.st.find(dst)
	return c, ok, nil
}

// Fingerprint is container id's fingerprint in the list held, waiting, while
// the CLI is reading it, for a list that carries it.
func (p *pushedIndex) Fingerprint(ctx context.Context, id string) (string, bool) {
	for {
		p.mu.Lock()
		st, applied := p.st, p.applied
		var fp string
		held, reading := false, false
		if st != nil && st.err == nil {
			fp, held, reading = st.fingerprints[id], st.images[id] != "", st.reading[id]
		}
		p.mu.Unlock()
		switch {
		case fp != "" && held:
			return fp, true
		case !reading:
			return "", false
		}
		select {
		case <-applied:
		case <-ctx.Done():
			return "", false
		}
	}
}

// Pending says whether a container could be at dst by the list held (none
// at the app container's own loopback), and which containers the CLI is
// placing by that list. It never waits.
func (p *pushedIndex) Pending(dst netip.AddrPort) ([]string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.st == nil || p.st.err != nil {
		return nil, false
	}
	return p.st.placingIDs(), p.st.couldBeAt(dst.Addr())
}

// AwaitReadsOf waits until the list held shows none of the containers ids
// still being placed by the CLI, or ctx ends.
func (p *pushedIndex) AwaitReadsOf(ctx context.Context, ids []string) {
	for {
		p.mu.Lock()
		st, applied := p.st, p.applied
		reading := st != nil && st.err == nil && st.placingAny(ids)
		p.mu.Unlock()
		if !reading {
			return
		}
		select {
		case <-applied:
		case <-ctx.Done():
			return
		}
	}
}

// Bind does nothing: a docker-mode agent reads no daemon.
func (p *pushedIndex) Bind(context.Context) {}

// Watch never waits: the CLI gives the agent its list before it asks it to
// start a session.
func (p *pushedIndex) Watch(context.Context, *zap.Logger) {}

// Endpoint names the daemon the CLI reads, for messages.
func (p *pushedIndex) Endpoint() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.endpoint == "" {
		return "the one keploy's CLI reads"
	}
	return p.endpoint + " (read by keploy's CLI)"
}

// UsePushed makes the agent name container destinations from the lists its
// keploy CLI gives it (ApplySnapshot): a docker-mode agent, which cannot read
// Docker, calls it at its start, under the proven switch.
func UsePushed() {
	Default.mu.Lock()
	defer Default.mu.Unlock()
	Default.res = newPushedIndex()
}

// TakesPushed reports whether this agent names container destinations from
// lists its CLI gives it.
func TakesPushed() bool {
	Default.mu.Lock()
	defer Default.mu.Unlock()
	_, ok := Default.res.(*pushedIndex)
	return ok
}

// ErrNotPushed is ApplySnapshot's error for an agent that reads Docker
// itself.
var ErrNotPushed = errors.New("this agent reads Docker itself and takes no container list")

// ApplySnapshot gives the agent a list from its CLI.
func ApplySnapshot(s Snapshot) error {
	Default.mu.Lock()
	p, ok := Default.res.(*pushedIndex)
	Default.mu.Unlock()
	if !ok {
		return ErrNotPushed
	}
	p.apply(s)
	return nil
}

// Pusher keeps the index on the keploy CLI's side for a docker-mode agent and
// gives it to the agent: when asked (Push, before each session starts and
// when the agent restarts), and on every change while the session lasts.
type Pusher struct {
	idx  *dockerIndex
	post func(context.Context, Snapshot) error
	// exclude is keploy's own agent container's name.
	exclude string
	source  string

	mu sync.Mutex
	// alive is the context of the session the index is read for and of its
	// change loop; changed is that session's change signal, which the index
	// tells and only that session's loop reads, so a loop whose session
	// ended never takes a change meant for the next one. loopDone closes
	// when the loop has ended. sentGen is the count of changes the agent was
	// last given; owed says the agent did not take the last list it was
	// given, which is then given again whether or not anything changed.
	// logger is the last Push's.
	alive    context.Context
	changed  chan struct{}
	loopDone chan struct{}
	sentGen  uint64
	owed     bool
	logger   *zap.Logger
}

// NewPusher is a pusher that gives each list with post, leaving out the
// container named agentContainer.
func NewPusher(post func(context.Context, Snapshot) error, agentContainer string) *Pusher {
	idx := newDockerIndex()
	idx.host = clientEndpoint()
	idx.waitOnce = true
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return &Pusher{idx: idx, post: post, exclude: agentContainer, source: hex.EncodeToString(b)}
}

// Push gives the agent the current list now, whether or not it changed (a
// restarted agent holds none), and returns whether the agent took it. It
// reads the daemon first, for a session whose context is ctx (afresh when the
// last reading failed or its session ended: a watch bound to an ended session
// is never used), and from then on gives the agent every change while ctx
// lasts.
func (p *Pusher) Push(ctx context.Context, logger *zap.Logger) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.logger = logger
	fresh := p.alive == nil || p.alive.Err() != nil
	if fresh {
		// A new session: a change signal of its own, which the index tells
		// from now on. The last session's loop, if it still runs, reads
		// only its own, and ends with its session.
		p.alive, p.changed = ctx, make(chan struct{}, 1)
		p.idx.setChanged(p.changed)
		p.idx.Bind(ctx)
	}
	p.idx.Watch(ctx, logger)
	err := p.send(ctx, true)
	if err != nil {
		// The agent did not take it: the loop gives it again, paced, until
		// it does (owed), though nothing changes meanwhile.
		select {
		case p.changed <- struct{}{}:
		default:
		}
	}
	if fresh {
		p.loopDone = make(chan struct{})
		go p.loop(ctx, p.changed, logger, p.loopDone)
	}
	return err
}

// send gives the agent the list as it is now: always when forced or owed
// (the agent did not take the last one), else only when it changed since the
// agent was last given it. p.mu is held.
func (p *Pusher) send(ctx context.Context, force bool) error {
	s, gen, ok := p.idx.snapshot(p.exclude)
	if !ok {
		return ctx.Err()
	}
	if !force && !p.owed && gen == p.sentGen {
		return nil
	}
	s.Source = p.source
	if err := p.post(ctx, s); err != nil {
		p.owed = true
		return err
	}
	p.sentGen, p.owed = gen, false
	if p.logger != nil {
		names := make([]string, 0, len(s.Containers))
		for _, c := range s.Containers {
			names = append(names, c.Name)
		}
		p.logger.Debug("gave keploy's agent the list of the Docker daemon's containers",
			zap.Uint64("seq", s.Seq), zap.Strings("containers", names), zap.String("err", s.Err))
	}
	return nil
}

// loop gives the agent each change while its session (ctx) lasts, as told on
// that session's change signal. An ended session's loop never sends: the
// next session's Push gives the agent the whole list, and its own loop each
// change after.
func (p *Pusher) loop(ctx context.Context, changed <-chan struct{}, logger *zap.Logger, done chan struct{}) {
	defer close(done)
	// retry is set while the agent has not taken the list as it is: a list
	// that failed is given again, paced, until the agent takes one, since
	// the agent may be waiting on it for a container's configuration (a
	// lost list would otherwise hold its parsers until the session ends).
	var retry <-chan time.Time
	backoff := pushRetryFirst
	warned := false
	for {
		select {
		case <-changed:
		case <-retry:
		case <-ctx.Done():
			return
		}
		p.mu.Lock()
		if ctx.Err() != nil {
			p.mu.Unlock()
			return
		}
		err := p.send(ctx, false)
		p.mu.Unlock()
		if err == nil || ctx.Err() != nil {
			retry, backoff, warned = nil, pushRetryFirst, false
			continue
		}
		if !warned && logger != nil {
			warned = true
			logger.Warn("keploy could not give its agent the list of the Docker daemon's containers: it gives it again until the agent takes it, and meanwhile the agent names container destinations from the last list it was given",
				zap.Error(err), zap.String("next_step", "check that keploy's agent is up"))
		}
		retry = time.After(backoff)
		if backoff < pushRetryMax {
			backoff *= 2
		}
	}
}

// pushRetryFirst and pushRetryMax pace giving a list again after the agent
// did not take it: they bound how often keploy asks, never for how long.
const (
	pushRetryFirst = 100 * time.Millisecond
	pushRetryMax   = 5 * time.Second
)

// clientEndpoint is the Docker endpoint the keploy CLI's own commands use:
// DOCKER_HOST, else the docker CLI's current context.
func clientEndpoint() string {
	if host := os.Getenv("DOCKER_HOST"); host != "" {
		return host
	}
	return contextEndpoint()
}
