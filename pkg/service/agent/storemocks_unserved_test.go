package agent

// Tests for leaving a set's Redis mocks out of the pool in a build without a
// Redis parser (redisskip.Recorded), on both store paths.

import (
	"context"
	"strings"
	"testing"
	"time"

	coreAgent "go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/redisskip"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// asyncProxy keeps the names of the async corpus the store loads. Every
// other Proxy method is the nil embedded one's: the store calls none.
type asyncProxy struct {
	coreAgent.Proxy
	async []string
}

func (p *asyncProxy) LoadAsyncMocks(mocks []*models.Mock) {
	p.async = nil
	for _, m := range mocks {
		p.async = append(p.async, m.Name)
	}
}

// genericMock is a Generic mock of connection conn recorded at at, whose
// first request is data.
func genericMock(name, conn string, at time.Time, data string) *models.Mock {
	return &models.Mock{Name: name, Kind: models.GENERIC, Spec: models.MockSpec{
		Metadata:         map[string]string{"connID": conn},
		ReqTimestampMock: at,
		ResTimestampMock: at.Add(time.Millisecond),
		GenericRequests:  []models.Payload{{Origin: models.FromClient, Message: []models.OutputBinary{{Type: models.String, Data: data}}}},
	}}
}

// redisSet is a set an older keploy recorded of a Redis connection (its
// HELLO a session mock, its GET and SET per-test ones) and of a memcached
// one. The per-test mocks are listed before the session ones, as the stream
// sends them, so the Redis connection's first exchange comes last.
func redisSet() (filtered, unfiltered []*models.Mock) {
	base := time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC)
	filtered = []*models.Mock{
		genericMock("mock-2", "1", base.Add(time.Second), "*2\r\n$3\r\nGET\r\n$1\r\nk\r\n"),
		genericMock("mock-3", "2", base.Add(2*time.Second), "get k\r\n"),
		genericMock("mock-4", "1", base.Add(3*time.Second), "*3\r\n$3\r\nSET\r\n$1\r\nk\r\n$1\r\nv\r\n"),
	}
	// Per-test (derived already, as a disk store's loader leaves them), so
	// that the stream parks them on disk.
	for _, m := range filtered {
		m.Spec.Metadata["type"] = "mocks"
		m.TestModeInfo.Lifetime, m.TestModeInfo.LifetimeDerived = models.LifetimePerTest, true
	}
	// One of each is async: the async corpus loses the one left out too.
	filtered[1].Spec.Async = &models.AsyncMeta{Lane: "poll"}
	filtered[2].Spec.Async = &models.AsyncMeta{Lane: "poll"}
	unfiltered = []*models.Mock{
		genericMock("mock-1", "1", base, "*2\r\n$5\r\nhello\r\n$1\r\n3\r\n"),
		mkMock("mock-5", models.DNS, base),
	}
	unfiltered[0].Spec.Metadata["type"] = "config"
	return filtered, unfiltered
}

func names(mocks []*models.Mock) string {
	var out []string
	for _, m := range mocks {
		out = append(out, m.Name)
	}
	return strings.Join(out, ",")
}

