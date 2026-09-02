package agent

import (
	"testing"

	coreAgent "go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/models"
)

// TestCollectAsyncMocksFiltersByMetadata proves collectAsyncMocks keeps only
// the async mocks (Spec.Async != nil), preserves their input order, and
// tolerates nil entries. This is the exact subset the agent hands
// Engine.Load, so a false negative here silently drops async mocks at replay.
func TestCollectAsyncMocksFiltersByMetadata(t *testing.T) {
	a := &models.Mock{Spec: models.MockSpec{Async: &models.AsyncMeta{Lane: "L"}}}
	b := &models.Mock{Spec: models.MockSpec{Metadata: map[string]string{}}}
	c := &models.Mock{Spec: models.MockSpec{Async: &models.AsyncMeta{Lane: "L"}}}
	got := collectAsyncMocks([]*models.Mock{a, b, c, nil})
	if len(got) != 2 || got[0] != a || got[1] != c {
		t.Fatalf("collectAsyncMocks = %v, want [a c]", got)
	}
}

// asyncLoaderStub records what the proxy layer is handed. It implements only
// the optional AsyncMockLoader capability, which is what loadAsyncIntoProxy
// type-asserts for.
type asyncLoaderStub struct {
	coreAgent.Proxy
	calls [][]*models.Mock
}

func (s *asyncLoaderStub) LoadAsyncMocks(mocks []*models.Mock) {
	s.calls = append(s.calls, mocks)
}

// A test-set holding NO async mocks has to clear the previous set's corpus.
//
// Engine.Load replaces rather than latches, so the clear is expressed by
// calling it with an empty slice. The early return that used to skip that call
// meant a set with no async mocks silently inherited the previous set's
// recordings and replayed them as its own.
func TestLoadAsyncIntoProxy_EmptyCorpusStillClearsThePreviousSet(t *testing.T) {
	stub := &asyncLoaderStub{}
	a := &Agent{Proxy: stub}

	a.loadAsyncIntoProxy([]*models.Mock{
		{Spec: models.MockSpec{Async: &models.AsyncMeta{Lane: "L"}}},
	})
	a.loadAsyncIntoProxy(nil) // next test-set has no async mocks

	if len(stub.calls) != 2 {
		t.Fatalf("LoadAsyncMocks called %d times, want 2: a set with no async mocks "+
			"never reached the engine, so it inherited the previous set's corpus",
			len(stub.calls))
	}
	if len(stub.calls[1]) != 0 {
		t.Fatalf("second call carried %d mocks, want 0", len(stub.calls[1]))
	}
}
