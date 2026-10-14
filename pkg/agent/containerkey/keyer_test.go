package containerkey

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// fakeIndex answers from a table of destinations, or with err.
type fakeIndex struct {
	at      map[string]Container // "ip:port" -> container
	err     error
	looks   int
	watches int
}

func (f *fakeIndex) Lookup(dst netip.AddrPort) (Container, bool, error) {
	f.looks++
	if f.err != nil {
		return Container{}, false, f.err
	}
	c, ok := f.at[dst.String()]
	return c, ok, nil
}

func (f *fakeIndex) Bind(context.Context)               {}
func (f *fakeIndex) Watch(context.Context, *zap.Logger) { f.watches++ }
func (f *fakeIndex) Endpoint() string                   { return "unix:///fake.sock" }

var (
	pgSuite = Container{ID: "suite", Image: "postgres:16", Port: 5432}
	pgA     = Container{ID: "a", Image: "postgres:16", Port: 5432}
	pgB     = Container{ID: "b", Image: "postgres:16", Port: 5432}
	redis   = Container{ID: "r", Image: "redis:7", Port: 6379}
)

func newFake() *fakeIndex {
	return &fakeIndex{at: map[string]Container{
		"127.0.0.1:40001": pgSuite,
		"127.0.0.1:40002": pgA,
		"127.0.0.1:40003": pgB,
		"172.17.0.9:5432": pgB, // pgB at its own IP
		"127.0.0.1:40004": redis,
		"[::1]:40002":     pgA,
	}}
}

func name(t *testing.T, k *Keyer, connID, dst string) string {
	t.Helper()
	_, key, _ := k.Name(zap.NewNop(), connID, dst)
	return key
}

func stamped(k *Keyer, connID string) string {
	m := &models.Mock{ConnectionID: connID}
	k.Stamp(m)
	return m.Spec.Metadata[MetadataKey]
}

func TestKeyerNumbersAnImagesContainersByFirstConnection(t *testing.T) {
	k := NewKeyer(newFake())
	if got := name(t, k, "1", "127.0.0.1:40002"); got != "container:postgres:16:5432#1" {
		t.Fatalf("first container = %q", got)
	}
	if got := name(t, k, "2", "127.0.0.1:40003"); got != "container:postgres:16:5432#2" {
		t.Fatalf("second container = %q", got)
	}
	// Another connection to the first one, by another address of it, keeps
	// its number; so does the second at its own IP.
	if got := name(t, k, "3", "[::1]:40002"); got != "container:postgres:16:5432#1" {
		t.Fatalf("first container again = %q", got)
	}
	if got := name(t, k, "4", "172.17.0.9:5432"); got != "container:postgres:16:5432#2" {
		t.Fatalf("second container at its IP = %q", got)
	}
	// Each image counts its own containers.
	if got := name(t, k, "5", "127.0.0.1:40004"); got != "container:redis:7:6379#1" {
		t.Fatalf("other image = %q", got)
	}
}

// A destination the index does not know when the connection is accepted is
// named by its address for the connection's life, even if the index learns it
// later: a connection's key never changes, and nothing waits for Docker.
// Only container connections are held.
func TestKeyerNamesOnlyAtAccept(t *testing.T) {
	f := newFake()
	k := NewKeyer(f)
	for _, dst := range []string{"127.0.0.1:9", "not-an-address", "", "127.0.0.1:99999", "127.0.0.1:40050"} {
		if c, key, ok := k.Name(zap.NewNop(), "x"+dst, dst); ok || key != "" || c != (Container{}) {
			t.Fatalf("Name(%q) = %+v, %q, %v", dst, c, key, ok)
		}
	}
	f.at["127.0.0.1:40050"] = pgA
	if got := stamped(k, "x127.0.0.1:40050"); got != "" {
		t.Fatalf("a connection named by its address was stamped %q later", got)
	}
	if len(k.conns) != 0 {
		t.Fatalf("the keyer holds %d connections that are not containers", len(k.conns))
	}
}

// A test's own containers are numbered within the test, so each test that
// starts its own container gets #2 after the suite's #1, as it does when it
// runs alone; the suite's container keeps #1 inside every test.
func TestKeyerNumbersEachTestOnItsOwnAfterTheRun(t *testing.T) {
	k := NewKeyer(newFake())
	if got := name(t, k, "boot", "127.0.0.1:40001"); got != "container:postgres:16:5432#1" {
		t.Fatalf("suite container = %q", got)
	}

	k.BeginTest()
	if got := name(t, k, "t1-own", "127.0.0.1:40002"); got != "container:postgres:16:5432#2" {
		t.Fatalf("test 1's own container = %q", got)
	}
	if got := name(t, k, "t1-suite", "127.0.0.1:40001"); got != "container:postgres:16:5432#1" {
		t.Fatalf("suite container inside test 1 = %q", got)
	}
	k.EndTest()

	k.BeginTest()
	if got := name(t, k, "t2-own", "127.0.0.1:40003"); got != "container:postgres:16:5432#2" {
		t.Fatalf("test 2's own container = %q, want the number test 1's had", got)
	}
	k.EndTest()

	// Outside tests the run numbers on from its own: the tests' containers
	// are new to it.
	if got := name(t, k, "after", "127.0.0.1:40003"); got != "container:postgres:16:5432#2" {
		t.Fatalf("run scope after the tests = %q", got)
	}

	alone := NewKeyer(newFake())
	_ = name(t, alone, "boot", "127.0.0.1:40001")
	alone.BeginTest()
	if got := name(t, alone, "t2-own", "127.0.0.1:40003"); got != "container:postgres:16:5432#2" {
		t.Fatalf("test 2 run alone = %q", got)
	}
}

