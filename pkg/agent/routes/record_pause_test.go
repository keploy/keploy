package routes

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/agent/memoryguard"
	syncmgr "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/service/agent"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// pausingSvc is a service that can pause recording, as the agent's own does:
// it keeps the pause it was asked for, and fails or disowns the capability on
// request.
type pausingSvc struct {
	agent.Service // nil: any other call panics loudly
	paused        bool
	reason        string
	pauses        []string
	resumes       int
	err           error
	unsupported   bool
}

func (s *pausingSvc) PauseRecording(_ context.Context, reason string) error {
	if s.err != nil {
		return s.err
	}
	s.paused, s.reason = true, reason
	s.pauses = append(s.pauses, reason)
	return nil
}

func (s *pausingSvc) ResumeRecording(context.Context) error {
	if s.err != nil {
		return s.err
	}
	s.paused, s.reason = false, ""
	s.resumes++
	return nil
}

func (s *pausingSvc) RecordingPaused() (bool, string, bool) {
	return s.paused, s.reason, !s.unsupported
}

// unpausableSvc is a service built before the pause: it has none of its methods.
type unpausableSvc struct{ agent.Service }

// unsupportedErr is a service's "this agent cannot pause at all".
type unsupportedErr struct{}

func (unsupportedErr) Error() string                { return "this agent's proxy cannot pause or resume recording" }
func (unsupportedErr) RecordPauseUnsupported() bool { return true }

// pauseRouter is the agent's router as the server mounts it: every route
// behind the token guard.
func pauseRouter(svc agent.Service, token string) http.Handler {
	r := chi.NewRouter()
	r.Use(Authenticate(zap.NewNop(), token))
	DefaultRoutes{}.New(r, svc, zap.NewNop())
	return r
}

// askAgent sends one request to h with the session's token, unless tok is "".
func askAgent(t *testing.T, h http.Handler, method, path, body, tok string) *httptest.ResponseRecorder {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, r)
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// agentResp decodes the AgentResp every pause and resume answers with.
func agentResp(t *testing.T, rec *httptest.ResponseRecorder) models.AgentResp {
	t.Helper()
	var out models.AgentResp
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out), "the answer is not an AgentResp: %s", rec.Body.String())
	return out
}

// POST /agent/record/pause pauses recording with the reason its optional body
// gives, POST /agent/record/resume ends the pause, each answers with an
// AgentResp, and each can be repeated.
func TestRecordPauseAndResumeRoutes(t *testing.T) {
	svc := &pausingSvc{}
	h := pauseRouter(svc, testToken)

	for _, body := range []string{`{"reason":"rollout"}`, `{"reason":"rollout"}`, "", "  ", `{}`} {
		rec := askAgent(t, h, http.MethodPost, "/agent/record/pause", body, testToken)
		require.Equal(t, http.StatusOK, rec.Code, "body %q: %s", body, rec.Body.String())
		require.Equal(t, models.AgentResp{IsSuccess: true}, agentResp(t, rec))
		require.True(t, svc.paused)
	}
	require.Equal(t, []string{"rollout", "rollout", "", "", ""}, svc.pauses, "each pause carries its body's reason; a body is optional")

	for i := 0; i < 2; i++ {
		rec := askAgent(t, h, http.MethodPost, "/agent/record/resume", "", testToken)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.Equal(t, models.AgentResp{IsSuccess: true}, agentResp(t, rec))
		require.False(t, svc.paused)
	}
	require.Equal(t, 2, svc.resumes)

	// A resume takes no body, and ignores one.
	rec := askAgent(t, h, http.MethodPost, "/agent/record/resume", `{"reason":"x"}`, testToken)
	require.Equal(t, http.StatusOK, rec.Code)

	// POST only.
	for _, path := range []string{"/agent/record/pause", "/agent/record/resume"} {
		rec := askAgent(t, h, http.MethodGet, path, "", testToken)
		require.Equal(t, http.StatusMethodNotAllowed, rec.Code, path)
	}
}

