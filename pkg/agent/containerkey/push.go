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
}

// SnapshotContainer is one running container of a Snapshot.
type SnapshotContainer struct {
	ID    string `json:"id"`
	Image string `json:"image"`
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
	at := map[string]int{}
	for id, image := range st.images {
		if exclude != "" && st.names[id] == "/"+exclude {
			continue
		}
		c := SnapshotContainer{ID: id, Image: image, Name: strings.TrimPrefix(st.names[id], "/")}
		for _, a := range st.ownIPs[id] {
			c.IPs = append(c.IPs, a.String())
		}
		at[id] = len(s.Containers)
		s.Containers = append(s.Containers, c)
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
	// source and seq are the list held.
	source string
	seq    uint64
	// endpoint is the daemon the CLI reads, for messages.
	endpoint string
}

func newPushedIndex() *pushedIndex { return &pushedIndex{} }

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
}

// Lookup reads the list held; before the first, nothing is a container.
func (p *pushedIndex) Lookup(dst netip.AddrPort) (Container, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.st == nil {
		return Container{}, false, nil
	}
	if p.st.err != nil {
		return Container{}, false, p.st.err
	}
	c, ok := p.st.find(dst)
	return c, ok, nil
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
	idx     *dockerIndex
	post    func(context.Context, Snapshot) error
	changed chan struct{}
	// exclude is keploy's own agent container's name.
	exclude string
	source  string

	mu sync.Mutex
	// alive is the context of the session the index is read for and of its
	// change loop; loopDone closes when that loop has ended (one loop at a
	// time: a new session's starts after the last one's ended). sentGen is
	// the count of changes the agent was last given. logger is the last
	// Push's.
	alive    context.Context
	loopDone chan struct{}
	sentGen  uint64
	logger   *zap.Logger
}

// NewPusher is a pusher that gives each list with post, leaving out the
// container named agentContainer.
func NewPusher(post func(context.Context, Snapshot) error, agentContainer string) *Pusher {
	idx := newDockerIndex()
	idx.host = clientEndpoint()
	idx.waitOnce = true
	changed := make(chan struct{}, 1)
	idx.changed = changed
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return &Pusher{idx: idx, post: post, changed: changed, exclude: agentContainer, source: hex.EncodeToString(b)}
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
	if p.alive != nil && p.alive.Err() != nil && p.loopDone != nil {
		// The last session's change loop ends with its context, which has
		// ended: wait for it outside p.mu, which it may be waiting for, so
		// the new session's loop is the only one.
		done := p.loopDone
		p.mu.Unlock()
		<-done
		p.mu.Lock()
	}
	p.logger = logger
	fresh := p.alive == nil || p.alive.Err() != nil
	if fresh {
		p.alive = ctx
		p.idx.Bind(ctx)
	}
	p.idx.Watch(ctx, logger)
	err := p.send(ctx, true)
	if fresh {
		p.loopDone = make(chan struct{})
		go p.loop(ctx, logger, p.loopDone)
	}
	return err
}

// send gives the agent the list as it is now: always when forced, else only
// when it changed since the agent was last given it. p.mu is held.
func (p *Pusher) send(ctx context.Context, force bool) error {
	s, gen, ok := p.idx.snapshot(p.exclude)
	if !ok {
		return ctx.Err()
	}
	if !force && gen == p.sentGen {
		return nil
	}
	s.Source = p.source
	if err := p.post(ctx, s); err != nil {
		return err
	}
	p.sentGen = gen
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

// loop gives the agent each change while its session lasts. An ended
// session's loop never sends: the next session's Push gives the agent the
// whole list before that session's loop starts.
func (p *Pusher) loop(ctx context.Context, logger *zap.Logger, done chan struct{}) {
	defer close(done)
	for {
		select {
		case <-p.changed:
			p.mu.Lock()
			if ctx.Err() != nil {
				p.mu.Unlock()
				return
			}
			err := p.send(ctx, false)
			p.mu.Unlock()
			if err != nil && ctx.Err() == nil && logger != nil {
				// The next change, the next session start or the agent's
				// restart gives it again.
				logger.Warn("keploy could not give its agent the list of the Docker daemon's containers: until it does, the agent names container destinations from the last list it was given",
					zap.Error(err), zap.String("next_step", "check that keploy's agent is up; the next change, session start or agent restart gives it the list again"))
			}
		case <-ctx.Done():
			return
		}
	}
}

// clientEndpoint is the Docker endpoint the keploy CLI's own commands use:
// DOCKER_HOST, else the docker CLI's current context.
func clientEndpoint() string {
	if host := os.Getenv("DOCKER_HOST"); host != "" {
		return host
	}
	return contextEndpoint()
}