// Both stores of a test set's mocks, whole and streamed (where a connection's
// first exchange comes after its others), leave out every mock of the Redis
// connection and keep the rest in order, the async corpus included, and say
// once how many they left out, why, and which. A mapping that names a mock
// left out owns nothing for it, and is not taken for one of a recording that
// is not loaded.
func TestTheStoreLeavesOutTheSetsRedisMocks(t *testing.T) {
	for _, path := range []string{"StoreMocks", "StoreMocksStream"} {
		t.Run(path, func(t *testing.T) {
			RegisterHooks(&captureHook{})
			core, logs := observer.New(zapcore.InfoLevel)
			proxy := &asyncProxy{}
			a := strictAgent(true)
			a.logger, a.Proxy = zap.New(core), proxy
			filtered, unfiltered := redisSet()
			switch path {
			case "StoreMocks":
				if err := a.StoreMocks(context.Background(), filtered, unfiltered); err != nil {
					t.Fatal(err)
				}
			default:
				header, dec := readHeaderAndDecoder(t, encodeStreamBody(t, filtered, unfiltered))
				if err := a.StoreMocksStream(context.Background(), header, dec); err != nil {
					t.Fatal(err)
				}
			}
			s := loadStorage(t, a)
			// The stream parks the per-test mocks on disk as it reads
			// them, before the set's Redis mocks are known: those stay
			// there, and every read of the disk leaves them out.
			if path == "StoreMocksStream" && (s.diskMocks == nil || s.diskMocks.Len() != 3) {
				t.Fatalf("the stream parked %v per-test mocks on disk, want all three", s.diskMocks)
			}
			if got := names(s.filtered); got != "" && got != "mock-3" {
				t.Errorf("the per-test mocks kept resident are %s, want memcached's alone (or none, on disk)", got)
			}
			if got := names(s.unfiltered); got != "mock-5" {
				t.Errorf("the session mocks stored are %s, want the DNS one alone", got)
			}
			if got := strings.Join(proxy.async, ","); got != "mock-3" {
				t.Errorf("the async corpus loaded is %q, want memcached's alone", got)
			}
			// What a test is served: nothing of the Redis connection, from
			// memory or from the disk.
			served, _, err := a.mocksForParams(context.Background(), models.MockFilterParams{})
			if err != nil {
				t.Fatal(err)
			}
			if got := names(served); got != "mock-3" {
				t.Errorf("a test is staged %s, want memcached's per-test mock alone", got)
			}
			// Nor is a Redis mock parked on disk an earlier place a learned
			// value is read from (the HTTP learn step's recordings before a
			// mock): its call is made for real.
			if before := a.diskRecordedBefore(); path == "StoreMocksStream" {
				if before == nil {
					t.Fatal("the stream's store reads no recording made before an instant, though it parked three")
				}
				var visited []*models.Mock
				if err := before(time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), func(m *models.Mock) bool {
					visited = append(visited, m)
					return true
				}); err != nil {
					t.Fatal(err)
				}
				if got := names(visited); got != "mock-3" {
					t.Errorf("the recordings read before an instant from the disk are %s, want memcached's alone", got)
				}
			}
			said := logs.FilterMessage(redisskip.LeftOutNotice).All()
			if len(said) != 1 || said[0].Level != zapcore.WarnLevel {
				t.Fatalf("the store said it left Redis mocks out %d times (%v), want once at warn", len(said), said)
			}
			if fields := said[0].ContextMap(); fields["mocks"] != int64(3) {
				t.Errorf("the store said it left out %v mocks, want 3", fields["mocks"])
			}
			// What the agent says it holds is what it was sent, the mocks
			// it left out counted in: keploy test's compose guard takes a
			// count short of that for an agent that lost its mocks.
			if stats, err := a.MockStats(context.Background()); err != nil || stats.Loaded != 5 {
				t.Errorf("the agent says it holds %d mocks (%v), want the five it was sent", stats.Loaded, err)
			}
			found, err := a.mocksByName([]string{"mock-3", "mock-2", "mock-1"})
			if err != nil {
				t.Fatalf("a mapping naming mocks the store left out was refused: %v", err)
			}
			if len(found) != 1 || found["mock-3"] == nil {
				t.Fatalf("the mapping found %v, want mock-3 alone", found)
			}
			if _, err := a.mocksByName([]string{"never-recorded"}); err == nil {
				t.Fatal("a mapping naming a mock the set never had was not refused")
			}
		})
	}
}

// A build that links a Redis parser (Keploy Enterprise's, registered as
// integrations.REDIS) stores every mock, and says nothing.
func TestTheStoreKeepsRedisMocksInABuildWithARedisParser(t *testing.T) {
	integrations.Register(integrations.REDIS, &integrations.Parsers{
		Initializer: func(*zap.Logger) integrations.Integrations { return nil },
		Priority:    200,
	})
	t.Cleanup(func() { delete(integrations.Registered, integrations.REDIS) })
	RegisterHooks(&captureHook{})
	core, logs := observer.New(zapcore.InfoLevel)
	a := &Agent{logger: zap.New(core)}
	filtered, unfiltered := redisSet()
	if err := a.StoreMocks(context.Background(), filtered, unfiltered); err != nil {
		t.Fatal(err)
	}
	s := loadStorage(t, a)
	if len(s.filtered) != 3 || len(s.unfiltered) != 2 {
		t.Fatalf("stored %s and %s, want every mock", names(s.filtered), names(s.unfiltered))
	}
	if logs.FilterMessage(redisskip.LeftOutNotice).Len() != 0 {
		t.Fatalf("the store said it left Redis mocks out: %v", logs.All())
	}
}

