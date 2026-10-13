package containerkey

import (
	"context"
	"errors"
	"net/netip"
	"sync"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/go-connections/nat"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// notFound is the daemon's answer for a container that is gone.
type notFound struct{}

func (notFound) Error() string { return "No such container" }
func (notFound) NotFound()     {}

// fakeDocker is a daemon: its containers, an event stream a test feeds, and a
// record of the calls made, so a test can see a lookup never asks it.
type fakeDocker struct {
	mu         sync.Mutex
	host       string
	containers map[string]container.InspectResponse
	listErr    error
	// hang makes the list block until its context ends: a daemon that
	// accepts and never answers.
	hang bool
	// block holds the inspect of a container until its channel closes;
	// entered is told each held inspect as it begins.
	block   map[string]chan struct{}
	entered chan string
	// beforeList runs as the list is asked for; listGate, when set, holds
	// the list until it closes.
	beforeList func()
	listGate   chan struct{}
	msgs       chan events.Message
	errs       chan error
	calls      int
	inspects   int
	closes     int
	opts       events.ListOptions
}

func newFakeDocker() *fakeDocker {
	return &fakeDocker{
		host:       "unix:///var/run/docker.sock",
		containers: map[string]container.InspectResponse{},
		block:      map[string]chan struct{}{},
		entered:    make(chan string, 64),
		msgs:       make(chan events.Message),
	}
}

func (f *fakeDocker) ContainerList(ctx context.Context, _ container.ListOptions) ([]container.Summary, error) {
	f.mu.Lock()
	before, gate := f.beforeList, f.listGate
	f.mu.Unlock()
	if before != nil {
		before()
	}
	if gate != nil {
		<-gate
	}
	f.mu.Lock()
	f.calls++
	hang := f.hang
	var out []container.Summary
	for id, c := range f.containers {
		out = append(out, summaryOf(id, c))
	}
	err := f.listErr
	f.mu.Unlock()
	if hang {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return out, err
}

func (f *fakeDocker) ContainerInspect(ctx context.Context, id string) (container.InspectResponse, error) {
	f.mu.Lock()
	f.calls++
	f.inspects++
	block := f.block[id]
	f.mu.Unlock()
	if block != nil {
		f.entered <- id
		select {
		case <-block:
		case <-ctx.Done():
			return container.InspectResponse{}, ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	got, ok := f.containers[id]
	if !ok {
		return container.InspectResponse{}, notFound{}
	}
	return got, nil
}

// Events streams what the test sends, and, as the real client does, says the
// stream's context ended and closes when it ends.
func (f *fakeDocker) Events(ctx context.Context, opts events.ListOptions) (<-chan events.Message, <-chan error) {
	f.mu.Lock()
	f.opts = opts
	f.errs = make(chan error, 1)
	errs, msgs := f.errs, f.msgs
	f.mu.Unlock()
	out := make(chan events.Message)
	outErrs := make(chan error, 1)
	go func() {
		defer close(outErrs)
		for {
			select {
			case m := <-msgs:
				select {
				case out <- m:
				case <-ctx.Done():
					outErrs <- ctx.Err()
					return
				}
			case err := <-errs:
				outErrs <- err
				return
			case <-ctx.Done():
				outErrs <- ctx.Err()
				return
			}
		}
	}()
	return out, outErrs
}

func (f *fakeDocker) DaemonHost() string { return f.host }

func (f *fakeDocker) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closes++
	return nil
}

func (f *fakeDocker) set(id string, c container.InspectResponse) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.containers[id] = c
}

func (f *fakeDocker) remove(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.containers, id)
}

func (f *fakeDocker) count() (calls, inspects, closes int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls, f.inspects, f.closes
}

// endStream ends the event stream as a daemon that went away does.
func (f *fakeDocker) endStream(err error) {
	f.mu.Lock()
	errs := f.errs
	f.mu.Unlock()
	errs <- err
}

// summaryOf is what the list shows of a container.
func summaryOf(id string, c container.InspectResponse) container.Summary {
	s := container.Summary{ID: id, Image: c.Config.Image, ImageID: c.Image}
	for p, bs := range c.NetworkSettings.Ports {
		for _, b := range bs {
			hp := uint16(0)
			for _, ch := range b.HostPort {
				hp = hp*10 + uint16(ch-'0')
			}
			s.Ports = append(s.Ports, container.Port{IP: b.HostIP, PrivatePort: uint16(p.Int()), PublicPort: hp, Type: p.Proto()})
		}
	}
	s.NetworkSettings = &container.NetworkSettingsSummary{Networks: c.NetworkSettings.Networks}
	return s
}

// running is an inspect of a running container created from image, with
// the IPs and published ports given ("5432/tcp" -> bindings).
func running(image, imageID string, ips []string, ports nat.PortMap) container.InspectResponse {
	nets := map[string]*network.EndpointSettings{}
	for i, ip := range ips {
		e := &network.EndpointSettings{}
		if netip.MustParseAddr(ip).Is4() {
			e.IPAddress = ip
		} else {
			e.GlobalIPv6Address = ip
		}
		nets[string(rune('a'+i))] = e
	}
	return container.InspectResponse{
		ContainerJSONBase: &container.ContainerJSONBase{Image: imageID, State: &container.State{Running: true}},
		Config:            &container.Config{Image: image},
		NetworkSettings: &container.NetworkSettings{
			NetworkSettingsBase: container.NetworkSettingsBase{Ports: ports},
			Networks:            nets,
		},
	}
}

func published(hostIP, hostPort string) []nat.PortBinding {
	return []nat.PortBinding{{HostIP: hostIP, HostPort: hostPort}}
}

// newIndex is an index of f on a host with the addresses given; applied is
// told each container read once the index holds it.
func newIndex(f *fakeDocker, addrs ...string) (*dockerIndex, chan string) {
	applied := make(chan string, 64)
	r := &dockerIndex{newAPI: func(string) (dockerAPI, error) { return f, nil }, applied: func(id string) { applied <- id },
		socketExists: func(string) bool { return true }}
	r.hostAddrs = func() ([]netip.Addr, error) {
		out := make([]netip.Addr, 0, len(addrs))
		for _, a := range addrs {
			out = append(out, netip.MustParseAddr(a))
		}
		return out, nil
	}
	return r, applied
}

// current is r's current watch.
func current(r *dockerIndex) *watchState {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cur
}

// stopIndex ends r's current watch, as the agent's end would, and waits for
// it to close its client.
func stopIndex(r *dockerIndex) {
	if st := current(r); st != nil {
		r.fail(st, errors.New("test over"))
		<-st.done
	}
}

// watching is an index of f that has its first list in.
func watching(t *testing.T, f *fakeDocker, addrs ...string) (*dockerIndex, chan string) {
	t.Helper()
	r, applied := newIndex(f, addrs...)
	r.Watch(t.Context(), nil)
	t.Cleanup(func() { stopIndex(r) })
	for len(applied) > 0 {
		<-applied
	}
	return r, applied
}

func lookup(t *testing.T, r *dockerIndex, dst string) (Container, bool) {
	t.Helper()
	c, ok, err := r.Lookup(netip.MustParseAddrPort(dst))
	if err != nil {
		t.Fatalf("Lookup(%s): %v", dst, err)
	}
	return c, ok
}

func start(id string) events.Message {
	return events.Message{Type: events.ContainerEventType, Action: events.ActionStart, Actor: events.Actor{ID: id}}
}

func die(id string) events.Message {
	return events.Message{Type: events.ContainerEventType, Action: events.ActionDie, Actor: events.Actor{ID: id}}
}

func testcontainers() *fakeDocker {
	f := newFakeDocker()
	f.set("pg", running("docker.io/library/postgres:16-alpine", "sha256:feed", []string{"172.17.0.2", "fd00::2"}, nat.PortMap{
		"5432/tcp": append(published("0.0.0.0", "49153"), published("::", "49153")...),
		"5433/udp": published("0.0.0.0", "49160"),
	}))
	f.set("ls", running("localstack/localstack:3.8", "sha256:beef", []string{"172.17.0.3"}, nat.PortMap{
		"4566/tcp": published("127.0.0.1", "49200"),
	}))
	f.set("redis", running("redis", "sha256:cafe", nil, nat.PortMap{
		"6379/tcp": published("10.0.0.5", "49300"),
	}))
	return f
}

func TestIndexFindsPublishedPortsOnThisHost(t *testing.T) {
	r, _ := watching(t, testcontainers(), "10.0.0.5", "192.168.1.20")
	for dst, want := range map[string]Container{
		"127.0.0.1:49153":    {ID: "pg", Image: "postgres:16-alpine", Port: 5432},
		"[::1]:49153":        {ID: "pg", Image: "postgres:16-alpine", Port: 5432},
		"192.168.1.20:49153": {ID: "pg", Image: "postgres:16-alpine", Port: 5432}, // all addresses
		"127.0.0.1:49200":    {ID: "ls", Image: "localstack/localstack:3.8", Port: 4566},
		"10.0.0.5:49300":     {ID: "redis", Image: "redis:latest", Port: 6379}, // bound to that address
	} {
		got, ok := lookup(t, r, dst)
		if !ok || got != want {
			t.Errorf("Lookup(%s) = %+v, %v; want %+v", dst, got, ok, want)
		}
	}
}

func TestIndexFindsAContainersOwnIP(t *testing.T) {
	r, _ := watching(t, testcontainers())
	for dst, want := range map[string]Container{
		"172.17.0.2:5432": {ID: "pg", Image: "postgres:16-alpine", Port: 5432},
		"[fd00::2]:5432":  {ID: "pg", Image: "postgres:16-alpine", Port: 5432},
		"172.17.0.3:4566": {ID: "ls", Image: "localstack/localstack:3.8", Port: 4566},
	} {
		got, ok := lookup(t, r, dst)
		if !ok || got != want {
			t.Errorf("Lookup(%s) = %+v, %v; want %+v", dst, got, ok, want)
		}
	}
}

// The first list fills the index from what the list carries: it reads no
// container on its own, unless the list shows its image as an image ID.
func TestIndexFillsFromTheList(t *testing.T) {
	f := testcontainers()
	f.set("moved", running("sha256:abcdef0123456789", "sha256:abcdef0123456789aa", nil, nat.PortMap{"80/tcp": published("0.0.0.0", "40100")}))
	r, applied := newIndex(f)
	r.Watch(t.Context(), nil)
	t.Cleanup(func() { stopIndex(r) })
	if id := <-applied; id != "moved" {
		t.Fatalf("read %q on its own", id)
	}
	if _, inspects, _ := f.count(); inspects != 1 {
		t.Fatalf("the first list read %d containers on their own, want only the one listed by image ID", inspects)
	}
	if c, _ := lookup(t, r, "127.0.0.1:40100"); c.ID != "moved" {
		t.Fatalf("the container listed by image ID = %+v", c)
	}
}

func TestIndexLeavesEverythingElse(t *testing.T) {
	r, _ := watching(t, testcontainers(), "192.168.1.20")
	for _, dst := range []string{
		"127.0.0.1:5432",     // no container publishes it
		"127.0.0.1:49160",    // published for UDP only
		"192.168.1.20:49200", // published on 127.0.0.1 only
		"[::1]:49200",        // nor on ::1
		"127.0.0.2:49200",    // nor on another loopback address
		"10.0.0.9:49153",     // another machine with the same port number
		"172.17.0.4:5432",    // no container has that IP
		"127.0.0.1:49300",    // published on 10.0.0.5 only
		"203.0.113.7:49153",  // a public address
	} {
		if got, ok := lookup(t, r, dst); ok {
			t.Errorf("Lookup(%s) = %+v, want no container", dst, got)
		}
	}
}

// A port bound on the IPv4 wildcard is not one bound on IPv6, nor the
// reverse; HostIP "" binds both.
func TestIndexKeepsAddressFamiliesApart(t *testing.T) {
	f := newFakeDocker()
	f.set("v4", running("nginx", "sha256:1", nil, nat.PortMap{"80/tcp": published("0.0.0.0", "40080")}))
	f.set("v6", running("nginx", "sha256:1", nil, nat.PortMap{"80/tcp": published("::", "40081")}))
	f.set("both", running("nginx", "sha256:1", nil, nat.PortMap{"80/tcp": published("", "40082")}))
	r, _ := watching(t, f)
	if _, ok := lookup(t, r, "[::1]:40080"); ok {
		t.Fatal("::1 reached a port bound on 0.0.0.0")
	}
	if _, ok := lookup(t, r, "127.0.0.1:40081"); ok {
		t.Fatal("127.0.0.1 reached a port bound on ::")
	}
	for _, dst := range []string{"127.0.0.1:40082", "[::1]:40082"} {
		if _, ok := lookup(t, r, dst); !ok {
			t.Fatalf("%s did not reach a port bound on all addresses", dst)
		}
	}
}

// The index follows Docker's events: a container that starts is found, one
// that dies is gone, a port another container then takes is the new one's,
// and a container that joins a network is found at its new IP. A lookup
// never asks Docker. The stream is asked only for those events, from the
// moment before the list.
func TestIndexFollowsDockersEvents(t *testing.T) {
	f := newFakeDocker()
	r, applied := watching(t, f)
	if f.opts.Since == "" || len(f.opts.Filters.Get("event")) != 6 || len(f.opts.Filters.Get("type")) != 2 {
		t.Fatalf("the event stream was asked with %+v", f.opts)
	}
	if _, ok := lookup(t, r, "127.0.0.1:49153"); ok {
		t.Fatal("found a container before any started")
	}

	f.set("a", running("postgres:16", "sha256:a", []string{"172.18.0.2"}, nat.PortMap{"5432/tcp": published("0.0.0.0", "49153")}))
	f.msgs <- start("a")
	<-applied
	if c, ok := lookup(t, r, "127.0.0.1:49153"); !ok || c.ID != "a" {
		t.Fatalf("after a started: %+v, %v", c, ok)
	}

	calls, _, _ := f.count()
	for i := 0; i < 100; i++ {
		lookup(t, r, "127.0.0.1:49153")
		lookup(t, r, "127.0.0.1:1")
	}
	if now, _, _ := f.count(); now != calls {
		t.Fatalf("lookups asked Docker %d times", now-calls)
	}

	f.remove("a")
	f.msgs <- die("a")
	f.set("b", running("redis:7", "sha256:b", nil, nat.PortMap{"6379/tcp": published("0.0.0.0", "49153")}))
	f.msgs <- start("b")
	<-applied
	if c, ok := lookup(t, r, "127.0.0.1:49153"); !ok || c.ID != "b" || c.Port != 6379 {
		t.Fatalf("the port b took after a died: %+v, %v", c, ok)
	}
	if _, ok := lookup(t, r, "172.18.0.2:5432"); ok {
		t.Fatal("a's IP is still indexed after it died")
	}

	f.set("b", running("redis:7", "sha256:b", []string{"172.20.0.5"}, nat.PortMap{"6379/tcp": published("0.0.0.0", "49153")}))
	f.msgs <- events.Message{Type: events.NetworkEventType, Action: events.ActionConnect, Actor: events.Actor{ID: "net1", Attributes: map[string]string{"container": "b"}}}
	<-applied
	if c, ok := lookup(t, r, "172.20.0.5:6379"); !ok || c.ID != "b" {
		t.Fatalf("b at the IP it joined: %+v, %v", c, ok)
	}

	// A start whose container is gone by the time it is read drops nothing
	// else and stops nothing.
	f.msgs <- start("gone")
	<-applied
	if _, ok, err := r.Lookup(netip.MustParseAddrPort("127.0.0.1:49153")); !ok || err != nil {
		t.Fatalf("after a vanished container: %v, %v", ok, err)
	}
}

// A read that is stuck (a container Docker holds locked) holds up no other
// container, and a die that comes while a container is read wins over the
// read.
func TestIndexReadsContainersSideBySide(t *testing.T) {
	f := newFakeDocker()
	r, applied := watching(t, f)
	stuck := make(chan struct{})
	f.mu.Lock()
	f.block["slow"] = stuck
	f.mu.Unlock()
	f.set("slow", running("postgres:16", "sha256:s", nil, nat.PortMap{"5432/tcp": published("0.0.0.0", "49001")}))
	f.set("fast", running("redis:7", "sha256:f", nil, nat.PortMap{"6379/tcp": published("0.0.0.0", "49002")}))
	f.msgs <- start("slow")
	<-f.entered // slow's read is in flight, as of its start
	f.msgs <- start("fast")
	if id := <-applied; id != "fast" {
		t.Fatalf("applied %q first; the stuck read held up the others", id)
	}
	if _, ok := lookup(t, r, "127.0.0.1:49002"); !ok {
		t.Fatal("the fast container is not indexed while another read is stuck")
	}
	// slow dies while its read is stuck. The read still answers it running
	// (it began before the die): only its count tells it lost.
	f.msgs <- die("slow")
	f.msgs <- start("fence") // applied after the die, so the die is in
	<-applied
	close(stuck)
	if id := <-applied; id != "slow" {
		t.Fatalf("applied %q, want the stuck read", id)
	}
	if _, ok := lookup(t, r, "127.0.0.1:49001"); ok {
		t.Fatal("a read that ran across the container's die brought it back")
	}
}

// A port another container now publishes is that container's, even when the
// die of the one before it never reached the index (Docker drops events for
// a subscriber it waits on).
func TestIndexGivesAReusedPortToItsNewContainer(t *testing.T) {
	f := newFakeDocker()
	f.set("a", running("postgres:16", "sha256:a", nil, nat.PortMap{"5432/tcp": published("0.0.0.0", "49153")}))
	r, applied := watching(t, f)
	f.set("b", running("redis:7", "sha256:b", nil, nat.PortMap{"6379/tcp": published("0.0.0.0", "49153")}))
	f.msgs <- start("b")
	<-applied
	if c, ok := lookup(t, r, "127.0.0.1:49153"); !ok || c.ID != "b" {
		t.Fatalf("the reused port = %+v, %v; want b's", c, ok)
	}
	// a, when its die does come, takes nothing of b's.
	f.msgs <- die("a")
	f.msgs <- start("gone")
	<-applied
	if c, ok := lookup(t, r, "127.0.0.1:49153"); !ok || c.ID != "b" {
		t.Fatalf("after a's late die = %+v, %v; want b's", c, ok)
	}
}

// A failure holds for every lookup until the next Watch, which starts a fresh
// watch; the failed one closes its client, and nothing it does afterwards
// reaches the new one.
func TestIndexFailureHoldsUntilTheNextWatch(t *testing.T) {
	f := newFakeDocker()
	r, _ := watching(t, f)
	first := current(r)
	f.endStream(errors.New("unexpected EOF"))
	<-first.done
	if _, _, closes := f.count(); closes != 1 {
		t.Fatalf("the failed watch closed its client %d times", closes)
	}
	if _, _, err := r.Lookup(netip.MustParseAddrPort("127.0.0.1:1")); err == nil {
		t.Fatal("the failure did not hold")
	}

	r.Watch(t.Context(), nil)
	if _, _, err := r.Lookup(netip.MustParseAddrPort("127.0.0.1:1")); err != nil {
		t.Fatalf("the next Watch did not start afresh: %v", err)
	}
	// A late change from the failed watch is dropped.
	r.put(first, entry{id: "ghost", image: "nginx:latest", ips: []netip.Addr{netip.MustParseAddr("172.17.0.99")}}, 0)
	if _, ok := lookup(t, r, "172.17.0.99:80"); ok {
		t.Fatal("a failed watch changed the current one")
	}
	r.fail(first, errors.New("late"))
	if _, _, err := r.Lookup(netip.MustParseAddrPort("127.0.0.1:1")); err != nil {
		t.Fatalf("a failed watch's late failure reached the current one: %v", err)
	}
}

// A daemon that accepts and never answers holds a session's start only until
// the session ends. That fails the watch, so the next session starts a fresh
// one instead of waiting on the stuck one; a lookup never waits. The wait is
// said at Info, once per wait.
func TestASessionThatEndsBeforeTheListFailsItsWatch(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	f := newFakeDocker()
	f.hang = true
	r, _ := newIndex(f)
	t.Cleanup(func() { stopIndex(r) })
	ctx, cancel := context.WithCancel(t.Context())
	returned := make(chan struct{})
	go func() { r.Watch(ctx, zap.New(core)); close(returned) }()
	if _, ok, _ := r.Lookup(netip.MustParseAddrPort("127.0.0.1:1")); ok {
		t.Fatal("found a container during the hang")
	}
	cancel()
	<-returned
	if logs.FilterMessageSnippet("waiting for it to list them").Len() != 1 {
		t.Fatalf("the wait on the daemon was not said once at Info: %v", logs.All())
	}
	if _, _, err := r.Lookup(netip.MustParseAddrPort("127.0.0.1:1")); err == nil {
		t.Fatal("the session ended and its watch did not fail")
	}
	f.mu.Lock()
	f.hang = false
	f.mu.Unlock()
	r.Watch(t.Context(), nil)
	if _, _, err := r.Lookup(netip.MustParseAddrPort("127.0.0.1:1")); err != nil {
		t.Fatalf("the next session did not start a fresh watch: %v", err)
	}
}

// A session whose context has already ended never fails a watch that has its
// list: the next session keeps it. A watch that needs no wait says nothing.
func TestAnEndedSessionLeavesAListedWatch(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	f := newFakeDocker()
	r, _ := watching(t, f)
	done, cancel := context.WithCancel(t.Context())
	cancel()
	for i := 0; i < 50; i++ {
		r.Watch(done, zap.New(core))
		if _, _, err := r.Lookup(netip.MustParseAddrPort("127.0.0.1:1")); err != nil {
			t.Fatalf("an ended session failed a listed watch: %v", err)
		}
	}
	if logs.Len() != 0 {
		t.Fatalf("a watch that needed no wait said %d lines", logs.Len())
	}
}

// The agent's end stops the watch: it closes its client, and lookups then
// say why.
func TestTheAgentsEndStopsTheWatch(t *testing.T) {
	f := newFakeDocker()
	r, _ := newIndex(f)
	agentCtx, stopAgent := context.WithCancel(t.Context())
	r.Bind(agentCtx)
	r.Watch(t.Context(), nil)
	st := current(r)
	stopAgent()
	<-st.done
	if _, _, closes := f.count(); closes != 1 {
		t.Fatalf("closed the client %d times", closes)
	}
	if _, _, err := r.Lookup(netip.MustParseAddrPort("127.0.0.1:1")); !errors.Is(err, context.Canceled) {
		t.Fatalf("a lookup after the agent's end = %v", err)
	}
}

// Two sessions that start a watch at once, both building a client before
// either installs its watch: exactly one watch runs, and the other's client
// is closed at once, so no watch outlives the index's knowledge of it.
func TestTwoSessionsStartingAtOnceRunOneWatch(t *testing.T) {
	for round := 0; round < 20; round++ {
		f1, f2 := newFakeDocker(), newFakeDocker()
		var mu sync.Mutex
		built := 0
		both := make(chan struct{})
		r := &dockerIndex{socketExists: func(string) bool { return true },
			hostAddrs: func() ([]netip.Addr, error) { return nil, nil }}
		r.newAPI = func(string) (dockerAPI, error) {
			mu.Lock()
			built++
			mine := built
			if built == 2 {
				close(both)
			}
			mu.Unlock()
			<-both // neither installs before both have built
			if mine == 1 {
				return f1, nil
			}
			return f2, nil
		}
		var wg sync.WaitGroup
		for range 2 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				r.Watch(t.Context(), nil)
			}()
		}
		wg.Wait()
		stopIndex(r)
		_, _, c1 := f1.count()
		_, _, c2 := f2.count()
		if c1+c2 != 2 {
			t.Fatalf("round %d: the clients were closed %d and %d times: a watch was left running", round, c1, c2)
		}
	}
}

