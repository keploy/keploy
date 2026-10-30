package proxy

import (
	"context"
	"reflect"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/agent/proxy/internal/testnet"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/agent/proxy/synhold"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

func TestRecordedPortsKnowsOnlyWhatTheRecordingCalled(t *testing.T) {
	var r recordedPorts
	r.add([]*models.Mock{
		{Kind: models.HTTP, Spec: models.MockSpec{HTTPReq: &models.HTTPReq{Header: map[string]string{"Host": "127.0.0.1:9000"}}}},
		{Kind: models.Postgres, Spec: models.MockSpec{Metadata: map[string]string{"destAddr": "127.0.0.1:5432"}}},
		{Kind: models.DNS},
	})
	for _, p := range []uint32{9000, 5432} {
		if !r.has(p) {
			t.Fatalf("port %d was called in the recording", p)
		}
	}
	if r.has(41234) {
		t.Fatal("a port the recording never called must not count")
	}
}

func TestRecordedPortsTreatsAnUnknownDestinationAsAnyPort(t *testing.T) {
	var r recordedPorts
	r.add([]*models.Mock{{Kind: models.GENERIC}})
	if !r.has(41234) {
		t.Fatal("a mock with no known destination could be for any port")
	}
}

func TestADeadPortIsAChildOnlyWhenEveryRecordingStartedIt(t *testing.T) {
	child := &models.Mock{Kind: models.HTTP, Spec: models.MockSpec{Metadata: map[string]string{"destAddr": "127.0.0.1:9000", "startedByTests": "true"}}}
	plain := &models.Mock{Kind: models.HTTP, Spec: models.MockSpec{Metadata: map[string]string{"destAddr": "127.0.0.1:9000"}}}
	var r recordedPorts
	r.add([]*models.Mock{child})
	if !r.child(9000) {
		t.Fatal("a port only test-started dependencies were called on is theirs")
	}
	r.add([]*models.Mock{plain})
	if r.child(9000) {
		t.Fatal("a port another recording called as an outside service must be served from mocks when it is down")
	}
}

func TestAnHTTPHostWithoutAPortUsesTheSchemePort(t *testing.T) {
	var r recordedPorts
	r.add([]*models.Mock{
		{Kind: models.HTTP, Spec: models.MockSpec{HTTPReq: &models.HTTPReq{URL: "https://api.example.com/x", Header: map[string]string{"Host": "api.example.com"}}}},
		{Kind: models.HTTP, Spec: models.MockSpec{HTTPReq: &models.HTTPReq{URL: "http://plain.example.com/x", Header: map[string]string{"Host": "plain.example.com"}}}},
	})
	if !r.has(443) || !r.has(80) || r.has(41234) {
		t.Fatal("a host without a port is called on its scheme's port, and no other")
	}
}

func TestRecordedPortsStartEmptyAfterReset(t *testing.T) {
	var r recordedPorts
	r.add([]*models.Mock{{Kind: models.GENERIC}})
	r.reset()
	if r.has(41234) {
		t.Fatal("a new session must not keep the previous session's ports")
	}
}

// A connection failure says the destination was NOT reached, and it carries no
// metadata.destAddr: counted like any mock without a known port, it would make
// every port "recorded" and switch off the loopback refusal rules for the
// whole test set.
func TestRecordedPortsIgnoreConnectionFailures(t *testing.T) {
	cf := &models.Mock{Kind: models.ConnectionFailure, Spec: models.MockSpec{
		ConnFailure: &models.ConnFailureSpec{Address: "127.0.0.1:5432", Phase: models.ConnFailurePhaseConnect, Outcome: models.ConnFailureRefused},
	}}
	var r recordedPorts
	r.add([]*models.Mock{cf}, []*models.Mock{
		{Kind: models.HTTP, Spec: models.MockSpec{Metadata: map[string]string{"destAddr": "127.0.0.1:9000"}}},
	})
	if !r.has(9000) {
		t.Fatal("port 9000 was called in the recording")
	}
	for _, p := range []uint32{5432, 41234} {
		if r.has(p) {
			t.Fatalf("port %d must not count as recorded: a connection failure is not a call that reached it", p)
		}
	}
}

// localPortsStore is a set's on-disk store as keploy mock replay keeps it
// under the proven switch (keploy #4970): a startup call recorded before the
// first test, a local dependency the recording reached only inside a test
// (the outside service at plain), and one only a process the test command
// started reached (at kid).
func localPortsStore(t *testing.T, plain, kid string) *DiskMocks {
	t.Helper()
	s, err := NewDiskMocks(zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	boot := perTestMockAt("boot", 5)
	boot.TestModeInfo.Lifetime = models.LifetimeSession
	boot.Spec.Metadata = map[string]string{"destAddr": "10.0.0.1:443"}
	inTest := perTestMockAt("in-test", 20)
	inTest.Spec.Metadata = map[string]string{"destAddr": plain}
	inTestLater := perTestMockAt("in-test-later", 30)
	inTestLater.Spec.Metadata = map[string]string{"destAddr": plain}
	started := perTestMockAt("started-by-tests", 25)
	started.Spec.Metadata = map[string]string{"destAddr": kid, "startedByTests": "true"}
	for _, m := range []*models.Mock{boot, inTest, inTestLater, started} {
		if err := s.Add(m); err != nil {
			t.Fatal(err)
		}
	}
	s.Finalize()
	return s
}

// replayLocalPorts drives what the agent does for one set of a native keploy
// mock replay (mockMode) or keploy test: the session's Mock (its reset of the
// staged ports), the store's publication (SeedRecordedDestinations, from
// LoadDestinations), and the set's staging of only what was recorded before
// its first test. It returns how the handshake hold decides a connect to a
// local address, nothing listening there.
func replayLocalPorts(t *testing.T, mockMode bool, store *DiskMocks) func(dest string) synhold.Outcome {
	t.Helper()
	recorded.forget()
	t.Cleanup(recorded.forget)
	session := &agent.Session{Mode: models.MODE_TEST}
	session.Mocking = true
	p := &Proxy{logger: zap.NewNop(), mockMode: mockMode, appPID: 1}
	p.setSession(session)
	recorded.reset() // the session's Mock
	seed, err := store.LoadDestinations()
	if err != nil {
		t.Fatal(err)
	}
	p.SeedRecordedDestinations(seed)
	boot, err := store.LoadBefore(time.Unix(10, 0))
	if err != nil {
		t.Fatal(err)
	}
	if err := p.SetMocksWithWindow(context.Background(), boot, nil, models.BaseTime, time.Unix(40, 0)); err != nil {
		t.Fatal(err)
	}
	return func(dest string) synhold.Outcome {
		return p.decideHandshake(fakeHandshakeDest{dest: ipv4Dest(t, dest)})(context.Background(), appEnd, proxyEnd).Outcome
	}
}

// Under the proven switch keploy mock replay stages only what was recorded
// before the set's first test (keploy #4970); a test's own mocks reach the
// proxy through the exact view, never staged. A local dependency the
// recording reached only inside a test is still a recorded port: its connect
// is not refused while it is stopped, so the test is served its mocks. A port
// the recording never reached is still refused, and so is one only a process
// the test command started reached. A Mock after the store keeps them; a
// recording forgets them.
func TestAPortReachedOnlyInsideATestIsRecordedUnderTheProvenSwitch(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	plain, kid, never := testnet.RefusedAddr(t, "tcp"), testnet.RefusedAddr(t, "tcp"), testnet.RefusedAddr(t, "tcp")
	decide := replayLocalPorts(t, true, localPortsStore(t, plain, kid))
	if got := decide(plain); got != synhold.Accept {
		t.Fatalf("a stopped local dependency the recording reached only inside a test: %v, want accept", got)
	}
	if got := decide(never); got != synhold.Refuse {
		t.Fatalf("a local port the recording never reached: %v, want refuse", got)
	}
	if got := decide(kid); got != synhold.Refuse {
		t.Fatalf("a local port only a process the test command started reached: %v, want refuse", got)
	}
	recorded.reset() // a Mock published after the store (keploy test's order)
	if got := decide(plain); got != synhold.Accept {
		t.Fatalf("after a Mock that follows the store: %v, want accept", got)
	}
	recorded.forget() // a Record
	if got := decide(plain); got != synhold.Refuse {
		t.Fatalf("after a recording starts: %v, want refuse", got)
	}
}

// The agent's own Mock and Record decide what becomes of the seed: a real
// proxy's Mock after the store keeps the ports the store seeded (keploy
// test publishes the store before its Mock), and its Record forgets them (a
// recording reaches nothing a previous replay's store reached, so a call to a
// stopped one of those ports is not left to the record path).
func TestAProxysMockKeepsTheSeededPortsAndItsRecordForgetsThem(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	plain, kid := testnet.RefusedAddr(t, "tcp"), testnet.RefusedAddr(t, "tcp")
	store := localPortsStore(t, plain, kid)
	recorded.forget()
	t.Cleanup(recorded.forget)
	t.Cleanup(func() { syncMock.Get().SetOutputChannel(nil) })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	p := New(zap.NewNop(), fakeDestInfo{}, config.New())
	p.nsswitchData = []byte("preseeded-by-test") // Mock leaves the host's DNS alone
	seed, err := store.LoadDestinations()
	if err != nil {
		t.Fatal(err)
	}
	p.SeedRecordedDestinations(seed)
	port := uint32(mustPort(t, plain))
	if !RecordedPort(port) {
		t.Fatal("the store's seed did not list the port the recording reached only inside a test")
	}
	if err := p.Mock(ctx, models.OutgoingOptions{Mocking: true}); err != nil {
		t.Fatal(err)
	}
	if !RecordedPort(port) {
		t.Fatal("a Proxy.Mock after the store dropped the seeded port")
	}
	if err := p.Record(ctx, make(chan *models.Mock, 16), models.OutgoingOptions{}); err != nil {
		t.Fatal(err)
	}
	if RecordedPort(port) {
		t.Fatal("a Proxy.Record kept the seeded port")
	}
}

// keploy test's handshake hold refuses no local connect, with the switch on or
// off: the same store and staging accept every one of them, as before.
func TestKeployTestAcceptsTheLocalPortsOfTheSameStore(t *testing.T) {
	for _, on := range []string{"1", "0"} {
		t.Run("KEPLOY_PROVEN="+on, func(t *testing.T) {
			t.Setenv(provenswitch.Env, on)
			plain, kid, never := testnet.RefusedAddr(t, "tcp"), testnet.RefusedAddr(t, "tcp"), testnet.RefusedAddr(t, "tcp")
			decide := replayLocalPorts(t, false, localPortsStore(t, plain, kid))
			for _, dest := range []string{plain, kid, never} {
				if got := decide(dest); got != synhold.Accept {
					t.Fatalf("keploy test, %s: %v, want accept", dest, got)
				}
			}
		})
	}
}

// Without the switch a set's ports are, as before, only what its stagings
// staged: the store seeds none, so a port reached only inside a test is
// refused until a staging of its test's window stages its mock, and recorded
// from then on.
func TestWithoutTheSwitchThePortsAreOnlyWhatIsStaged(t *testing.T) {
	t.Setenv(provenswitch.Env, "0")
	plain, kid := testnet.RefusedAddr(t, "tcp"), testnet.RefusedAddr(t, "tcp")
	store := localPortsStore(t, plain, kid)
	if seed, err := store.LoadDestinations(); err != nil || len(seed) != 0 {
		t.Fatalf("the store keeps %d destinations (%v); want none of an HTTP service off the default MySQL ports", len(seed), err)
	}
	decide := replayLocalPorts(t, true, store)
	if got := decide(plain); got != synhold.Refuse {
		t.Fatalf("before its test's staging: %v, want refuse, as before", got)
	}
	inTest, err := store.LoadWindow(time.Unix(15, 0), time.Unix(22, 0))
	if err != nil || len(inTest) != 1 {
		t.Fatalf("the test's window holds %d mocks (%v); want its one", len(inTest), err)
	}
	recorded.add(inTest)
	if got := decide(plain); got != synhold.Accept {
		t.Fatalf("after its test's staging: %v, want accept", got)
	}
}

// A local MySQL the recording reached only inside a test: the store keeps its
// first mock as a destination for the MySQL rules, with the switch on or off.
// Under the switch that mock is its port's first too, kept once, and the seed
// lists its port, so its connect is not refused while it is stopped. Without
// the switch the store seeds no port from it: its connect is refused before
// its test's staging, as before.
func TestAStoredLocalMySQLDestinationIsARecordedPortOnlyUnderTheSwitch(t *testing.T) {
	for _, on := range []string{"1", "0"} {
		t.Run(provenswitch.Env+"="+on, func(t *testing.T) {
			t.Setenv(provenswitch.Env, on)
			db := testnet.RefusedAddr(t, "tcp")
			s, err := NewDiskMocks(zap.NewNop())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close() })
			inTest := perTestMockAt("db-in-test", 20)
			inTest.Kind = models.MySQL
			inTest.Spec.Metadata = map[string]string{"destAddr": db}
			if err := s.Add(inTest); err != nil {
				t.Fatal(err)
			}
			s.Finalize()
			if seed, err := s.LoadDestinations(); err != nil || len(seed) != 1 {
				t.Fatalf("the store keeps %d destinations (%v); want the MySQL one, once", len(seed), err)
			}
			decide := replayLocalPorts(t, true, s)
			want := synhold.Refuse
			if on == "1" {
				want = synhold.Accept
			}
			if got := decide(db); got != want {
				t.Fatalf("a stopped local MySQL the recording reached only inside a test, before its test's staging: %v, want %v", got, want)
			}
		})
	}
}

