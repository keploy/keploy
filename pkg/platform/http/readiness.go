package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// AppReadiness asks the agent what it knows of the application's listens
// from since on and of its start's progress (GET /app/readiness?since=N,
// under the proven switch). An error is "unknown", never "not ready".
func (a *AgentClient) AppReadiness(ctx context.Context, since uint64) (models.AppReadiness, error) {
	if a.conf.Agent.AgentURI == "" {
		return models.AppReadiness{}, errors.New("no keploy agent to ask")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.conf.Agent.AgentURI+"/app/readiness?since="+strconv.FormatUint(since, 10), nil)
	if err != nil {
		return models.AppReadiness{}, err
	}
	res, err := a.client.Do(req)
	if err != nil {
		return models.AppReadiness{}, fmt.Errorf("ask the agent whether the application listens: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	body, err := readAgentBody(res)
	if err != nil {
		return models.AppReadiness{}, err
	}
	if res.StatusCode == http.StatusNotFound || res.StatusCode == http.StatusNotImplemented {
		// An agent without the route (older, another platform's) or
		// without the watch.
		return models.AppReadiness{}, fmt.Errorf("%w: %v", models.ErrReadinessUnsupported, agentRespErr("app readiness", res, body))
	}
	if res.StatusCode != http.StatusOK {
		return models.AppReadiness{}, agentRespErr("app readiness", res, body)
	}
	var out models.AppReadiness
	if err := json.Unmarshal(body, &out); err != nil {
		return models.AppReadiness{}, fmt.Errorf("read the agent's answer to whether the application listens: %w", err)
	}
	return out, nil
}

// MarkReadiness is a set's mark before keploy starts its application anew
// (POST /app/readiness/mark): the agent forgets what the last set's
// processes did, and answers the counts its listens begin after. 404/501 is
// an agent that cannot (models.ErrReadinessUnsupported).
func (a *AgentClient) MarkReadiness(ctx context.Context) (models.AppReadiness, error) {
	if a.conf.Agent.AgentURI == "" {
		return models.AppReadiness{}, errors.New("no keploy agent to ask")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.conf.Agent.AgentURI+"/app/readiness/mark", nil)
	if err != nil {
		return models.AppReadiness{}, err
	}
	res, err := a.client.Do(req)
	if err != nil {
		return models.AppReadiness{}, fmt.Errorf("mark the set's start on the agent: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	body, err := readAgentBody(res)
	if err != nil {
		return models.AppReadiness{}, err
	}
	if res.StatusCode == http.StatusNotFound || res.StatusCode == http.StatusNotImplemented {
		return models.AppReadiness{}, fmt.Errorf("%w: %v", models.ErrReadinessUnsupported, agentRespErr("app readiness mark", res, body))
	}
	if res.StatusCode != http.StatusOK {
		return models.AppReadiness{}, agentRespErr("app readiness mark", res, body)
	}
	var out models.AppReadiness
	if err := json.Unmarshal(body, &out); err != nil {
		return models.AppReadiness{}, fmt.Errorf("read the agent's answer to the set's mark: %w", err)
	}
	return out, nil
}

// PublishedContainerPort says which container port the keploy agent
// container, which publishes the application's ports in Docker mode (docker
// run's -p and a compose service's ports both land on it), maps hostPort to:
// the lowest, when several do. false outside Docker mode or when it cannot
// tell.
func (a *AgentClient) PublishedContainerPort(ctx context.Context, hostPort uint16) (uint16, bool) {
	if a.conf == nil || a.dockerClient == nil || a.conf.KeployContainer == "" || !utils.IsDockerCmd(utils.CmdType(a.conf.CommandType)) {
		return 0, false
	}
	// No timer: the daemon answers late on a loaded machine, and a late
	// answer is the right one (a timeout would read the compose file's
	// guess instead).
	inspect, err := a.dockerClient.ContainerInspect(ctx, a.conf.KeployContainer)
	if err != nil || inspect.NetworkSettings == nil {
		a.logger.Debug("could not inspect the keploy agent container for the application's published ports",
			zap.String("container", a.conf.KeployContainer), zap.Error(err))
		return 0, false
	}
	var best uint16
	for containerPort, bindings := range inspect.NetworkSettings.Ports {
		if containerPort.Proto() != "tcp" {
			continue
		}
		c, err := strconv.ParseUint(containerPort.Port(), 10, 16)
		if err != nil {
			continue
		}
		for _, b := range bindings {
			if b.HostPort == strconv.Itoa(int(hostPort)) && (best == 0 || uint16(c) < best) {
				best = uint16(c)
			}
		}
	}
	return best, best != 0
}
