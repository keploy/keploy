package agent

import (
	"context"
	"time"

	httpparser "go.keploy.io/server/v3/pkg/agent/proxy/integrations/http"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// The scope API lets a user's own test runner mark per-test boundaries so
// Keploy can attribute captured mocks to a test (record) and serve only that
// test's mocks (replay). A pytest fixture / go-test helper / jest hook / curl
// script calls, using the KEPLOY_MOCK_AGENT URL Keploy exports into the wrapped
// command:
//
//	POST {url}/agent/scope/begin  {"name":"<test>"}
//	POST {url}/agent/scope/end    {"name":"<test>"}
//
// Scoping is entirely optional: with no scope calls the set records/replays
// suite-level, which is still correct.

// BeginScope opens a per-test scope. In record mode it stamps the begin time
// (agent clock) so captured mocks can later be bucketed to this test. In test
// mode it restricts the served pool to this test's mocks when the CLI supplied
// a mapping table for it.
func (a *Agent) BeginScope(ctx context.Context, name string) error {
	if name == "" {
		return nil
	}
	if a.config != nil && a.config.Agent.Mode == models.MODE_TEST {
		a.scopeMu.Lock()
		names, ok := a.scopeTable[name]
		a.scopeMu.Unlock()
		if !ok || len(names) == 0 {
			// No per-test mapping for this test — leave the whole pool armed.
			return nil
		}
		a.logger.Debug("scope begin: restricting served pool to test", zap.String("test", name), zap.Int("mocks", len(names)))
		return a.UpdateMockParams(ctx, models.MockFilterParams{
			MockMapping:     names,
			UseMappingBased: true,
			AfterTime:       models.BaseTime,
			BeforeTime:      time.Now(),
		})
	}

	// Record mode: mark the window start (agent clock). We deliberately do NOT
	// touch the syncMock ingress-correlation machinery here — that is driven by
	// incoming requests, which mock mode has none of. The window is correlated
	// to captured mocks purely by request timestamp after the run.
	a.scopeMu.Lock()
	if a.scopeOpen == nil {
		a.scopeOpen = make(map[string]time.Time)
	}
	a.scopeOpen[name] = time.Now()
	a.scopeMu.Unlock()
	a.logger.Debug("scope begin (record)", zap.String("test", name))
	return nil
}

// EndScope closes a per-test scope. In record mode it records the [begin, now]
// window for later correlation. In test mode it restores the whole pool so a
// call made between tests still matches.
func (a *Agent) EndScope(ctx context.Context, name string) error {
	if name == "" {
		return nil
	}
	if a.config != nil && a.config.Agent.Mode == models.MODE_TEST {
		a.scopeMu.Lock()
		_, scoped := a.scopeTable[name]
		a.scopeMu.Unlock()
		if !scoped {
			return nil
		}
		a.logger.Debug("scope end: restoring whole pool", zap.String("test", name))
		return a.UpdateMockParams(ctx, models.MockFilterParams{
			AfterTime:  models.BaseTime,
			BeforeTime: time.Now(),
		})
	}

	a.scopeMu.Lock()
	start, ok := a.scopeOpen[name]
	if ok {
		delete(a.scopeOpen, name)
		a.scopeWindows = append(a.scopeWindows, models.ScopeWindow{Name: name, Start: start, End: time.Now()})
	}
	a.scopeMu.Unlock()
	a.logger.Debug("scope end (record)", zap.String("test", name))
	return nil
}

// GetScopeWindows returns the per-test windows collected this record session,
// consumed by the CLI to build mappings.yaml.
func (a *Agent) GetScopeWindows(_ context.Context) ([]models.ScopeWindow, error) {
	a.scopeMu.Lock()
	defer a.scopeMu.Unlock()
	out := make([]models.ScopeWindow, len(a.scopeWindows))
	copy(out, a.scopeWindows)
	return out, nil
}

// SetScopeTable installs the replay-time per-test name→mock-names table the CLI
// read from mappings.yaml.
func (a *Agent) SetScopeTable(_ context.Context, table map[string][]string) error {
	a.scopeMu.Lock()
	a.scopeTable = table
	a.scopeMu.Unlock()
	return nil
}

// DrainCapturedMocks returns and clears the mocks captured on miss during a
// `--on-miss record` replay session, so the CLI can append them to the set.
func (a *Agent) DrainCapturedMocks(_ context.Context) ([]*models.Mock, error) {
	return httpparser.DrainCaptured(), nil
}

// MockStats returns a non-draining snapshot for /agent/mock/stats. Consumed and
// missed totals are surfaced in the CLI's end-of-run summary (they drain their
// capture windows), so this live endpoint reports the loaded count only.
func (a *Agent) MockStats(_ context.Context) (models.MockStats, error) {
	a.scopeMu.Lock()
	loaded := a.loadedMocks
	a.scopeMu.Unlock()
	return models.MockStats{Loaded: loaded}, nil
}
