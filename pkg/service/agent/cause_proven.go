package agent

import (
	"context"
	"net/http"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// Cause takes a call of the open test that its harness saw a repeating timer
// make (models.CauseReq, POST /agent/cause). A record keeps it, in order, with
// the test's window (models.ScopeWindow.Timers): keploy mock record gives its
// recording to the set's worker scope with the stretch of writes it was
// recorded in. A replay needs none of it (a timer's recording is in the worker
// scope already) and takes it unkept, as it does one sent while no test is
// open: a poller's tick between tests is no test's anyway.
func (a *Agent) Cause(ctx context.Context, req models.CauseReq) *models.ScopeError {
	if req.Cause != models.CauseTimer || req.Method == "" || req.URL == "" || req.N < 0 {
		return scopeErr(http.StatusBadRequest, models.ScopeErrBadRequest, "a cause names a timer's call by its method, url and order", "send cause \"timer\", method, url and n")
	}
	if err := a.wire.lock(ctx); err != nil {
		return abandoned(err)
	}
	defer a.wire.unlock()
	if a.replaying() {
		return nil
	}
	a.provenMu.Lock()
	id := a.proven.open
	a.provenMu.Unlock()
	if id == "" {
		return nil
	}
	k := scopeKey{name: id}
	a.scopeMu.Lock()
	if a.scopeMeta == nil {
		a.scopeMeta = make(map[scopeKey]scopeMeta)
	}
	meta := a.scopeMeta[k]
	meta.timers = append(meta.timers, models.TimerCall{Method: req.Method, Host: req.Host, URL: req.URL, Body: req.Body, N: req.N})
	a.scopeMeta[k] = meta
	a.scopeMu.Unlock()
	a.logger.Debug("kept a call a repeating timer made, with the test's recording", zap.String("test", id), zap.String("url", req.URL), zap.Int("n", req.N))
	return nil
}
