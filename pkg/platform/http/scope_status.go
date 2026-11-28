package http

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"go.keploy.io/server/v3/pkg/models"
)

// ScopeStatus queries the agent's cgroup-based Go toolchain scoping state
// (GET /agent/scope-status). Returns zero ScopeStatus if an older agent does
// not implement the route (404).
func (a *AgentClient) ScopeStatus(ctx context.Context) (models.ScopeStatus, error) {
	req, err := newAgentRequest(ctx, http.MethodGet, fmt.Sprintf("%s/scope-status", a.conf.AgentURI()), nil)
	if err != nil {
		return models.ScopeStatus{}, err
	}
	res, err := doAgent(&a.client, req)
	if err != nil {
		return models.ScopeStatus{}, fmt.Errorf("failed to read agent scope status: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, res.Body)
		_ = res.Body.Close()
	}()
	if res.StatusCode == http.StatusNotFound {
		return models.ScopeStatus{}, nil
	}
	if res.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(res.Body)
		return models.ScopeStatus{}, fmt.Errorf("agent scope status returned %d: %s", res.StatusCode, strings.TrimSpace(string(b)))
	}
	var st models.ScopeStatus
	if err := json.NewDecoder(res.Body).Decode(&st); err != nil {
		return models.ScopeStatus{}, fmt.Errorf("decode agent scope status: %w", err)
	}
	return st, nil
}
