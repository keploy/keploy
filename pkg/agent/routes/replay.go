// Package routes defines the routes for the agent to mock outgoing requests, set mocks and get consumed mocks.
package routes

import (
	"context"
	"encoding/gob"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/render"
	"github.com/klauspost/compress/zstd"
	"go.keploy.io/server/v3/pkg/agent/ids"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/pkg/service/agent"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

func (a *Agent) MockOutgoing(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	var OutgoingReq models.OutgoingReq
	if err := json.NewDecoder(r.Body).Decode(&OutgoingReq); err != nil {
		respondAgent(w, r, http.StatusBadRequest, err)
		return
	}

	if err := a.svc.MockOutgoing(r.Context(), OutgoingReq.OutgoingOptions); err != nil {
		utils.LogError(a.logger, err, "failed to mock outgoing")
		respondAgent(w, r, http.StatusInternalServerError, err)
		return
	}
	// The learn step reads keploy mock's tests as client tests (models.
	// ClientTestsReadHeader): keploy mock keeps what it learns only when the
	// agent says so.
	if s, ok := a.svc.(clientTestsReader); ok && OutgoingReq.OutgoingOptions.Learn && s.ReadsClientTests() {
		w.Header().Set(models.ClientTestsReadHeader, models.ClientTestsRead)
	}

	respondAgent(w, r, http.StatusOK, nil)
}

// clientTestsReader says whether the agent's learn step reads its tests as
// keploy mock's client tests (proven switch).
type clientTestsReader interface {
	ReadsClientTests() bool
}

// respondAgent writes an AgentResp with the given status. It exists so no
// handler can repeat either of the two mistakes this file used to make:
// rendering a bare `error` (which does not survive the wire — see
// models.AgentResp), and calling render.Status AFTER render.JSON.
//
// render.Status only stashes the code in the request context; render.JSON is
// what calls WriteHeader. Calling Status second is therefore a NO-OP and the
// reply goes out as 200 — which is how /updatemockparams came to answer every
// failure with "HTTP 200 {\"isSuccess\":false}", defeating any status check a
// client might make.
func respondAgent(w http.ResponseWriter, r *http.Request, status int, err error) {
	// The agent names its version on every answer (models.AgentVersionHeader).
	if utils.Version != "" {
		w.Header().Set(models.AgentVersionHeader, utils.Version)
	}
	resp := models.AgentResp{IsSuccess: err == nil}
	if err != nil {
		resp.ErrorMsg = err.Error()
	}
	render.Status(r, status)
	render.JSON(w, r, resp)
}

func (a *Agent) GetConsumedMocks(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	consumedMocks, err := a.svc.GetConsumedMocks(r.Context())
	if err != nil {
		// The CLI decodes a 200 here straight into []models.MockState, so
		// a failure MUST carry a non-2xx status — otherwise the error
		// object lands in the slice decoder and the caller sees
		// "cannot unmarshal object into Go value of type
		// []models.MockState" instead of the reason below.
		respondAgent(w, r, http.StatusInternalServerError, err)
		return
	}

	render.Status(r, http.StatusOK)
	render.JSON(w, r, consumedMocks)
}

func (a *Agent) GetMockErrors(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	mockErrors, err := a.svc.GetMockErrors(r.Context())
	if err != nil && !errors.Is(err, models.ErrUnmatchedCallsUnread) {
		respondAgent(w, r, http.StatusInternalServerError, err)
		return
	}
	// An incomplete read still hands back what was read, as the body every
	// client decodes, and says it is incomplete in a header (see
	// Proxy.GetMockErrors), with the proven switch on or off.
	if err != nil {
		w.Header().Set(models.UnmatchedCallsUnreadHeader, headerSafe(err.Error()))
	}

	render.Status(r, http.StatusOK)
	render.JSON(w, r, mockErrors)
}

// headerSafe makes text one header value: control characters, newlines among
// them, become spaces, so no cause can end the header early.
func headerSafe(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
}

// BeginTestErrorCapture opens a per-test mock-error capture window in the proxy
// so the next GetMockErrors returns only this test's misses. With ?carry=1 it
// carries in the misses made since the previous test's window closed (the
// proxy's ContinueTestErrorCapture): the replayer asks for that for every test
// after a set's first. Implemented via capability type-assertions so the
// agent.Service interface stays unchanged.
func (a *Agent) BeginTestErrorCapture(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	var err error
	if c, ok := a.svc.(interface {
		ContinueTestErrorCapture(context.Context) error
	}); ok && r.URL.Query().Get("carry") == "1" {
		err = c.ContinueTestErrorCapture(r.Context())
	} else if b, ok := a.svc.(interface {
		BeginTestErrorCapture(context.Context) error
	}); ok {
		err = b.BeginTestErrorCapture(r.Context())
	}
	if err != nil {
		respondAgent(w, r, http.StatusInternalServerError, err)
		return
	}
	render.Status(r, http.StatusOK)
	render.JSON(w, r, map[string]string{"status": "ok"})
}

// storeMocksBody is a /storemocks body with its Content-Encoding undone: the
// body itself when there is none, a streaming zstd reader for "zstd". Any other
// encoding is an error, since reading it as gob would only fail later and less
// clearly. Close releases the zstd reader; it does not close the request body.
func storeMocksBody(r *http.Request) (io.ReadCloser, error) {
	switch enc := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Encoding"))); enc {
	case "", "identity":
		return io.NopCloser(r.Body), nil
	case models.MockStreamEncodingZstd:
		zr, err := zstd.NewReader(r.Body,
			zstd.WithDecoderConcurrency(1),
			zstd.WithDecoderMaxWindow(models.MockStreamZstdWindow))
		if err != nil {
			return nil, fmt.Errorf("storemocks: start the zstd decoder: %w", err)
		}
		return zr.IOReadCloser(), nil
	default:
		return nil, fmt.Errorf("storemocks: unsupported Content-Encoding %q; this agent accepts %q or none", enc, models.MockStreamEncodingZstd)
	}
}