// A connection is judged by its first exchange as recorded, before the
// store refuses any mock: under the proven switch, a first exchange whose
// variables keploy cannot follow would be refused, and the connection is
// still the Redis one the skip takes at replay (its HELLO), as keploy test
// judges it; its next exchange, a module command outside the skip's table,
// is left out with it. A mock left out is never served, so the store says
// nothing of not following its variables (an ERROR), and counts it as held.
func TestTheStoreJudgesAConnectionByItsFirstExchangeBeforeRefusingAny(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	base := time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC)
	// perTest: the Redis connection's mocks are per-test ones, which the
	// stream parks on disk as it reads them, but the one it refuses.
	set := func(perTest bool) (filtered, unfiltered []*models.Mock) {
		hello := genericMock("mock-1", "1", base, "*2\r\n$5\r\nhello\r\n$1\r\n3\r\n")
		hello.Spec.Variables = models.MockVariables{"v": {}}
		graph := genericMock("mock-2", "1", base.Add(time.Second), "*2\r\n$11\r\nGRAPH.QUERY\r\n$1\r\ng\r\n")
		// A memcached get with variables keploy cannot follow is refused,
		// and says why.
		get := genericMock("mock-3", "2", base.Add(2*time.Second), "get k\r\n")
		get.Spec.Variables = models.MockVariables{"v": {}}
		if !perTest {
			return nil, []*models.Mock{hello, graph, get}
		}
		for _, m := range []*models.Mock{hello, graph} {
			m.Spec.Metadata["type"] = "mocks"
			m.TestModeInfo.Lifetime, m.TestModeInfo.LifetimeDerived = models.LifetimePerTest, true
		}
		return []*models.Mock{hello, graph}, []*models.Mock{get}
	}
	for _, path := range []string{"StoreMocks", "StoreMocksStream", "StoreMocksStream per-test"} {
		t.Run(path, func(t *testing.T) {
			RegisterHooks(&captureHook{})
			core, logs := observer.New(zapcore.InfoLevel)
			a := strictAgent(true)
			a.logger, a.Proxy = zap.New(core), &asyncProxy{}
			filtered, unfiltered := set(strings.HasSuffix(path, "per-test"))
			switch path {
			case "StoreMocks":
				if err := a.StoreMocks(context.Background(), filtered, unfiltered); err != nil {
					t.Fatal(err)
				}
			default:
				header, dec := readHeaderAndDecoder(t, encodeStreamBody(t, filtered, unfiltered))
				if err := a.StoreMocksStream(context.Background(), header, dec); err != nil {
					t.Fatal(err)
				}
			}
			s := loadStorage(t, a)
			if !s.leftOut["mock-1"] || !s.leftOut["mock-2"] || len(s.unfiltered) != 0 {
				t.Fatalf("the store left out %v and kept %s: the connection, which opens with HELLO, was judged by an exchange after its first", s.leftOut, names(s.unfiltered))
			}
			if len(s.refused) != 1 || !s.refused["mock-3"] {
				t.Fatalf("the store refused %v, want mock-3 alone: a mock it leaves out is never served, so it is not refused", s.refused)
			}
			said := logs.FilterMessageSnippet("keploy cannot follow the variables").All()
			if len(said) != 1 || said[0].ContextMap()["mock"] != "mock-3" {
				t.Fatalf("the store said it cannot follow variables %d times (%v), want once, of mock-3", len(said), said)
			}
			if why, want := said[0].ContextMap()["why"], a.unfollowable(unfiltered[len(unfiltered)-1]); want == "" || why != want {
				t.Errorf("the store said mock-3 is refused because %q, want %q", why, want)
			}
			if stats, err := a.MockStats(context.Background()); err != nil || stats.Loaded != 2 {
				t.Errorf("the agent says it holds %d mocks (%v), want the two it left out: the refused one is not held", stats.Loaded, err)
			}
			// The per-test GRAPH.QUERY is parked on disk; the HELLO, held
			// back to be refused, never is.
			if path == "StoreMocksStream per-test" && (s.diskMocks == nil || s.diskMocks.Len() != 1) {
				t.Errorf("the stream parked %v per-test mocks on disk, want mock-2 alone", s.diskMocks)
			}
		})
	}
}