// A mock whose destination its recording does not say could be for any
// port, whether a staging stages it or the store seeds it: under the switch
// one recorded only inside a test makes every local port a recorded one, as
// it did when keploy mock replay staged the whole recording. Without the
// switch only its test's staging does.
func TestAMockOfNoKnownDestinationInsideATestCountsForEveryPort(t *testing.T) {
	for _, on := range []string{"1", "0"} {
		t.Run(provenswitch.Env+"="+on, func(t *testing.T) {
			t.Setenv(provenswitch.Env, on)
			local := testnet.RefusedAddr(t, "tcp")
			s, err := NewDiskMocks(zap.NewNop())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close() })
			inTest := perTestMockAt("no-address-in-test", 20)
			inTest.Kind = models.GENERIC
			if err := s.Add(inTest); err != nil {
				t.Fatal(err)
			}
			s.Finalize()
			decide := replayLocalPorts(t, true, s)
			want := synhold.Refuse
			if on == "1" {
				want = synhold.Accept
			}
			if got := decide(local); got != want {
				t.Fatalf("a local port, beside a mock of no known destination recorded inside a test: %v, want %v", got, want)
			}
		})
	}
}

// A port a process the test command started reached, and the recording also
// reached as an outside dependency, is not the started process's alone,
// whether each reach is staged or seeded: the two are read together, as when
// keploy mock replay staged the whole recording. Under the switch, with the
// outside dependency reached only inside tests (seeded), its connect is
// served from mocks while it is down, whether the started process reached it
// inside a test too (seeded) or before the first test (staged).
func TestAPortAlsoReachedAsAnOutsideDependencyIsNoChild(t *testing.T) {
	for _, c := range []struct {
		name      string
		startedAt int64
	}{
		{"both reaches inside tests", 20},
		{"the started process's reach before the first test", 5},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv(provenswitch.Env, "1")
			shared := testnet.RefusedAddr(t, "tcp")
			s, err := NewDiskMocks(zap.NewNop())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close() })
			started := perTestMockAt("started", c.startedAt)
			started.Spec.Metadata = map[string]string{"destAddr": shared, "startedByTests": "true"}
			outside := perTestMockAt("outside-in-test", 25)
			outside.Spec.Metadata = map[string]string{"destAddr": shared}
			for _, m := range []*models.Mock{started, outside} {
				if err := s.Add(m); err != nil {
					t.Fatal(err)
				}
			}
			s.Finalize()
			decide := replayLocalPorts(t, true, s)
			if got := decide(shared); got != synhold.Accept {
				t.Fatalf("a stopped local port reached by a started process and, only inside tests, as an outside dependency: %v, want accept", got)
			}
		})
	}
}