// A host with no Docker daemon socket has nothing to read: the index says so
// at once, asks nothing, and the session says nothing at Info.
func TestIndexWithNoDaemonSocket(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	f := newFakeDocker()
	r, _ := newIndex(f)
	r.socketExists = func(string) bool { return false }
	r.Watch(t.Context(), zap.New(core))
	if _, _, err := r.Lookup(netip.MustParseAddrPort("127.0.0.1:1")); !errors.Is(err, errNoDaemon) {
		t.Fatalf("Lookup with no socket = %v", err)
	}
	if calls, _, closes := f.count(); calls != 0 || closes != 1 {
		t.Fatalf("asked a daemon that has no socket %d times, closed the client %d times", calls, closes)
	}
	if logs.Len() != 0 {
		t.Fatalf("a host with no daemon said %v at Info", logs.All())
	}
}

// A container created from an image ID keeps the ID: it names no
// repository. A short all-hex repository name is a name.
func TestIndexKeepsAnImageID(t *testing.T) {
	f := newFakeDocker()
	f.set("byid", running("feed1234abcd", "sha256:feed1234abcdef00", nil, nat.PortMap{"80/tcp": published("0.0.0.0", "40090")}))
	f.set("bysha", running("sha256:feed1234abcd", "sha256:feed1234abcdef00", nil, nat.PortMap{"80/tcp": published("0.0.0.0", "40091")}))
	f.set("named", running("cafe", "sha256:cafe0123", nil, nat.PortMap{"80/tcp": published("0.0.0.0", "40092")}))
	r, applied := newIndex(f)
	r.Watch(t.Context(), nil)
	t.Cleanup(func() { stopIndex(r) })
	<-applied // the two listed by image ID are read on their own
	<-applied
	for dst, want := range map[string]string{
		"127.0.0.1:40090": "feed1234abcd",
		"127.0.0.1:40091": "sha256:feed1234abcd",
		"127.0.0.1:40092": "cafe:latest",
	} {
		if c, _ := lookup(t, r, dst); c.Image != want {
			t.Errorf("%s: image = %q, want %q", dst, c.Image, want)
		}
	}
}