func TestKeyerStampsByConnectionWhileItIsOpen(t *testing.T) {
	k := NewKeyer(newFake())
	_ = name(t, k, "7", "127.0.0.1:40004")

	if got := stamped(k, "7"); got != "container:redis:7:6379#1" {
		t.Fatalf("by ConnectionID = %q", got)
	}
	// HTTP's recorder carries the connection in its metadata instead.
	byMeta := &models.Mock{Spec: models.MockSpec{Metadata: map[string]string{"connID": "7", "operation": "GET"}}}
	k.Stamp(byMeta)
	if got := byMeta.Spec.Metadata[MetadataKey]; got != "container:redis:7:6379#1" || byMeta.Spec.Metadata["operation"] != "GET" {
		t.Fatalf("by metadata connID = %v", byMeta.Spec.Metadata)
	}
	if got := stamped(k, "8"); got != "" {
		t.Fatalf("another connection's mock was stamped %q", got)
	}
	k.Stamp(nil)
	k.Stamp(&models.Mock{})

	// A closed connection is gone, so the keyer holds only open ones.
	k.Forget("7")
	if got := stamped(k, "7"); got != "" || len(k.conns) != 0 {
		t.Fatalf("after Forget: stamped %q, %d connections held", got, len(k.conns))
	}

	_ = name(t, k, "9", "127.0.0.1:40003")
	k.Reset()
	if got := stamped(k, "9"); got != "" {
		t.Fatalf("a reset keyer still stamped %q", got)
	}
	if got := name(t, k, "10", "127.0.0.1:40003"); got != "container:postgres:16:5432#1" {
		t.Fatalf("numbering after Reset = %q", got)
	}
}

// The parsers reach their connection's key through their context.
func TestFromContextIsTheConnectionsKey(t *testing.T) {
	k := NewKeyer(newFake())
	_ = name(t, k, "1", "127.0.0.1:40002")
	if got, ok := FromContext(WithConn(context.Background(), k, "1")); !ok || got != "container:postgres:16:5432#1" {
		t.Fatalf("FromContext = %q, %v", got, ok)
	}
	if _, ok := FromContext(WithConn(context.Background(), k, "2")); ok {
		t.Fatal("an unnamed connection has a key")
	}
	if _, ok := FromContext(context.Background()); ok {
		t.Fatal("a context with no connection has a key")
	}
}

func TestKeyerSaysOncePerSessionThatDockerCannotBeRead(t *testing.T) {
	core, logs := observer.New(zap.DebugLevel)
	logger := zap.New(core)
	f := &fakeIndex{err: errors.New("dial unix /var/run/docker.sock: connect: permission denied")}
	k := NewKeyer(f)
	for _, id := range []string{"1", "2", "3"} {
		if _, key, ok := k.Name(logger, id, "127.0.0.1:40002"); ok || key != "" {
			t.Fatalf("named %q with Docker unreadable", key)
		}
	}
	warns := logs.FilterLevelExact(zapcore.WarnLevel)
	if warns.Len() != 1 {
		t.Fatalf("warned %d times, want once", warns.Len())
	}
	if e := warns.All()[0]; e.ContextMap()["dockerEndpoint"] != "unix:///fake.sock" {
		t.Fatalf("the warning does not name the endpoint: %v", e.ContextMap())
	}
	k.Reset()
	_, _, _ = k.Name(logger, "1", "127.0.0.1:40002")
	if logs.FilterLevelExact(zapcore.WarnLevel).Len() != 2 {
		t.Fatal("a new session did not warn again")
	}

	// A host with no Docker daemon socket never warns (the agent says it
	// once, at Info), and the agent stopping says nothing.
	for _, quiet := range []error{fmt.Errorf("%w at /var/run/docker.sock", errNoDaemon), context.Canceled} {
		core, logs := observer.New(zap.DebugLevel)
		f.err = quiet
		k := NewKeyer(f)
		_, _, _ = k.Name(zap.New(core), "1", "127.0.0.1:40002")
		if logs.FilterLevelExact(zapcore.WarnLevel).Len() != 0 {
			t.Fatalf("%v warned", quiet)
		}
	}
}

