package http

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"time"

	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/agent/proxy/relay"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// AnswerFault is the fault a served mock's answer ends in, for a mock whose
// own (Spec.Fault) it overrides, or nil for the mock's own. keploy enterprise
// sets one (SetAnswerFault) so the faults it writes on mocks are enacted the
// way a recorded one is (serveFault). keploy mock replay's summary names each
// served mock's own fault (Mock.FaultServed); a hook that serves another one
// names it in its own output.
type AnswerFault func(ctx context.Context, stub *models.Mock) *models.Fault

// SetAnswerFault sets the fault hook replay asks before it serves a mock's
// answer; nil leaves every mock its own fault.
func (h *HTTP) SetAnswerFault(f AnswerFault) {
	if f == nil {
		h.answerFaultHook.Store(nil)
		return
	}
	h.answerFaultHook.Store(&f)
}

// answerFault is the fault stub's answer ends in at this serve: the hook's,
// when it gives one, else the mock's own (Spec.Fault, nil for a whole answer).
// A hook cannot take a recorded fault away: its nil is the mock's own.
func (h *HTTP) answerFault(ctx context.Context, stub *models.Mock) *models.Fault {
	if hook := h.answerFaultHook.Load(); hook != nil {
		if f := (*hook)(ctx, stub); f != nil {
			return f
		}
	}
	return stub.Spec.Fault
}

// faultResetGrace bounds how long replay waits for the application to
// acknowledge the bytes sent before a reset (relay.ResetClientOnClose): as
// long as the relay waits at record.
const faultResetGrace = relay.DefaultHalfCloseGrace

// serveFault enacts fault, the way stub's call ended, on the application's
// connection, as the upstream gave it at record:
//   - reset: the head and BodyBytes of the body when the fault came after the
//     head, then a reset (SO_LINGER 0, once the application acknowledged
//     every byte, relay.ResetClientOnClose);
//   - eof: nothing, then the connection's close;
//   - truncate: the head as recorded, with its Content-Length, and BodyBytes
//     of the body, then the close;
//   - timeout: the head and BodyBytes when after the head, then nothing more,
//     the connection held until the application closes it or ctx ends: its
//     own client's timeout ends it, never a clock here;
//   - delay: the whole answer once Delay has passed, or nothing when ctx ends
//     first.
//
// It reports whether the call is over, the connection to be closed: every
// kind but a delay that ran its course, after which the caller serves the
// whole answer. A fault this keploy cannot enact, or a mock whose answer
// cannot carry it, is an error naming why, and nothing is served: never the
// head as a whole answer.
func (h *HTTP) serveFault(ctx context.Context, clientConn net.Conn, stub *models.Mock, fault *models.Fault) (bool, error) {
	if err := fault.Validate(); err != nil {
		return true, fmt.Errorf("http: mock %q: %w", stub.Name, err)
	}
	if fault.Kind == models.FaultDelay {
		held := time.NewTimer(fault.Delay)
		defer held.Stop()
		select {
		case <-held.C:
			h.Logger.Info("served a mock's fault", zap.String("mock", stub.Name), zap.String("fault", fault.String()))
			return false, nil
		case <-ctx.Done():
			return true, nil
		}
	}
	if fault.SendsHead() {
		head, err := h.faultHead(stub, fault)
		if err != nil {
			return true, err
		}
		if _, err := clientConn.Write(head); err != nil {
			if ctx.Err() != nil {
				return true, nil
			}
			return true, fmt.Errorf("http: write the head of mock %q, cut short: %w", stub.Name, err)
		}
	}
	h.Logger.Info("served a mock's fault", zap.String("mock", stub.Name), zap.String("fault", fault.String()))
	switch fault.Kind {
	case models.FaultReset:
		if !relay.ResetClientOnClose(ctx, clientConn, faultResetGrace) && ctx.Err() == nil {
			h.Logger.Warn("the mock's recorded reset was served as a close: the application did not acknowledge what it was sent before it, or this platform cannot reset the socket",
				zap.String("mock", stub.Name))
		}
	case models.FaultTimeout:
		holdUntilClosed(ctx, clientConn)
	}
	return true, nil
}

// faultHead is the wire bytes a fault after the head sends: stub's status
// line and headers as recorded, Content-Length and Transfer-Encoding as the
// upstream sent them (not worked out over the body, which is cut short), then
// the first BodyBytes of the body as it came on the wire: never compressed or
// rewritten here. A mock whose body holds fewer bytes than that, or a truncate
// whose head does not frame its body (no Content-Length, not chunked: the
// application would read the cut as the body's end), is refused. A recorded
// truncate always has one: an upstream's close ends a body its head did not
// frame whole.
func (h *HTTP) faultHead(stub *models.Mock, fault *models.Fault) ([]byte, error) {
	name := stub.Name
	loaded, err := withResponse(stub)
	if err != nil {
		return nil, err
	}
	resp := loaded.Spec.HTTPResp
	if resp == nil || resp.StatusCode == 0 {
		return nil, fmt.Errorf("http: mock %q has a fault after the head (%s) and no recorded head", name, fault)
	}
	if fault.BodyBytes > int64(len(resp.Body)) {
		return nil, fmt.Errorf("http: mock %q: its fault sends %d body bytes and its body holds %d", name, fault.BodyBytes, len(resp.Body))
	}
	protoMajor, protoMinor := 1, 1
	if loaded.Spec.HTTPReq != nil {
		protoMajor, protoMinor = loaded.Spec.HTTPReq.ProtoMajor, loaded.Spec.HTTPReq.ProtoMinor
	}
	header := pkg.ToWireHTTPHeader(resp.Header, resp.HeaderLineLengths)
	if fault.Kind == models.FaultTruncate && header.Get("Content-Length") == "" && header.Get("Transfer-Encoding") == "" {
		return nil, fmt.Errorf("http: mock %q: its answer is cut short (%s) and its head has no Content-Length or Transfer-Encoding, so the application would read the cut as the body's end; give the head the whole body's Content-Length", name, fault)
	}
	out := make([]byte, 0, 256+int(fault.BodyBytes))
	out = append(out, "HTTP/"...)
	out = strconv.AppendInt(out, int64(protoMajor), 10)
	out = append(out, '.')
	out = strconv.AppendInt(out, int64(protoMinor), 10)
	out = append(out, ' ')
	out = strconv.AppendInt(out, int64(resp.StatusCode), 10)
	out = append(out, ' ')
	out = append(out, reasonPhrase(resp)...)
	out = append(out, "\r\n"...)
	for key, values := range header {
		for _, value := range values {
			out = appendHeaderLine(out, key, value)
		}
	}
	out = append(out, "\r\n"...)
	return append(out, resp.Body[:fault.BodyBytes]...), nil
}

// holdUntilClosed reads, and drops, what the application sends on c until it
// closes c or ctx ends (decodeHTTP's InterruptOnDone ends the read then): a
// timeout answers nothing more, and it is the application's own client that
// gives up.
func holdUntilClosed(ctx context.Context, c net.Conn) {
	var buf [512]byte
	for ctx.Err() == nil {
		if _, err := c.Read(buf[:]); err != nil {
			return
		}
	}
}