// StoreMocks receives the mock corpus as a stream: a gob MockStreamHeader
// followed by one gob Mock per frame, decoded mock-by-mock by StoreMocksStream.
// The stream may be zstd-compressed (Content-Encoding: zstd), which the client
// does only after this agent's /health has advertised it.
func (a *Agent) StoreMocks(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/x-gob")

	writeErr := func(status int, err error) {
		w.WriteHeader(status)
		// Encode errors used to be discarded here, which hid the real
		// problem: gob ABORTS on a non-nil `error` field ("type not
		// registered for interface"). The abort happens PARTWAY -- a partial
		// type descriptor is already on the wire -- so the CLI got a truncated
		// value, failed with "unexpected EOF", and could only report
		// "storemocks http <status>". ErrorMsg is a string, so this now
		// encodes; log it if it ever does not.
		if encErr := gob.NewEncoder(w).Encode(models.AgentResp{ErrorMsg: err.Error()}); encErr != nil {
			utils.LogError(a.logger, encErr, "failed to encode storemocks error response",
				zap.String("underlying", err.Error()))
		}
	}

	body, err := storeMocksBody(r)
	if err != nil {
		w.Header().Set("Accept-Encoding", models.MockStreamEncodingZstd)
		writeErr(http.StatusUnsupportedMediaType, err)
		return
	}
	defer body.Close()

	dec := gob.NewDecoder(body)
	var header models.MockStreamHeader
	if err := dec.Decode(&header); err != nil {
		writeErr(http.StatusBadRequest, fmt.Errorf("storemocks: decode stream header: %w", err))
		return
	}

	streamer, ok := a.svc.(interface {
		StoreMocksStream(context.Context, models.MockStreamHeader, *gob.Decoder) error
	})
	if !ok {
		writeErr(http.StatusInternalServerError, fmt.Errorf("storemocks: service does not support streaming"))
		return
	}

	if err := streamer.StoreMocksStream(r.Context(), header, dec); err != nil {
		writeErr(http.StatusInternalServerError, err)
		return
	}
	// Under the proven switch, an agent that serves a test the recordings it
	// shares with the tests that ran beside it says so (a header, which a
	// client that predates it never reads): the client sends them only then.
	if s, ok := a.svc.(interface{ ServesExactConcurrent() bool }); ok && provenswitch.On() && s.ServesExactConcurrent() {
		w.Header().Set(models.ExactConcurrentHeader, models.ExactConcurrentServes)
	}
	w.WriteHeader(http.StatusOK)
	_ = gob.NewEncoder(w).Encode(models.AgentResp{IsSuccess: true})
}