// A daemon on another machine (Docker in Docker, a remote DOCKER_HOST) is
// not read: its ports and IPs are not this host's.
func TestIndexReadsOnlyThisHostsDaemon(t *testing.T) {
	addrs := func() ([]netip.Addr, error) { return []netip.Addr{netip.MustParseAddr("192.168.1.20")}, nil }
	for endpoint, want := range map[string]bool{
		"unix:///var/run/docker.sock":       true,
		"unix:///run/user/1000/docker.sock": true,
		"npipe:////./pipe/docker_engine":    true,
		"tcp://127.0.0.1:2375":              true,
		"tcp://localhost:2375":              true,
		"tcp://192.168.1.20:2376":           true,
		"tcp://docker:2375":                 false,
		"tcp://10.0.0.9:2375":               false,
		"ssh://user@build-host":             false,
		"not a url":                         false,
	} {
		if got := onThisHost(endpoint, addrs); got != want {
			t.Errorf("onThisHost(%q) = %v, want %v", endpoint, got, want)
		}
	}
	f := newFakeDocker()
	f.host = "tcp://docker:2375"
	r := &dockerIndex{newAPI: func(string) (dockerAPI, error) { return f, nil }, hostAddrs: addrs}
	r.Watch(t.Context(), nil)
	if _, _, err := r.Lookup(netip.MustParseAddrPort("127.0.0.1:1")); err == nil {
		t.Fatal("a remote daemon was taken for this host's")
	}
	if calls, _, closes := f.count(); calls != 0 || closes != 1 {
		t.Fatalf("a remote daemon was asked %d times and its client closed %d times", calls, closes)
	}
	if r.Endpoint() != "tcp://docker:2375" {
		t.Fatalf("Endpoint() = %q", r.Endpoint())
	}
}

