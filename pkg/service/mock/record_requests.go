package mock

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	rec "go.keploy.io/server/v3/pkg/service/record"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// caseCapture stores the app's incoming requests as test cases while the runner runs.
type caseCapture struct {
	done  chan struct{}
	mu    sync.Mutex
	cases []capturedMock
}

func (m *mockService) requests() bool {
	if !m.config.Mock.RecordRequests {
		return false
	}
	_, ok := m.instrumentation.(IncomingReader)
	switch {
	case !ok || m.testDB == nil:
		m.logger.Warn("the app's incoming requests are not recorded: this build has no test-case store or incoming stream for them")
	default:
		return true
	}
	return false
}

// captureCases starts storing incoming requests when on; nil when requests are off or their stream cannot be read.
func (m *mockService) captureCases(captureCtx, persistCtx context.Context, name string, seen *atomic.Int64, on bool) *caseCapture {
	if !on {
		return nil
	}
	incoming, err := m.instrumentation.(IncomingReader).GetIncoming(captureCtx, models.IncomingOptions{Filters: m.config.Record.Filters})
	if err != nil {
		m.logger.Warn("the app's incoming requests are not recorded: could not read them from the agent", zap.Error(err))
		return nil
	}
	c := &caseCapture{done: make(chan struct{})}
	go func() {
		defer utils.Recover(m.logger)
		defer close(c.done)
		for tc := range incoming {
			seen.Add(1)
			if err := m.hooks.BeforeTestCaseInsert(persistCtx, &rec.TestCaseContext{TestCase: tc, TestSetID: name}); err != nil {
				m.logger.Debug("BeforeTestCaseInsert hook failed", zap.Error(err), zap.String("case", tc.Name))
			}
			if err := m.testDB.InsertTestCase(persistCtx, tc, name, true); err != nil {
				utils.LogError(m.logger, err, "failed to persist test case", zap.String("case", tc.Name))
				continue
			}
			if err := m.hooks.AfterTestCaseInsert(persistCtx, &rec.TestCaseContext{TestCase: tc, TestSetID: name}); err != nil {
				m.logger.Debug("AfterTestCaseInsert hook failed", zap.Error(err), zap.String("case", tc.Name))
			}
			c.add(tc)
		}
	}()
	return c
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
