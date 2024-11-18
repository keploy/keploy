//go:build windows

package hooks

import (
	"context"
	"errors"
	"net"

	"github.com/Microsoft/go-winio"
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
	pipeConfig := &winio.PipeConfig{
		InputBufferSize:  IPCBufSize,
		OutputBufferSize: IPCBufSize,
		MessageMode:      true,
	}
	listener, err := winio.ListenPipe(p.Name, pipeConfig)
	if err != nil {
		p.Logger.Error("failed to create named pipe", zap.Error(err))
		return nil, errors.New("failed to start pipe")
	}
	conn, err := listener.Accept()
	if err != nil {
		p.Logger.Error("failed to accept connection")
	}
	return conn, nil
}
