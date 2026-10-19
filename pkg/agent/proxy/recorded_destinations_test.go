package proxy

import (
	"context"
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
		"an HTTP mock":                 {Kind: models.HTTP, Spec: models.MockSpec{Metadata: map[string]string{"destAddr": "10.0.0.2:80"}}},
		"a MySQL mock, no address":     destMock(models.MySQL, map[string]string{}),
		"an HTTP/2 mock, no authority": {Kind: models.HTTP2, Spec: models.MockSpec{HTTP2Req: &models.HTTP2Req{Scheme: "https"}}},
		"no mock":                      nil,
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
		if err := p.BeginTestSet(ctx, time.Time{}, nil, nil, nil); err != nil {
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
				if err := p.BeginTestSet(ctx, time.Time{}, nil, nil, nil); err != nil {
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
		if err := p.BeginTestSet(ctx, time.Time{}, nil, staged, nil); err != nil {
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