func (a *Agent) UpdateMockParams(w http.ResponseWriter, r *http.Request) {

	start := time.Now()

	w.Header().Set("Content-Type", "application/json")
	var updateParamsReq models.UpdateMockParamsReq
	if err := json.NewDecoder(r.Body).Decode(&updateParamsReq); err != nil {
		respondAgent(w, r, http.StatusBadRequest, err)
		return
	}

	// A test set's staging and exact serving's pool call begin no test
	// (BeginsTestVariables below): values they gave would be bound nowhere.
	if provenswitch.On() && !updateParamsReq.FilterParams.BeginsTestVariables() && len(updateParamsReq.FilterParams.Variables) > 0 {
		respondAgent(w, r, http.StatusUnprocessableEntity, errors.New("a filter that stages a test set or serves the pool between test cases begins no test, and gives no values to bind"))
		return
	}
	// Under the proven switch a test case's mock filter a client sends
	// starts a test: keploy test sends one per test case, before the test
	// case is sent, so a test case begins with none of the previous test
	// case's variables bound (the run's stay: MockManager.NewVariableSet).
	// The filter carries the values the test case's request sends: they are
	// bound as the scope begins. The agent's own filters, as a test of
	// keploy mock replay begins and ends, do not come through here.
	// Exact serving's pool call (models.ExactServing, between test cases)
	// begins no test: the variables the test case bound stay readable, and
	// keploy test reads them after it. Nor does a test set's staging: the
	// set's variables began at its Mock, and what its start-up binds after
	// the staging is the run's, which every test of the set reads
	// (MockFilterParams.BeginsTestVariables).
	opens := provenswitch.On() && updateParamsReq.FilterParams.BeginsTestVariables()
	opener, oneStep := a.svc.(testCaseOpener)
	if opens && !oneStep {
		// An agent that cannot open a test case in one step opens none whose
		// variables it keeps: begun apart, after the test's mocks went live,
		// a call they answer between the two binds in no scope of the
		// test's, and before a set's first test in the run's, which every
		// later test of the set reads. Nor one whose request gave values it
		// would drop: every call that uses one would be left unbound, a
		// failure of the app that is keploy's. Refused before the filter
		// applies, so nothing it stages is served.
		if keepsVariables(a.svc) {
			refuseOpen(w, r, a.logger, errOpensApart)
			return
		}
		if len(updateParamsReq.FilterParams.Variables) > 0 {
			refuseOpen(w, r, a.logger, errCannotBindGiven)
			return
		}
	}
	if opens && oneStep {
		// One step: the scope begins as the test's mocks go live, so no
		// call they answer binds before it.
		if err := opener.OpenTestCase(r.Context(), updateParamsReq.FilterParams); err != nil {
			utils.LogError(a.logger, err, "failed to open the test case")
			respondAgent(w, r, http.StatusInternalServerError, err)
			return
		}
		// The learn step took the test case's request (models.
		// TestRequestReadHeader): the replay keeps what it learned for the
		// test only when the agent says so.
		if s, ok := a.svc.(testRequestReader); ok && s.ReadsTestRequest() {
			w.Header().Set(models.TestRequestReadHeader, models.TestRequestRead)
		}
	} else if err := a.svc.UpdateMockParams(r.Context(), updateParamsReq.FilterParams); err != nil {
		utils.LogError(a.logger, err, "failed to update mock params")
		respondAgent(w, r, http.StatusInternalServerError, err)
		return
	}

	a.logger.Debug("Time taken to update mock params duration :", zap.Duration("duration", time.Since(start)))

	// A header, not a body field: a client that predates it never looks at
	// headers, so its handling of this response cannot change.
	if s, ok := a.svc.(interface {
		ReadsConsumedForPerTestOnly(agent.Service) bool
	}); ok && s.ReadsConsumedForPerTestOnly(a.svc) {
		w.Header().Set(models.ConsumedScopeHeader, models.ConsumedScopePerTest)
	}
	// A set's staging is answered with the background rules it is served by
	// (models.BackgroundRulesHeader): the replay relaxes today's checks for
	// it only when the agent says it serves it by S12's.
	if ex := updateParamsReq.FilterParams.Exact; ex != nil && ex.Stage {
		if s, ok := a.svc.(interface{ ServedBackground() string }); ok {
			if rules := s.ServedBackground(); rules != "" {
				w.Header().Set(models.BackgroundRulesHeader, rules)
			}
		}
	}
	respondAgent(w, r, http.StatusOK, nil)
}

