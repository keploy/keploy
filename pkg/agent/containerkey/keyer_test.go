package containerkey

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// fakeIndex answers from a table of destinations, or with err. A container's
// fingerprint is its Container's, else fps'; one in pending waits until its
// channel closes, saying so on asked. reads is the containers it is reading
// (Pending), each until its channel closes (AwaitReadsOf); awaited counts the
// waits for them; noContainerAt is addresses no container can be at.
type fakeIndex struct {
	mu            sync.Mutex
	at            map[string]Container // "ip:port" -> container
	fps           map[string]string
	pending       map[string]chan struct{}
	asked         chan string
	err           error
	looks         int
	watches       int
	reads         map[string]chan struct{}
	awaited       int
	awaitedCh     chan struct{} // told each AwaitReadsOf, if set
	noContainerAt map[netip.Addr]bool
}

func (f *fakeIndex) Lookup(dst netip.AddrPort) (Container, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.looks++
	if f.err != nil {
		return Container{}, false, f.err
	}
	c, ok := f.at[dst.String()]
	return c, ok, nil
}

func (f *fakeIndex) Fingerprint(ctx context.Context, id string) (string, bool) {
	f.mu.Lock()
	wait := f.pending[id]
	asked := f.asked
	f.mu.Unlock()
	if wait != nil {
		if asked != nil {
			asked <- id
		}
		select {
		case <-wait:
		case <-ctx.Done():
			return "", false
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	fp, ok := f.fps[id]
	return fp, ok
}

func (f *fakeIndex) Bind(context.Context) {}
func (f *fakeIndex) Pending(dst netip.AddrPort) ([]string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var ids []string
	for id, ch := range f.reads {
		select {
		case <-ch:
		default:
			ids = append(ids, id)
		}
	}
	return ids, !f.noContainerAt[dst.Addr()]
}

func (f *fakeIndex) AwaitReadsOf(ctx context.Context, ids []string) {
	f.mu.Lock()
	f.awaited++
	if f.awaitedCh != nil {
		f.awaitedCh <- struct{}{}
	}
	var chans []chan struct{}
	for _, id := range ids {
		if ch := f.reads[id]; ch != nil {
			chans = append(chans, ch)
		}
	}
	f.mu.Unlock()
	for _, ch := range chans {
		select {
		case <-ch:
		case <-ctx.Done():
			return
		}
	}
}

func (f *fakeIndex) Watch(context.Context, *zap.Logger) { f.watches++ }
func (f *fakeIndex) Endpoint() string                   { return "unix:///fake.sock" }

const (
	fpDefault = "aaaaaaaaaaaa" // postgres as the suite runs it
	fpOther   = "bbbbbbbbbbbb" // postgres with another database
	fpRedis   = "cccccccccccc"
)

var (
	pgSuite = Container{ID: "suite", Image: "postgres:16", Port: 5432, Fingerprint: fpDefault}
	pgA     = Container{ID: "a", Image: "postgres:16", Port: 5432, Fingerprint: fpDefault}
	pgB     = Container{ID: "b", Image: "postgres:15", Port: 5432, Fingerprint: fpOther}
	pgTwin  = Container{ID: "twin", Image: "postgres:16", Port: 5432, Fingerprint: fpDefault}
	redis   = Container{ID: "r", Image: "redis:7", Port: 6379, Fingerprint: fpRedis}
)

func newFake() *fakeIndex {
	return &fakeIndex{at: map[string]Container{
		"127.0.0.1:40001": pgSuite,
		"127.0.0.1:40002": pgA,
		"127.0.0.1:40003": pgB,
		"172.17.0.9:5432": pgB, // pgB at its own IP
		"127.0.0.1:40004": redis,
		"127.0.0.1:40005": pgTwin,
		"[::1]:40002":     pgA,
	}, fps: map[string]string{}, pending: map[string]chan struct{}{}}
}

const (
	keyDefault = "container:postgres:5432@" + fpDefault
	keyOther   = "container:postgres:5432@" + fpOther
	keyRedis   = "container:redis:6379@" + fpRedis
)

func name(t *testing.T, k *Keyer, connID, dst string) string {
	t.Helper()
	_, key, _ := k.Name(zap.NewNop(), connID, dst)
	return key
}

// use is a call matched on connection connID now.
func use(k *Keyer, connID string) (string, State) {
	return StateFromContext(WithConn(context.Background(), k, connID))
}

func stamped(k *Keyer, connID string) string {
	m := &models.Mock{ConnectionID: connID}
	k.Stamp(m)
	return m.Spec.Metadata[MetadataKey]
}

// A container is named by its repository, its port and its configuration:
// the same whichever address or tag it is reached by, and whichever order
// the tests start or use containers in.
func TestKeyerNamesAContainerByItsConfiguration(t *testing.T) {
	k := NewKeyer(newFake())
	for _, c := range []struct{ conn, dst, want string }{
		{"1", "127.0.0.1:40002", keyDefault},
		{"2", "127.0.0.1:40003", keyOther}, // postgres:15, configured otherwise
		{"3", "[::1]:40002", keyDefault},   // another address of the first
		{"4", "172.17.0.9:5432", keyOther}, // the second at its own IP
		{"5", "127.0.0.1:40004", keyRedis},
	} {
		if got := name(t, k, c.conn, c.dst); got != c.want {
			t.Fatalf("Name(%s) = %q, want %q", c.dst, got, c.want)
		}
	}
	// In the other order the keys are the same: no number to swap.
	other := NewKeyer(newFake())
	if got := name(t, other, "2", "127.0.0.1:40003"); got != keyOther {
		t.Fatalf("second container first = %q", got)
	}
	if got := name(t, other, "1", "127.0.0.1:40002"); got != keyDefault {
		t.Fatalf("first container second = %q", got)
	}
}

// A destination the index does not place when the connection is accepted is
// looked up again at the connection's first use (a call matched, a mock
// stamped), so a container Docker started a moment before is still named;
// one the index never places stays no container's, and so does an address
// no container could be at. Only container connections are held.
func TestKeyerNamesAContainerReadLateAtItsFirstUse(t *testing.T) {
	f := newFake()
	k := NewKeyer(f)
	for _, dst := range []string{"127.0.0.1:9", "not-an-address", "", "127.0.0.1:99999", "127.0.0.1:40050", "93.184.216.34:443"} {
		if c, key, ok := k.Name(zap.NewNop(), "x"+dst, dst); ok || key != "" || c != (Container{}) {
			t.Fatalf("Name(%q) = %+v, %q, %v", dst, c, key, ok)
		}
	}
	if len(k.unplaced) != 2 {
		t.Fatalf("the keyer holds %d unplaced connections, want the two to a loopback address", len(k.unplaced))
	}
	f.mu.Lock()
	f.at["127.0.0.1:40050"] = pgA
	f.at["93.184.216.34:443"] = pgB
	f.mu.Unlock()
	if got := stamped(k, "x127.0.0.1:40050"); got != keyDefault {
		t.Fatalf("a connection to a container the index placed after its accept was stamped %q", got)
	}
	if key, st := use(k, "x127.0.0.1:40050"); st != Keyed || key != keyDefault {
		t.Fatalf("its use = %q, %v", key, st)
	}
	if key, st := use(k, "x127.0.0.1:9"); st != NotContainer || key != "" {
		t.Fatalf("a destination the index never placed = %q, %v", key, st)
	}
	if key, st := use(k, "x93.184.216.34:443"); st != NotContainer || key != "" {
		t.Fatalf("a public address was looked up again: %q, %v", key, st)
	}
	if len(k.unplaced) != 0 {
		t.Fatalf("the keyer holds %d unplaced connections after their first uses, want none", len(k.unplaced))
	}
	_ = name(t, k, "closed", "127.0.0.1:41000")
	k.Forget("closed")
	if len(k.unplaced) != 0 {
		t.Fatalf("a closed unplaced connection is still held")
	}
}

// A connection the index did not place is looked up again once, at its first
// use: a later use neither waits nor looks it up again, so a native service
// on this host costs each call nothing.
func TestKeyerLooksAConnectionUpAgainOnce(t *testing.T) {
	f := newFake()
	k := NewKeyer(f)
	_ = name(t, k, "native", "127.0.0.1:6000")
	if _, st := use(k, "native"); st != NotContainer {
		t.Fatalf("first use = %v", st)
	}
	f.mu.Lock()
	looks := f.looks
	f.at["127.0.0.1:6000"] = pgA // a container the index placed there since
	f.mu.Unlock()
	for i := 0; i < 3; i++ {
		if _, st := use(k, "native"); st != NotContainer {
			t.Fatalf("use %d = %v, want the first use's outcome", i+2, st)
		}
	}
	if m := (&models.Mock{ConnectionID: "native"}); true {
		k.Stamp(m)
		if m.Spec.Metadata[InstanceKey] != "" {
			t.Fatalf("stamped %v, want nothing of a container", m.Spec.Metadata)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.looks != looks {
		t.Fatalf("later uses looked the destination up %d more times", f.looks-looks)
	}
}

// The first use waits for the reads that were in flight when the connection
// was accepted, and for no read started since: containers that keep starting
// never hold a call.
func TestKeyerWaitsOnlyForTheReadsInFlightAtTheAccept(t *testing.T) {
	f := newFake()
	early := make(chan struct{})
	f.reads = map[string]chan struct{}{"early": early}
	k := NewKeyer(f)
	_ = name(t, k, "c1", "127.0.0.1:40060")
	// A read that starts after the accept, and does not end while c1 is used.
	late := make(chan struct{})
	f.mu.Lock()
	f.reads["late"] = late
	f.mu.Unlock()
	got := make(chan State, 1)
	go func() {
		_, st := use(k, "c1")
		got <- st
	}()
	// The container the early read was of is at the destination once it ends.
	f.mu.Lock()
	f.at["127.0.0.1:40060"] = pgA
	f.mu.Unlock()
	close(early)
	if st := <-got; st != Keyed {
		t.Fatalf("use = %v, want the container the early read placed", st)
	}
	// No read in flight at its accept: its first use waits for none.
	close(late)
	f.mu.Lock()
	awaited := f.awaited
	f.mu.Unlock()
	_ = name(t, k, "c2", "127.0.0.1:40061")
	if _, st := use(k, "c2"); st != NotContainer {
		t.Fatalf("use = %v", st)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.awaited != awaited {
		t.Fatalf("a use with no read in flight at its accept waited for reads")
	}
}

// Uses that come while a connection's one look-up again waits take its
// outcome: one call on a connection does not see a container another misses.
func TestKeyerUsesWhileALookUpRunsTakeItsOutcome(t *testing.T) {
	f := newFake()
	read := make(chan struct{})
	f.reads = map[string]chan struct{}{"r": read}
	f.awaitedCh = make(chan struct{}, 4)
	k := NewKeyer(f)
	_ = name(t, k, "c1", "127.0.0.1:40070")
	first := make(chan State, 1)
	go func() {
		_, st := use(k, "c1")
		first <- st
	}()
	<-f.awaitedCh // the second use starts once the first is waiting
	second := make(chan State, 1)
	go func() {
		_, st := use(k, "c1")
		second <- st
	}()
	f.mu.Lock()
	f.at["127.0.0.1:40070"] = pgA
	f.mu.Unlock()
	close(read)
	if a, b := <-first, <-second; a != Keyed || b != Keyed {
		t.Fatalf("uses = %v, %v, want both the container", a, b)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.awaited != 1 {
		t.Fatalf("waited for the reads %d times, want once", f.awaited)
	}
}

// An address no container can be at by the index (a docker-mode agent's
// loopback, the app container's own) is not looked up again at all.
func TestKeyerDoesNotLookUpAgainWhereNoContainerCanBe(t *testing.T) {
	f := newFake()
	f.noContainerAt = map[netip.Addr]bool{netip.MustParseAddr("127.0.0.1"): true}
	k := NewKeyer(f)
	_ = name(t, k, "c1", "127.0.0.1:6000")
	if len(k.unplaced) != 0 {
		t.Fatalf("held %d unplaced connections", len(k.unplaced))
	}
}

// Two containers configured the same have one key and cannot be told apart.
// Where one scope uses both, the key is withheld from every use of either
// from then on, so neither takes the other's recordings; a new scope starts
// with none used.
func TestKeyerWithholdsTwoContainersConfiguredTheSame(t *testing.T) {
	k := NewKeyer(newFake())
	_ = name(t, k, "a", "127.0.0.1:40002")
	_ = name(t, k, "twin", "127.0.0.1:40005")
	_ = name(t, k, "b", "127.0.0.1:40003")

	k.BeginTest()
	if key, st := use(k, "a"); st != Keyed || key != keyDefault {
		t.Fatalf("the first container of its configuration = %q, %v", key, st)
	}
	if _, st := use(k, "a"); st != Keyed {
		t.Fatalf("its second use = %v", st)
	}
	if _, st := use(k, "twin"); st != Withheld {
		t.Fatalf("a second container configured the same = %v, want Withheld", st)
	}
	if _, st := use(k, "a"); st != Withheld {
		t.Fatalf("the first after its twin was used = %v, want Withheld", st)
	}
	if key, st := use(k, "b"); st != Keyed || key != keyOther {
		t.Fatalf("a container configured otherwise = %q, %v", key, st)
	}
	if _, ok := FromContext(WithConn(context.Background(), k, "twin")); ok {
		t.Fatal("FromContext gave a withheld key")
	}
	k.EndTest()

	k.BeginTest()
	if _, st := use(k, "twin"); st != Keyed {
		t.Fatalf("in a new test, the twin alone = %v", st)
	}
	k.EndTest()
	// Outside tests the run is a scope of its own.
	if _, st := use(k, "a"); st != Keyed {
		t.Fatalf("outside tests, the first alone = %v", st)
	}
}

// Concurrent first uses of two containers of one image, in either order:
// configured differently, each is always named the same; configured the
// same, both are withheld. Never swapped.
func TestKeyerConcurrentFirstUsesNeverSwap(t *testing.T) {
	for _, order := range [][]string{{"a", "b"}, {"b", "a"}} {
		k := NewKeyer(newFake())
		_ = name(t, k, "a", "127.0.0.1:40002")
		_ = name(t, k, "b", "127.0.0.1:40003")
		k.BeginTest()
		got := map[string]string{}
		var wg sync.WaitGroup
		var mu sync.Mutex
		for _, c := range order {
			wg.Add(1)
			go func(c string) {
				defer wg.Done()
				key, st := use(k, c)
				mu.Lock()
				defer mu.Unlock()
				if st != Keyed {
					t.Errorf("order %v: %s = %v", order, c, st)
				}
				got[c] = key
			}(c)
		}
		wg.Wait()
		if got["a"] != keyDefault || got["b"] != keyOther {
			t.Fatalf("order %v: keys %v", order, got)
		}
	}
	for _, order := range [][]string{{"a", "twin"}, {"twin", "a"}} {
		k := NewKeyer(newFake())
		_ = name(t, k, "a", "127.0.0.1:40002")
		_ = name(t, k, "twin", "127.0.0.1:40005")
		k.BeginTest()
		var wg sync.WaitGroup
		for _, c := range order {
			wg.Add(1)
			go func(c string) {
				defer wg.Done()
				_, _ = use(k, c)
			}(c)
		}
		wg.Wait()
		for _, c := range order {
			if _, st := use(k, c); st != Withheld {
				t.Fatalf("order %v: %s = %v after both were used, want Withheld", order, c, st)
			}
		}
	}
}

// A key waits for its container's configuration: a call matched before the
// index has read it waits for the read, and a container whose configuration
// cannot be read is withheld.
func TestKeyerWaitsForTheConfiguration(t *testing.T) {
	f := newFake()
	late := Container{ID: "late", Image: "postgres:16", Port: 5432}
	f.at["127.0.0.1:40006"] = late
	f.fps["late"] = fpDefault
	f.pending["late"] = make(chan struct{})
	f.asked = make(chan string, 1)
	k := NewKeyer(f)
	if c, key, ok := k.Name(zap.NewNop(), "late", "127.0.0.1:40006"); !ok || key != "" || c.ID != "late" {
		t.Fatalf("Name before the read = %+v, %q, %v", c, key, ok)
	}
	type res struct {
		key string
		st  State
	}
	done := make(chan res, 1)
	go func() {
		key, st := use(k, "late")
		done <- res{key, st}
	}()
	<-f.asked
	close(f.pending["late"])
	if r := <-done; r.st != Keyed || r.key != keyDefault {
		t.Fatalf("after the read = %q, %v", r.key, r.st)
	}

	unread := Container{ID: "unread", Image: "postgres:16", Port: 5432}
	f.at["127.0.0.1:40007"] = unread
	_ = name(t, k, "unread", "127.0.0.1:40007")
	if key, st := use(k, "unread"); st != Withheld || key != "" {
		t.Fatalf("a container whose configuration cannot be read = %q, %v", key, st)
	}
}

// Stamp writes the key, the container's instance and the scope the request
// was sent in: a mock handed over after its test ended is filed under its
// own test, and a connection closed a moment ago is still stamped.
func TestKeyerStampsTheKeyInstanceAndScope(t *testing.T) {
	k := NewKeyer(newFake())
	_ = name(t, k, "7", "127.0.0.1:40004")
	_ = name(t, k, "a", "127.0.0.1:40002")
	_ = name(t, k, "twin", "127.0.0.1:40005")

	stamp := func(connID string, at time.Time) map[string]string {
		m := &models.Mock{ConnectionID: connID}
		m.Spec.ReqTimestampMock = at
		k.Stamp(m)
		return m.Spec.Metadata
	}
	outside := stamp("7", time.Now())
	if outside[MetadataKey] != keyRedis || outside[InstanceKey] == "" || outside[ScopeKey] == "" {
		t.Fatalf("stamped %v", outside)
	}
	if outside[InstanceKey] == redis.ID {
		t.Fatal("the instance is the container's ID")
	}

	k.BeginTest()
	inTest1 := time.Now()
	a1 := stamp("a", inTest1)
	twin1 := stamp("twin", inTest1)
	k.EndTest()
	k.BeginTest()
	// Handed over in test 2, sent in test 1.
	late := stamp("a", inTest1)
	now := stamp("a", time.Now())
	k.EndTest()

	if a1[ScopeKey] == outside[ScopeKey] || a1[ScopeKey] != twin1[ScopeKey] || late[ScopeKey] != a1[ScopeKey] || now[ScopeKey] == a1[ScopeKey] {
		t.Fatalf("scopes: outside %s, test 1 %s %s, late %s, test 2 %s", outside[ScopeKey], a1[ScopeKey], twin1[ScopeKey], late[ScopeKey], now[ScopeKey])
	}
	if a1[MetadataKey] != twin1[MetadataKey] || a1[InstanceKey] == twin1[InstanceKey] || late[InstanceKey] != a1[InstanceKey] {
		t.Fatalf("instances: a %s, twin %s, late a %s", a1[InstanceKey], twin1[InstanceKey], late[InstanceKey])
	}

	// HTTP's recorder carries the connection in its metadata instead.
	byMeta := &models.Mock{Spec: models.MockSpec{Metadata: map[string]string{"connID": "7", "operation": "GET"}}}
	k.Stamp(byMeta)
	if byMeta.Spec.Metadata[MetadataKey] != keyRedis || byMeta.Spec.Metadata["operation"] != "GET" {
		t.Fatalf("by metadata connID = %v", byMeta.Spec.Metadata)
	}
	if got := stamped(k, "8"); got != "" {
		t.Fatalf("another connection's mock was stamped %q", got)
	}
	k.Stamp(nil)
	k.Stamp(&models.Mock{})

	// A connection that closed is still stamped, for a mock handed over late.
	k.Forget("7")
	if got := stamped(k, "7"); got != keyRedis {
		t.Fatalf("after Forget: stamped %q", got)
	}
	k.Reset()
	if got := stamped(k, "7"); got != "" {
		t.Fatalf("a reset keyer still stamped %q", got)
	}
}

// A container whose configuration cannot be read is stamped as a container
// (its instance and scope) with no key: no call has its key, and no keyed or
// unkeyed container call takes it by address.
func TestKeyerStampsAnUnreadContainerWithoutAKey(t *testing.T) {
	f := newFake()
	f.at["127.0.0.1:40007"] = Container{ID: "unread", Image: "postgres:16", Port: 5432}
	k := NewKeyer(f)
	_ = name(t, k, "u", "127.0.0.1:40007")
	m := &models.Mock{ConnectionID: "u"}
	k.Stamp(m)
	if _, ok := m.Spec.Metadata[MetadataKey]; ok || m.Spec.Metadata[InstanceKey] == "" || !RecordedContainer(m) {
		t.Fatalf("stamped %v", m.Spec.Metadata)
	}
}

// Only the closed connections a late mock may still need are kept.
func TestKeyerKeepsABoundedNumberOfClosedConnections(t *testing.T) {
	k := NewKeyer(newFake())
	for i := 0; i < keptClosed+10; i++ {
		id := strconv.Itoa(i)
		_ = name(t, k, id, "127.0.0.1:40004")
		k.Forget(id)
	}
	if len(k.conns) != keptClosed || len(k.closed) != keptClosed {
		t.Fatalf("held %d connections, %d closed, want %d", len(k.conns), len(k.closed), keptClosed)
	}
	if got := stamped(k, "0"); got != "" {
		t.Fatalf("the oldest closed connection was still stamped %q", got)
	}
	if got := stamped(k, strconv.Itoa(keptClosed+9)); got != keyRedis {
		t.Fatalf("the newest closed connection was stamped %q", got)
	}
}

// The parsers reach their connection's key through their context.
func TestFromContextIsTheConnectionsKey(t *testing.T) {
	k := NewKeyer(newFake())
	_ = name(t, k, "1", "127.0.0.1:40002")
	if got, ok := FromContext(WithConn(context.Background(), k, "1")); !ok || got != keyDefault {
		t.Fatalf("FromContext = %q, %v", got, ok)
	}
	if _, ok := FromContext(WithConn(context.Background(), k, "2")); ok {
		t.Fatal("an unnamed connection has a key")
	}
	if _, ok := FromContext(context.Background()); ok {
		t.Fatal("a context with no connection has a key")
	}
}

// A recording's mocks of one key, in one scope, from two containers are
// ambiguous; from one container, or one per scope, they are not.
func TestRecordedAmbiguity(t *testing.T) {
	mock := func(key, inst, scope string) *models.Mock {
		return &models.Mock{Spec: models.MockSpec{Metadata: map[string]string{MetadataKey: key, InstanceKey: inst, ScopeKey: scope}}}
	}
	k := NewKeyer(newFake())
	x1, y1 := mock(keyDefault, "x", "s2"), mock(keyDefault, "y", "s2")
	x2, y3 := mock(keyDefault, "x", "s3"), mock(keyDefault, "y", "s4")
	other := mock(keyOther, "z", "s2")
	legacy := &models.Mock{Spec: models.MockSpec{Metadata: map[string]string{MetadataKey: "container:postgres:16:5432#1"}}}
	var tally RecordedTally
	for _, m := range []*models.Mock{x1, y1, x2, y3, other, legacy, nil} {
		tally.Add(m)
	}
	k.SetRecorded(&tally)
	for _, c := range []struct {
		m    *models.Mock
		want bool
	}{{x1, true}, {y1, true}, {x2, false}, {y3, false}, {other, false}, {legacy, false}, {nil, false}} {
		if got := k.RecordedAmbiguous(c.m); got != c.want {
			t.Fatalf("RecordedAmbiguous(%v) = %v, want %v", c.m, got, c.want)
		}
	}
	k.SetRecorded(&RecordedTally{})
	if k.RecordedAmbiguous(x1) {
		t.Fatal("the next set's tally kept the last one's ambiguity")
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
		got, err := ParseAddrPort(in)
		if err != nil || got.String() != want {
			t.Fatalf("ParseAddrPort(%q) = %v, %v", in, got, err)
		}
	}
	for _, in := range []string{"", "127.0.0.1", "host:80", "127.0.0.1:x"} {
		if _, err := ParseAddrPort(in); err == nil {
			t.Fatalf("ParseAddrPort(%q) read it", in)
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
	if first.Spec.Metadata[MetadataKey] != keyRedis || first.Spec.Metadata["type"] != "config" {
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
	// A Lookup uses no container in the scope: two containers configured
	// the same, both looked up, leave the one then used Keyed. And it holds
	// no connection the index has not placed.
	twins := NewKeyer(newFake())
	for _, dst := range []string{"127.0.0.1:40002", "127.0.0.1:40005", "127.0.0.1:40099"} {
		_, _, _ = twins.Lookup(dst)
	}
	_ = name(t, twins, "a", "127.0.0.1:40002")
	if _, st := use(twins, "a"); st != Keyed {
		t.Fatalf("after Lookups of both twins, a use of one is %v, want Keyed", st)
	}
	if len(twins.unplaced) != 0 {
		t.Fatalf("Lookups held %d connections the index has not placed", len(twins.unplaced))
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

// A new session forgets the last one's scopes: its spans start afresh.
func TestKeyerResetForgetsTheScopes(t *testing.T) {
	k := NewKeyer(newFake())
	k.BeginTest()
	k.EndTest()
	k.BeginTest()
	k.Reset()
	if len(k.spans) != 1 || k.test != nil {
		t.Fatalf("after Reset the keyer holds %d spans (test open %v), want the new run's", len(k.spans), k.test != nil)
	}
}

// The setup between two tests is a scope of its own: two containers
// configured the same, each used by its own test and by the fixture before
// it, are not ambiguous.
func TestKeyerTheSetupBetweenTestsIsAScope(t *testing.T) {
	k := NewKeyer(newFake())
	_ = name(t, k, "a", "127.0.0.1:40002")
	_ = name(t, k, "twin", "127.0.0.1:40005")
	if _, st := use(k, "a"); st != Keyed { // setup before test 1
		t.Fatalf("setup before test 1: %v", st)
	}
	k.BeginTest()
	_, _ = use(k, "a")
	k.EndTest()
	if _, st := use(k, "twin"); st != Keyed { // setup before test 2
		t.Fatalf("setup before test 2: %v, want Keyed: the first setup's container is not this one's", st)
	}
}

// A wait for a container's configuration no call bounds (Stamp) ends with
// the session: at the next session's start, or the agent's end. The context
// the session was started with does not bound it: the replay's is its
// request's, which ends as soon as it is answered.
func TestAStampWaitEndsWithTheSession(t *testing.T) {
	// A connection to a container whose configuration is being read.
	setup := func(t *testing.T) (*Keyer, *fakeIndex, chan struct{}) {
		f := newFake()
		read := make(chan struct{})
		f.at["127.0.0.1:40080"] = Container{ID: "late", Image: "postgres:16", Port: 5432}
		f.pending["late"] = read
		f.asked = make(chan string, 1)
		k := NewKeyer(f)
		_ = name(t, k, "c1", "127.0.0.1:40080")
		return k, f, read
	}
	stamp := func(k *Keyer) <-chan *models.Mock {
		done := make(chan *models.Mock, 1)
		go func() {
			m := &models.Mock{ConnectionID: "c1"}
			k.Stamp(m)
			done <- m
		}()
		return done
	}
	t.Run("the request that started it ends", func(t *testing.T) {
		k, f, read := setup(t)
		k.Bind(t.Context())
		req, answered := context.WithCancel(t.Context())
		k.Watch(req, nil)
		answered()
		done := stamp(k)
		<-f.asked // the stamp waits for the read
		f.mu.Lock()
		f.fps["late"] = fpDefault
		f.mu.Unlock()
		close(read)
		if m := <-done; m.Spec.Metadata[MetadataKey] != keyDefault {
			t.Fatalf("stamped %v, want its key", m.Spec.Metadata)
		}
	})
	for _, end := range []struct {
		name string
		end  func(k *Keyer, agentEnds context.CancelFunc)
	}{
		{"the next session starts", func(k *Keyer, _ context.CancelFunc) { k.Reset(); k.Watch(context.Background(), nil) }},
		{"the agent ends", func(_ *Keyer, agentEnds context.CancelFunc) { agentEnds() }},
	} {
		t.Run(end.name, func(t *testing.T) {
			k, f, _ := setup(t)
			agentCtx, agentEnds := context.WithCancel(t.Context())
			defer agentEnds()
			k.Bind(agentCtx)
			k.Watch(t.Context(), nil)
			done := stamp(k)
			<-f.asked
			end.end(k, agentEnds)
			m := <-done
			if m.Spec.Metadata[MetadataKey] != "" || m.Spec.Metadata[InstanceKey] == "" {
				t.Fatalf("stamped %v, want a container with no key", m.Spec.Metadata)
			}
		})
	}
}

// A use that finds its connection placed by another use since it looked
// takes that container.
func TestKeyerAUseTakesAPlacementAnotherUseMade(t *testing.T) {
	k := NewKeyer(newFake())
	k.mu.Lock()
	k.conns["c1"] = pgA // placed by another use; unplaced no longer holds it
	k.mu.Unlock()
	if c, ok := k.place(context.Background(), "c1"); !ok || c != pgA {
		t.Fatalf("place = %+v, %v", c, ok)
	}
}

// Placed is a connection's container: the one Name gave it, or, for one the
// index was placing at the accept, the one it finds once that is placed.
func TestKeyerPlacedWaitsForThePlacingsAtTheAccept(t *testing.T) {
	f := newFake()
	read := make(chan struct{})
	f.reads = map[string]chan struct{}{"r": read}
	k := NewKeyer(f)
	if c, ok := k.Placed(context.Background(), "named"); ok {
		t.Fatalf("a connection never named reaches %+v", c)
	}
	_ = name(t, k, "a", "127.0.0.1:40002")
	if c, ok := k.Placed(context.Background(), "a"); !ok || c != pgA {
		t.Fatalf("Placed(a) = %+v, %v", c, ok)
	}
	f.awaitedCh = make(chan struct{}, 1)
	_ = name(t, k, "late", "127.0.0.1:40090")
	got := make(chan Container, 1)
	go func() {
		c, _ := k.Placed(context.Background(), "late")
		got <- c
	}()
	select {
	case <-f.awaitedCh: // Placed waits for the placing in flight at the accept
	case c := <-got:
		t.Fatalf("Placed did not wait for the placing in flight at the accept (got %+v)", c)
	}
	f.mu.Lock()
	f.at["127.0.0.1:40090"] = redis
	f.mu.Unlock()
	close(read)
	if c := <-got; c != redis {
		t.Fatalf("Placed(late) = %+v, want the container placed after the accept", c)
	}
}

// PlacedOrPlacing says what is placed now, or gives a wait for a placing:
// none when there is nothing to wait for (no container could be there, the
// index places none now, or it cannot be read). The wait runs off the
// caller's path and says what the destination reaches, or that it cannot be
// told when the session ends first.
func TestKeyerPlacedOrPlacingWaitsOffThePathForThePlacingsNow(t *testing.T) {
	type told struct {
		c            Container
		ok, resolved bool
	}
	f := newFake()
	k := NewKeyer(f)
	if c, ok, wait := k.PlacedOrPlacing("127.0.0.1:40002"); !ok || c != pgA || wait != nil {
		t.Fatalf("placed now: %+v %v, wait %v", c, ok, wait != nil)
	}
	if _, ok, wait := k.PlacedOrPlacing("127.0.0.1:40090"); ok || wait != nil {
		t.Fatal("a wait with nothing placing")
	}

	read := make(chan struct{})
	f.reads = map[string]chan struct{}{"r": read}
	f.awaitedCh = make(chan struct{}, 1)
	_, ok, wait := k.PlacedOrPlacing("127.0.0.1:40090")
	if ok || wait == nil {
		t.Fatal("no wait for the placing")
	}
	if f.awaited != 0 {
		t.Fatal("waited before asked to")
	}
	got := make(chan told, 1)
	wait(func(c Container, ok, resolved bool) { got <- told{c, ok, resolved} })
	<-f.awaitedCh // waiting, and the call has returned
	f.mu.Lock()
	f.at["127.0.0.1:40090"] = redis
	f.mu.Unlock()
	close(read)
	if g := <-got; !g.resolved || !g.ok || g.c != redis {
		t.Fatalf("told %+v; want the container placed meanwhile", g)
	}

	// A placing that does not end before the session does cannot be told.
	f.reads = map[string]chan struct{}{"s": make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	k.Bind(ctx)
	k.Watch(ctx, zap.NewNop())
	_, _, wait = k.PlacedOrPlacing("127.0.0.1:40091")
	if wait == nil {
		t.Fatal("no wait for the placing")
	}
	wait(func(c Container, ok, resolved bool) { got <- told{c, ok, resolved} })
	<-f.awaitedCh
	cancel()
	if g := <-got; g.resolved || g.ok {
		t.Fatalf("told %+v; want unresolved", g)
	}

	f.noContainerAt = map[netip.Addr]bool{netip.MustParseAddr("127.0.0.1"): true}
	if _, ok, wait := k.PlacedOrPlacing("127.0.0.1:40092"); ok || wait != nil {
		t.Fatal("a wait where no container could be")
	}
	f.noContainerAt = nil
	f.err = errors.New("no Docker here")
	if _, ok, wait := k.PlacedOrPlacing("127.0.0.1:40093"); ok || wait != nil {
		t.Fatal("a wait on an index that cannot be read")
	}
}

// ParseAddrPort reads every form the proxy writes a destination in: the
// dual-stack [a.b.c.d]:p, and [::ffff:a.b.c.d]:p, as that IPv4 destination.
func TestParseAddrPortReadsEveryFormTheProxyWrites(t *testing.T) {
	for in, want := range map[string]string{
		"10.0.4.7:6379":          "10.0.4.7:6379",
		"[10.0.4.7]:6379":        "10.0.4.7:6379",
		"[::ffff:10.0.4.7]:6379": "10.0.4.7:6379",
		"[::1]:5432":             "[::1]:5432",
		"[2001:db8::7]:443":      "[2001:db8::7]:443",
	} {
		got, err := ParseAddrPort(in)
		if err != nil || got != netip.MustParseAddrPort(want) {
			t.Fatalf("ParseAddrPort(%q) = %v, %v; want %s", in, got, err, want)
		}
	}
	for _, bad := range []string{"10.0.4.7", "db:5432", "[10.0.4.7]:x", "[10.0.4.7]:70000"} {
		if _, err := ParseAddrPort(bad); err == nil {
			t.Fatalf("ParseAddrPort(%q) read it", bad)
		}
	}
}

// KeyFromContext says what a connection's destination is without counting a
// use of it: a container asked about is not one of two configured the same
// in the scope.
func TestKeyFromContextCountsNoUse(t *testing.T) {
	k := NewKeyer(newFake())
	_ = name(t, k, "a", "127.0.0.1:40002")
	_ = name(t, k, "twin", "127.0.0.1:40005")
	k.BeginTest()
	defer k.EndTest()
	if key, isContainer, keyed := KeyFromContext(WithConn(context.Background(), k, "twin")); !keyed || !isContainer || key != keyDefault {
		t.Fatalf("KeyFromContext = %q, %v, %v; want %q, true, true", key, isContainer, keyed, keyDefault)
	}
	if key, st := use(k, "a"); st != Keyed || key != keyDefault {
		t.Fatalf("a use after the twin was only asked about = %q, %v; want Keyed", key, st)
	}
}

// Whether the test set the agent was given was recorded naming containers is
// read from its whole tally: a stamp of any mock counts, a container whose
// key could not be read among them; the next set's tally replaces it.
func TestRecordingNamesContainersFollowsTheTestSet(t *testing.T) {
	k := NewKeyer(newFake())
	ctx := WithConn(context.Background(), k, "c")
	if RecordingNamesContainers(ctx) {
		t.Fatal("named before any test set was given")
	}
	var named RecordedTally
	named.Add(&models.Mock{Spec: models.MockSpec{Metadata: map[string]string{InstanceKey: "i1"}}})
	k.SetRecorded(&named)
	if !RecordingNamesContainers(ctx) {
		t.Fatal("a set with a container stamp (its key unread) is not named")
	}
	k.SetRecorded(&RecordedTally{})
	if RecordingNamesContainers(ctx) {
		t.Fatal("the previous set's stamp outlived its replacement")
	}
	if RecordingNamesContainers(context.Background()) {
		t.Fatal("named with no keyer in ctx")
	}
}
