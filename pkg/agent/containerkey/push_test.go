package containerkey

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/docker/docker/api/types/events"
	"github.com/docker/go-connections/nat"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// A docker-mode agent names from the CLI's list exactly as the CLI's own
// index does: a container's IPs, and a published port reached at one of the
// host's addresses (the bridge gateway the app container sees). Its loopback
// is the app container's own, so a published port is not reached there, not
// even one bound to the host's 127.0.0.1.
func TestAPushedSnapshotNamesAsTheIndexDoes(t *testing.T) {
	f := testcontainers()
	r, _ := watching(t, f, "172.17.0.1", "10.0.0.5")
	s, _, ok := r.snapshot("")
	if !ok || s.Endpoint != "unix:///var/run/docker.sock" {
		t.Fatalf("snapshot = %+v, %v", s, ok)
	}
	p := newPushedIndex()
	p.apply(s)
	for _, dst := range []string{"172.17.0.2:5432", "[fd00::2]:5432", "172.17.0.3:4566", "172.17.0.1:49153", "10.0.0.5:49300"} {
		want, _ := lookup(t, r, dst)
		got, ok, err := p.Lookup(netip.MustParseAddrPort(dst))
		if err != nil || !ok || got != want {
			t.Errorf("pushed Lookup(%s) = %+v, %v, %v; want %+v", dst, got, ok, err, want)
		}
	}
	for _, dst := range []string{"127.0.0.1:49153", "[::1]:49153", "127.0.0.1:49200", "203.0.113.7:49153"} {
		if got, ok, _ := p.Lookup(netip.MustParseAddrPort(dst)); ok {
			t.Errorf("pushed Lookup(%s) = %+v; the app container's loopback is its own", dst, got)
		}
	}
	if p.Endpoint() != "unix:///var/run/docker.sock (read by keploy's CLI)" {
		t.Fatalf("Endpoint() = %q", p.Endpoint())
	}
}

// keploy's own agent container is left out of the list: its keys would
// change with every keploy release.
func TestASnapshotLeavesOutKeploysAgentContainer(t *testing.T) {
	f := testcontainers()
	agent := running("ghcr.io/keploy/keploy:v3.6.118", "sha256:k", []string{"172.17.0.50"}, nat.PortMap{})
	agent.ContainerJSONBase.Name = "/keploy-v3-abcd"
	f.set("agent", agent)
	r, _ := watching(t, f)
	s, _, _ := r.snapshot("keploy-v3-abcd")
	for _, c := range s.Containers {
		if c.ID == "agent" {
			t.Fatal("keploy's agent container is in the list")
		}
	}
	if len(s.Containers) != 3 {
		t.Fatalf("the list has %d containers, want the three others", len(s.Containers))
	}
	all, _, _ := r.snapshot("")
	if len(all.Containers) != 4 {
		t.Fatalf("without an agent container to leave out the list has %d containers", len(all.Containers))
	}
	for _, c := range all.Containers {
		if c.ID == "agent" && c.Name != "keploy-v3-abcd" {
			t.Fatalf("the list names the agent's container %q, want its name without Docker's slash", c.Name)
		}
	}
}

// A CLI that cannot read Docker says so in its list, and the agent then
// names by address with that error; a host with no daemon is errNoDaemon.
// An index with neither its list nor a failure has nothing to give.
func TestASnapshotCarriesTheCLIsFailure(t *testing.T) {
	f := newFakeDocker()
	f.listErr = errors.New("permission denied")
	r, _ := newIndex(f)
	r.Watch(t.Context(), nil)
	s, _, ok := r.snapshot("")
	if !ok || s.Err == "" || s.NoDaemon {
		t.Fatalf("snapshot of a failed index = %+v, %v", s, ok)
	}
	p := newPushedIndex()
	p.apply(s)
	if _, _, err := p.Lookup(netip.MustParseAddrPort("172.17.0.2:5432")); err == nil {
		t.Fatal("a failed CLI's list named destinations")
	}

	r2, _ := newIndex(newFakeDocker())
	r2.socketExists = func(string) bool { return false }
	r2.Watch(t.Context(), nil)
	s2, _, _ := r2.snapshot("")
	s2.Source = "another CLI" // its own source, so not an older list of the first
	p.apply(s2)
	if _, _, err := p.Lookup(netip.MustParseAddrPort("172.17.0.2:5432")); !errors.Is(err, errNoDaemon) {
		t.Fatalf("a no-daemon list = %v", err)
	}

	hung := newFakeDocker()
	hung.hang = true
	r3, _ := newIndex(hung)
	st3, api3, ep3 := r3.begin("", nil) // a watch whose list never comes
	r3.mu.Lock()
	r3.cur, r3.endpoint = st3, ep3
	r3.mu.Unlock()
	go r3.watch(st3.ctx, st3, api3)
	t.Cleanup(func() { stopIndex(r3) })
	if _, _, ok := r3.snapshot(""); ok {
		t.Fatal("an index without its list gave a list")
	}
}

