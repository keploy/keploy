//go:build windows

package hooks

import (
	"context"
	"net"
	"os"

	"go.uber.org/zap"
)

type Pipe struct {
	Name   string
	Logger *zap.Logger
}

func (p *Pipe) NewPipe(name string) *Pipe {
	return &Pipe{
		Name: name,
	}
}

const (
	IPCBufSize = 4096
)

func (p *Pipe) Start(ctx context.Context) (net.Conn, error) {
	socketPath := `C:\my.sock`

	// Clean up the socket file if it already exists
	if _, err := os.Stat(socketPath); err == nil {
		os.Remove(socketPath)
	}

	// Create a Unix socket listener
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, err
	}

	defer listener.Close()
	conn, err := listener.Accept()
	if err != nil {
		p.Logger.Error("failed to accept connection")
	}
	return conn, nil
}