// A build that links a Redis parser leaves nothing out, and its stream
// refuses a mock whose variables keploy cannot follow as it reads it, a
// Redis connection's too.
func TestTheStreamOfABuildWithARedisParserRefusesAsItReads(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	integrations.Register(integrations.REDIS, &integrations.Parsers{
		Initializer: func(*zap.Logger) integrations.Integrations { return nil },
		Priority:    200,
	})
	t.Cleanup(func() { delete(integrations.Registered, integrations.REDIS) })
	RegisterHooks(&captureHook{})
	core, logs := observer.New(zapcore.InfoLevel)
	a := strictAgent(true)
	a.logger, a.Proxy = zap.New(core), &asyncProxy{}
	base := time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC)
	hello := genericMock("mock-1", "1", base, "*2\r\n$5\r\nhello\r\n$1\r\n3\r\n")
	hello.Spec.Variables = models.MockVariables{"v": {}}
	ping := genericMock("mock-2", "1", base.Add(time.Second), "*1\r\n$4\r\nping\r\n")
	header, dec := readHeaderAndDecoder(t, encodeStreamBody(t, nil, []*models.Mock{hello, ping}))
	if err := a.StoreMocksStream(context.Background(), header, dec); err != nil {
		t.Fatal(err)
	}
	s := loadStorage(t, a)
	if len(s.leftOut) != 0 || names(s.unfiltered) != "mock-2" || !s.refused["mock-1"] {
		t.Fatalf("stored %s, left out %v, refused %v: want mock-2 stored, nothing left out, mock-1 refused", names(s.unfiltered), s.leftOut, s.refused)
	}
	if n := logs.FilterMessageSnippet("keploy cannot follow the variables").Len(); n != 1 {
		t.Fatalf("the store said it cannot follow mock-1's variables %d times, want once", n)
	}
}

// Only a build that can leave Redis mocks out holds its refusals until the
// set is read: one with a Redis parser refuses (and says why) as it reads,
// as before, which shows when the stream breaks off after the refused mock.
func TestTheStreamRefusesAsItReadsOnlyWhereNothingIsLeftOut(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	for _, tc := range []struct {
		name   string
		parser bool
		said   int
	}{{"a build with a Redis parser", true, 1}, {"a build without one", false, 0}} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.parser {
				integrations.Register(integrations.REDIS, &integrations.Parsers{
					Initializer: func(*zap.Logger) integrations.Integrations { return nil },
					Priority:    200,
				})
				t.Cleanup(func() { delete(integrations.Registered, integrations.REDIS) })
			}
			RegisterHooks(&captureHook{})
			core, logs := observer.New(zapcore.InfoLevel)
			a := strictAgent(true)
			a.logger, a.Proxy = zap.New(core), &asyncProxy{}
			get := genericMock("mock-1", "2", time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC), "get k\r\n")
			get.Spec.Variables = models.MockVariables{"v": {}}
			header, dec := readHeaderAndDecoder(t, encodeStreamBody(t, nil, []*models.Mock{get}))
			header.UnfilteredCount++ // a mock the stream never sends
			if err := a.StoreMocksStream(context.Background(), header, dec); err == nil {
				t.Fatal("precondition: a stream that breaks off was stored")
			}
			if n := logs.FilterMessageSnippet("keploy cannot follow the variables").Len(); n != tc.said {
				t.Fatalf("before the stream broke off, the store said %d times that it cannot follow mock-1's variables, want %d", n, tc.said)
			}
		})
	}
}
