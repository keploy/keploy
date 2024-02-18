package proxy

import (
	"bufio"
	"go.uber.org/zap"
	"io"
	"net"
	"sync"
)

type TLSConn struct {
	net.Conn
	r      io.Reader
	logger *zap.Logger
	mu     sync.Mutex
}

func (c *TLSConn) Read(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(p) == 0 {
		c.logger.Debug("the length is 0 for the reading from customConn")
	}
	return c.r.Read(p)
}

type Conn struct {
	net.Conn
	r bufio.Reader
}

func (c *Conn) Read(b []byte) (n int, err error) {
	return c.r.Read(b)
}