// The agent never waits for a list; it keeps the newest list of a source,
// so one that arrives late never replaces a newer one, and takes any list
// of another source (a client of another process).
func TestAPushedIndexKeepsTheNewestList(t *testing.T) {
	p := newPushedIndex()
	p.Watch(t.Context(), nil) // returns at once
	if _, ok, err := p.Lookup(netip.MustParseAddrPort("172.17.0.2:5432")); ok || err != nil {
		t.Fatalf("before any list = %v, %v", ok, err)
	}
	list := func(source string, seq uint64, id string) Snapshot {
		return Snapshot{Source: source, Seq: seq, Containers: []SnapshotContainer{{ID: id, Image: "postgres:16", IPs: []string{"172.17.0.2"}}}}
	}
	p.apply(list("a", 2, "new"))
	p.apply(list("a", 1, "old"))
	if c, _, _ := p.Lookup(netip.MustParseAddrPort("172.17.0.2:5432")); c.ID != "new" {
		t.Fatalf("a late older list replaced a newer one: %+v", c)
	}
	p.apply(list("b", 1, "other"))
	if c, _, _ := p.Lookup(netip.MustParseAddrPort("172.17.0.2:5432")); c.ID != "other" {
		t.Fatalf("another source's list was not taken: %+v", c)
	}
}

func TestApplySnapshotOnlyOnAPushedAgent(t *testing.T) {
	saved := Default
	t.Cleanup(func() { Default = saved })
	Default = NewKeyer(newDockerIndex())
	if TakesPushed() {
		t.Fatal("an agent that reads Docker says it takes lists")
	}
	if err := ApplySnapshot(Snapshot{}); !errors.Is(err, ErrNotPushed) {
		t.Fatalf("an agent that reads Docker took a list: %v", err)
	}
	UsePushed()
	if !TakesPushed() {
		t.Fatal("a docker-mode agent says it takes no lists")
	}
	if err := ApplySnapshot(Snapshot{Containers: []SnapshotContainer{{ID: "r", Image: "redis:7", IPs: []string{"172.17.0.9"}}}}); err != nil {
		t.Fatal(err)
	}
	if got := name(t, Default, "1", "172.17.0.9:6379"); got != "container:redis:7:6379#1" {
		t.Fatalf("a pushed agent named %q", got)
	}
}

// posts records what a pusher gave, and tells sent of each post.
type posts struct {
	mu   sync.Mutex
	got  []Snapshot
	sent chan struct{}
	err  error
}

func newPosts() *posts { return &posts{sent: make(chan struct{}, 64)} }

func (p *posts) post(_ context.Context, s Snapshot) error {
	p.mu.Lock()
	p.got = append(p.got, s)
	err := p.err
	p.mu.Unlock()
	p.sent <- struct{}{}
	return err
}

func (p *posts) last() Snapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.got[len(p.got)-1]
}

func (p *posts) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.got)
}

// testPusher is a pusher over f.
func testPusher(f *fakeDocker, ps *posts) *Pusher {
	p := NewPusher(ps.post, "")
	p.idx.newAPI = func(string) (dockerAPI, error) { return f, nil }
	p.idx.socketExists = func(string) bool { return true }
	p.idx.hostAddrs = func() ([]netip.Addr, error) { return []netip.Addr{netip.MustParseAddr("172.17.0.1")}, nil }
	return p
}

