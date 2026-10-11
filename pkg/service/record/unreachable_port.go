package record

import (
	"context"
	"fmt"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// unreachablePortWarner warns while recording, once per app port, that the test
// cases recorded on that port will not reach the app on a replay that sends
// them where it does by default: replay sends each test from the host to the
// port it was recorded on, and the instrumentation says the host cannot reach
// the app there (in Docker mode, a port the docker command does not publish as
// itself; the app calling its own in-container server is recorded as ingress on
// such a port). A replay with a port map in keploy.yml (test.replaceWith) can
// still send those tests to a host port their port is published on, and the
// reason says so where there is one. Without it the first sign is a replay
// refusing every one of those tests.
//
// It asks about the address replay will use by default: test.host and the
// recorded port. A keploy.yml that sends replay elsewhere (test.port, a
// protocol port, replaceWith) makes that the wrong address to ask about, so
// then it asks nothing rather than warn about a port replay will not use.
type unreachablePortWarner struct {
	logger  *zap.Logger
	reach   pkg.AppPortReachability
	host    string
	checked map[uint16]bool
}

func newUnreachablePortWarner(logger *zap.Logger, instrumentation Instrumentation, cfg *config.Config) *unreachablePortWarner {
	reach, ok := instrumentation.(pkg.AppPortReachability)
	if !ok || cfg == nil || replayTargetRedirected(cfg.Test) {
		return &unreachablePortWarner{}
	}
	host := cfg.Test.Host
	if host == "" {
		host = "localhost"
	}
	return &unreachablePortWarner{logger: logger, reach: reach, host: host, checked: map[uint16]bool{}}
}

// check asks about tc's app port the first time a test case is recorded on it.
func (w *unreachablePortWarner) check(ctx context.Context, tc *models.TestCase) {
	if w.reach == nil || tc == nil || tc.AppPort == 0 || w.checked[tc.AppPort] {
		return
	}
	w.checked[tc.AppPort] = true
	// It runs in line with the recording; reach bounds its own wait.
	reason := w.reach.UnreachableAppPort(ctx, w.host, tc.AppPort, tc.AppPort)
	if reason == "" {
		return
	}
	w.logger.Warn(fmt.Sprintf("test cases recorded on the app's port %d cannot be replayed at that port from the host: %s", tc.AppPort, reason),
		zap.String("first testcase on the port", tc.Name))
}

// replayTargetRedirected reports whether the test config sends replay somewhere
// other than test.host and each test's recorded port.
func replayTargetRedirected(t config.Test) bool {
	if t.Port != 0 || t.GRPCPort != 0 || t.SSEPort != 0 {
		return true
	}
	for _, p := range t.Protocol {
		if p.Port != 0 {
			return true
		}
	}
	rw := t.ReplaceWith
	return len(rw.Global.URL) > 0 || len(rw.Global.Port) > 0 || len(rw.TestSets) > 0
}