func TestSetDockerHostPointsTheIndexAtIt(t *testing.T) {
	r := newDockerIndex()
	saved := Default
	Default = NewKeyer(r)
	t.Cleanup(func() { Default = saved })
	SetDockerHost("unix:///run/user/1000/docker.sock")
	if got := r.Endpoint(); got != "unix:///run/user/1000/docker.sock" {
		t.Fatalf("Endpoint() = %q", got)
	}
}

// Under the switch the native agent is told the tests' Docker endpoint:
// DOCKER_HOST, else the docker CLI's current context. Without the switch,
// nothing.
func TestAgentArgsCarryTheDockerEndpointOnlyUnderTheSwitch(t *testing.T) {
	saved := contextEndpoint
	t.Cleanup(func() { contextEndpoint = saved })
	contextEndpoint = func() string { return "unix:///home/u/.docker/desktop/docker.sock" }

	asked := 0
	contextEndpoint = func() string { asked++; return "unix:///home/u/.docker/desktop/docker.sock" }
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("KEPLOY_PROVEN", "")
	if got := AgentArgs(); got != nil || asked != 0 {
		t.Fatalf("switch off: AgentArgs() = %q, the docker CLI asked %d times", got, asked)
	}
	t.Setenv("DOCKER_HOST", "unix:///run/user/1000/docker.sock")
	t.Setenv("KEPLOY_PROVEN", "1")
	if got := AgentArgs(); len(got) != 1 || got[0] != "--docker-host=unix:///run/user/1000/docker.sock" {
		t.Fatalf("DOCKER_HOST: AgentArgs() = %q", got)
	}
	t.Setenv("DOCKER_HOST", "")
	if got := AgentArgs(); len(got) != 1 || got[0] != "--docker-host=unix:///home/u/.docker/desktop/docker.sock" {
		t.Fatalf("the current context: AgentArgs() = %q", got)
	}
	contextEndpoint = func() string { return "" }
	if got := AgentArgs(); got != nil {
		t.Fatalf("no endpoint known: AgentArgs() = %q", got)
	}
}