// Both routes are behind the router's token guard, as every route but
// /agent/health: without the session's token neither reaches the service.
func TestRecordPauseRoutesNeedTheSessionToken(t *testing.T) {
	svc := &pausingSvc{}
	h := pauseRouter(svc, testToken)
	for _, path := range []string{"/agent/record/pause", "/agent/record/resume"} {
		for name, tok := range map[string]string{"no token": "", "a wrong token": strings.Repeat("f", len(testToken))} {
			rec := askAgent(t, h, http.MethodPost, path, `{"reason":"rollout"}`, tok)
			require.Equal(t, http.StatusUnauthorized, rec.Code, "%s with %s", path, name)
		}
	}
	require.Empty(t, svc.pauses, "a pause without the token reached the service")
	require.Zero(t, svc.resumes, "a resume without the token reached the service")

	// A browser's request is refused whatever it carries.
	req := httptest.NewRequest(http.MethodPost, "/agent/record/pause", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Origin", "https://example.test")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusForbidden, rec.Code)
	require.Empty(t, svc.pauses)
}

// A body that is not the request's JSON is refused with a 400 and pauses
// nothing; a service's failure is a 500 with its error; and an agent that
// cannot pause answers 501, whether its service has no such methods or says
// its proxy cannot. Every one of them is an AgentResp that says it failed.
func TestRecordPauseRoutesFailInTheAgentRespShape(t *testing.T) {
	t.Run("a body that is not JSON", func(t *testing.T) {
		svc := &pausingSvc{}
		rec := askAgent(t, pauseRouter(svc, testToken), http.MethodPost, "/agent/record/pause", `{"reason":`, testToken)
		require.Equal(t, http.StatusBadRequest, rec.Code)
		out := agentResp(t, rec)
		require.False(t, out.IsSuccess)
		require.NotEmpty(t, out.ErrorMsg)
		require.False(t, svc.paused)
	})
	t.Run("a service that fails", func(t *testing.T) {
		svc := &pausingSvc{err: errors.New("the proxy is not up")}
		for _, path := range []string{"/agent/record/pause", "/agent/record/resume"} {
			rec := askAgent(t, pauseRouter(svc, testToken), http.MethodPost, path, "", testToken)
			require.Equal(t, http.StatusInternalServerError, rec.Code, path)
			require.Equal(t, models.AgentResp{ErrorMsg: "the proxy is not up"}, agentResp(t, rec), path)
		}
	})
	t.Run("a service whose proxy cannot pause", func(t *testing.T) {
		svc := &pausingSvc{err: unsupportedErr{}}
		for _, path := range []string{"/agent/record/pause", "/agent/record/resume"} {
			rec := askAgent(t, pauseRouter(svc, testToken), http.MethodPost, path, "", testToken)
			require.Equal(t, http.StatusNotImplemented, rec.Code, path)
			require.False(t, agentResp(t, rec).IsSuccess, path)
		}
	})
	t.Run("a service with no pause at all", func(t *testing.T) {
		for _, path := range []string{"/agent/record/pause", "/agent/record/resume"} {
			rec := askAgent(t, pauseRouter(unpausableSvc{}, testToken), http.MethodPost, path, `{"reason":"rollout"}`, testToken)
			require.Equal(t, http.StatusNotImplemented, rec.Code, path)
			out := agentResp(t, rec)
			require.False(t, out.IsSuccess, path)
			require.Equal(t, recordPauseUnsupported, out.ErrorMsg, path)
		}
	})
}

// The agent's own service answers "cannot pause" in the way the routes turn
// into a 501: its proxy here is none at all.
func TestTheAgentServiceWithoutAPausingProxyAnswers501(t *testing.T) {
	svc := &agent.Agent{}
	for _, path := range []string{"/agent/record/pause", "/agent/record/resume"} {
		rec := askAgent(t, pauseRouter(svc, testToken), http.MethodPost, path, "", testToken)
		require.Equal(t, http.StatusNotImplemented, rec.Code, path)
		require.False(t, agentResp(t, rec).IsSuccess, path)
	}
	rec := askAgent(t, pauseRouter(svc, testToken), http.MethodGet, "/agent/health", "", "")
	require.Empty(t, rec.Header().Values(models.AgentCapabilitiesHeader), "an agent that cannot pause lists the capability")
	require.Equal(t, "false", rec.Header().Get(models.RecordPausedHeader))
}

// GET /agent/health, which needs no token, lists the pause among the agent's
// capabilities and says whether recording is paused, as the control plane
// reads them before and after it asks. An agent whose service cannot pause
// lists no such capability and is never paused.
func TestHealthSaysWhetherRecordingCanBeAndIsPaused(t *testing.T) {
	svc := &pausingSvc{}
	h := pauseRouter(svc, testToken)
	health := func(h http.Handler) http.Header {
		rec := askAgent(t, h, http.MethodGet, "/agent/health", "", "")
		require.Equal(t, http.StatusOK, rec.Code)
		return rec.Header()
	}

	got := health(h)
	require.Equal(t, "record-pause", got.Get("X-Keploy-Agent-Capabilities"))
	require.Equal(t, "false", got.Get("X-Keploy-Record-Paused"))

	require.Equal(t, http.StatusOK, askAgent(t, h, http.MethodPost, "/agent/record/pause", `{"reason":"rollout"}`, testToken).Code)
	got = health(h)
	require.Equal(t, "record-pause", got.Get("X-Keploy-Agent-Capabilities"))
	require.Equal(t, "true", got.Get("X-Keploy-Record-Paused"))

	require.Equal(t, http.StatusOK, askAgent(t, h, http.MethodPost, "/agent/record/resume", "", testToken).Code)
	require.Equal(t, "false", health(h).Get("X-Keploy-Record-Paused"))

	for name, svc := range map[string]agent.Service{
		"a service with no pause at all":        unpausableSvc{},
		"a service whose proxy cannot pause it": &pausingSvc{unsupported: true},
	} {
		got := health(pauseRouter(svc, testToken))
		require.Empty(t, got.Values("X-Keploy-Agent-Capabilities"), name)
		require.Equal(t, "false", got.Get("X-Keploy-Record-Paused"), name)
	}
}

// No test case whose window overlaps a pause the control plane asked for is
// streamed: not one in flight as the pause began, one within it, or one in
// flight as it ended. Those wholly before it and after it are. The session's
// summary and its stream's end marker say what the pause cost as the control
// plane's, not as memory pressure.
func TestTheIncomingStreamLeavesOutWhatAControlPlanePauseOverlaps(t *testing.T) {
	t.Cleanup(func() {
		memoryguard.SetExternalPause(false, "")
		syncmgr.ResetPauseCausesForTest()
	})
	syncmgr.Get().SetWatermark(nil) // no hold: test cases go at once
	svc := streamsSvc{tc: make(chan *models.TestCase, 8), mocks: make(chan *models.Mock), maps: make(chan models.TestMockMapping)}
	core, logs := observer.New(zapcore.DebugLevel)
	a := &Agent{logger: zap.New(core), svc: svc}
	tok := rand.Text()
	resp, done := openHeldStream(t, a.HandleIncoming, models.IncomingReq{}, tok)
	defer done()

	tick := func() time.Time {
		time.Sleep(5 * time.Millisecond)
		now := time.Now()
		time.Sleep(5 * time.Millisecond)
		return now
	}
	before := tick()
	memoryguard.SetExternalPause(true, "rollout")
	during := tick()
	// A request the ingress read during the pause, and a mock of a call made
	// during it: neither is recorded.
	syncmgr.Get().NoteNotRecordedUnderPressure("10.0.0.1:5000 -> 127.0.0.1:8080")
	m := &models.Mock{Kind: models.Kind("Kafka"), ConnectionID: "paused"}
	m.Spec.ReqTimestampMock = during
	m.Spec.ResTimestampMock = during.Add(time.Millisecond)
	require.Equal(t, syncmgr.MockDroppedUnderPressure, syncmgr.Get().AddMock(m))
	memoryguard.SetExternalPause(false, "")
	after := tick()

	windowed := func(name string, from, to time.Time) *models.TestCase {
		return &models.TestCase{Name: name, Kind: models.HTTP,
			HTTPReq: models.HTTPReq{Timestamp: from}, HTTPResp: models.HTTPResp{Timestamp: to}}
	}
	svc.tc <- windowed("before-the-pause", before, before)
	svc.tc <- windowed("in-flight-as-it-began", before, during)
	svc.tc <- windowed("within-it", during, during)
	svc.tc <- windowed("in-flight-as-it-ended", during, after)
	svc.tc <- windowed("after-the-resume", after, after)
	endRecordingStreams(tok)

	_, params, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	require.NoError(t, err)
	mr := multipart.NewReader(resp.Body, params["boundary"])
	var got []string
	var sum models.StreamEndSummary
	for {
		part, err := mr.NextPart()
		require.NoError(t, err, "the stream ended without its marker, after %v", got)
		if part.FormName() == models.StreamEndPart {
			require.NoError(t, json.NewDecoder(part).Decode(&sum))
			break
		}
		if part.FormName() == "metadata" {
			var tc models.TestCase
			require.NoError(t, json.NewDecoder(part).Decode(&tc))
			got = append(got, tc.Name)
		}
	}
	require.Equal(t, []string{"before-the-pause", "after-the-resume"}, got,
		"only the test cases the pause did not overlap are recorded")
	require.Equal(t, 2, sum.TestCasesSent)
	require.Equal(t, 3, sum.TestCasesSuppressed)
	require.Equal(t, 3, sum.SuppressedByPause)
	require.Equal(t, models.PressureSkipSummary{Requests: 1, Connections: 1}, sum.NotRecordedWhilePaused)
	require.Zero(t, sum.NotRecordedUnderPressure, "the control plane's pause was reported as memory pressure")
	require.Equal(t, int64(1), sum.MocksDropped.ByControlPlanePause)
	require.Zero(t, sum.MocksDropped.ByPressure, "the control plane's pause was reported as memory pressure")
	require.False(t, sum.MocksDropped.Dropped(), "a mock dropped for a pause that was asked for is no loss a test case can feel")

	done() // the handler has returned, its summary said
	require.Empty(t, logs.FilterMessageSnippet("the agent was short of memory").All(),
		"the agent said it was short of memory; it was paused on request")
	require.Empty(t, logs.FilterMessage(models.MocksDroppedSaid).All(),
		"the agent warned of dropped mocks; the pause that dropped them was asked for")
	said := logs.FilterMessageSnippet("the control plane had paused recording when they came").All()
	require.Len(t, said, 1)
	require.Equal(t, zapcore.InfoLevel, said[0].Level)
	require.Equal(t, uint64(1), said[0].ContextMap()["requests_not_recorded_while_paused"])
	sumLine := logs.FilterMessage("agent: recording complete").All()
	require.Len(t, sumLine, 1)
	f := sumLine[0].ContextMap()
	require.Equal(t, int64(1), f["mocks_dropped_by_control_plane_pause"])
	require.Equal(t, int64(0), f["mocks_dropped_by_pressure"])
	require.Equal(t, uint64(1), f["tcs_not_recorded_while_paused"])
	require.Equal(t, uint64(0), f["tcs_not_recorded_under_pressure"])
	require.Equal(t, int64(3), f["tcs_suppressed_by_control_plane_pause"])
}
