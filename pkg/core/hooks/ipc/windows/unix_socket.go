//go:build windows

package hooks

import (
	"context"
	"net"
	"os"

	"go.uber.org/zap"
)

type UnixSocket struct {
	Path   string
	Logger *zap.Logger
}

func (u *UnixSocket) NewUnixSocket(path string) *UnixSocket {
	return &UnixSocket{
		Path: path,
	}
}

const (
	IPCBufSize = 4096
)

func (u *UnixSocket) Start(ctx context.Context) (net.Conn, error) {
	socketPath := u.Path

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
		u.Logger.Error("failed to accept connection")
	}
	return conn, nil
}