// A container the list shows by image ID is read off the first list's way:
// one Docker holds locked never holds up a session's start, nor the other
// containers.
func TestAStuckImageIDReadHoldsUpNoSession(t *testing.T) {
	f := testcontainers()
	stuck := make(chan struct{})
	f.block["moved"] = stuck
	f.set("moved", running("sha256:abcdef0123456789", "sha256:abcdef0123456789aa", nil, nat.PortMap{"80/tcp": published("0.0.0.0", "40100")}))
	r, applied := newIndex(f)
	t.Cleanup(func() { stopIndex(r) })
	r.Watch(t.Context(), nil) // returns with the read still stuck
	if _, ok := lookup(t, r, "127.0.0.1:49153"); !ok {
		t.Fatal("the listed containers are not indexed while an image-ID read is stuck")
	}
	close(stuck)
	if id := <-applied; id != "moved" {
		t.Fatalf("applied %q", id)
	}
	if c, ok := lookup(t, r, "127.0.0.1:40100"); !ok || c.Image != "sha256:abcdef0123456789" {
		t.Fatalf("the image-ID container once read = %+v, %v", c, ok)
	}
}

// One read per container at a time: events that come while it is stuck
// mark it to be read once more, never another read beside it.
func TestOneReadPerContainerAtATime(t *testing.T) {
	f := newFakeDocker()
	r, applied := watching(t, f)
	stuck := make(chan struct{})
	f.mu.Lock()
	f.block["x"] = stuck
	f.mu.Unlock()
	f.set("x", running("postgres:16", "sha256:x", nil, nat.PortMap{"5432/tcp": published("0.0.0.0", "49401")}))
	_, before, _ := f.count()
	f.msgs <- start("x")
	<-f.entered // x's read is in flight, as of its first start
	for i := 0; i < 4; i++ {
		f.msgs <- start("x")
	}
	f.msgs <- start("fence")
	<-applied // fence: every start of x before it was taken in
	if _, inspects, _ := f.count(); inspects != before+2 {
		t.Fatalf("%d inspects for five starts of a stuck container and a fence, want one each", inspects-before)
	}
	f.mu.Lock()
	delete(f.block, "x")
	f.mu.Unlock()
	close(stuck)
	<-applied // the stuck read: superseded by the starts after it
	<-applied // the one read again
	if _, inspects, _ := f.count(); inspects != before+3 {
		t.Fatalf("%d inspects, want the stuck read, the fence and one more", inspects-before)
	}
	if _, ok := lookup(t, r, "127.0.0.1:49401"); !ok {
		t.Fatal("x is not indexed after its read ran again")
	}
}

