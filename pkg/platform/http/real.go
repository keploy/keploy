package http

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"

	"go.keploy.io/server/v3/pkg/models"
)

// PushRealDeps hands the agent the run's mock.real (models.RealDepsReq) as the
// run starts, under the proven switch: the destinations it relays to the real
// thing, never recording or mocking them (design v14 §4). The list is given
// again to a restarted agent. An agent that cannot take it is an error: its
// destinations would be recorded and mocked.
func (a *AgentClient) PushRealDeps(ctx context.Context, real []string) (retErr error) {
	defer func() {
		if retErr == nil && a.session().recording() {
			recorded := slices.Clone(real)
			a.session().record(stepRealDeps, func(c context.Context) error { return a.PushRealDeps(c, recorded) })
		}
	}()
	body, err := json.Marshal(models.RealDepsReq{Real: real})
	if err != nil {
		return fmt.Errorf("failed to marshal mock.real: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("%s/real", a.conf.Agent.AgentURI), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send the agent mock.real: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, res.Body)
		_ = res.Body.Close()
	}()
	if res.StatusCode == http.StatusNotFound {
		return models.ErrNoRealRoute
	}
	if res.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(res.Body)
		return fmt.Errorf("the agent did not take mock.real: status %d: %s", res.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}

// RealRun reads what the run saw of mock.real beyond its tests
// (models.RealRun), once, after the run's last scope windows: the read takes
// the capture_incomplete evidence charged to no test. It has no deadline:
// the agent's answer, or the connection's close, ends it.
func (a *AgentClient) RealRun(ctx context.Context) (models.RealRun, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/real/run", a.conf.Agent.AgentURI), nil)
	if err != nil {
		return models.RealRun{}, err
	}
	res, err := a.client.Do(req)
	if err != nil {
		return models.RealRun{}, fmt.Errorf("failed to read the run's mock.real destinations from the agent: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, res.Body)
		_ = res.Body.Close()
	}()
	if res.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(res.Body)
		return models.RealRun{}, fmt.Errorf("the agent did not report the run's mock.real destinations: status %d: %s", res.StatusCode, strings.TrimSpace(string(b)))
	}
	var run models.RealRun
	if err := json.NewDecoder(res.Body).Decode(&run); err != nil {
		return models.RealRun{}, fmt.Errorf("failed to decode the run's mock.real destinations: %w", err)
	}
	return run, nil
}
