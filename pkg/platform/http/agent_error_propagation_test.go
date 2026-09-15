package http_test

// THE AGENT'S FAILURE REASON MUST REACH THE CALLER.
//
// Every test here drives the REAL AgentClient against the REAL agent handlers
// (pkg/agent/routes) over a real socket, with a service that fails. Before the
// fix each of these returned the DECODER's complaint instead of the agent's
// reason, because AgentResp.Error was an `error` interface that no wire format
// can carry:
//
//	UpdateMockParams  json: cannot unmarshal object into Go struct field AgentResp.error of type error
//	GetConsumedMocks  json: cannot unmarshal object into Go value of type []models.MockState
//	MockOutgoing      json: cannot unmarshal string into Go struct field AgentResp.error of type error
//	StoreMocks        storemocks http 500
//
// A masked failure is worse than a loud one: it cost a kafka e2e lane a real
// diagnosis, and it makes every agent-side fault look like a keploy bug.

import (
	"context"
	"encoding/gob"
	"errors"
	"io"
	nethttp "net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.keploy.io/server/v3/pkg/agent/routes"
	"go.keploy.io/server/v3/pkg/models"
	agentsvc "go.keploy.io/server/v3/pkg/service/agent"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

// theReason is distinctive enough that finding it in the returned error proves
// it travelled the whole way from the agent's service layer.
const theReason = "mock manager unavailable: connection refused"

// failingSvc fails every agent operation with theReason. The embedded nil
// interface satisfies agent.Service; unexercised methods are never reached.
type failingSvc struct {
	agentsvc.Service
	err error
}

func (s *failingSvc) MockOutgoing(context.Context, models.OutgoingOptions) error { return s.err }
func (s *failingSvc) UpdateMockParams(context.Context, models.MockFilterParams) error {
	return s.err
}
func (s *failingSvc) GetConsumedMocks(context.Context) ([]models.MockState, error) {
	return nil, s.err
}
func (s *failingSvc) GetMockErrors(context.Context) ([]models.UnmatchedCall, error) {
	return nil, s.err
}
func (s *failingSvc) StoreMocks(context.Context, []*models.Mock, []*models.Mock) error {
	return s.err
}
func (s *failingSvc) StoreMocksStream(context.Context, models.MockStreamHeader, *gob.Decoder) error {
	return s.err
}
func (s *failingSvc) GetOutgoing(context.Context, models.OutgoingOptions) (<-chan *models.Mock, error) {
	return nil, s.err
}
func (s *failingSvc) GetMapping(context.Context) (<-chan models.TestMockMapping, error) {
	return nil, s.err
}
func (s *failingSvc) StartIncomingProxy(context.Context, models.IncomingOptions) (chan *models.TestCase, error) {
	return nil, s.err
}

func failingAgentServer(t *testing.T) *httptest.Server {
	t.Helper()
	r := chi.NewRouter()
	routes.DefaultRoutes{}.New(r, &failingSvc{err: errors.New(theReason)}, zap.NewNop())
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv
}

// TestAgentClient_SurfacesAgentFailureReason is the regression test for the
// defect. Each call must come back carrying the agent's own words.
func TestAgentClient_SurfacesAgentFailureReason(t *testing.T) {
	srv := failingAgentServer(t)
	c := newClient(t, srv.URL+"/agent")
	ctx := context.Background()

	for _, tc := range []struct {
		name string
		call func() error
	}{
		{"MockOutgoing", func() error { return c.MockOutgoing(ctx, models.OutgoingOptions{}) }},
		{"UpdateMockParams", func() error { return c.UpdateMockParams(ctx, models.MockFilterParams{}) }},
		{"GetConsumedMocks", func() error { _, err := c.GetConsumedMocks(ctx); return err }},
		{"StoreMocks", func() error { return c.StoreMocks(ctx, nil, nil) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			if err == nil {
				t.Fatal("agent failed but the client reported success")
			}
			if !strings.Contains(err.Error(), theReason) {
				t.Fatalf("the agent's reason did not survive the wire.\n got: %v\nwant it to contain: %q", err, theReason)
			}
			// The specific way this used to break: the decoder's complaint
			// replaced the agent's reason. Guard against any regression to it.
			if strings.Contains(err.Error(), "cannot unmarshal") {
				t.Fatalf("decoder complaint leaked into the caller's error: %v", err)
			}
		})
	}
}

// TestAgentClient_StreamEndpointsRejectNon2xx covers the same bug class on the
// long-lived streams. These used to point a gob/JSON decoder at an http.Error
// text body, log "failed to decode mock from stream", close the channel empty
// and let replay continue with ZERO mocks — a silent pass-through.
func TestAgentClient_StreamEndpointsRejectNon2xx(t *testing.T) {
	srv := failingAgentServer(t)
	c := newClient(t, srv.URL+"/agent")

	// These endpoints require an errgroup in the context; supply one so the
	// test exercises the status handling and not the errgroup guard. Without
	// the fix GetOutgoing returns (channel, nil) here and the channel simply
	// closes empty — the silent zero-mock replay this test exists to stop.
	grp, gctx := errgroup.WithContext(context.Background())
	ctx := context.WithValue(gctx, models.ErrGroupKey, grp)

	if ch, err := c.GetOutgoing(ctx, models.OutgoingOptions{}); err == nil {
		var n int
		for range ch {
			n++
		}
		t.Fatalf("GetOutgoing reported success for a 500 reply and yielded %d mocks; a failing agent must not look like an empty recording", n)
	} else if !strings.Contains(err.Error(), theReason) {
		t.Fatalf("GetOutgoing lost the agent's reason: %v", err)
	}

	if ch, err := c.GetMappings(ctx, models.IncomingOptions{}); err == nil {
		for range ch {
		}
		t.Fatal("GetMappings reported success for a 500 reply")
	} else if !strings.Contains(err.Error(), theReason) {
		t.Fatalf("GetMappings lost the agent's reason: %v", err)
	}

	// GetIncoming is the record hot path and had no coverage at all: deleting
	// its status check left every test green.
	if ch, err := c.GetIncoming(ctx, models.IncomingOptions{}); err == nil {
		var n int
		for range ch {
			n++
		}
		t.Fatalf("GetIncoming reported success for a 500 reply and yielded %d test cases; "+
			"a failing agent must not look like an empty recording", n)
	} else if !strings.Contains(err.Error(), theReason) {
		t.Fatalf("GetIncoming lost the agent's reason: %v", err)
	}
}

// TestAgentHandlers_FailuresCarryTheirStatus pins the other half of the defect:
// render.Status was being called AFTER render.JSON, which is a no-op, so
// /updatemockparams answered every failure with HTTP 200. Any client status
// check would have been defeated by that, so the wire format alone is not
// enough — the status has to be right too.
func TestAgentHandlers_FailuresCarryTheirStatus(t *testing.T) {
	srv := failingAgentServer(t)

	for _, tc := range []struct {
		name, method, path, body string
		wantStatus               int
	}{
		{"updatemockparams service failure", "POST", "/agent/updatemockparams", `{"filterParams":{}}`, nethttp.StatusInternalServerError},
		{"updatemockparams bad body", "POST", "/agent/updatemockparams", `not json`, nethttp.StatusBadRequest},
		{"mock service failure", "POST", "/agent/mock", `{"outgoingOptions":{}}`, nethttp.StatusInternalServerError},
		{"mock bad body", "POST", "/agent/mock", `not json`, nethttp.StatusBadRequest},
		{"consumedmocks service failure", "GET", "/agent/consumedmocks", "", nethttp.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var rdr io.Reader
			if tc.body != "" {
				rdr = strings.NewReader(tc.body)
			}
			req, err := nethttp.NewRequest(tc.method, srv.URL+tc.path, rdr)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")
			res, err := srv.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			raw, _ := io.ReadAll(res.Body)

			if res.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body: %s)", res.StatusCode, tc.wantStatus, raw)
			}
			// The body must never contain the old encoded-interface artifact.
			if strings.Contains(string(raw), `"error":{}`) {
				t.Fatalf("error field serialized as an empty object — the message was destroyed: %s", raw)
			}
		})
	}
}