// Push gives the agent the list before it returns, then every change; each
// list of a pusher is newer than the last, from one source.
func TestPushGivesTheListNowAndEveryChange(t *testing.T) {
	f := testcontainers()
	ps := newPosts()
	p := testPusher(f, ps)
	if err := p.Push(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	if ps.count() != 1 || len(ps.last().Containers) != 3 || ps.last().Err != "" || ps.last().Source == "" {
		t.Fatalf("Push gave %d lists, the last %+v", ps.count(), ps.last())
	}
	<-ps.sent
	first := ps.last()
	f.set("new", running("nginx", "sha256:n", []string{"172.17.0.9"}, nat.PortMap{}))
	f.msgs <- start("new")
	<-ps.sent
	if s := ps.last(); len(s.Containers) != 4 || s.Seq <= first.Seq || s.Source != first.Source {
		t.Fatalf("after a start = %+v", s)
	}
	f.remove("pg")
	f.msgs <- die("pg")
	<-ps.sent
	if len(ps.last().Containers) != 3 {
		t.Fatalf("after a die = %+v", ps.last())
	}
	if err := p.Push(t.Context(), nil); err != nil || ps.count() != 4 {
		t.Fatalf("a second Push in the session: %v, %d lists", err, ps.count())
	}
}

// A CLI that cannot read Docker gives that once, and asks the daemon again
// only at the next Push: never in a loop, and never twice for one failure.
func TestPushAsksADaemonItCannotReadOncePerPush(t *testing.T) {
	f := newFakeDocker()
	f.listErr = errors.New("Cannot connect to the Docker daemon")
	ps := newPosts()
	p := testPusher(f, ps)
	if err := p.Push(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	if ps.count() != 1 || ps.last().Err == "" {
		t.Fatalf("Push gave %d lists, the last %+v; want the failure once", ps.count(), ps.last())
	}
	if calls, _, _ := f.count(); calls != 1 {
		t.Fatalf("asked the daemon %d times", calls)
	}
	if err := p.Push(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	if calls, _, _ := f.count(); calls != 2 || ps.count() != 2 {
		t.Fatalf("after the next Push: %d asks, %d lists; want 2 and 2", calls, ps.count())
	}
}

// A session whose context ended leaves the next one a new change loop on a
// fresh reading of the daemon.
func TestPushFollowsTheSessions(t *testing.T) {
	f := newFakeDocker()
	ps := newPosts()
	p := testPusher(f, ps)
	first, end := context.WithCancel(t.Context())
	if err := p.Push(first, nil); err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	oldDone := p.loopDone
	p.mu.Unlock()
	end()
	<-oldDone
	f.mu.Lock()
	f.msgs = make(chan events.Message)
	f.mu.Unlock()
	calls, _, _ := f.count()
	if err := p.Push(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	if now, _, _ := f.count(); now != calls+1 {
		t.Fatalf("the next session read the daemon %d times, want afresh once", now-calls)
	}
	if s := ps.last(); s.Err != "" {
		t.Fatalf("the next session was given %+v", s)
	}
}

// tookTheChange waits until p's change loop has taken the change told: with
// p.mu held by the test, the loop then waits for it.
func tookTheChange(p *Pusher) {
	for len(p.changed) != 0 {
		time.Sleep(time.Millisecond)
	}
}

// A change the agent was given by a session start's push is not given again
// by the change loop, which took it before that push.
func TestAChangeTheAgentWasGivenIsNotGivenAgain(t *testing.T) {
	f := testcontainers()
	ps := newPosts()
	p := testPusher(f, ps)
	applied := make(chan string, 64)
	p.idx.applied = func(id string) { applied <- id }
	if err := p.Push(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	f.set("new", running("nginx", "sha256:n", []string{"172.17.0.9"}, nat.PortMap{}))
	f.msgs <- start("new")
	<-applied
	tookTheChange(p)                 // the loop holds the change, and waits for p.mu
	err := p.send(t.Context(), true) // a session start's push, under p.mu as Push sends
	p.changed <- struct{}{}          // the loop takes this once done with the change it holds
	p.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	tookTheChange(p)
	if n := ps.count(); n != 2 {
		t.Fatalf("%d lists; want 2, the first push and the forced one: the loop gave the change again", n)
	}
}

// An ended session's change loop gives the agent nothing, even a change it
// took before the session ended.
func TestAnEndedSessionsLoopGivesNothing(t *testing.T) {
	f := testcontainers()
	ps := newPosts()
	p := testPusher(f, ps)
	applied := make(chan string, 64)
	p.idx.applied = func(id string) { applied <- id }
	first, end := context.WithCancel(t.Context())
	if err := p.Push(first, nil); err != nil {
		t.Fatal(err)
	}
	<-ps.sent
	p.mu.Lock()
	done := p.loopDone
	f.set("new", running("nginx", "sha256:n", []string{"172.17.0.9"}, nat.PortMap{}))
	f.msgs <- start("new")
	<-applied
	tookTheChange(p)
	end()
	p.mu.Unlock()
	<-done
	if n := ps.count(); n != 1 {
		t.Fatalf("the ended session's loop gave the agent %d lists", n-1)
	}
}

// A watch bound to a session that ended is not used by the next session,
// even before it notices its end: it is about to fail, and that failure
// would be the next session's list.
func TestAWatchOfAnEndedSessionIsNotUsed(t *testing.T) {
	f := newFakeDocker()
	r, _ := newIndex(f)
	ended, end := context.WithCancel(t.Context())
	st, api, endpoint := r.begin("", ended) // never run: it never notices its end
	_ = api.Close()
	st.listed = true
	st.once.Do(func() { close(st.synced) })
	close(st.done)
	r.mu.Lock()
	r.cur, r.endpoint = st, endpoint
	r.mu.Unlock()
	if !live(st) {
		t.Fatal("a listed watch of a session still on is not used")
	}
	end()
	r.Watch(t.Context(), nil)
	t.Cleanup(func() { stopIndex(r) })
	if current(r) == st {
		t.Fatal("the next session used the watch of an ended one")
	}
}

// The CLI's pusher reads the daemon afresh for each session: it says at Info
// only the first time that it waits for the list, at Debug after.
func TestThePusherSaysItWaitsOnce(t *testing.T) {
	f := newFakeDocker()
	ps := newPosts()
	p := testPusher(f, ps)
	waits := make(chan zapcore.Level, 4)
	core, _ := observer.New(zap.DebugLevel)
	logger := zap.New(zapcore.RegisterHooks(core, func(e zapcore.Entry) error {
		if strings.Contains(e.Message, "waiting for it to list them") {
			waits <- e.Level
		}
		return nil
	}))
	var levels []zapcore.Level
	for range 2 {
		gate := make(chan struct{})
		f.mu.Lock()
		f.listGate = gate
		f.msgs = make(chan events.Message)
		f.mu.Unlock()
		session, end := context.WithCancel(t.Context())
		pushed := make(chan error, 1)
		go func() { pushed <- p.Push(session, logger) }()
		levels = append(levels, <-waits)
		close(gate)
		if err := <-pushed; err != nil {
			t.Fatal(err)
		}
		end()
	}
	if levels[0] != zapcore.InfoLevel || levels[1] != zapcore.DebugLevel {
		t.Fatalf("the waits were said at %v, want Info then Debug", levels)
	}
}

// clientEndpoint is DOCKER_HOST, else the docker CLI's current context.
func TestClientEndpoint(t *testing.T) {
	saved := contextEndpoint
	t.Cleanup(func() { contextEndpoint = saved })
	contextEndpoint = func() string { return "unix:///ctx.sock" }
	t.Setenv("DOCKER_HOST", "unix:///env.sock")
	if got := clientEndpoint(); got != "unix:///env.sock" {
		t.Fatalf("with DOCKER_HOST = %q", got)
	}
	t.Setenv("DOCKER_HOST", "")
	if got := clientEndpoint(); got != "unix:///ctx.sock" {
		t.Fatalf("from the context = %q", got)
	}
}