// A destroyed container's event count goes (no event names it again), at
// once, or when a read of it in flight ends; a container's create is passed
// over.
func TestADestroyedContainerLeavesNoCount(t *testing.T) {
	f := newFakeDocker()
	r, applied := watching(t, f)
	f.msgs <- events.Message{Type: events.ContainerEventType, Action: events.ActionCreate, Actor: events.Actor{ID: "made"}}
	f.set("a", running("redis:7", "sha256:a", nil, nat.PortMap{}))
	f.msgs <- start("a")
	<-applied
	f.remove("a")
	f.msgs <- die("a")
	f.msgs <- events.Message{Type: events.ContainerEventType, Action: events.ActionDestroy, Actor: events.Actor{ID: "a"}}

	stuck := make(chan struct{})
	f.mu.Lock()
	f.block["b"] = stuck
	f.mu.Unlock()
	f.set("b", running("redis:7", "sha256:b", nil, nat.PortMap{}))
	f.msgs <- start("b")
	f.remove("b")
	f.msgs <- events.Message{Type: events.ContainerEventType, Action: events.ActionDestroy, Actor: events.Actor{ID: "b"}}
	f.msgs <- start("fence")
	<-applied
	st := current(r)
	r.mu.Lock()
	_, a := st.seq["a"]
	_, b := st.seq["b"]
	_, made := st.seq["made"]
	r.mu.Unlock()
	if a || made || !b {
		t.Fatalf("counts after a destroy: a=%v create=%v b(read in flight)=%v", a, made, b)
	}
	close(stuck)
	<-applied
	r.mu.Lock()
	_, b = st.seq["b"]
	r.mu.Unlock()
	if b {
		t.Fatal("b's count stayed after its read ended")
	}
}

