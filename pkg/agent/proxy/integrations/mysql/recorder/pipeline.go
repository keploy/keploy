package recorder

import (
	"io"
	"net"

	mysqlUtils "go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/utils"
	"go.uber.org/zap"
)

// packetMsg carries a complete MySQL packet pre-fetched from the ring buffer.
type packetMsg struct {
	data []byte
	err  error
}

// packetPipeline pre-fetches MySQL packets from a connection's ring buffer
// into a buffered channel, acting as a message queue. A dedicated reader
// goroutine drains the ring buffer as fast as possible while the parser
// consumes packets at its own pace. This eliminates any back-pressure
// from parser processing on ring buffer draining.
//
// After a packet is consumed by the parser, its byte slice becomes eligible
// for GC — no manual flush needed.
type packetPipeline struct {
	ch   <-chan packetMsg
	conn net.Conn // underlying connection (for decodeCtx map keys)
	done chan struct{}
}

const pipelineBufSize = 256 // pre-fetch up to 256 complete MySQL packets

func newPacketPipeline(logger *zap.Logger, conn net.Conn) *packetPipeline {
	ch := make(chan packetMsg, pipelineBufSize)
	done := make(chan struct{})

	go func() {
		defer close(ch)
		defer close(done)
		for {
			// ReadPacketBuffer reads the 4-byte header + payload as a
			// single contiguous []byte.  On TeeForwardConn this reads
			// from the ring buffer (already forwarded data).
			data, err := mysqlUtils.ReadPacketBuffer(nil, logger, conn)

			// Non-blocking send: if channel is full we still send
			// (blocks here rather than on ring buffer, which is the
			// whole point — ring buffer stays drained).
			ch <- packetMsg{data: data, err: err}

			if err != nil {
				return
			}
		}
	}()

	return &packetPipeline{ch: ch, conn: conn, done: done}
}

// ReadPacket returns the next pre-fetched MySQL packet from the pipeline.
// Returns immediately if a packet is already buffered in the channel.
func (p *packetPipeline) ReadPacket() ([]byte, error) {
	msg, ok := <-p.ch
	if !ok {
		return nil, io.EOF
	}
	return msg.data, msg.err
}

// Wait blocks until the reader goroutine has finished.
func (p *packetPipeline) Wait() {
	<-p.done
}
