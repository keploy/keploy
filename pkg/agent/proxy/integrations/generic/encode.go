package generic

import (
	"context"
	"io"
	"net"
	"sync"
	"time"

	"go.keploy.io/server/v3/pkg/agent/memoryguard"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/util"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// captureTeeWriter forwards data to dest (critical path) and non-blocking
// tees a copy to a capture channel. If the channel is full or recording is
// paused, capture is silently dropped — forwarding is never affected.
type captureTeeWriter struct {
	dest      io.Writer
	ch        chan []byte
	stopped   bool
	closeOnce *sync.Once // shared with the forwarding goroutine's defer close
}

func (w *captureTeeWriter) stop() {
	w.stopped = true
	// Close the channel so the consumer goroutine can exit promptly
	// instead of blocking until the connection ends.
	w.closeOnce.Do(func() { close(w.ch) })
}

func (w *captureTeeWriter) Write(p []byte) (int, error) {
	// Forward to destination first — this is the critical path.
	n, err := w.dest.Write(p)
	if err != nil {
		return n, err
	}
	// Non-blocking tee to capture channel.
	if !w.stopped {
		if memoryguard.IsRecordingPaused() {
			w.stop()
			return n, nil
		}
		buf := make([]byte, len(p))
		copy(buf, p)
		select {
		case w.ch <- buf:
		default:
			w.stop()
		}
	}
	return n, nil
}

func encodeGeneric(ctx context.Context, logger *zap.Logger, reqBuf []byte, clientConn, destConn net.Conn, mocks chan<- *models.Mock, opts models.OutgoingOptions) error {

	// Forward initial request buffer to destination immediately.
	_, err := destConn.Write(reqBuf)
	if err != nil {
		utils.LogError(logger, err, "failed to write request message to the destination server")
		return err
	}

	// If recording is already paused, pure passthrough.
	if memoryguard.IsRecordingPaused() {
		return forwardBidirectional(clientConn, destConn)
	}

	// Capture channels — background goroutine reads these to create mocks.
	// Buffered to absorb brief processing delays without blocking forwarding.
	clientCapChan := make(chan []byte, 256)
	destCapChan := make(chan []byte, 256)

	// Seed initial request buffer into capture.
	initialBuf := make([]byte, len(reqBuf))
	copy(initialBuf, reqBuf)
	clientCapChan <- initialBuf

	// Start background mock creator.
	go createGenericMocksAsync(ctx, logger, clientCapChan, destCapChan)

	// Forward bidirectionally at wire speed with non-blocking capture tee.
	// Each tee writer shares a closeOnce with the deferred close so the
	// channel is closed exactly once (by whichever fires first: capture
	// stop or connection end).
	clientCloseOnce := &sync.Once{}
	destCloseOnce := &sync.Once{}

	done := make(chan struct{}, 2)
	go func() {
		defer clientCloseOnce.Do(func() { close(clientCapChan) })
		tee := &captureTeeWriter{dest: destConn, ch: clientCapChan, closeOnce: clientCloseOnce}
		_, _ = io.Copy(tee, clientConn)
		done <- struct{}{}
	}()
	go func() {
		defer destCloseOnce.Do(func() { close(destCapChan) })
		tee := &captureTeeWriter{dest: clientConn, ch: destCapChan, closeOnce: destCloseOnce}
		_, _ = io.Copy(tee, destConn)
		done <- struct{}{}
	}()
	<-done
	<-done

	return nil
}

// forwardBidirectional does raw TCP passthrough without any capture.
func forwardBidirectional(clientConn, destConn net.Conn) error {
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		done <- struct{}{}
	}
	go cp(destConn, clientConn)
	go cp(clientConn, destConn)
	<-done
	<-done
	return nil
}

// createGenericMocksAsync reads captured data from both channels and creates
// mock entries based on request-response alternation. Runs in a background
// goroutine — never blocks the forwarding path.
func createGenericMocksAsync(ctx context.Context, logger *zap.Logger, clientCh, destCh <-chan []byte) {
	var genericRequests []models.Payload
	var genericResponses []models.Payload
	prevChunkWasReq := true // first chunk is always a request (initial reqBuf)
	reqTimestampMock := time.Now()
	var resTimestampMock time.Time

	flushMock := func() {
		if len(genericRequests) == 0 || len(genericResponses) == 0 {
			return
		}
		metadata := make(map[string]string)
		metadata["type"] = "config"
		if connID, ok := ctx.Value(models.ClientConnectionIDKey).(string); ok {
			metadata["connID"] = connID
		}
		mock := &models.Mock{
			Version: models.GetVersion(),
			Name:    "mocks",
			Kind:    models.GENERIC,
			Spec: models.MockSpec{
				GenericRequests:  genericRequests,
				GenericResponses: genericResponses,
				ReqTimestampMock: reqTimestampMock,
				ResTimestampMock: resTimestampMock,
				Metadata:         metadata,
			},
		}
		if mgr := syncMock.Get(); mgr != nil {
			mgr.AddMock(mock)
		}
		genericRequests = nil
		genericResponses = nil
	}

	for clientCh != nil || destCh != nil {
		select {
		case <-ctx.Done():
			flushMock()
			return
		case buf, ok := <-clientCh:
			if !ok {
				clientCh = nil
				continue
			}
			// New request after response — previous exchange complete.
			if !prevChunkWasReq && len(genericRequests) > 0 && len(genericResponses) > 0 {
				flushMock()
				reqTimestampMock = time.Now()
			}
			genericRequests = append(genericRequests, encodePayload(buf, models.FromClient))
			prevChunkWasReq = true

		case buf, ok := <-destCh:
			if !ok {
				destCh = nil
				continue
			}
			if prevChunkWasReq {
				reqTimestampMock = time.Now()
			}
			genericResponses = append(genericResponses, encodePayload(buf, models.FromServer))
			resTimestampMock = time.Now()
			prevChunkWasReq = false
		}
	}
	flushMock()
}

func encodePayload(buf []byte, origin models.OriginType) models.Payload {
	bufStr := string(buf)
	dataType := models.String
	if !util.IsASCII(string(buf)) {
		bufStr = util.EncodeBase64(buf)
		dataType = "binary"
	}
	return models.Payload{
		Origin: origin,
		Message: []models.OutputBinary{
			{
				Type: dataType,
				Data: bufStr,
			},
		},
	}
}
