package http

import (
	"context"
	"net/netip"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/containerkey"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// containerIndex places addresses at containers, as the index of the local
// Docker daemon places published ports.
type containerIndex map[string]containerkey.Container

func (r containerIndex) Lookup(dst netip.AddrPort) (containerkey.Container, bool, error) {
	c, ok := r[dst.String()]
	return c, ok, nil
}
func (containerIndex) Fingerprint(context.Context, string) (string, bool) { return "", false }
func (containerIndex) Pending(netip.AddrPort) ([]string, bool)            { return nil, true }
func (containerIndex) AwaitReadsOf(context.Context, []string)             {}
func (containerIndex) Bind(context.Context)                               {}
func (containerIndex) Watch(context.Context, *zap.Logger)                 {}
func (containerIndex) Endpoint() string                                   { return "unix:///fake.sock" }

const (
	fpQueue  = "aaaaaaaaaaaa" // localstack configured for the orders queue
	fpEvents = "bbbbbbbbbbbb" // localstack configured for the events queue
)

func localstack(id, fp string) containerkey.Container {
	return containerkey.Container{ID: id, Image: "localstack/localstack:3.8", Port: 4566, Fingerprint: fp}
}

func keyOfContainer(c containerkey.Container) string {
	k, _ := containerkey.KeyOf(c)
	return k.String()
}

// session is a keyer over idx whose connections are named as the proxy names
// them, and a context per connection.
type session struct {
	k *containerkey.Keyer
}

func newSession(idx containerIndex) *session {
	return &session{k: containerkey.NewKeyer(idx)}
}

func (s *session) conn(id, addr string) context.Context {
	s.k.Name(zap.NewNop(), id, addr)
	return containerkey.WithConn(context.Background(), s.k, id)
}

// recordedFrom is a recording of a GET of path on host, stamped as the
// keyer stamps one of c's connections in scope.
func recordedFrom(name, path, host string, c containerkey.Container, scope string) *models.Mock {
	m := onHost(name, path, host)
	m.Spec.Metadata = map[string]string{
		containerkey.MetadataKey: keyOfContainer(c),
		containerkey.InstanceKey: "inst-" + c.ID,
		containerkey.ScopeKey:    scope,
	}
	return m
}

// useDefaultKeyer makes the agent's keyer k for the test: the matcher reads a
// recording's ambiguity from it.
func useDefaultKeyer(t *testing.T, k *containerkey.Keyer, recorded ...*models.Mock) {
	t.Helper()
	saved := containerkey.Default
	containerkey.Default = k
	t.Cleanup(func() { containerkey.Default = saved })
	var tally containerkey.RecordedTally
	for _, m := range recorded {
		tally.Add(m)
	}
	k.SetRecorded(&tally)
}

// A container is the same place across runs by its key: a request reaches
// it on another published port than the recording's, and still matches the
// recording of that container (of another version of the image, too), never
// a recording of another container. A recording with no container instance
// is compared by its authority.
func TestStrictMatchesAContainerByItsKey(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	s := newSession(containerIndex{"127.0.0.1:50001": localstack("live", fpQueue)})
	rec := recordedFrom("ls", "/queue", "localhost:49153", containerkey.Container{ID: "old", Image: "localstack/localstack:3.7", Port: 4566, Fingerprint: fpQueue}, "s1")
	other := recordedFrom("events", "/queue", "localhost:49153", localstack("old2", fpEvents), "s1")
	useDefaultKeyer(t, s.k, rec, other)
	ctx := s.conn("c1", "127.0.0.1:50001")

	if got, _ := matchOnce(t, ctx, getTo("/queue", "localhost:50001"), other, rec); got == nil || got.Name != "ls" {
		t.Fatalf("served %v, want ls (the same repository, port and configuration)", got)
	}
	if got, diag := matchOnce(t, ctx, getTo("/queue", "localhost:50001"), other); got != nil || diag == nil {
		t.Fatalf("a recording of a container configured otherwise was served: %v", got)
	}
	plain := onHost("plain", "/queue", "localhost:49153")
	if got, _ := matchOnce(t, ctx, getTo("/queue", "localhost:50001"), plain); got != nil {
		t.Fatalf("a recording with no container, on another port, was served: %s", got.Name)
	}
	if got, _ := matchOnce(t, ctx, getTo("/queue", "localhost:49153"), plain); got == nil {
		t.Fatal("a recording with no container, at the same authority, was not served")
	}
}

// A container Docker gave the recorded container's port this run is another
// container, and misses its recordings: the port reuse keys exist for.
func TestStrictAnotherContainerAtTheSameAddressMisses(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	s := newSession(containerIndex{"127.0.0.1:49153": localstack("live", fpEvents)})
	rec := recordedFrom("ls", "/queue", "localhost:49153", localstack("old", fpQueue), "s1")
	pg := recordedFrom("pg", "/queue", "localhost:49153", containerkey.Container{ID: "pg", Image: "postgres:16", Port: 4566, Fingerprint: fpEvents}, "s1")
	useDefaultKeyer(t, s.k, rec, pg)
	ctx := s.conn("c1", "127.0.0.1:49153")
	for _, m := range []*models.Mock{rec, pg} {
		got, diag := matchOnce(t, ctx, getTo("/queue", "localhost:49153"), m)
		if got != nil {
			t.Fatalf("another container's recording at the same address was served: %s", got.Name)
		}
		live := liveDestination(ctx, getTo("/queue", "localhost:49153"))
		if d := destinationDiff(live, m); len(d) != 1 || d[0].Path != "container" {
			t.Fatalf("%s: the destination diff is %+v, want the container (diag %+v)", m.Name, d, diag)
		}
	}
}

// A recording whose container no key names (its configuration was not read)
// answers no call, and the miss says the container differs, not the host,
// whatever the call reached: a container, or none at the very authority.
func TestStrictARecordingOfAnUnnamedContainerMissesOnTheContainer(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	s := newSession(containerIndex{"127.0.0.1:50001": localstack("live", fpQueue)})
	unnamed := onHost("unnamed", "/queue", "localhost:49153")
	unnamed.Spec.Metadata = map[string]string{containerkey.InstanceKey: "inst-old", containerkey.ScopeKey: "s1"}
	useDefaultKeyer(t, s.k, unnamed)
	for _, c := range []struct {
		name string
		ctx  context.Context
		host string
	}{
		{"a container", s.conn("c1", "127.0.0.1:50001"), "localhost:50001"},
		{"no container, at the recorded authority", context.Background(), "localhost:49153"},
	} {
		if got, _ := matchOnce(t, c.ctx, getTo("/queue", c.host), unnamed); got != nil {
			t.Fatalf("%s: served %s", c.name, got.Name)
		}
		live := liveDestination(c.ctx, getTo("/queue", c.host))
		d := destinationDiff(live, unnamed)
		if len(d) != 1 || d[0].Path != "container" || d[0].Expected == d[0].Actual {
			t.Fatalf("%s: the destination diff is %+v, want the container", c.name, d)
		}
	}
}

// A name under .localhost is a virtual host like any other: a gateway on
// this host (Traefik's whoami.localhost) routes by it.
func TestStrictANameUnderLocalhostIsAVirtualHost(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	s := newSession(containerIndex{"127.0.0.1:50001": localstack("live", fpQueue)})
	rec := recordedFrom("api", "/obj", "api.localhost:49153", localstack("old", fpQueue), "s1")
	useDefaultKeyer(t, s.k, rec)
	ctx := s.conn("c1", "127.0.0.1:50001")
	if got, _ := matchOnce(t, ctx, getTo("/obj", "api.localhost:50001"), rec); got == nil {
		t.Fatal("the same name on another port was not served")
	}
	for _, h := range []string{"web.localhost:50001", "localhost:50001"} {
		if got, _ := matchOnce(t, ctx, getTo("/obj", h), rec); got != nil {
			t.Fatalf("%s was served the recording for api.localhost", h)
		}
	}
}

// Behind one container the hostname the client sent must agree too: a
// gateway or LocalStack routes by virtual host. Addresses (an IP literal or
// localhost) name no virtual host, so a container reached by address on a
// random port still matches its recording by address, while a recording by
// name answers no call by address.
func TestStrictComparesTheVirtualHostBehindAContainer(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	s := newSession(containerIndex{"127.0.0.1:50001": localstack("live", fpQueue)})
	byName := recordedFrom("bucket-a", "/obj", "bucket-a.s3.localhost.localstack.cloud:4566", localstack("old", fpQueue), "s1")
	byAddr := recordedFrom("by-addr", "/obj", "localhost:49153", localstack("old", fpQueue), "s1")
	useDefaultKeyer(t, s.k, byName, byAddr)
	ctx := s.conn("c1", "127.0.0.1:50001")
	if got, _ := matchOnce(t, ctx, getTo("/obj", "bucket-a.s3.localhost.localstack.cloud:50001"), byName); got == nil {
		t.Fatal("the same virtual host on another port was not served")
	}
	if got, _ := matchOnce(t, ctx, getTo("/obj", "bucket-b.s3.localhost.localstack.cloud:50001"), byName); got != nil {
		t.Fatal("another virtual host behind the same container was served")
	}
	for _, h := range []string{"localhost:50001", "127.0.0.1:50001", "[::1]:50001"} {
		if got, _ := matchOnce(t, ctx, getTo("/obj", h), byAddr); got == nil {
			t.Fatalf("%s: a recording by address was not served to a call by address", h)
		}
		if got, _ := matchOnce(t, ctx, getTo("/obj", h), byName); got != nil {
			t.Fatalf("%s: a recording by virtual host was served to a call by address", h)
		}
	}
}

// Two containers configured the same cannot be told apart: where the live
// scope used both, or one recorded scope holds recordings of both, neither
// is answered by a container's recording, by key or by address, so neither
// takes the other's answer. A recording with no container is still compared
// by its authority.
func TestStrictContainersConfiguredTheSameMiss(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	s := newSession(containerIndex{"127.0.0.1:49153": localstack("live-a", fpQueue), "127.0.0.1:49154": localstack("live-b", fpQueue)})
	rec := recordedFrom("a", "/queue", "localhost:49153", localstack("old-a", fpQueue), "s1")
	useDefaultKeyer(t, s.k, rec)
	a := s.conn("a", "127.0.0.1:49153")
	b := s.conn("b", "127.0.0.1:49154")
	if got, _ := matchOnce(t, a, getTo("/queue", "localhost:49153"), rec); got == nil {
		t.Fatal("the first container of its configuration, alone in the scope, was not served")
	}
	if got, _ := matchOnce(t, b, getTo("/queue", "localhost:49154"), rec); got != nil {
		t.Fatalf("a second container configured the same was served %s", got.Name)
	}
	if got, _ := matchOnce(t, a, getTo("/queue", "localhost:49153"), rec); got != nil {
		t.Fatalf("the first container, once its twin was used, was served %s at its own address", got.Name)
	}
	plain := onHost("plain", "/queue", "localhost:49153")
	if got, _ := matchOnce(t, a, getTo("/queue", "localhost:49153"), plain); got == nil {
		t.Fatal("a recording with no container, at the same authority, was not served")
	}

	// Recorded: one scope answered two containers of one key.
	s2 := newSession(containerIndex{"127.0.0.1:50001": localstack("live", fpQueue)})
	x := recordedFrom("x", "/queue", "localhost:49153", localstack("rec-x", fpQueue), "s7")
	y := recordedFrom("y", "/queue", "localhost:49154", localstack("rec-y", fpQueue), "s7")
	useDefaultKeyer(t, s2.k, x, y)
	ctx := s2.conn("c", "127.0.0.1:50001")
	if got, _ := matchOnce(t, ctx, getTo("/queue", "localhost:50001"), x, y); got != nil {
		t.Fatalf("a recording of an ambiguous key was served: %s", got.Name)
	}
	if got, _ := matchOnce(t, ctx, getTo("/queue", "localhost:49153"), x); got != nil {
		t.Fatalf("a recording of an ambiguous key was served by address: %s", got.Name)
	}
	// Nor to a call keploy does not know as a container's (Docker unread, or
	// a container the index has not read): an address names no container.
	if got, _ := matchOnce(t, context.Background(), getTo("/queue", "localhost:49153"), x); got != nil {
		t.Fatalf("a recording of an ambiguous key was served by address to an unknown destination: %s", got.Name)
	}
	unread := onHost("unread", "/queue", "localhost:49153")
	unread.Spec.Metadata = map[string]string{containerkey.InstanceKey: "inst-u", containerkey.ScopeKey: "s1"}
	if got, _ := matchOnce(t, context.Background(), getTo("/queue", "localhost:49153"), unread); got != nil {
		t.Fatalf("a recording of a container whose key was unreadable was served by address: %s", got.Name)
	}
}

// Concurrent first uses of two containers of one image, in either order:
// configured differently, each call gets its own container's recording;
// configured the same, both miss. Never swapped (founder decision 19).
func TestStrictConcurrentFirstUsesNeverSwap(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	base := time.Unix(1_700_000_000, 0)
	for _, tc := range []struct {
		name    string
		fpA     string
		fpB     string
		swapped bool
	}{
		{"configured differently", fpQueue, fpEvents, false},
		{"configured the same", fpQueue, fpQueue, true},
	} {
		for _, order := range [][]string{{"a", "b"}, {"b", "a"}} {
			t.Run(tc.name+"/"+order[0]+" first", func(t *testing.T) {
				// Recorded: both containers, one scope, A's call answered "A".
				ra := recordedFrom("rec-a", "/queue", "localhost:49153", localstack("rec-a", tc.fpA), "s1")
				rb := recordedFrom("rec-b", "/queue", "localhost:49154", localstack("rec-b", tc.fpB), "s1")
				ra.Spec.ReqTimestampMock, rb.Spec.ReqTimestampMock = base, base.Add(time.Second)
				s := newSession(containerIndex{"127.0.0.1:50001": localstack("live-a", tc.fpA), "127.0.0.1:50002": localstack("live-b", tc.fpB)})
				useDefaultKeyer(t, s.k, ra, rb)
				addr := map[string]string{"a": "127.0.0.1:50001", "b": "127.0.0.1:50002"}
				host := map[string]string{"a": "localhost:50001", "b": "localhost:50002"}
				got := map[string]string{}
				var mu sync.Mutex
				var wg sync.WaitGroup
				for _, c := range order {
					ctx := s.conn(c, addr[c])
					wg.Add(1)
					go func(c string, ctx context.Context) {
						defer wg.Done()
						db := &mockMemDb{mocks: []*models.Mock{ra, rb}, updateUnFilteredReturn: true}
						ok, stub, _, err := newHTTP().match(ctx, getTo("/queue", host[c]), db, nil, nil, nil, true, false, false, true, true)
						if err != nil {
							t.Error(err)
							return
						}
						name := ""
						if ok {
							name = stub.Name
						}
						mu.Lock()
						got[c] = name
						mu.Unlock()
					}(c, ctx)
				}
				wg.Wait()
				if tc.swapped {
					if got["a"] != "" || got["b"] != "" {
						t.Fatalf("containers configured the same were served %v, want both to miss", got)
					}
					return
				}
				if got["a"] != "rec-a" || got["b"] != "rec-b" {
					t.Fatalf("served %v, want each its own container's recording", got)
				}
			})
		}
	}
}

// The whole chain, record to replay, for two containers of one image used
// at once in one scope, in either order: the record keyer stamps each
// recording, the agent tallies the set, and the replay keyer names the live
// containers. Configured differently, each call gets its own container's
// answer; configured the same, every call misses. Never swapped.
func TestStrictContainersFromRecordToReplayNeverSwap(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	for _, tc := range []struct {
		name     string
		fpA, fpB string
		same     bool
	}{
		{"configured differently", fpQueue, fpEvents, false},
		{"configured the same", fpQueue, fpQueue, true},
	} {
		for _, first := range []string{"a", "b"} {
			t.Run(tc.name+"/"+first+" first", func(t *testing.T) {
				// Record: A answers "A", B answers "B", both in one scope.
				rec := newSession(containerIndex{"127.0.0.1:49153": localstack("rec-a", tc.fpA), "127.0.0.1:49154": localstack("rec-b", tc.fpB)})
				rec.k.BeginTest()
				var mocks []*models.Mock
				for i, c := range []struct{ id, addr, host string }{{"a", "127.0.0.1:49153", "localhost:49153"}, {"b", "127.0.0.1:49154", "localhost:49154"}} {
					rec.k.Name(zap.NewNop(), c.id, c.addr)
					m := onHost("rec-"+c.id, "/queue", c.host)
					m.ConnectionID = c.id
					m.Spec.ReqTimestampMock = time.Now().Add(time.Duration(i) * time.Millisecond)
					rec.k.Stamp(m)
					mocks = append(mocks, m)
				}
				rec.k.EndTest()

				// Replay: new containers on new ports.
				live := newSession(containerIndex{"127.0.0.1:50001": localstack("live-a", tc.fpA), "127.0.0.1:50002": localstack("live-b", tc.fpB)})
				useDefaultKeyer(t, live.k, mocks...)
				live.k.BeginTest()
				addr := map[string]string{"a": "127.0.0.1:50001", "b": "127.0.0.1:50002"}
				host := map[string]string{"a": "localhost:50001", "b": "localhost:50002"}
				order := []string{first, map[string]string{"a": "b", "b": "a"}[first]}
				got := map[string]string{}
				for _, c := range order {
					ctx := live.conn(c, addr[c])
					db := &mockMemDb{mocks: mocks, updateUnFilteredReturn: true}
					ok, stub, _, err := newHTTP().match(ctx, getTo("/queue", host[c]), db, nil, nil, nil, true, false, false, true, true)
					if err != nil {
						t.Fatal(err)
					}
					if ok {
						got[c] = stub.Name
					}
				}
				if tc.same {
					if len(got) != 0 {
						t.Fatalf("containers configured the same were served %v, want both to miss", got)
					}
					return
				}
				if got["a"] != "rec-a" || got["b"] != "rec-b" {
					t.Fatalf("served %v, want each its own container's recording", got)
				}
			})
		}
	}
}

// A recording made before secrets were left out of fingerprints names its
// container by the legacy key, and a live container whose legacy key that is
// matches it; one whose legacy key is another (its password changed since)
// misses, and the miss shows the live container by its current key, never
// by the legacy one, which a secret's value moves.
func TestStrictMatchesALegacyRecordingByTheLegacyKey(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	live := containerkey.Container{ID: "live", Image: "localstack/localstack:3.8", Port: 4566, Fingerprint: "s" + fpQueue, Legacy: "cccccccccccc"}
	s := newSession(containerIndex{"127.0.0.1:50001": live})
	legacy := func(name, legacyFP string) *models.Mock {
		m := onHost(name, "/queue", "localhost:49153")
		m.Spec.Metadata = map[string]string{
			containerkey.MetadataKey: "container:localstack/localstack:4566@" + legacyFP,
			containerkey.InstanceKey: "inst-" + name,
			containerkey.ScopeKey:    "s1",
		}
		return m
	}
	same, changed := legacy("same", "cccccccccccc"), legacy("changed", "dddddddddddd")
	useDefaultKeyer(t, s.k, same, changed)
	ctx := s.conn("c1", "127.0.0.1:50001")
	if got, _ := matchOnce(t, ctx, getTo("/queue", "localhost:50001"), changed, same); got == nil || got.Name != "same" {
		t.Fatalf("served %v, want the legacy recording of this container", got)
	}
	if got, _ := matchOnce(t, ctx, getTo("/queue", "localhost:50001"), changed); got != nil {
		t.Fatalf("a legacy recording of a container configured otherwise was served: %s", got.Name)
	}
	d := destinationDiff(liveDestination(ctx, getTo("/queue", "localhost:50001")), changed)
	if len(d) != 1 || d[0].Path != "container" || d[0].Actual != keyOfContainer(live) {
		t.Fatalf("the miss shows %+v, want the live container by its current key %s", d, keyOfContainer(live))
	}
}