// A container the first list has but that died after the stream began is
// left to its die: the list does not bring it back.
func TestAListedContainerLosesToALaterDie(t *testing.T) {
	f := newFakeDocker()
	f.set("dead", running("postgres:16", "sha256:d", nil, nat.PortMap{"5432/tcp": published("0.0.0.0", "49501")}))
	r, applied := newIndex(f)
	t.Cleanup(func() { stopIndex(r) })
	f.mu.Lock()
	f.beforeList = func() {
		f.msgs <- die("dead")
		f.msgs <- start("fence")
		<-applied // the die is in
	}
	f.mu.Unlock()
	r.Watch(t.Context(), nil)
	if _, ok := lookup(t, r, "127.0.0.1:49501"); ok {
		t.Fatal("the first list brought back a container that had died since the stream began")
	}
}

// A session whose context ends just as the list comes in never fails the
// watch the list made good: the next session keeps it.
func TestASessionEndingAsTheListComesInLeavesItsWatch(t *testing.T) {
	for i := 0; i < 64; i++ {
		f := newFakeDocker()
		release := make(chan struct{})
		f.listGate = release
		r, _ := newIndex(f)
		ctx, cancel := context.WithCancel(t.Context())
		core := hookCore(func() {
			close(release)
			<-current(r).synced
			cancel()
		})
		r.Watch(ctx, zap.New(core))
		if _, _, err := r.Lookup(netip.MustParseAddrPort("127.0.0.1:1")); err != nil {
			t.Fatalf("round %d: a session that ended as its list came in failed the watch: %v", i, err)
		}
		stopIndex(r)
	}
}