// Only a session start reads Docker; naming a connection never does.
func TestKeyerWatchesOnlyWhenTold(t *testing.T) {
	f := newFake()
	k := NewKeyer(f)
	_ = name(t, k, "1", "127.0.0.1:40002")
	if f.watches != 0 {
		t.Fatalf("naming a connection started the index %d times", f.watches)
	}
	k.Watch(t.Context(), zap.NewNop())
	if f.watches != 1 {
		t.Fatalf("Watch started the index %d times", f.watches)
	}
}

func TestParseAddrPort(t *testing.T) {
	for in, want := range map[string]string{
		"127.0.0.1:80":        "127.0.0.1:80",
		"[::1]:80":            "[::1]:80",
		"[::ffff:10.0.0.1]:9": "10.0.0.1:9",
	} {
		got, err := parseAddrPort(in)
		if err != nil || got.String() != want {
			t.Fatalf("parseAddrPort(%q) = %v, %v", in, got, err)
		}
	}
	for _, in := range []string{"", "127.0.0.1", "host:80", "127.0.0.1:x"} {
		if _, err := parseAddrPort(in); err == nil {
			t.Fatalf("parseAddrPort(%q) read it", in)
		}
	}
}

// Stamp never writes into the map the parser gave: a parser may hand several
// mocks one metadata map, and the outgoing stream may be encoding an earlier
// one while a later one is stamped.
func TestStampCopiesTheMetadata(t *testing.T) {
	k := NewKeyer(newFake())
	_ = name(t, k, "7", "127.0.0.1:40004")
	shared := map[string]string{"connID": "7", "type": "config"}
	first := &models.Mock{Spec: models.MockSpec{Metadata: shared}}
	k.Stamp(first)
	if _, ok := shared[MetadataKey]; ok {
		t.Fatal("Stamp wrote into the parser's metadata map")
	}
	if first.Spec.Metadata[MetadataKey] != "container:redis:7:6379#1" || first.Spec.Metadata["type"] != "config" {
		t.Fatalf("the stamped mock's metadata = %v", first.Spec.Metadata)
	}
}

// A host with no Docker daemon socket is said once per agent at Info, then
// at Debug: tests that reach a daemon keploy cannot see are not silently
// unkeyed, and a run with no Docker is not noisy.
func TestNoDaemonIsSaidOncePerAgentAtInfo(t *testing.T) {
	noDaemonSaid.Store(false)
	t.Cleanup(func() { noDaemonSaid.Store(false) })
	core, logs := observer.New(zap.DebugLevel)
	f := &fakeIndex{err: fmt.Errorf("%w at /var/run/docker.sock", errNoDaemon)}
	for i := 0; i < 3; i++ {
		k := NewKeyer(f) // a session each
		_, _, _ = k.Name(zap.New(core), "1", "127.0.0.1:40002")
	}
	if n := logs.FilterLevelExact(zapcore.InfoLevel).Len(); n != 1 {
		t.Fatalf("said %d times at Info, want once", n)
	}
	if n := logs.FilterLevelExact(zapcore.DebugLevel).Len(); n != 2 {
		t.Fatalf("said %d times at Debug, want the other two sessions", n)
	}
}

// Lookup only asks: it keys nothing and says nothing, so the keys Name gives
// after it, and the warning that Docker cannot be read, are what they are
// without it.
func TestLookupChangesNothingNameGivesOrSays(t *testing.T) {
	asked, plain := NewKeyer(newFake()), NewKeyer(newFake())
	if c, ok, err := asked.Lookup("127.0.0.1:40003"); err != nil || !ok || c != pgB {
		t.Fatalf("Lookup = %v, %v, %v; want pgB", c, ok, err)
	}
	if _, ok, err := asked.Lookup("not an address"); ok || err != nil {
		t.Fatalf("Lookup of a bad address = %v, %v", ok, err)
	}
	for i, dst := range []string{"127.0.0.1:40002", "127.0.0.1:40003"} {
		id := fmt.Sprint(i)
		if got, want := name(t, asked, id, dst), name(t, plain, id, dst); got != want {
			t.Fatalf("%s: keyed %q after a Lookup, %q without", dst, got, want)
		}
	}
	if stamped(asked, "never-named") != "" {
		t.Fatal("a Lookup held a key")
	}

	core, logs := observer.New(zap.DebugLevel)
	f := newFake()
	f.err = errors.New("dial unix /var/run/docker.sock: connect: permission denied")
	k := NewKeyer(f)
	if _, _, err := k.Lookup("127.0.0.1:40002"); err == nil {
		t.Fatal("Lookup hid that Docker cannot be read")
	}
	_, _, _ = k.Name(zap.New(core), "1", "127.0.0.1:40002")
	if logs.FilterLevelExact(zapcore.WarnLevel).Len() != 1 {
		t.Fatal("a Lookup before the session's first Name swallowed its warning")
	}
}
