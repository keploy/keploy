package proxy

import (
	"context"
	"reflect"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/agent/proxy/internal/testnet"
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

// keploy test refuses no local connect, with the switch on or off: the same
// store and staging accept every one of them, as before.
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