// Every read outcome and a watch's end are said at Debug, so whoever waits
// for a container can tell it will not be indexed.
func TestTheIndexSaysWhatItDidWithAContainer(t *testing.T) {
	core, logs := observer.New(zap.DebugLevel)
	f := newFakeDocker()
	r, applied := newIndex(f)
	r.Watch(t.Context(), zap.New(core))
	f.msgs <- start("gone")
	<-applied
	if logs.FilterMessage("did not index a container").FilterField(zap.String("containerID", "gone")).Len() != 1 {
		t.Fatalf("a container found gone was not said: %v", logs.All())
	}
	st := current(r)
	f.endStream(errors.New("unexpected EOF"))
	<-st.done // after the stream's reader, which says it
	if logs.FilterMessage("the container index stopped").Len() != 1 {
		t.Fatalf("the watch's end was not said: %v", logs.All())
	}
}

// A container that dies and is destroyed as a watch begins, before its
// first list reaches it, stays out: the list's add loses to the destroy.
func TestADestroyedContainerStaysOutOfTheFirstList(t *testing.T) {
	f := newFakeDocker()
	f.set("gone", running("postgres:16", "sha256:g", []string{"172.17.0.40"}, nat.PortMap{"5432/tcp": published("0.0.0.0", "49601")}))
	r, applied := newIndex(f)
	t.Cleanup(func() { stopIndex(r) })
	f.mu.Lock()
	f.beforeList = func() {
		f.msgs <- die("gone")
		f.msgs <- events.Message{Type: events.ContainerEventType, Action: events.ActionDestroy, Actor: events.Actor{ID: "gone"}}
		f.msgs <- start("fence")
		<-applied // the die and the destroy are in
	}
	f.mu.Unlock()
	r.Watch(t.Context(), nil)
	for _, dst := range []string{"127.0.0.1:49601", "172.17.0.40:5432"} {
		if _, ok := lookup(t, r, dst); ok {
			t.Fatalf("%s: the first list brought back a container destroyed as the watch began", dst)
		}
	}
	st := current(r)
	r.mu.Lock()
	_, kept := st.seq["gone"]
	r.mu.Unlock()
	if kept {
		t.Fatal("the destroyed container's count stayed after the first list")
	}
}

// hookCore is a logger core that runs fn on the first entry at Info: the
// moment Watch says it waits.
func hookCore(fn func()) zapcore.Core {
	var once sync.Once
	core, _ := observer.New(zap.InfoLevel)
	return zapcore.RegisterHooks(core, func(e zapcore.Entry) error {
		if e.Level == zapcore.InfoLevel {
			once.Do(fn)
		}
		return nil
	})
}