// TestAgentClient_MisroutedAgentURIReportsTheMisroute pins the ORDER inside
// agentRespErr: the status is consulted BEFORE the decode result.
//
// The scenario is real and cost a diagnosis before: AgentURI must carry the
// "/agent" suffix because the handlers mount under it, and when it is missing
// every call 404s with chi's plain-text "404 page not found". That body is not
// an AgentResp, so it fails to decode — and if the decode error is reported
// first, the caller is told "cannot unmarshal number into models.AgentResp"
// and goes looking for a protocol bug instead of a misconfigured URI.
//
// Without the ordering this test passes anyway on a well-formed failure reply,
// which is exactly why the ordering had no coverage.
func TestAgentClient_MisroutedAgentURIReportsTheMisroute(t *testing.T) {
	srv := failingAgentServer(t)
	// Deliberately WITHOUT the "/agent" suffix: every route 404s.
	c := newClient(t, srv.URL)
	ctx := context.Background()

	for _, tc := range []struct {
		name string
		call func() error
	}{
		{"UpdateMockParams", func() error { return c.UpdateMockParams(ctx, models.MockFilterParams{}) }},
		{"GetConsumedMocks", func() error { _, err := c.GetConsumedMocks(ctx); return err }},
		{"MockOutgoing", func() error { return c.MockOutgoing(ctx, models.OutgoingOptions{}) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			if err == nil {
				t.Fatal("a 404 from a misrouted AgentURI was reported as success")
			}
			// Must be reported AS A STATUS FAILURE. Asserting merely that the
			// text mentions 404, or lacks "cannot unmarshal", is not enough: the
			// decode-first error embeds `status: 404` too, and a non-JSON body
			// yields a SYNTAX error ("invalid character 'p'...") rather than an
			// unmarshal-type one. Both weaker forms pass with the order inverted.
			if !strings.Contains(err.Error(), "failed (status 404") {
				t.Fatalf("the misroute was not reported as a status failure — the caller cannot "+
					"tell a bad URI from a protocol bug.\n got: %v", err)
			}
			if strings.Contains(err.Error(), "failed to decode response body") {
				t.Fatalf("the decoder's complaint was reported instead of the 404 — "+
					"agentRespErr must consult the status BEFORE the decode result: %v", err)
			}
		})
	}
}

