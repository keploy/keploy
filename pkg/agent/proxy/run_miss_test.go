package proxy

import "go.uber.org/zap"

// mockFailed and sendMockNotFoundError are the tests' shorthand for a miss
// of a connection that names no process and no test worker: the run's.
// Production has neither: each of its misses names its connection's process
// and test worker (mockFailedFrom, sendMockNotFoundErrorFrom), so none lands
// in the run's window, which no worker's test reads.
func (p *Proxy) mockFailed(logger *zap.Logger, err error) error {
	return p.mockFailedFrom(logger, err, 0, 0, nil)
}

func (p *Proxy) sendMockNotFoundError(err error) {
	p.sendMockNotFoundErrorFrom(err, 0, 0, nil)
}
