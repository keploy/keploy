package generic

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// statedOf is each mock's stated lifetime and type, in order.
func statedOf(mocks []*models.Mock) []string {
	var out []string
	for _, m := range mocks {
		out = append(out, m.Spec.Metadata[models.MetadataLifetimeKey]+"/"+m.Spec.Metadata["type"])
	}
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Both recorders state a connection's first exchange, its preamble, session
// and every later one per-test, and keep the type tag "config", as before they
// stated anything.
func TestTheRecordersStateAConnectionsPreambleSessionAndTheRestPerTest(t *testing.T) {
	want := []string{
		models.MetadataLifetimeSession + "/config",
		models.MetadataLifetimePerTest + "/config",
		models.MetadataLifetimePerTest + "/config",
	}
	t.Run("legacy", func(t *testing.T) {
		out := make(chan *models.Mock, 3)
		mgr := syncMock.New(zap.NewNop())
		mgr.SetOutputChannel(out)
		ctx := syncMock.NewContext(context.WithValue(context.Background(), models.ClientConnectionIDKey, "c1"), mgr)
		clientCh, destCh := make(chan []byte), make(chan []byte)
		go func() {
			for _, x := range []string{"hello", "get a", "get b"} {
				clientCh <- []byte(x)
				destCh <- []byte("ok " + x)
			}
			close(clientCh)
			close(destCh)
		}()
		createGenericMocksAsync(ctx, zap.NewNop(), clientCh, destCh)
		var got []*models.Mock
		for len(got) < 3 {
			got = append(got, <-out)
		}
		if s := statedOf(got); !equal(s, want) {
			t.Fatalf("stated %v, want %v", s, want)
		}
	})
	t.Run("v2", func(t *testing.T) {
		clientCh := make(chan fakeconn.Chunk, 3)
		destCh := make(chan fakeconn.Chunk, 3)
		go func() {
			for i, x := range []string{"hello", "get a", "get b"} {
				at := time.Unix(int64(1000+10*i), 0)
				clientCh <- fakeconn.Chunk{Dir: fakeconn.FromClient, Bytes: []byte(x), ReadAt: at}
				time.Sleep(10 * time.Millisecond)
				destCh <- fakeconn.Chunk{Dir: fakeconn.FromDest, Bytes: []byte("ok " + x), WrittenAt: at.Add(time.Second)}
				time.Sleep(10 * time.Millisecond)
			}
			close(clientCh)
			close(destCh)
		}()
		sess, mocks := newV2Session(t, clientCh, destCh)
		if err := dispatch(t, sess); err != nil {
			t.Fatal(err)
		}
		close(mocks)
		if s := statedOf(drainMocks(mocks)); !equal(s, want) {
			t.Fatalf("stated %v, want %v", s, want)
		}
	})
}

// A stated per-test exchange is a test mock (owned and judged under the
// proven switch); the preamble and an exchange recorded before the recorder
// stated anything are protocol-phase. Serving does not read the statement.
func TestAStatedPerTestExchangeIsATestMockServedAsBefore(t *testing.T) {
	mk := func(stated string) *models.Mock {
		m := &models.Mock{Kind: models.GENERIC, Spec: models.MockSpec{Metadata: map[string]string{"type": "config"}}}
		if stated != "" {
			m.Spec.Metadata[models.MetadataLifetimeKey] = stated
		}
		return m
	}
	for stated, protocol := range map[string]bool{models.MetadataLifetimePerTest: false, models.MetadataLifetimeSession: true, "": true} {
		m := mk(stated)
		if got := m.IsProtocolPhase(); got != protocol {
			t.Errorf("stated %q: IsProtocolPhase %v, want %v", stated, got, protocol)
		}
		m.DeriveLifetime()
		if m.TestModeInfo.Lifetime != models.LifetimeSession {
			t.Errorf("stated %q: derived %v, want session as before", stated, m.TestModeInfo.Lifetime)
		}
		if m.StatesLifetime() {
			t.Errorf("stated %q: StatesLifetime, which switch-off serving reads", stated)
		}
	}
}

// exactStore is a store under exact serving (the proven switch) whose open
// test owns own, with every mock staged: what keploy's mock manager tells.
type exactStore struct {
	integrations.MockMemDb
	exact  bool
	own    map[string]bool
	staged []*models.Mock
}

func (s exactStore) FindStagedNotOwned(_ models.Kind, match func(*models.Mock) bool) string {
	if !s.exact {
		return ""
	}
	for _, m := range s.staged {
		if !s.own[m.Name] && match(m) {
			return m.Name
		}
	}
	return ""
}
func (s exactStore) GetSessionMocks() ([]*models.Mock, error)         { return nil, nil }
func (s exactStore) GetPerTestMocksInWindow() ([]*models.Mock, error) { return nil, nil }

// Under exact serving, a request no mock answers that is a later exchange of
// a connection the open test does not own is named, with what likely happened
// and what to do; one the test owns, a preamble, or no recorded exchange, is
// not.
func TestAMissOnAnExchangeTheTestDoesNotOwnIsNamed(t *testing.T) {
	staged := func(name, req, stated string) *models.Mock {
		m := exchange(name, req)
		m.Spec.Metadata = map[string]string{"type": "config", models.MetadataLifetimeKey: stated}
		return m
	}
	store := exactStore{
		exact: true,
		own:   map[string]bool{"mock-get-y": true},
		staged: []*models.Mock{
			staged("mock-hello", "HELLO", models.MetadataLifetimeSession),
			staged("mock-auth-x", "AUTH step2", models.MetadataLifetimePerTest),
			staged("mock-get-y", "GET y", models.MetadataLifetimePerTest),
		},
	}
	for req, want := range map[string]string{"AUTH step2": "mock-auth-x", "GET y": "", "HELLO": "", "SET z": ""} {
		if got := notOwnedExchange([][]byte{[]byte(req)}, store); got != want {
			t.Errorf("%s: %q, want %q", req, got, want)
		}
	}
	store.exact = false
	if got := notOwnedExchange([][]byte{[]byte("AUTH step2")}, store); got != "" {
		t.Fatalf("without exact serving: %q", got)
	}
	// The miss the parser files names it, says it is not the test's, and
	// what likely happened.
	store.exact = true
	r := missReport(t, store, "AUTH step2")
	if r.ClosestMock != "mock-auth-x" || !r.NotOwned || !strings.Contains(r.NextSteps, "Likely, at replay this test made the call on a connection it opened itself") {
		t.Fatalf("the miss names %q (not owned %v) and says %q", r.ClosestMock, r.NotOwned, r.NextSteps)
	}
}
