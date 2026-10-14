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
	"time"

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
	return a.pushRealDeps(ctx, models.RealDepsReq{Real: real})
}

// KeepRealDeps is PushRealDeps for a later push of the same run (keploy
// test's next test set): an agent that already holds the list keeps what the
// run saw of it (models.RealDepsReq.Keep).
func (a *AgentClient) KeepRealDeps(ctx context.Context, real []string) error {
	return a.pushRealDeps(ctx, models.RealDepsReq{Real: real, Keep: true})
}

// pushRealDeps posts req to the agent's /real.
func (a *AgentClient) pushRealDeps(ctx context.Context, req0 models.RealDepsReq) error {
	body, err := json.Marshal(req0)
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
// (models.RealRun) since the last read: keploy mock and keploy record once,
// after the run's last windows; keploy test once per test set. The read
// takes the capture_incomplete evidence charged to no test. It has no deadline:
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

// RealTest ends the open test case for mock.real (models.RealTest) and reads
// what it did: its REAL rows and the capture_incomplete evidence charged to
// it. It has no deadline: the agent's answer, or the connection's close,
// ends it.
func (a *AgentClient) RealTest(ctx context.Context) (models.RealTest, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/real/test", a.conf.Agent.AgentURI), nil)
	if err != nil {
		return models.RealTest{}, err
	}
	res, err := a.client.Do(req)
	if err != nil {
		return models.RealTest{}, fmt.Errorf("failed to read the test case's mock.real use from the agent: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, res.Body)
		_ = res.Body.Close()
	}()
	if res.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(res.Body)
		return models.RealTest{}, fmt.Errorf("the agent did not report the test case's mock.real use: status %d: %s", res.StatusCode, strings.TrimSpace(string(b)))
	}
	var got models.RealTest
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		return models.RealTest{}, fmt.Errorf("failed to decode the test case's mock.real use: %w", err)
	}
	return got, nil
}

// BeforeSimulateReal is BeforeSimulate for a test case whose recording lists
// mock.real destinations (real): after the hook's work, the run's clock goes
// on real time, not the recorded request's (design v14 §3.4, F3). Unlike
// BeforeSimulate it fails when the agent cannot be asked or refuses: the
// case must not run at its recorded time against a dependency that answers
// at real time. Without an agent there is no clock to move.
func (a *AgentClient) BeforeSimulateReal(ctx context.Context, timestamp *time.Time, testSetID, tcName string, real []string) error {
	if a.conf.Agent.AgentURI == "" {
		return nil
	}
	req0 := models.BeforeSimulateRequest{TestSetID: testSetID, TestCaseName: tcName, Real: real}
	if timestamp != nil {
		req0.TimeStamp = *timestamp
	}
	body, err := json.Marshal(req0)
	if err != nil {
		return fmt.Errorf("failed to marshal payload: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.conf.Agent.AgentURI+"/hooks/before-simulate", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	// No deadline of keploy's: the agent's answer, or the connection's
	// close, ends it.
	res, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to ask keploy's agent to put a mock.real test case on the real clock: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, res.Body)
		_ = res.Body.Close()
	}()
	if res.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(res.Body)
		return fmt.Errorf("keploy's agent did not put a mock.real test case on the real clock: status %d: %s", res.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}
