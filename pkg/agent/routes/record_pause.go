package routes

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/render"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// The agent's control plane pauses recording on a running agent and resumes
// it (models.RecordPausePath, models.RecordResumePath), for example while the
// application's rollout is in progress. The handlers reach the capability by
// type-assertion, so agent.Service and its hand-written doubles stay as they
// are (the pattern of scope.go).

// recordPauser is a service that can pause and resume recording.
type recordPauser interface {
	PauseRecording(ctx context.Context, reason string) error
	ResumeRecording(ctx context.Context) error
}

// recordPauseReporter is a service that says whether recording is paused for
// the control plane, and whether it can pause at all (supported: its proxy
// may not be able to).
type recordPauseReporter interface {
	RecordingPaused() (paused bool, reason string, supported bool)
}

// maxRecordPauseBody bounds the pause request's body: a reason is a few words.
const maxRecordPauseBody = 64 << 10

// recordPauseUnsupported is the answer of an agent whose service cannot pause
// recording: 501, in the AgentResp shape the routes answer with.
const recordPauseUnsupported = "this agent cannot pause or resume recording"

// recordPauseUnsupporter is an error of a service's pause or resume that says
// the agent cannot pause at all (its proxy cannot), as against a pause that
// failed: matched by what the error says of itself, so this package need not
// know the service's error.
type recordPauseUnsupporter interface{ RecordPauseUnsupported() bool }

// answerRecordPause writes the AgentResp of a pause or a resume.
func answerRecordPause(w http.ResponseWriter, r *http.Request, status int, errMsg string) {
	w.Header().Set("Content-Type", "application/json")
	render.Status(r, status)
	render.JSON(w, r, models.AgentResp{ErrorMsg: errMsg, IsSuccess: errMsg == ""})
}

// pauseFailureStatus is the status for err from a service's pause or resume:
// 501 when it says the agent cannot pause at all, 500 otherwise.
func pauseFailureStatus(err error) int {
	var u recordPauseUnsupporter
	if errors.As(err, &u) && u.RecordPauseUnsupported() {
		return http.StatusNotImplemented
	}
	return http.StatusInternalServerError
}

// HandleRecordPause is POST /agent/record/pause: it pauses recording. The
// body, optional, is a models.RecordPauseReq. Idempotent.
func (a *Agent) HandleRecordPause(w http.ResponseWriter, r *http.Request) {
	svc, ok := a.svc.(recordPauser)
	if !ok {
		answerRecordPause(w, r, http.StatusNotImplemented, recordPauseUnsupported)
		return
	}
	var req models.RecordPauseReq
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRecordPauseBody))
	if err != nil {
		answerRecordPause(w, r, http.StatusBadRequest, "could not read the request body: "+err.Error())
		return
	}
	if len(strings.TrimSpace(string(body))) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			answerRecordPause(w, r, http.StatusBadRequest, `the request body is not JSON of the form {"reason":"..."}: `+err.Error())
			return
		}
	}
	if err := svc.PauseRecording(r.Context(), req.Reason); err != nil {
		a.logger.Error("failed to pause recording for the control plane", zap.Error(err),
			zap.String("next_step", "the agent is still recording; retry the pause, and if it keeps failing restart the agent"))
		answerRecordPause(w, r, pauseFailureStatus(err), err.Error())
		return
	}
	answerRecordPause(w, r, http.StatusOK, "")
}

// HandleRecordResume is POST /agent/record/resume: it ends the control
// plane's pause. No body. Idempotent.
func (a *Agent) HandleRecordResume(w http.ResponseWriter, r *http.Request) {
	svc, ok := a.svc.(recordPauser)
	if !ok {
		answerRecordPause(w, r, http.StatusNotImplemented, recordPauseUnsupported)
		return
	}
	if err := svc.ResumeRecording(r.Context()); err != nil {
		a.logger.Error("failed to resume recording for the control plane", zap.Error(err),
			zap.String("next_step", "the agent may still be paused (GET /agent/health says: "+models.RecordPausedHeader+"); retry the resume, and if it keeps failing restart the agent, which starts unpaused"))
		answerRecordPause(w, r, pauseFailureStatus(err), err.Error())
		return
	}
	answerRecordPause(w, r, http.StatusOK, "")
}

// setRecordPauseHeaders adds to a /agent/health answer what the agent can do
// (models.AgentCapabilitiesHeader) and whether its control plane has paused
// recording (models.RecordPausedHeader). An agent whose service cannot pause
// lists no such capability, and is never paused.
func (a *Agent) setRecordPauseHeaders(h http.Header) {
	paused, supported := false, false
	if _, ok := a.svc.(recordPauser); ok {
		supported = true
		if rep, ok := a.svc.(recordPauseReporter); ok {
			paused, _, supported = rep.RecordingPaused()
		}
	}
	if supported {
		h.Set(models.AgentCapabilitiesHeader, strings.Join([]string{models.CapabilityRecordPause}, ","))
	}
	h.Set(models.RecordPausedHeader, strconv.FormatBool(paused))
}
