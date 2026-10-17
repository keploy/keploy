package containerkey

import (
	"context"
	"errors"
	"net/netip"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/docker/docker/api/types/events"
	"github.com/docker/go-connections/nat"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// A docker-mode agent names from the CLI's list exactly as the CLI's own
// index does: a container's IPs, and a published port reached at one of the
// host's addresses (the bridge gateway the app container sees). Its loopback
// is the app container's own, so a published port is not reached there, not
// even one bound to the host's 127.0.0.1, and not at an unspecified address,
// though the host's own addresses, as the CLI reads them, include loopback.
func TestAPushedSnapshotNamesAsTheIndexDoes(t *testing.T) {
	f := testcontainers()
	r, _ := watching(t, f, "127.0.0.1", "::1", "172.17.0.1", "10.0.0.5")
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
	for _, dst := range []string{"127.0.0.1:49153", "[::1]:49153", "127.0.0.1:49200", "0.0.0.0:49153", "[::]:49153", "203.0.113.7:49153"} {
		if got, ok, _ := p.Lookup(netip.MustParseAddrPort(dst)); ok {
			t.Errorf("pushed Lookup(%s) = %+v; the app container's loopback is its own", dst, got)
		}
	}
	if p.Endpoint() != "unix:///var/run/docker.sock (read by keploy's CLI)" {
		t.Fatalf("Endpoint() = %q", p.Endpoint())
	}
}

// A container that took an IP whose old holder's die never arrived holds it
// alone in the list, as it does in the CLI's index: the old container's own
// record of its IPs still has it, and the agent must not be given both.
func TestASnapshotGivesAReusedIPToItsNewContainer(t *testing.T) {
	f := newFakeDocker()
	f.set("pg", running("postgres:16", "sha256:p", []string{"172.18.0.2"}, nat.PortMap{}))
	r, applied := watching(t, f)
	f.set("redis", running("redis:7", "sha256:r", []string{"172.18.0.2"}, nat.PortMap{}))
	f.msgs <- start("redis")
	<-applied
	s, _, ok := r.snapshot("")
	if !ok {
		t.Fatal("no snapshot")
	}
	for _, c := range s.Containers {
		switch {
		case c.ID == "pg" && len(c.IPs) != 0:
			t.Fatalf("the list gives the dead holder %v", c.IPs)
		case c.ID == "redis" && (len(c.IPs) != 1 || c.IPs[0] != "172.18.0.2"):
			t.Fatalf("the list gives the new holder %v", c.IPs)
		}
	}
	p := newPushedIndex()
	p.apply(s)
	if got, ok, err := p.Lookup(netip.MustParseAddrPort("172.18.0.2:6379")); err != nil || !ok || got.ID != "redis" {
		t.Fatalf("pushed Lookup = %+v, %v, %v; want redis", got, ok, err)
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
	// Before any list nothing is a container, and the lookup says why, so
	// the session says it once (Keyer).
	if _, ok, err := p.Lookup(netip.MustParseAddrPort("172.17.0.2:5432")); ok || !errors.Is(err, errNoList) {
		t.Fatalf("before any list = %v, %v; want none, and errNoList", ok, err)
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
	if err := ApplySnapshot(Snapshot{Containers: []SnapshotContainer{{ID: "r", Image: "redis:7", IPs: []string{"172.17.0.9"}, Fingerprint: fpRedis}}}); err != nil {
		t.Fatal(err)
	}
	if got := name(t, Default, "1", "172.17.0.9:6379"); got != keyRedis {
		t.Fatalf("a pushed agent named %q", got)
	}
}

// The Docker host's addresses a docker-mode agent knows are the ones its CLI
// gave it last; an agent that reads Docker itself, or was given none, knows
// none.
func TestHostAddrIsTheHostsAddressesTheCLIGave(t *testing.T) {
	saved := Default
	t.Cleanup(func() { Default = saved })
	bridge := netip.MustParseAddr("172.17.0.1")
	Default = NewKeyer(newDockerIndex())
	if HostAddr(bridge) {
		t.Fatal("an agent that reads Docker knows the host's addresses from a list")
	}
	UsePushed()
	if HostAddr(bridge) {
		t.Fatal("an agent given no list knows the host's addresses")
	}
	if err := ApplySnapshot(Snapshot{Seq: 1, Local: []string{"172.17.0.1", "192.168.1.10"}, Containers: []SnapshotContainer{}}); err != nil {
		t.Fatal(err)
	}
	if !HostAddr(netip.MustParseAddr("::ffff:172.17.0.1")) || !HostAddr(netip.MustParseAddr("192.168.1.10")) {
		t.Fatal("an address of the host the CLI gave is not the host's")
	}
	if HostAddr(netip.MustParseAddr("10.1.2.3")) {
		t.Fatal("another machine's address is the host's")
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

// waitPost waits until the agent's last list satisfies ok.
func waitPost(ps *posts, ok func(Snapshot) bool) Snapshot {
	for {
		if ps.count() > 0 {
			if s := ps.last(); ok(s) {
				return s
			}
		}
		<-ps.sent
	}
}

// settled: the agent was given every listed container with its
// configuration read (the list's reads, beside its first push).
func settled(s Snapshot) bool {
	for _, c := range s.Containers {
		if c.Reading || c.Fingerprint == "" {
			return false
		}
	}
	return true
}

// settle waits until the agent's list is settled, and returns how many lists
// it was given by then, with every post told.
func settle(ps *posts) int {
	waitPost(ps, settled)
	for len(ps.sent) > 0 {
		<-ps.sent
	}
	return ps.count()
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
	ps.mu.Lock()
	got := ps.got[0]
	ps.mu.Unlock()
	if len(got.Containers) != 3 || got.Err != "" || got.Source == "" {
		t.Fatalf("Push gave %+v", got)
	}
	// Each listed container's configuration, read beside the list, is given
	// as a change.
	settle(ps)
	first := ps.last()
	f.set("new", running("nginx", "sha256:n", []string{"172.17.0.9"}, nat.PortMap{}))
	f.msgs <- start("new")
	if s := waitPost(ps, func(s Snapshot) bool { return len(s.Containers) == 4 && settled(s) }); s.Seq <= first.Seq || s.Source != first.Source {
		t.Fatalf("after a start = %+v", s)
	}
	f.remove("pg")
	f.msgs <- die("pg")
	waitPost(ps, func(s Snapshot) bool { return len(s.Containers) == 3 })
	n := settle(ps)
	if err := p.Push(t.Context(), nil); err != nil || ps.count() != n+1 {
		t.Fatalf("a second Push in the session: %v, %d lists after %d", err, ps.count(), n)
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

// Each session's change loop reads a change signal of its own, which the
// index tells from that session's Push on: the last session's loop, which
// may still run when the next session starts, never takes a change meant for
// the next one, and the next session's loop gives the agent each change.
func TestAnEndedSessionsLoopNeverTakesTheNextOnesChange(t *testing.T) {
	f := testcontainers()
	ps := newPosts()
	p := testPusher(f, ps)
	first, end := context.WithCancel(t.Context())
	if err := p.Push(first, nil); err != nil {
		t.Fatal(err)
	}
	settle(ps)
	p.mu.Lock()
	last := p.changed
	p.mu.Unlock()
	oldWatch := current(p.idx)
	end()
	<-oldWatch.done // the last session's watch reads no more events; its loop is not waited for
	f.mu.Lock()
	f.msgs = make(chan events.Message)
	f.mu.Unlock()
	if err := p.Push(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	settle(ps)
	p.mu.Lock()
	next := p.changed
	p.mu.Unlock()
	p.idx.mu.Lock()
	told := p.idx.changed
	p.idx.mu.Unlock()
	if next == last || told != next {
		t.Fatalf("the next session's change signal is the last one's (%v), or the index tells another (%v)", next == last, told != next)
	}
	f.set("new", running("nginx", "sha256:n", []string{"172.17.0.9"}, nat.PortMap{}))
	f.msgs <- start("new")
	waitPost(ps, func(s Snapshot) bool { return len(s.Containers) == 4 })
}

// A watch that fails mid-session (the CLI's event stream ended) gives the
// agent a list that says so: it then names by address, and says why, rather
// than naming from a list nothing keeps.
func TestAWatchThatFailsMidSessionIsPushed(t *testing.T) {
	f := testcontainers()
	ps := newPosts()
	p := testPusher(f, ps)
	if err := p.Push(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	settle(ps)
	f.endStream(errors.New("unexpected EOF"))
	if s := waitPost(ps, func(s Snapshot) bool { return s.Err != "" }); len(s.Containers) != 0 {
		t.Fatalf("after the stream ended the agent was given %+v; want the failure", s)
	}
}

// oneChange is one change to p's index, told once: a container the index
// now holds. (A container an event starts is told as it is placed, put in and
// read: several signals, of which the loop may hold any.)
func oneChange(p *Pusher) {
	p.idx.mu.Lock()
	st := p.idx.cur
	p.idx.mu.Unlock()
	e := entry{id: "new", image: "nginx"}
	e.addIP("172.17.0.9")
	p.idx.put(st, e, 0)
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
	for range 3 {
		<-applied // the listed containers' configurations
	}
	n0 := settle(ps)
	p.mu.Lock()
	oneChange(p)
	tookTheChange(p)                 // the loop holds the change, and waits for p.mu
	err := p.send(t.Context(), true) // a session start's push, under p.mu as Push sends
	p.changed <- struct{}{}          // the loop takes this once done with the change it holds
	p.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	tookTheChange(p)
	if n := ps.count(); n != n0+1 {
		t.Fatalf("%d lists after %d; want only the forced one more: the loop gave the change again", n, n0)
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
	for range 3 {
		<-applied // the listed containers' configurations
	}
	n0 := settle(ps)
	p.mu.Lock()
	done := p.loopDone
	oneChange(p)
	tookTheChange(p)
	end()
	p.mu.Unlock()
	<-done
	if n := ps.count(); n != n0 {
		t.Fatalf("the ended session's loop gave the agent %d lists", n-n0)
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

// A docker-mode agent takes a container's fingerprint from its CLI's lists:
// one the CLI is still reading waits for the list that carries it, and one
// the list holds without (its read failed) has none.
func TestAPushedAgentWaitsForTheFingerprint(t *testing.T) {
	p := newPushedIndex()
	p.apply(Snapshot{Source: "s", Seq: 1, Containers: []SnapshotContainer{
		{ID: "late", Image: "postgres:16", IPs: []string{"172.17.0.2"}, Reading: true},
		{ID: "none", Image: "postgres:16", IPs: []string{"172.17.0.3"}},
	}})
	if fp, ok := p.Fingerprint(t.Context(), "none"); ok || fp != "" {
		t.Fatalf("a container with no fingerprint and no read = %q, %v", fp, ok)
	}
	got := make(chan string, 1)
	go func() {
		fp, _ := p.Fingerprint(t.Context(), "late")
		got <- fp
	}()
	p.apply(Snapshot{Source: "s", Seq: 2, Containers: []SnapshotContainer{
		{ID: "late", Image: "postgres:16", IPs: []string{"172.17.0.2"}, Fingerprint: fpDefault},
	}})
	if fp := <-got; fp != fpDefault {
		t.Fatalf("the fingerprint once listed = %q", fp)
	}
	if c, ok, _ := p.Lookup(netip.MustParseAddrPort("172.17.0.2:5432")); !ok || c.Fingerprint != fpDefault {
		t.Fatalf("Lookup = %+v, %v", c, ok)
	}
}

// errAgent503 is an agent that did not take a list.
var errAgent503 = errors.New("the agent answered 503")

// A list the agent did not take is given again until it takes one: the
// agent may be waiting on it for a container's configuration.
func TestALostListIsGivenAgain(t *testing.T) {
	f := testcontainers()
	// ls's configuration comes in after Push's own list: the list lost is
	// one the change loop gives, which it must give again.
	lsRead := make(chan struct{})
	f.block["ls"] = lsRead
	agent := newPushedIndex()
	var mu sync.Mutex
	failed := false
	took := make(chan Snapshot, 64)
	post := func(_ context.Context, s Snapshot) error {
		mu.Lock()
		defer mu.Unlock()
		if settled(s) && !failed {
			failed = true // the list that carries the fingerprints is lost once
			return errAgent503
		}
		agent.apply(s)
		took <- s
		return nil
	}
	p := NewPusher(post, "")
	p.idx.newAPI = func(string) (dockerAPI, error) { return f, nil }
	p.idx.socketExists = func(string) bool { return true }
	p.idx.hostAddrs = func() ([]netip.Addr, error) { return nil, nil }
	if err := p.Push(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	close(lsRead)
	for s := range took {
		if settled(s) {
			break
		}
	}
	k := NewKeyer(agent)
	if _, _, ok := k.Name(zap.NewNop(), "c1", "172.17.0.3:4566"); !ok {
		t.Fatal("ls not named")
	}
	m := &models.Mock{ConnectionID: "c1"}
	k.Stamp(m)
	if m.Spec.Metadata[MetadataKey] == "" {
		t.Fatalf("stamped %v, want ls's key", m.Spec.Metadata)
	}
}

// A container whose configuration the CLI is still reading is listed as
// being read, so the agent waits for the list that carries it.
func TestTheCLIListsAContainerItIsReadingAsBeingRead(t *testing.T) {
	f := testcontainers()
	stuck := make(chan struct{})
	f.block["ls"] = stuck
	ps := newPosts()
	p := testPusher(f, ps)
	if err := p.Push(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	reading := false
	for _, c := range ps.last().Containers {
		if c.ID == "ls" {
			reading = c.Reading && c.Fingerprint == ""
		}
	}
	close(stuck)
	if !reading {
		t.Fatalf("the list %+v does not show ls as being read", ps.last())
	}
	waitPost(ps, settled)
}

// A listed container the CLI could not read is listed as read, with no
// fingerprint: the agent stops waiting for it at once (its calls are a
// container's with no key), instead of waiting for a list that never comes.
func TestTheCLIListsAContainerItCouldNotReadAsRead(t *testing.T) {
	f := testcontainers()
	gate := make(chan struct{})
	f.block["ls"] = gate
	f.inspectErr = map[string]error{"ls": errors.New("Error response from daemon: 500")}
	agent := newPushedIndex()
	ps := newPosts()
	p := NewPusher(func(ctx context.Context, s Snapshot) error {
		agent.apply(s)
		return ps.post(ctx, s)
	}, "")
	p.idx.newAPI = func(string) (dockerAPI, error) { return f, nil }
	p.idx.socketExists = func(string) bool { return true }
	p.idx.hostAddrs = func() ([]netip.Addr, error) { return nil, nil }
	if err := p.Push(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	<-f.entered
	// Every other container's read is in and given to the agent first, so
	// the read of ls ending is the only change left to give it.
	waitPost(ps, func(s Snapshot) bool {
		for _, c := range s.Containers {
			if c.ID != "ls" && c.Fingerprint == "" {
				return false
			}
		}
		return true
	})
	close(gate)
	readOf := func(id string) func(Snapshot) bool {
		return func(s Snapshot) bool {
			for _, c := range s.Containers {
				if c.ID == id {
					return !c.Reading
				}
			}
			return false
		}
	}
	s := waitPost(ps, readOf("ls"))
	for _, c := range s.Containers {
		if c.ID == "ls" && c.Fingerprint != "" {
			t.Fatalf("ls listed with a fingerprint %q", c.Fingerprint)
		}
	}
	k := NewKeyer(agent)
	if _, _, ok := k.Name(zap.NewNop(), "c1", "172.17.0.3:4566"); !ok {
		t.Fatal("ls not named")
	}
	if key, st := StateFromContext(WithConn(t.Context(), k, "c1")); st != Withheld || key != "" {
		t.Fatalf("a call to ls = %q, %v, want a container with no key", key, st)
	}
}

// A list the agent did not take when Push gave it is given again, though
// nothing changes after: a restarted agent holds no list until it takes one.
func TestPushGivesAListTheAgentDidNotTakeAgain(t *testing.T) {
	f := testcontainers()
	ps := newPosts()
	p := testPusher(f, ps)
	if err := p.Push(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	settle(ps)
	// The agent restarts; the next session's Push finds it not yet up.
	ps.mu.Lock()
	ps.err = errors.New("connection refused")
	ps.mu.Unlock()
	if err := p.Push(t.Context(), nil); err == nil {
		t.Fatal("Push said the agent took the list")
	}
	n := ps.count()
	ps.mu.Lock()
	ps.err = nil
	ps.mu.Unlock()
	// Nothing changes on the daemon: the list is given again all the same.
	for ps.count() == n {
		<-ps.sent
	}
	if !settled(ps.last()) {
		t.Fatalf("given again %+v", ps.last())
	}
}

// The pushed index answers Pending and AwaitReadsOf from the list held: the
// containers it shows being placed, and none at the app container's own
// loopback; a wait for some of them ends with the list that shows them
// placed, whatever it shows of others.
func TestThePushedIndexWaitsOnlyForThePlacingsAsked(t *testing.T) {
	agent := newPushedIndex()
	agent.apply(Snapshot{Source: "s", Seq: 1, Placing: []string{"a", "b"}})
	ids, could := agent.Pending(netip.MustParseAddrPort("172.17.0.9:5432"))
	if !could || !reflect.DeepEqual(ids, []string{"a", "b"}) {
		t.Fatalf("Pending = %v, %v", ids, could)
	}
	if _, could := agent.Pending(netip.MustParseAddrPort("127.0.0.1:5432")); could {
		t.Fatal("a container could be at the app container's own loopback")
	}
	done := make(chan struct{})
	go func() {
		agent.AwaitReadsOf(t.Context(), []string{"a"})
		close(done)
	}()
	// a is placed; b, still being placed, is not waited for.
	agent.apply(Snapshot{Source: "s", Seq: 2, Placing: []string{"b"}, Containers: []SnapshotContainer{
		{ID: "a", Image: "postgres:16", IPs: []string{"172.17.0.2"}, Reading: true},
	}})
	<-done
}

// The CLI lists a container an event started as being placed until it is
// placed: an agent connection accepted meanwhile waits for it, as the CLI's
// own index would, and is named.
func TestTheCLIListsAContainerItIsPlacingAsBeingPlaced(t *testing.T) {
	f := testcontainers()
	agent := newPushedIndex()
	ps := newPosts()
	p := NewPusher(func(ctx context.Context, s Snapshot) error {
		agent.apply(s)
		return ps.post(ctx, s)
	}, "")
	p.idx.newAPI = func(string) (dockerAPI, error) { return f, nil }
	p.idx.socketExists = func(string) bool { return true }
	p.idx.hostAddrs = func() ([]netip.Addr, error) { return nil, nil }
	if err := p.Push(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	settle(ps)
	g := make(chan struct{})
	read := make(chan struct{})
	f.mu.Lock()
	f.placeGate["svc"] = g
	f.block["svc"] = read // only the list places it
	f.mu.Unlock()
	f.set("svc", running("nginx", "sha256:n", []string{"172.17.0.9"}, nat.PortMap{}))
	f.msgs <- start("svc")
	<-f.placeEntered
	waitPost(ps, func(s Snapshot) bool { return slices.Contains(s.Placing, "svc") })
	spy := &awaitSpy{resolver: agent, awaiting: make(chan []string, 1)}
	k := NewKeyer(spy)
	k.Name(zap.NewNop(), "c1", "172.17.0.9:80")
	got := make(chan State, 1)
	go func() {
		_, st := StateFromContext(WithConn(t.Context(), k, "c1"))
		got <- st
	}()
	select {
	case ids := <-spy.awaiting:
		if !reflect.DeepEqual(ids, []string{"svc"}) {
			t.Fatalf("the first use waits for %v", ids)
		}
	case st := <-got:
		t.Fatalf("the first use returned %v while the CLI was placing the container", st)
	}
	close(g)
	close(read) // its key waits for its configuration
	if st := <-got; st == NotContainer {
		t.Fatalf("a connection accepted while the CLI placed the container it reaches is no container's")
	}
}

// awaitSpy is a resolver that tells awaiting what each AwaitReadsOf waits
// for, as it begins.
type awaitSpy struct {
	resolver
	awaiting chan []string
}

func (s *awaitSpy) AwaitReadsOf(ctx context.Context, ids []string) {
	s.awaiting <- slices.Clone(ids)
	s.resolver.AwaitReadsOf(ctx, ids)
}

// A placing that ends with nothing placed (the container started and exited:
// the list has nothing) reaches a docker-mode agent: the CLI gives a list
// without it.
func TestTheCLIListsAPlacingThatFoundNothingAsEnded(t *testing.T) {
	f := testcontainers()
	agent := newPushedIndex()
	ps := newPosts()
	p := NewPusher(func(ctx context.Context, s Snapshot) error {
		agent.apply(s)
		return ps.post(ctx, s)
	}, "")
	p.idx.newAPI = func(string) (dockerAPI, error) { return f, nil }
	p.idx.socketExists = func(string) bool { return true }
	p.idx.hostAddrs = func() ([]netip.Addr, error) { return nil, nil }
	if err := p.Push(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	settle(ps)
	g := make(chan struct{})
	f.mu.Lock()
	f.placeGate["quick"] = g
	f.mu.Unlock()
	c := running("redis", "sha256:r", nil, nat.PortMap{})
	c.State.Running = false // exited at once: the list has nothing
	f.set("quick", c)
	f.msgs <- start("quick")
	<-f.placeEntered
	waitPost(ps, func(s Snapshot) bool { return slices.Contains(s.Placing, "quick") })
	close(g)
	waitPost(ps, func(s Snapshot) bool { return !slices.Contains(s.Placing, "quick") })
}

// settlingPusher is a CLI's pusher over f giving its lists to agent, and,
// when answers, answering its settles as the routes do (AwaitSettleAsk).
func settlingPusher(t *testing.T, f *fakeDocker, agent *pushedIndex, answers bool) (*Pusher, chan Snapshot) {
	t.Helper()
	var ask func(context.Context, string, uint64) (SettleAsk, error)
	if answers {
		ask = func(ctx context.Context, boot string, after uint64) (SettleAsk, error) {
			return agent.awaitAsk(ctx, boot, after), nil
		}
	}
	p, took, _ := askingPusher(t, f, agent, ask, nil)
	return p, took
}

// askingPusher is settlingPusher asking for the agent's settles with ask
// (none when nil), and losing each list lose says the agent does not take;
// end ends its session.
func askingPusher(t *testing.T, f *fakeDocker, agent *pushedIndex, ask func(context.Context, string, uint64) (SettleAsk, error), lose func(Snapshot) bool) (p *Pusher, took chan Snapshot, end context.CancelFunc) {
	t.Helper()
	took = make(chan Snapshot, 64)
	p = NewPusher(func(_ context.Context, s Snapshot) error {
		if lose != nil && lose(s) {
			return errors.New("the agent did not take the list")
		}
		agent.apply(s)
		took <- s
		return nil
	}, "")
	p.idx.newAPI = func(string) (dockerAPI, error) { return f, nil }
	p.idx.socketExists = func(string) bool { return true }
	p.idx.hostAddrs = func() ([]netip.Addr, error) { return []netip.Addr{netip.MustParseAddr("172.17.0.1")}, nil }
	if ask != nil {
		p.AnswerSettles(ask)
	}
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(func() {
		cancel()
		stopIndex(p.idx)
	})
	if err := p.Push(ctx, nil); err != nil {
		t.Fatal(err)
	}
	return p, took, cancel
}

// A docker-mode agent's first use that finds nothing placed asks its CLI for
// a list of the daemon (pushedIndex.Settle), and waits for the list that
// answers it: a container Docker started before the use, whose start event
// the CLI's index has not taken, is named all the same.
func TestADockerModeAgentNamesAContainerWhoseStartTheCLIHasNotTaken(t *testing.T) {
	f := newFakeDocker()
	agent := newPushedIndex()
	_, took := settlingPusher(t, f, agent, true)
	if s := <-took; !s.Settles {
		t.Fatalf("the CLI's list %+v does not say it answers settles", s)
	}
	k := NewKeyer(agent)
	f.set("tc", running("postgres:16", "sha256:feed", []string{"172.17.0.7"}, nat.PortMap{}))
	if c, _, ok := k.Name(zap.NewNop(), "c1", "172.17.0.7:5432"); ok {
		t.Fatalf("the agent named %+v before its CLI listed it", c)
	}
	want := "container:postgres:5432@" + fpEmpty
	if key, st := use(k, "c1"); st != Keyed || key != want {
		t.Fatalf("the first use = %q, %v; want %q, Keyed", key, st, want)
	}
}

// An agent whose CLI predates settles (its lists do not say it answers them)
// waits for none: its first use places what the lists say, as before.
func TestADockerModeAgentWhoseCLIAnswersNoSettlesWaitsForNone(t *testing.T) {
	f := newFakeDocker()
	agent := newPushedIndex()
	_, took := settlingPusher(t, f, agent, false)
	if s := <-took; s.Settles || s.Settled != 0 {
		t.Fatalf("a CLI that answers no settles gave %+v", s)
	}
	k := NewKeyer(agent)
	f.set("tc", running("postgres:16", "sha256:feed", []string{"172.17.0.7"}, nat.PortMap{}))
	_ = name(t, k, "c1", "172.17.0.7:5432")
	if _, st := use(k, "c1"); st != NotContainer {
		t.Fatalf("the first use = %v, want NotContainer: the lists say none", st)
	}
	agent.mu.Lock()
	defer agent.mu.Unlock()
	if agent.asked != 0 {
		t.Fatalf("the agent asked %d settles of a CLI that answers none", agent.asked)
	}
}

// The CLI's count of an agent's settles is of one run of the agent (boot):
// asked with another run's count, the agent answers its first settle. A
// list answers only its own run's settles, and a late list's answer counts,
// though the list itself is older than the one held.
func TestASettleAnswerIsOfTheAgentsRunAndALateListCounts(t *testing.T) {
	agent := newPushedIndex()
	agent.apply(Snapshot{Source: "cli", Seq: 1, Settles: true, Containers: []SnapshotContainer{}})
	asks := make(chan SettleAsk, 1)
	go func() { asks <- agent.awaitAsk(t.Context(), "an-older-run", 5) }()
	done := make(chan struct{})
	go func() {
		agent.Settle(t.Context())
		close(done)
	}()
	ask := <-asks
	if ask.Asked != 1 || ask.Boot != agent.boot {
		t.Fatalf("asked with another run's count = %+v, want this run's first settle", ask)
	}
	agent.apply(Snapshot{Source: "cli", Seq: 3, Settles: true, Settled: 1, SettledBoot: "an-older-run", Containers: []SnapshotContainer{}})
	agent.mu.Lock()
	answered := agent.answered
	agent.mu.Unlock()
	if answered != 0 {
		t.Fatalf("a list answering another run's settle answered this run's (%d)", answered)
	}
	agent.apply(Snapshot{Source: "cli", Seq: 2, Settles: true, Settled: 1, SettledBoot: agent.boot, Containers: []SnapshotContainer{}})
	<-done // the late list's answer ends it
	if agent.seq != 3 {
		t.Fatalf("the late list replaced the newer one: seq %d", agent.seq)
	}
	// A settle while the agent's list says its CLI cannot read the daemon
	// waits for nothing.
	agent.apply(Snapshot{Source: "cli", Seq: 4, Settles: true, Err: "no daemon", Containers: []SnapshotContainer{}})
	agent.Settle(t.Context())
}

// A first use where no container is still has its settle answered, though
// the CLI's list of the daemon places nothing (the list it gives then is the
// one it gave before, marked): the agent's commonest settle, a connection to
// a service on the host or in a private network. The CLI's next ask is for
// the agent's settles after the one it answered, of the agent's run: it
// answers each settle once.
func TestTheCLIAnswersASettleThatPlacesNothingOnce(t *testing.T) {
	f := newFakeDocker()
	agent := newPushedIndex()
	told := make(chan SettleAsk, 8) // what the CLI was told of, at each of its asks
	_, took, _ := askingPusher(t, f, agent, func(ctx context.Context, boot string, after uint64) (SettleAsk, error) {
		told <- SettleAsk{Asked: after, Boot: boot}
		return agent.awaitAsk(ctx, boot, after), nil
	}, nil)
	<-took
	if got := <-told; got != (SettleAsk{}) {
		t.Fatalf("the CLI's first ask was told of %+v, want nothing", got)
	}
	k := NewKeyer(agent)
	_ = name(t, k, "c1", "172.17.0.7:5432")
	if _, st := use(k, "c1"); st != NotContainer {
		t.Fatalf("the first use = %v, want NotContainer: no container is there", st)
	}
	if s := answering(took, 1); s.SettledBoot != agent.boot {
		t.Fatalf("the CLI answered with %+v, want the agent's first settle, of its run", s)
	}
	if got := <-told; got != (SettleAsk{Asked: 1, Boot: agent.boot}) {
		t.Fatalf("the CLI's next ask was told of %+v, want the settle it answered", got)
	}
}

// A list answering a settle that the agent did not take is given again,
// though nothing changed since: the agent's settle waits for it.
func TestALostSettleAnswerIsGivenAgain(t *testing.T) {
	f := newFakeDocker()
	agent := newPushedIndex()
	var lost sync.Once
	_, took, _ := askingPusher(t, f, agent, func(ctx context.Context, boot string, after uint64) (SettleAsk, error) {
		return agent.awaitAsk(ctx, boot, after), nil
	}, func(s Snapshot) bool {
		gone := false
		if s.Settled == 1 {
			lost.Do(func() { gone = true }) // the first list answering it
		}
		return gone
	})
	<-took
	k := NewKeyer(agent)
	_ = name(t, k, "c1", "172.17.0.7:5432")
	if _, st := use(k, "c1"); st != NotContainer {
		t.Fatalf("the first use = %v, want NotContainer: no container is there", st)
	}
	answering(took, 1) // given again
}

// answering is the first list of took that answers the agent's settle n.
func answering(took chan Snapshot, n uint64) Snapshot {
	for {
		if s := <-took; s.Settled >= n {
			return s
		}
	}
}

// An agent that takes no settle asks (one that predates them) is asked once
// a session: the CLI stops asking, though the session lasts.
func TestTheCLIStopsAskingAnAgentThatTakesNoSettles(t *testing.T) {
	f := newFakeDocker()
	agent := newPushedIndex()
	var mu sync.Mutex
	n := 0
	p, took, _ := askingPusher(t, f, agent, func(context.Context, string, uint64) (SettleAsk, error) {
		mu.Lock()
		defer mu.Unlock()
		n++
		return SettleAsk{}, ErrNoSettles
	}, nil)
	<-took
	p.mu.Lock()
	done := p.settlesDone
	p.mu.Unlock()
	<-done // its session lasts: the CLI stopped asking
	mu.Lock()
	defer mu.Unlock()
	if n != 1 {
		t.Fatalf("an agent that takes no settle asks was asked %d times, want once", n)
	}
}

// A session's first ask is told of the settles the CLI answered in the
// sessions before: the next session lists the daemon only for a settle the
// agent asked since, never again for one it answered.
func TestTheNextSessionAsksForTheSettlesSinceTheLastAnswered(t *testing.T) {
	f := newFakeDocker()
	agent := newPushedIndex()
	told := make(chan SettleAsk, 8) // what the CLI was told of, at each of its asks
	p, took, end := askingPusher(t, f, agent, func(ctx context.Context, boot string, after uint64) (SettleAsk, error) {
		told <- SettleAsk{Asked: after, Boot: boot}
		return agent.awaitAsk(ctx, boot, after), nil
	}, nil)
	<-took
	<-told
	k := NewKeyer(agent)
	_ = name(t, k, "c1", "172.17.0.7:5432")
	_, _ = use(k, "c1")
	answering(took, 1)
	<-told // the session's next ask, held
	p.mu.Lock()
	done := p.settlesDone
	p.mu.Unlock()
	end()
	<-done
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if err := p.Push(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if got := <-told; got != (SettleAsk{Asked: 1, Boot: agent.boot}) {
		t.Fatalf("the next session's first ask was told of %+v, want the settle answered before", got)
	}
}

// An identical put (a placing's list landing after the read that placed the
// container) sends the agent no list: nothing a list shows changed.
func TestAnIdenticalPutSendsNoList(t *testing.T) {
	f := testcontainers()
	ps := newPosts()
	p := testPusher(f, ps)
	if err := p.Push(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	// The listed containers' reads are in, and no event came: nothing else
	// changes the index. Under the pusher's lock its change loop sends
	// nothing, and the send below gives the agent every change counted
	// before the put, so the count after it is the put's alone.
	waitPost(ps, settled)
	st := current(p.idx)
	f.mu.Lock()
	e := summaryEntry(summaryOf("pg", f.containers["pg"]))
	f.mu.Unlock()
	e.placed, e.fromList = true, true
	p.idx.mu.Lock()
	seq := st.seq["pg"]
	p.idx.mu.Unlock()
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.send(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	n := ps.count()
	if !p.idx.put(st, e, seq) {
		t.Fatal("the put was refused")
	}
	if err := p.send(t.Context(), false); err != nil { // as the change loop sends: only what changed
		t.Fatal(err)
	}
	if got := ps.count(); got != n {
		t.Fatalf("%d lists after %d: an identical put sent one", got, n)
	}
}
