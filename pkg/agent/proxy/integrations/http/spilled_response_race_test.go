package http_test

import (
	"sync"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy"
	kphttp "go.keploy.io/server/v3/pkg/agent/proxy/integrations/http"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// Serving a spilled response must not write to the mock, because the mock is
// shared and another goroutine reads it whole while the serve is in flight.
//
// Proxy.handleConnection runs in its own goroutine per accepted connection,
// and the MockManager getters hand out the STORED pointer, not a copy. So two
// connections can match the same mock: one consumes and serves it, while the
// other is still inside updateMock, which dereferences the whole struct by
// value -- `updatedMock := *matchedMock` and `deleteMock := *matchedMock`
// (match.go:1233, :1269). Those reads cover Spec.HTTPResp and the spill
// hydrator, the two fields the old mutating hydrate wrote.
//
// The goroutines below are siblings with no happens-before edge, which is what
// the race detector keys on -- so this reports deterministically rather than
// depending on an interleaving. It is meaningless without -race; the CI lane
// in .github/workflows/go-test.yaml runs this package under -race for that
// reason.
func TestServingASpilledMockDoesNotWriteToTheSharedMock(t *testing.T) {
	body := bodyOfSize(spillMinBytes + 1)
	store, err := proxy.NewDiskMocks(zap.NewNop())
	if err != nil {
		t.Fatalf("NewDiskMocks: %v", err)
	}
	defer func() { _ = store.Close() }()
	if err := store.Add(perTestHTTPMock("mock-shared", body)); err != nil {
		t.Fatalf("DiskMocks.Add: %v", err)
	}
	loaded, err := store.LoadByNames([]string{"mock-shared"})
	if err != nil || len(loaded) != 1 {
		t.Fatalf("LoadByNames: %v (n=%d)", err, len(loaded))
	}
	shared := loaded[0]

	h := kphttp.NewForTest()
	var wg sync.WaitGroup
	wg.Add(3)

	// The serving connection.
	go func() {
		defer wg.Done()
		if _, err := h.BuildMockResponseBytesForTest(shared); err != nil {
			t.Errorf("serialize: %v", err)
		}
	}()
	// A second connection that matched the same mock and is serving it too.
	// Not reachable on today's consume path, but cheap insurance against a
	// future non-consuming serve.
	go func() {
		defer wg.Done()
		if _, err := h.BuildMockResponseBytesForTest(shared); err != nil {
			t.Errorf("concurrent serialize: %v", err)
		}
	}()
	// The losing connection, still inside updateMock. This is the exact read
	// match.go:1269 performs; keep it a whole-struct copy, because narrowing it
	// to one field would stop covering the racer.
	go func() {
		defer wg.Done()
		deleteMock := *shared
		_ = deleteMock.Name
	}()

	wg.Wait()

	if !shared.HasSpilledResponse() {
		t.Fatalf("serving cleared the hydrator on a shared pooled mock")
	}
	if shared.Spec.HTTPResp != nil {
		t.Fatalf("serving wrote the loaded response back onto a shared pooled mock")
	}
}

// A mock that never spilled must still serve, and must still not be written to.
func TestServingAResidentMockDoesNotWriteToTheSharedMock(t *testing.T) {
	resident := perTestHTTPMock("mock-small", "small body")
	h := kphttp.NewForTest()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if _, err := h.BuildMockResponseBytesForTest(resident); err != nil {
			t.Errorf("serialize: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		deleteMock := *resident
		_ = deleteMock.Name
	}()
	wg.Wait()

	if resident.Spec.HTTPResp == nil {
		t.Fatalf("a resident mock's response must survive a serve")
	}
	var _ *models.Mock = resident
}