// A recording replays nothing: its start forgets the ports a replay's
// stagings staged, not only those its store seeded, with the switch on or
// off. A stopped local port the recording reached before the first test,
// accepted while the set replays, is refused once a recording starts.
func TestARecordingForgetsTheStagedPorts(t *testing.T) {
	for _, on := range []string{"1", "0"} {
		t.Run(provenswitch.Env+"="+on, func(t *testing.T) {
			t.Setenv(provenswitch.Env, on)
			local := testnet.RefusedAddr(t, "tcp")
			s, err := NewDiskMocks(zap.NewNop())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close() })
			boot := perTestMockAt("boot", 5)
			boot.TestModeInfo.Lifetime = models.LifetimeSession
			boot.Spec.Metadata = map[string]string{"destAddr": local}
			if err := s.Add(boot); err != nil {
				t.Fatal(err)
			}
			s.Finalize()
			decide := replayLocalPorts(t, true, s)
			if got := decide(local); got != synhold.Accept {
				t.Fatalf("a stopped local port the staging staged: %v, want accept", got)
			}
			recorded.forget() // a Record
			if got := decide(local); got != synhold.Refuse {
				t.Fatalf("after a recording starts: %v, want refuse", got)
			}
		})
	}
}

// The store keeps the first mock of each port a recording reached, kept apart
// by whether a process the test command started reached it, beside the
// destinations' first mocks, each mock once.
func TestLoadDestinationsKeepsTheFirstMockOfEachReachedPort(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	s, err := NewDiskMocks(zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	at := func(name string, kind models.Kind, meta map[string]string, sec int64) *models.Mock {
		m := perTestMockAt(name, sec)
		m.Kind = kind
		m.Spec.Metadata = meta
		return m
	}
	for _, m := range []*models.Mock{
		at("api-later", models.HTTP, map[string]string{"destAddr": "127.0.0.1:18094"}, 30),
		at("api-first", models.HTTP, map[string]string{"destAddr": "127.0.0.1:18094"}, 10),
		at("api-started", models.HTTP, map[string]string{"destAddr": "127.0.0.1:18094", "startedByTests": "true"}, 40),
		at("db", models.MySQL, map[string]string{"destAddr": "10.0.0.2:6033"}, 20),
		at("db-later", models.MySQL, map[string]string{"destAddr": "10.0.0.2:6033"}, 50),
		at("no-port", models.GENERIC, map[string]string{}, 60),
		at("dns", models.DNS, map[string]string{"destAddr": "127.0.0.1:53"}, 1),
	} {
		if err := s.Add(m); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.LoadDestinations()
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, m := range got {
		out = append(out, m.Name)
	}
	if want := []string{"api-first", "db", "api-started", "no-port"}; !reflect.DeepEqual(out, want) {
		t.Fatalf("destinations %v; want the first mock of each destination and port, each once, in recorded order: %v", out, want)
	}
}
