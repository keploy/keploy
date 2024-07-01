//go:build linux

package proxy

import (
	"io"
	"net"
	"sync"

	"go.uber.org/zap"
)

type Conn struct {
	net.Conn
	r      io.Reader
	logger *zap.Logger
	mu     sync.Mutex
}

func (c *Conn) Read(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(p) == 0 {
		c.logger.Debug("the length is 0 for the reading from customConn")
	}
	return c.r.Read(p)
}