// testCaseOpener opens a keploy test case in one step (proven switch): its
// mock filter applies, and the test's variables begin with the values its
// request gave bound, as its mocks go live.
type testCaseOpener interface {
	OpenTestCase(ctx context.Context, params models.MockFilterParams) error
}

// testRequestReader says whether the agent's OpenTestCase hands the test
// case's request its filter carries to the learn step (proven switch).
type testRequestReader interface {
	ReadsTestRequest() bool
}

// variableStarter begins a test's variables with the values its test case's
// request gave bound (proven switch).
type variableStarter interface {
	StartTestVariables(given []models.VariableBinding) error
}

// testVariables is the agent's side of a test's variables (proven switch).
type testVariables interface {
	variableStarter
	TestVariables() ([]models.VariableBinding, bool)
}

// keepsVariables reports whether svc keeps the test variables a replay binds:
// it can begin a test's (variableStarter), or forget them.
func keepsVariables(svc any) bool {
	_, starts := svc.(variableStarter)
	_, resets := svc.(interface{ ResetVariables() })
	return starts || resets
}

var (
	// errOpensApart: an agent that keeps a replay's variables but cannot
	// begin them as a test case opens (testCaseOpener).
	errOpensApart = errors.New("this keploy agent cannot begin a test case's variables as the test case opens; run the keploy agent of this keploy version")
	// errCannotBindGiven: an agent that keeps no variables, given some.
	errCannotBindGiven = errors.New("this keploy agent cannot bind the values a test case's request sends; run the keploy agent of this keploy version")
)

// refuseOpen answers a test case's mock filter this agent cannot open.
func refuseOpen(w http.ResponseWriter, r *http.Request, logger *zap.Logger, err error) {
	utils.LogError(logger, err, "failed to open the test case")
	respondAgent(w, r, http.StatusNotImplemented, err)
}

// HandleTestVariables answers GET /agent/variables: the test variables the
// current test has bound, under the proven switch (a 404 without it, as
// before the switch existed).
func (a *Agent) HandleTestVariables(w http.ResponseWriter, r *http.Request) {
	s, ok := a.svc.(testVariables)
	if !provenswitch.On() || !ok {
		http.NotFound(w, r)
		return
	}
	bound, kept := s.TestVariables()
	if !kept {
		http.Error(w, "this keploy agent keeps no test variables", http.StatusNotImplemented)
		return
	}
	if bound == nil {
		bound = []models.VariableBinding{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(models.TestVariablesResp{Variables: bound})
}

func (a *Agent) HandleIDs(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(ids.Default.Pairs())
}
