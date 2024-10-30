//go:build windows

package windows

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"strconv"
	"time"

	"github.com/Microsoft/go-winio"
	"go.keploy.io/server/v2/pkg/core/hooks/ipc/grpc"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"
)

type Pipe struct {
	Name              string
	IncomingCh        map[IncomingChKey]chan grpc.Packet
	OutgoingCh        chan grpc.Packet
	InterCeptConfChan chan grpc.InterceptConf
	Logger            *zap.Logger
}

type IncomingChKey struct {
}

func (p *Pipe) NewPipe(name string) *Pipe {
	return &Pipe{
		Name:       name,
		IncomingCh: make(map[IncomingChKey]chan grpc.Packet),
		OutgoingCh: make(chan grpc.Packet),
	}
}

const (
	IPCBufSize = 4096
)

func (p *Pipe) Start(ctx context.Context) error {
	pipeConfig := &winio.PipeConfig{
		InputBufferSize:  IPCBufSize,
		OutputBufferSize: IPCBufSize,
		MessageMode:      true,
	}

	listener, err := winio.ListenPipe(p.Name, pipeConfig)
	if err != nil {
		p.Logger.Error("failed to create named pipe", zap.Error(err))
		return errors.New("failed to start pipe")
	}

	fmt.Printf("Named pipe server listening on %s\n", p.Name)

	go func() {
		conn, err := listener.Accept()
		if err != nil {
			p.Logger.Error("failed to accept connection")
		}
		p.handleConnection(ctx, conn, strconv.Itoa(os.Getpid()))
	}()
	time.Sleep(2 * time.Second)
	return nil
}

func (p *Pipe) handleConnection(_ context.Context, conn net.Conn, appPid string) {

	defer conn.Close()
	buf := make([]byte, IPCBufSize)

	var pid []string

	fmt.Println("hi")
	fmt.Println(appPid)

	pid = append(pid, appPid)

	conf := &grpc.InterceptConf{
		Default: true,
		Actions: pid,
	}

	inter := &grpc.FromProxy_InterceptConf{
		InterceptConf: conf,
	}

	fromProxy := &grpc.FromProxy{
		Message: inter,
	}

	data, err := proto.Marshal(fromProxy)
	if err != nil {
		log.Printf("Failed to marshal response: %v", err)
	}

	conn.Write(data)

	for {
		fmt.Println("came here")
		n, err := conn.Read(buf)
		var packet grpc.PacketWithMeta
		err = proto.Unmarshal(buf[:n], &packet)
		if err != nil {
			log.Printf("Client disconnected or error reading: %v", err)
			break
		}
		fmt.Println(buf[:n])
		fmt.Println("priitning")
		fmt.Println(packet.Data)
		fmt.Println(packet.TunnelInfo.Pid)
		fmt.Println(*packet.TunnelInfo.ProcessName)
		fmt.Println("data")
	}

	log.Println("Shutting down server after client disconnection.")
}
