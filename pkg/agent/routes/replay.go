// Package routes defines the routes for the agent to mock outgoing requests, set mocks and get consumed mocks.
package routes

import (
	"context"
	"encoding/gob"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/render"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

func (a *Agent) MockOutgoing(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	mockRes := models.AgentResp{
		Error:     nil,
		IsSuccess: true,
	}

	var OutgoingReq models.OutgoingReq
	err := json.NewDecoder(r.Body).Decode(&OutgoingReq)
	if err != nil {
		mockRes.Error = err
		mockRes.IsSuccess = false
		render.JSON(w, r, mockRes)
		render.Status(r, http.StatusBadRequest)
		return
	}

	err = a.svc.MockOutgoing(r.Context(), OutgoingReq.OutgoingOptions)
	if err != nil {
		// Bug fix: previously `render.JSON(w, r, err)` serialized the
		// raw Go error interface. Numeric-typed errors (e.g.,
		// syscall.Errno which is type Errno uintptr) marshaled as bare
		// JSON numbers, breaking the CLI's AgentResp decoder with
		// "cannot unmarshal number into Go value of type
		// models.AgentResp" — masking the real error message.
		// Always render the structured AgentResp so the CLI gets a
		// consistent shape and can extract the underlying error
		// string via mockRes.Error.
		// MARK_MOCKOUTGOING_FIX_2026_05_14: this string must appear in /usr/local/bin/keploy if the OSS replace is taking effect.
		a.logger.Info("MARK_MOCKOUTGOING_FIX_2026_05_14: MockOutgoing handler error path producing proper AgentResp",
			zap.Error(err))
		mockRes.IsSuccess = false
		mockRes.Error = nil // intentionally nil — error interface serializes inconsistently; surface message via a string field below
		render.Status(r, http.StatusInternalServerError)
		render.JSON(w, r, map[string]any{
			"isSuccess": false,
			"error":     err.Error(),
		})
		return
	}

	render.JSON(w, r, mockRes)
	render.Status(r, http.StatusOK)
}

func (a *Agent) GetConsumedMocks(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	consumedMocks, err := a.svc.GetConsumedMocks(r.Context())
	if err != nil {
		// Same bug class as MockOutgoing's old `render.JSON(w, r, err)`
		// — raw error interface produces inconsistent JSON shapes for
		// the caller. Return a structured error wrapper instead so the
		// CLI gets a predictable shape.
		render.Status(r, http.StatusInternalServerError)
		render.JSON(w, r, map[string]string{"error": err.Error()})
		return
	}

	render.JSON(w, r, consumedMocks)
	render.Status(r, http.StatusOK)
}

func (a *Agent) GetMockErrors(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	mockErrors, err := a.svc.GetMockErrors(r.Context())
	if err != nil {
		render.Status(r, http.StatusInternalServerError)
		render.JSON(w, r, map[string]string{"error": err.Error()})
		return
	}

	render.Status(r, http.StatusOK)
	render.JSON(w, r, mockErrors)
}

// BeginTestErrorCapture opens a per-test mock-error capture window in the proxy
// so the next GetMockErrors returns only this test's misses. Implemented via a
// capability type-assertion so the agent.Service interface stays unchanged.
func (a *Agent) BeginTestErrorCapture(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if b, ok := a.svc.(interface {
		BeginTestErrorCapture(context.Context) error
	}); ok {
		if err := b.BeginTestErrorCapture(r.Context()); err != nil {
			render.Status(r, http.StatusInternalServerError)
			render.JSON(w, r, map[string]string{"error": err.Error()})
			return
		}
	}
	render.Status(r, http.StatusOK)
	render.JSON(w, r, map[string]string{"status": "ok"})
}

// StoreMocks receives the mock corpus as a stream: a gob MockStreamHeader
// followed by one gob Mock per frame, decoded mock-by-mock by StoreMocksStream.
func (a *Agent) StoreMocks(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/x-gob")

	writeErr := func(status int, err error) {
		w.WriteHeader(status)
		_ = gob.NewEncoder(w).Encode(models.AgentResp{Error: err, IsSuccess: false})
	}

	dec := gob.NewDecoder(r.Body)
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
	w.WriteHeader(http.StatusOK)
	_ = gob.NewEncoder(w).Encode(models.AgentResp{IsSuccess: true})
}

func (a *Agent) UpdateMockParams(w http.ResponseWriter, r *http.Request) {

	start := time.Now()

	w.Header().Set("Content-Type", "application/json")
	var updateParamsReq models.UpdateMockParamsReq
	err := json.NewDecoder(r.Body).Decode(&updateParamsReq)

	updateParamsRes := models.AgentResp{
		Error:     nil,
		IsSuccess: true,
	}

	if err != nil {
		updateParamsRes.Error = err
		updateParamsRes.IsSuccess = false
		render.JSON(w, r, updateParamsRes)
		render.Status(r, http.StatusBadRequest)
		return
	}

	err = a.svc.UpdateMockParams(r.Context(), updateParamsReq.FilterParams)
	if err != nil {
		updateParamsRes.Error = err
		updateParamsRes.IsSuccess = false
		render.JSON(w, r, updateParamsRes)
		render.Status(r, http.StatusInternalServerError)
		return
	}

	a.logger.Debug("Time taken to update mock params duration :", zap.Duration("duration", time.Since(start)))

	render.JSON(w, r, updateParamsRes)
	render.Status(r, http.StatusOK)
}

// The consumer delivery-window routes.
//
// They mirror BeginTestErrorCapture exactly: a capability type-assertion on
// a.svc, so the agent.Service interface stays unchanged and an older or
// third-party service implementation keeps compiling. They are the REMOTE half
// of replay.ConsumerInstrumentation — `keploy test` always talks to the agent
// over HTTP (cli/provider hands the replayer an *http.AgentClient), so without
// these three routes a consumer test could never be armed in any deployment,
// only in an embedder that wires the agent service in-process.
//
// A missing capability answers 501 rather than 200-with-nothing. The replayer
// turns that into a FAILED test with CONSUMER_UNSUPPORTED_AGENT; answering OK
// would let a test whose message was never delivered report "the worker
// stopped producing".

// consumerRecordingReporter is the RECORD-side capability
// pkg/service/agent.Agent implements: the reconciliation of the last closed
// consumer recording session.
type consumerRecordingReporter interface {
	ConsumerRecordingReport() models.ConsumerRecordingReport
}

// ConsumerRecordingReport serves the last closed recording session's
// reconciliation, so `keploy record` can fail a degraded consumer recording
// instead of exiting 0 with a suite that is silently short (design §3 R6).
//
// A service without the capability answers 200 with an EMPTY report rather
// than 501, and the difference from the delivery-window routes is deliberate:
// there, a missing capability means a recorded message was never delivered and
// the test must be refused by name. Here it means only "this agent has nothing
// to report about consumer recording", which is true of every HTTP recording
// ever made, and a 501 would make the record command log a scary line on every
// ordinary run.
func (a *Agent) ConsumerRecordingReport(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	render.Status(r, http.StatusOK)
	reporter, ok := a.svc.(consumerRecordingReporter)
	if !ok {
		render.JSON(w, r, models.ConsumerRecordingReport{})
		return
	}
	render.JSON(w, r, reporter.ConsumerRecordingReport())
}

// consumerInstrumentation is the capability pkg/service/agent.Agent
// implements.
type consumerInstrumentation interface {
	ArmConsumerTrigger(ctx context.Context, arm models.ConsumerArm) error
	AwaitConsumerEffects(ctx context.Context, testID string) (*models.ConsumerResult, error)
	ResetConsumerGate(ctx context.Context, testSetID string) (int, error)
}

func (a *Agent) consumerSvc(w http.ResponseWriter, r *http.Request) (consumerInstrumentation, bool) {
	ci, ok := a.svc.(consumerInstrumentation)
	if !ok {
		render.Status(r, http.StatusNotImplemented)
		render.JSON(w, r, map[string]string{
			"error":    "this agent does not implement consumer instrumentation",
			"category": string(models.CategoryConsumerUnsupportedAgent),
		})
		return nil, false
	}
	return ci, true
}

// ArmConsumerTrigger opens the delivery window for one consumer test and hands
// its recorded trigger to the application.
func (a *Agent) ArmConsumerTrigger(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	ci, ok := a.consumerSvc(w, r)
	if !ok {
		return
	}
	var arm models.ConsumerArm
	if err := json.NewDecoder(r.Body).Decode(&arm); err != nil {
		render.Status(r, http.StatusBadRequest)
		render.JSON(w, r, map[string]string{"error": fmt.Sprintf("failed to decode the consumer arm: %s", err.Error())})
		return
	}
	if err := ci.ArmConsumerTrigger(r.Context(), arm); err != nil {
		render.Status(r, http.StatusInternalServerError)
		render.JSON(w, r, map[string]string{"error": err.Error()})
		return
	}
	render.Status(r, http.StatusOK)
	render.JSON(w, r, map[string]string{"status": "ok"})
}

// AwaitConsumerEffects blocks until the armed test's window closes and returns
// the result. It is a LONG POLL — it holds the connection for as long as the
// test's own completion timeout allows, which is what the client's absent
// request timeout is for.
func (a *Agent) AwaitConsumerEffects(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	ci, ok := a.consumerSvc(w, r)
	if !ok {
		return
	}
	testID := r.URL.Query().Get("testId")
	if testID == "" {
		render.Status(r, http.StatusBadRequest)
		render.JSON(w, r, map[string]string{"error": "await consumer effects: testId is required"})
		return
	}
	res, err := ci.AwaitConsumerEffects(r.Context(), testID)
	if err != nil {
		render.Status(r, http.StatusInternalServerError)
		render.JSON(w, r, map[string]string{"error": err.Error()})
		return
	}
	render.Status(r, http.StatusOK)
	render.JSON(w, r, res)
}

// ResetConsumerGate returns the delivery gate to its default-closed boot phase
// at a test-set boundary and answers with the number of effect records the
// reset left unattributed.
//
// THE TRAILING COUNT TRAVELS IN THE BODY OF A 200, NOT AS A 500. A worker that
// over-produced after the last test of a set is an APPLICATION regression; a
// 500 here means keploy failed. Collapsing the two would make every transport
// failure and every older agent's 501 read to the user as "your worker emits
// more messages than the recording says", which sends them to debug the wrong
// system.
func (a *Agent) ResetConsumerGate(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	ci, ok := a.consumerSvc(w, r)
	if !ok {
		return
	}
	trailing, err := ci.ResetConsumerGate(r.Context(), r.URL.Query().Get("testSetId"))
	if err != nil {
		render.Status(r, http.StatusInternalServerError)
		render.JSON(w, r, map[string]string{"error": err.Error()})
		return
	}
	render.Status(r, http.StatusOK)
	render.JSON(w, r, models.ConsumerResetResult{TrailingEffects: trailing})
}