// TestGetMappingsToleratesAnAgentWithoutTheRoute pins the version-skew carve
// out. /mappings arrived with test-mock mapping (keploy #3715); an older agent
// image 404s it. GetMappings runs inside the record errgroup, so returning an
// error for a MISSING ROUTE aborts the entire recording — turning a CLI that is
// merely ahead of the agent image into a total capture failure. The degraded
// behaviour (record without mappings) is the correct one.
func TestGetMappingsToleratesAnAgentWithoutTheRoute(t *testing.T) {
	// An agent that has no /mappings route at all.
	srv := httptest.NewServer(nethttp.HandlerFunc(func(w nethttp.ResponseWriter, _ *nethttp.Request) {
		nethttp.Error(w, "404 page not found", nethttp.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	c := newClient(t, srv.URL+"/agent")
	grp, gctx := errgroup.WithContext(context.Background())
	ctx := context.WithValue(gctx, models.ErrGroupKey, grp)

	ch, err := c.GetMappings(ctx, models.IncomingOptions{})
	if err != nil {
		t.Fatalf("a missing /mappings route was reported as a failure: %v\n"+
			"This runs inside the record errgroup, so this error aborts the whole recording.", err)
	}
	if ch == nil {
		t.Fatal("GetMappings returned a nil channel; ranging over it would block forever")
	}
	for range ch {
		t.Fatal("an agent without /mappings yielded a mapping")
	}
}
