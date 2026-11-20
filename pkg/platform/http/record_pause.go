package http

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// ErrRecordPauseUnsupported is what PauseRecording and ResumeRecording return
// from an agent that has no such routes (one built before them answers 404)
// or cannot pause (501): the caller goes on without the pause.
var ErrRecordPauseUnsupported = errors.New("the keploy agent cannot pause or resume recording")

// recordPauseTimeout bounds one pause or resume request. The resume resets
// the connections the pause passed through before it answers.
const recordPauseTimeout = 10 * time.Second

// PauseRecording asks the agent to pause recording (POST
// /agent/record/pause), with reason as free text for its logs: until
// ResumeRecording the agent passes the application's traffic through and
// records no test case and no mock. Idempotent. The pause is the agent
// process's memory, so it is asked again of an agent that restarted.
func (a *AgentClient) PauseRecording(ctx context.Context, reason string) error {
	body, err := json.Marshal(models.RecordPauseReq{Reason: reason})
	if err != nil {
		return fmt.Errorf("failed to encode the pause request: %w", err)
	}
	return a.postRecordPause(ctx, models.RecordPausePath, body)
}

// ResumeRecording asks the agent to end the pause PauseRecording began (POST
// /agent/record/resume). Idempotent.
func (a *AgentClient) ResumeRecording(ctx context.Context) error {
	return a.postRecordPause(ctx, models.RecordResumePath, nil)
}

// postRecordPause posts body to the agent's pause or resume route and reads
// its AgentResp.
func (a *AgentClient) postRecordPause(ctx context.Context, path string, body []byte) error {
	if a.conf.Agent.AgentURI == "" {
		return errors.New("the agent URI is empty: there is no agent to ask")
	}
	reqCtx, cancel := context.WithTimeout(ctx, recordPauseTimeout)
	defer cancel()
	req, err := newAgentRequest(reqCtx, http.MethodPost, a.conf.Agent.AgentURI+path, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("failed to create the request to %s: %w", path, err)
	}
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := doAgent(&a.client, req)
	if err != nil {
		return fmt.Errorf("failed to ask the agent (%s): %w", path, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusNotImplemented {
		return fmt.Errorf("%w (status %d from %s)", ErrRecordPauseUnsupported, resp.StatusCode, path)
	}
	var out models.AgentResp
	decodeErr := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("the agent answered %s with status %d: %s", path, resp.StatusCode, out.ErrorMsg)
	}
	if decodeErr != nil {
		return fmt.Errorf("failed to decode the agent's answer to %s: %w", path, decodeErr)
	}
	if !out.IsSuccess {
		return fmt.Errorf("the agent could not do it (%s): %s", path, out.ErrorMsg)
	}
	return nil
}
