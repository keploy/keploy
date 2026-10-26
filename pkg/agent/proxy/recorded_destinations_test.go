package proxy

import (
	"context"
	"io"
	"reflect"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/containerkey"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

func destMock(kind models.Kind, meta map[string]string) *models.Mock {
	return &models.Mock{Kind: kind, Spec: models.MockSpec{Metadata: meta}}
}

// A recorded destination is what the seeded rules read: a MySQL mock's
// address, whether the test command started it and the recorded container it
// ran in (two mocks of one share a key; any of those that differs makes
// another), or an HTTP/2 mock's scheme and authority, whatever address it was
// dialled at. A mock of another kind, and one that names no destination, has
// none.
func TestDestinationKey_NamesWhatTheSeededRulesRead(t *testing.T) {
	base := destMock(models.MySQL, map[string]string{"destAddr": "172.19.0.3:6033"})
	same := destMock(models.MySQL, map[string]string{"destAddr": "172.19.0.3:6033", "type": "mocks"})
	if destinationKey(base) == "" || destinationKey(base) != destinationKey(same) {
		t.Fatalf("two mocks of one destination have keys %q and %q", destinationKey(base), destinationKey(same))
	}
	for name, m := range map[string]*models.Mock{
		"another address":   destMock(models.MySQL, map[string]string{"destAddr": "172.19.0.3:6034"}),
		"started by tests":  destMock(models.MySQL, map[string]string{"destAddr": "172.19.0.3:6033", "startedByTests": "true"}),
		"another container": destMock(models.MySQL, map[string]string{"destAddr": "172.19.0.3:6033", containerkey.MetadataKey: "db"}),
		"another instance":  destMock(models.MySQL, map[string]string{"destAddr": "172.19.0.3:6033", containerkey.InstanceKey: "i-2"}),
	} {
		if k := destinationKey(m); k == "" || k == destinationKey(base) {
			t.Errorf("%s: key %q, want another destination", name, k)
		}
	}
	h2 := func(authority string) *models.Mock {
		return &models.Mock{Kind: models.HTTP2, Spec: models.MockSpec{
			Metadata: map[string]string{"destAddr": "10.0.0.9:8443"},
			HTTP2Req: &models.HTTP2Req{Scheme: "https", Authority: authority},
		}}
	}
	if a, b := destinationKey(h2("api:8443")), destinationKey(h2("auth:8443")); a == "" || a == b {
		t.Fatalf("two HTTP/2 authorities behind one address have keys %q and %q; want one each", a, b)
	}
	for name, m := range map[string]*models.Mock{
		"an HTTP mock off the default MySQL ports": {Kind: models.HTTP, Spec: models.MockSpec{Metadata: map[string]string{"destAddr": "10.0.0.2:80"}}},
		"an HTTP/2 mock, no authority":             {Kind: models.HTTP2, Spec: models.MockSpec{HTTP2Req: &models.HTTP2Req{Scheme: "https"}}},
		"no mock":                                  nil,
	} {
		if k := destinationKey(m); k != "" {
			t.Errorf("%s: key %q, want none", name, k)
		}
	}
}

// What a replay reads of a built-in default MySQL port (3306, 4000) is a
// destination too: a MySQL mock that names no address, which may be any default
// port's (one key for all of them), and any other dependency reached at a
// default port, wherever its mock names it (one key per port, whatever its
// kind or host).
func TestDestinationKey_NamesWhatADefaultPortHeld(t *testing.T) {
	if a, b := destinationKey(destMock(models.MySQL, map[string]string{})), destinationKey(destMock(models.MySQL, map[string]string{"type": "config"})); a == "" || a != b {
		t.Fatalf("MySQL mocks that name no address have keys %q and %q; want one, shared", a, b)
	}
	at4000 := map[string]*models.Mock{
		"HTTP, its Host":     httpMockTo("127.0.0.1:4000"),
		"HTTP, another host": httpMockTo("api.internal:4000"),
		"HTTP, its destAddr": httpMockAt("10.0.0.2:4000", "api.internal"),
		// destAddr is read before the Host: a virtual host naming
		// another port does not hide that the connection reached 4000.
		"HTTP, its destAddr, its Host naming another port": httpMockAt("10.0.0.2:4000", "api.internal:8080"),
		"gRPC, its :authority":                             grpcMockTo("orders:4000"),
		"HTTP/2, no authority":                             {Kind: models.HTTP2, Spec: models.MockSpec{Metadata: map[string]string{"destAddr": "10.0.0.2:4000"}, HTTP2Req: &models.HTTP2Req{Scheme: "http"}}},
		"generic, its destAddr":                            destMock(models.GENERIC, map[string]string{"destAddr": "10.0.0.2:4000"}),
	}
	want := destinationKey(httpMockTo("127.0.0.1:4000"))
	for name, m := range at4000 {
		if k := destinationKey(m); k == "" || k != want {
			t.Errorf("%s at 4000: key %q, want the port's %q", name, k, want)
		}
	}
	if k := destinationKey(httpMockTo("api:3306")); k == "" || k == want {
		t.Errorf("HTTP at 3306: key %q, want its own", k)
	}
	for name, m := range map[string]*models.Mock{
		"HTTP at 8080":                  {Kind: models.HTTP, Spec: models.MockSpec{HTTPReq: &models.HTTPReq{Header: map[string]string{"Host": "api:8080"}}}},
		"HTTP whose Host names no port": {Kind: models.HTTP, Spec: models.MockSpec{HTTPReq: &models.HTTPReq{URL: "/x", Header: map[string]string{"Host": "api.internal"}}}},
		// destAddr is read before the Host: a Host claiming 4000 on a
		// connection that reached 8080 says nothing of 4000.
		"HTTP whose destAddr names 8080, its Host claiming 4000": httpMockAt("10.0.0.9:8080", "api.internal:4000"),
		"a DNS answer naming 4000":                               destMock(models.DNS, map[string]string{"destAddr": "10.0.0.2:4000"}),
		"a connection failure at 4000":                           destMock(models.ConnectionFailure, map[string]string{"destAddr": "10.0.0.2:4000"}),
	} {
		if k := destinationKey(m); k != "" {
			t.Errorf("%s: key %q, want none", name, k)
		}
	}
}

// The on-disk store keeps the first mock it holds of each recorded
// destination, in recorded order.
func TestLoadDestinations_KeepsTheFirstMockOfEachDestination(t *testing.T) {
	s, err := NewDiskMocks(zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	at := func(name, addr string, sec int64) *models.Mock {
		m := perTestMockAt(name, sec)
		m.Kind = models.MySQL
		m.Spec.Metadata = map[string]string{"destAddr": addr}
		return m
	}
	for _, m := range []*models.Mock{
		at("a-late", "10.0.0.1:6033", 30), at("a-first", "10.0.0.1:6033", 10),
		at("b", "10.0.0.2:6033", 20), perTestMockAt("no-address", 5),
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
	if want := []string{"a-first", "b"}; !reflect.DeepEqual(out, want) {
		t.Fatalf("destinations %v; want the first mock of each, in recorded order: %v", out, want)
	}
}

// Seeded destinations are what every staging derives the MySQL ports from,
// beside what it stages, whichever order the seed and a session's start come
// in: keploy test stores (and seeds) a set before its session starts
// (Proxy.Mock, which marks the MySQL ports stale), compose the other way
// round. A non-default MySQL port recorded only inside tests is then known
// while the app starts, and in its tests; over a second set, the second
// set's seed is, and the first set's port is not.
func TestSeededDestinationsAreKnownAtEveryStaging(t *testing.T) {
	mysqlAt := func(addr string) *models.Mock {
		return &models.Mock{Kind: models.MySQL, Spec: models.MockSpec{Metadata: map[string]string{"destAddr": addr}, ReqTimestampMock: time.Unix(100, 0)}}
	}
	ctx := context.Background()
	stage := func(p *Proxy) {
		t.Helper()
		if err := p.SetMocksWithWindow(ctx, nil, nil, models.BaseTime, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	session := func(p *Proxy) {
		t.Helper()
		if err := p.Mock(ctx, models.OutgoingOptions{}); err != nil {
			t.Fatalf("Mock: %v", err)
		}
	}
	for name, seededFirst := range map[string]bool{"seeded, then the session starts": true, "the session starts, then seeded": false} {
		t.Run(name, func(t *testing.T) {
			p := testProxy(t)
			for i, set := range []struct {
				seed       []*models.Mock
				known, not uint32
			}{
				{[]*models.Mock{mysqlAt("172.19.0.3:6033")}, 6033, 0},
				{[]*models.Mock{mysqlAt("172.19.0.4:6034")}, 6034, 6033},
			} {
				if seededFirst {
					p.SeedRecordedDestinations(set.seed)
					session(p)
				} else {
					session(p)
					p.SeedRecordedDestinations(set.seed)
				}
				stage(p)
				if !p.mysqlPorts.Has(set.known) {
					t.Fatalf("set %d: the seeded MySQL port %d is not known at the staging", i+1, set.known)
				}
				if err := p.SetMocksWithWindow(ctx, nil, nil, time.Unix(200, 0), time.Unix(201, 0)); err != nil {
					t.Fatal(err)
				}
				if !p.mysqlPorts.Has(set.known) {
					t.Fatalf("set %d: the seeded MySQL port %d is not known in a test", i+1, set.known)
				}
				if set.not != 0 && p.mysqlPorts.Has(set.not) {
					t.Fatalf("set %d: the previous set's port %d outlived its staging", i+1, set.not)
				}
			}
		})
	}
}

// A destination recorded speaking HTTP/2 is offered h2 while the app starts,
// before any of its mocks is staged, and only then: from the set's first test
// on, the live trees decide, per window, as they always did. Through the
// replay's own calls, per set: Mock (the session, which reuses the manager
// and resets it for the set) and the store's seed, in either order, then the
// staging and a test. The next store's seed replaces the previous one.
func TestASeededHTTP2DestinationIsOfferedH2WhileTheAppStarts(t *testing.T) {
	h2At := func(authority string, at int64) []*models.Mock {
		return []*models.Mock{{Kind: models.HTTP2, Spec: models.MockSpec{
			HTTP2Req: &models.HTTP2Req{Scheme: "https", Authority: authority}, ReqTimestampMock: time.Unix(at, 0)}}}
	}
	ctx := context.Background()
	for name, seededFirst := range map[string]bool{"seeded, then the session starts": true, "the session starts, then seeded": false} {
		t.Run(name, func(t *testing.T) {
			p := testProxy(t)
			for i, set := range []struct {
				seed       []*models.Mock
				host, prev string
				test       time.Time
			}{
				{h2At("api.example:8443", 100), "api.example", "", time.Unix(200, 0)},
				{h2At("next.example:8443", 300), "next.example", "api.example", time.Unix(400, 0)},
			} {
				session := func() {
					t.Helper()
					if err := p.Mock(ctx, models.OutgoingOptions{}); err != nil {
						t.Fatalf("Mock: %v", err)
					}
				}
				if seededFirst {
					p.SeedRecordedDestinations(set.seed)
					session()
				} else {
					session()
					p.SeedRecordedDestinations(set.seed)
				}
				if err := p.SetMocksWithWindow(ctx, nil, nil, models.BaseTime, time.Now()); err != nil {
					t.Fatal(err)
				}
				if !p.replayTargetHasHTTP2Mock(set.host, 8443) {
					t.Fatalf("set %d: while the app starts, a destination recorded speaking HTTP/2 is not offered h2", i+1)
				}
				if p.replayTargetHasHTTP2Mock("other.example", 8443) || p.replayTargetHasHTTP2Mock(set.host, 9443) {
					t.Fatalf("set %d: another destination is offered h2", i+1)
				}
				if set.prev != "" && p.replayTargetHasHTTP2Mock(set.prev, 8443) {
					t.Fatalf("set %d: the previous store's HTTP/2 destination outlived the next store", i+1)
				}
				if err := p.SetMocksWithWindow(ctx, nil, nil, set.test, set.test.Add(time.Second)); err != nil {
					t.Fatal(err)
				}
				if p.replayTargetHasHTTP2Mock(set.host, 8443) {
					t.Fatalf("set %d: in a test whose window holds no HTTP/2 mock of the destination, the seed still offered h2", i+1)
				}
			}
		})
	}
}

// Under exact serving (KEPLOY_PROVEN) a test's own mocks live only in the open
// view, never in the staged trees. A TLS destination whose HTTP/2 mocks all
// belong to tests, connected lazily during test-1, is offered h2 from the
// view: the seed decides while the app starts, test-1's view once it opens.
// A test whose view owns none of the destination's HTTP/2 mocks is not
// offered h2, and a staged one the open view hides (the pool's, recorded
// between tests) offers h2 only while the view shows it.
func TestUnderExactServingTheOpenTestsHTTP2MocksOfferH2(t *testing.T) {
	ctx := context.Background()
	h2 := func(name string, at int64) *models.Mock {
		return &models.Mock{Name: name, Kind: models.HTTP2, Spec: models.MockSpec{
			HTTP2Req: &models.HTTP2Req{Scheme: "https", Authority: "api.example:8443"}, ReqTimestampMock: time.Unix(at, 0)}}
	}
	plain := &models.Mock{Name: "plain", Kind: models.HTTP}
	start := func(t *testing.T, seed, staged []*models.Mock) (*Proxy, *MockManager) {
		t.Helper()
		p := testProxy(t)
		p.SeedRecordedDestinations(seed)
		if err := p.Mock(ctx, models.OutgoingOptions{}); err != nil {
			t.Fatalf("Mock: %v", err)
		}
		if err := p.SetMocksWithWindow(ctx, staged, nil, models.BaseTime, time.Now()); err != nil {
			t.Fatal(err)
		}
		return p, p.getMockManager()
	}

	t.Run("the open test's own", func(t *testing.T) {
		own1 := h2("h2-test-1", 200)
		p, mm := start(t, []*models.Mock{own1}, nil)
		mm.ServeExactPool("-", nil)
		if !p.replayTargetHasHTTP2Mock("api.example", 8443) {
			t.Fatal("while the app starts, a destination recorded speaking HTTP/2 is not offered h2")
		}
		mm.ServeExactTest("test-1", []*models.Mock{own1})
		if !p.replayTargetHasHTTP2Mock("api.example", 8443) {
			t.Fatal("test-1 owns an HTTP/2 mock of the destination, and a connection it opens is offered http/1.1")
		}
		if p.replayTargetHasHTTP2Mock("api.example", 9443) {
			t.Fatal("another destination is offered h2")
		}
		mm.ServeExactTest("test-2", []*models.Mock{plain})
		if p.replayTargetHasHTTP2Mock("api.example", 8443) {
			t.Fatal("test-2 owns no HTTP/2 mock of the destination, and is offered h2")
		}
	})
	t.Run("a staged one the view hides", func(t *testing.T) {
		pool := h2("h2-pool", 400)
		p, mm := start(t, nil, []*models.Mock{pool})
		mm.ServeExactPool("k", []string{pool.Name})
		if !p.replayTargetHasHTTP2Mock("api.example", 8443) {
			t.Fatal("between tests the view serves the pool's HTTP/2 mock, and its destination is not offered h2")
		}
		mm.ServeExactTest("test-2", []*models.Mock{plain})
		if p.replayTargetHasHTTP2Mock("api.example", 8443) {
			t.Fatal("test-2's view hides the pool's HTTP/2 mock, and its destination is offered h2 from the staged trees")
		}
	})
}

// Under the proven switch a set's staging reads only what was recorded before
// its first test, and a test's own mocks reach the proxy as the test opens,
// which derives nothing (ServeExactTest). What a built-in default MySQL port
// held is then known only from the on-disk store's seed: an HTTP or gRPC
// service the app called on 4000 only inside its tests replays on its client's
// bytes, not greeted as MySQL, from the app's start on, whichever of the seed
// and the session comes first. MySQL recorded there (TiDB), or a MySQL mock
// that names no address, keeps the port MySQL, and each set's seed replaces
// the previous one's.
func TestASeededDefaultPortDestinationDecidesItsReplay(t *testing.T) {
	inTest := func(name string, m *models.Mock) *models.Mock {
		m.Name = name
		m.Spec.ReqTimestampMock = time.Unix(100, 0).UTC()
		m.Spec.ResTimestampMock = m.Spec.ReqTimestampMock.Add(time.Millisecond)
		m.TestModeInfo.Lifetime = models.LifetimePerTest
		return m
	}
	ctx := context.Background()
	const request = "GET /session HTTP/1.1\r\nHost: 127.0.0.1:4000\r\n\r\n"
	for name, seededFirst := range map[string]bool{"seeded, then the session starts": true, "the session starts, then seeded": false} {
		t.Run(name, func(t *testing.T) {
			p := testProxy(t)
			for i, set := range []struct {
				recorded []*models.Mock
				isMySQL  bool
				reason   string
			}{
				{[]*models.Mock{inTest("login", httpMockTo("127.0.0.1:4000"))}, false, "recorded-not-mysql"},
				{[]*models.Mock{inTest("tidb", mysqlMockAt("tidb:4000"))}, true, "known-port"},
				{[]*models.Mock{inTest("orders", grpcMockTo("orders:4000"))}, false, "recorded-not-mysql"},
				// Every mock keploy records names the address its
				// connection reached, read before its Host: a Host
				// naming no port, or another port, still says 4000,
				// and a Host claiming 4000 on a connection to 8080
				// says nothing of it.
				{[]*models.Mock{inTest("login", httpMockAt("127.0.0.1:4000", "api.internal"))}, false, "recorded-not-mysql"},
				{[]*models.Mock{inTest("login", httpMockAt("127.0.0.1:4000", "api.internal:8080"))}, false, "recorded-not-mysql"},
				{[]*models.Mock{inTest("login", httpMockAt("10.0.0.9:8080", "api.internal:4000"))}, true, "default-port"},
				{[]*models.Mock{inTest("login", httpMockTo("127.0.0.1:4000")), inTest("old-mysql", mysqlMockAt(""))}, true, "default-port"},
				{nil, true, "default-port"},
			} {
				s, err := NewDiskMocks(zap.NewNop())
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = s.Close() })
				for _, m := range set.recorded {
					if err := s.Add(m); err != nil {
						t.Fatal(err)
					}
				}
				seed, err := s.LoadDestinations()
				if err != nil {
					t.Fatal(err)
				}
				if seededFirst {
					p.SeedRecordedDestinations(seed)
				}
				if err := p.Mock(ctx, models.OutgoingOptions{}); err != nil {
					t.Fatalf("Mock: %v", err)
				}
				if !seededFirst {
					p.SeedRecordedDestinations(seed)
				}
				// The set's staging: nothing recorded before its first test.
				if err := p.SetMocksWithWindow(ctx, nil, nil, models.BaseTime, time.Now()); err != nil {
					t.Fatal(err)
				}
				app, srcConn := clientPair(t)
				if !set.isMySQL {
					if _, err := io.WriteString(app, request); err != nil {
						t.Fatal(err)
					}
				}
				r := awaitEvent(t, probeAsync(p, srcConn, "127.0.0.1:4000", 4000, models.MODE_TEST, models.OutgoingOptions{}), "the probe's verdict")
				if r.err != nil || r.probe.IsMySQL != set.isMySQL || r.probe.Reason != set.reason {
					t.Fatalf("set %d: IsMySQL=%v reason=%q err=%v, want %v/%s", i+1, r.probe.IsMySQL, r.probe.Reason, r.err, set.isMySQL, set.reason)
				}
			}
		})
	}
}
