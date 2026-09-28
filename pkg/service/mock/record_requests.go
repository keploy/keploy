package mock

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// caseCapture stores the app's incoming requests as test cases while the runner runs.
type caseCapture struct {
	done  chan struct{}
	mu    sync.Mutex
	cases []capturedMock
}

// captureCases starts storing incoming requests under --record-requests; nil when the flag is off.
func (m *mockService) captureCases(captureCtx, persistCtx context.Context, name string, seen *atomic.Int64) (*caseCapture, error) {
	if !m.config.Mock.RecordRequests {
		return nil, nil
	}
	reader, ok := m.instrumentation.(IncomingReader)
	if !ok || m.testDB == nil {
		return nil, errors.New("--record-requests needs a test-case store and an agent that streams incoming requests")
	}
	if len(config.GetByPassPorts(m.config)) == 0 {
		m.logger.Warn("--record-requests without --pass-through-ports <app port>: the test's calls to the app will be recorded as mocks, not as requests")
	}
	incoming, err := reader.GetIncoming(captureCtx, models.IncomingOptions{Filters: m.config.Record.Filters})
	if err != nil {
		return nil, err
	}
	c := &caseCapture{done: make(chan struct{})}
	go func() {
		defer utils.Recover(m.logger)
		defer close(c.done)
		for tc := range incoming {
			seen.Add(1)
			if err := m.testDB.InsertTestCase(persistCtx, tc, name, true); err != nil {
				utils.LogError(m.logger, err, "failed to persist test case", zap.String("case", tc.Name))
				continue
			}
			c.add(tc)
		}
	}()
	return c, nil
}

func (c *caseCapture) add(tc *models.TestCase) {
	if tc.Name == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cases = append(c.cases, capturedMock{name: tc.Name, ts: caseTime(tc), end: caseEnd(tc)})
}

// caseTime is when the app received the request, which decides the flow it belongs to.
func caseTime(tc *models.TestCase) time.Time {
	if !tc.HTTPReq.Timestamp.IsZero() {
		return tc.HTTPReq.Timestamp
	}
	return tc.GrpcReq.Timestamp
}

func caseEnd(tc *models.TestCase) time.Time {
	if !tc.HTTPResp.Timestamp.IsZero() {
		return tc.HTTPResp.Timestamp
	}
	return tc.GrpcResp.Timestamp
}

// wait blocks until the incoming stream has been drained, or grace has passed.
func (c *caseCapture) wait(grace time.Duration) {
	if c == nil {
		return
	}
	select {
	case <-c.done:
	case <-time.After(grace):
	}
}

// list is every stored case with the time its request arrived.
func (c *caseCapture) list() []capturedMock {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]capturedMock(nil), c.cases...)
}
