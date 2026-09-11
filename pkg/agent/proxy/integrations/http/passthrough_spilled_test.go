package http_test

import (
	"net/url"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	kphttp "go.keploy.io/server/v3/pkg/agent/proxy/integrations/http"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// poolDB is the minimum MockMemDb serveOnePassThroughMock reads: the per-test
// and session tiers. Everything else is unused by that path.
type poolDB struct {
	// Embedded as an interface, left nil: serveOnePassThroughMock calls only
	// the two accessors below, and anything else panicking is the signal that
	// this fake stopped matching the code under test.
	integrations.MockMemDb
	perTest []*models.Mock
	session []*models.Mock
}

func (p *poolDB) GetPerTestMocksInWindow() ([]*models.Mock, error) { return p.perTest, nil }
func (p *poolDB) GetSessionMocks() ([]*models.Mock, error)         { return p.session, nil }

// An egress-passthrough serve must return the RECORDED body even when that
// body was spilled to the agent's disk store.
//
// serveOnePassThroughMock draws candidates from the per-test pool as well as
// the session pool, and per-test is exactly the tier DiskMocks spills: a
// recorded body of at least responseSpillMinBytes comes back with
// Spec.HTTPResp nil and a lazy loader. Filtering on `Spec.HTTPResp == nil`
// therefore skips it, serveOnePassThroughMock returns nil, and decode.go falls
// back to writing a SYNTHETIC 200 with an empty body — the app is handed a
// wrong response, and the only log saying so is at Debug.
//
// This became reachable on the default path when tagged HTTP stopped being
// lax-promoted to session lifetime: session mocks are not disk-eligible, so
// before that change a passthrough candidate could never have been spilled.
func TestServeOnePassThroughMockServesASpilledBody(t *testing.T) {
	body := bodyOfSize(spillMinBytes + 1)
	store, err := proxy.NewDiskMocks(zap.NewNop())
	if err != nil {
		t.Fatalf("NewDiskMocks: %v", err)
	}
	defer func() { _ = store.Close() }()

	m := perTestHTTPMock("mock-telemetry", body)
	m.Spec.HTTPReq.URL = "http://collector.example.com:4318/v1/metrics"
	if err := store.Add(m); err != nil {
		t.Fatalf("DiskMocks.Add: %v", err)
	}
	loaded, err := store.LoadByNames([]string{"mock-telemetry"})
	if err != nil || len(loaded) != 1 {
		t.Fatalf("LoadByNames: %v (n=%d)", err, len(loaded))
	}
	if !loaded[0].HasSpilledResponse() {
		t.Fatalf("precondition: the mock should have spilled")
	}

	db := &poolDB{perTest: loaded}
	u, _ := url.Parse("http://collector.example.com:4318/v1/metrics")

	h := kphttp.NewForTest()
	out := h.ServeOnePassThroughMockForTest(db, "GET", u, "collector.example.com", 4318, nil)
	if out == nil {
		t.Fatalf("a spilled recording was skipped, so the caller writes a synthetic empty 200 instead of the recorded body")
	}
	if !strings.Contains(string(out), body[:64]) {
		t.Fatalf("served response does not carry the recorded body")
	}
}

// A mock that never spilled must still serve, unchanged.
func TestServeOnePassThroughMockServesAResidentBody(t *testing.T) {
	m := perTestHTTPMock("mock-small", "tiny body")
	m.Spec.HTTPReq.URL = "http://collector.example.com:4318/v1/metrics"

	db := &poolDB{session: []*models.Mock{m}}
	u, _ := url.Parse("http://collector.example.com:4318/v1/metrics")

	h := kphttp.NewForTest()
	out := h.ServeOnePassThroughMockForTest(db, "GET", u, "collector.example.com", 4318, nil)
	if out == nil {
		t.Fatalf("a resident recording must still be served")
	}
	if !strings.Contains(string(out), "tiny body") {
		t.Fatalf("served response does not carry the recorded body")
	}
}
