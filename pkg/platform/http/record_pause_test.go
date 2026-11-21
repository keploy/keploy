package http

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent/routes"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// pauseAgent is an agent, behind its token guard, that answers the pause and
// resume routes with status and an AgentResp, and keeps what it was asked.
func pauseAgent(t *testing.T, token string, status int, resp models.AgentResp) (*AgentClient, *[]string) {
	t.Helper()
	var asked []string
	h := routes.Authenticate(zap.NewNop(), token)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		asked = append(asked, r.Method+" "+r.URL.Path+" "+string(body))
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(resp)
	}))
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &AgentClient{
		logger: zap.NewNop(),
		client: agentHTTPClient(token),
		conf:   &config.Config{Agent: config.Agent{AgentURI: srv.URL + "/agent"}},
	}, &asked
}

// The client asks the agent to pause and to resume recording on the routes
// its control plane uses, with the session's token and the reason as the
// pause's body.
func TestAgentClientPausesAndResumesRecording(t *testing.T) {
	const token = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	a, asked := pauseAgent(t, token, http.StatusOK, models.AgentResp{IsSuccess: true})
	require.NoError(t, a.PauseRecording(context.Background(), "rollout"))
	require.NoError(t, a.ResumeRecording(context.Background()))
	require.Equal(t, []string{
		`POST /agent/record/pause {"reason":"rollout"}`,
		`POST /agent/record/resume `,
	}, *asked)
}

// An agent built before the routes (404), or one that cannot pause (501), is
// told apart from one that failed: the caller goes on without the pause.
func TestAgentClientTellsAnAgentThatCannotPauseFromOneThatFailed(t *testing.T) {
	const token = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	for _, status := range []int{http.StatusNotFound, http.StatusNotImplemented} {
		a, _ := pauseAgent(t, token, status, models.AgentResp{ErrorMsg: "no"})
		require.ErrorIs(t, a.PauseRecording(context.Background(), "rollout"), ErrRecordPauseUnsupported)
		require.ErrorIs(t, a.ResumeRecording(context.Background()), ErrRecordPauseUnsupported)
	}
	a, _ := pauseAgent(t, token, http.StatusInternalServerError, models.AgentResp{ErrorMsg: "the proxy is not up"})
	err := a.PauseRecording(context.Background(), "rollout")
	require.Error(t, err)
	require.False(t, errors.Is(err, ErrRecordPauseUnsupported))
	require.Contains(t, err.Error(), "the proxy is not up")

	a, _ = pauseAgent(t, token, http.StatusOK, models.AgentResp{ErrorMsg: "refused"})
	require.ErrorContains(t, a.ResumeRecording(context.Background()), "refused")
}
