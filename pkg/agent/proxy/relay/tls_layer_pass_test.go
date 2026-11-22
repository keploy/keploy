package relay

import (
	"crypto/tls"
	"net"
	"testing"

	"go.uber.org/zap"

	pTls "go.keploy.io/server/v3/pkg/agent/proxy/tls"
	"go.keploy.io/server/v3/pkg/agent/proxy/util"
)

// layerHolder stands in for the connection the proxy closes with care
// (pkg/agent/proxy's trackedConn).
type layerHolder struct {
	net.Conn
	held *tls.Conn
}

func (h *layerHolder) HoldTLSLayer(l *tls.Conn) { h.held = l }

// The relay's client-side TLS upgrade hands the handshake its own wrappers
// (prependingConn, with the bytes it stashed; readTrackingConn), which expose
// no NetConn: the layer the handshake serves still reaches the connection
// under them that the proxy closes with care, also under a util.SafeConn. It
// stopped at the first wrapper, and the proxy's close charged the client's
// unread close_notify as a loss.
func TestTheTLSLayerReachesThroughTheRelaysWrappers(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	h := &layerHolder{Conn: a}
	layer := tls.Server(b, &tls.Config{})
	for name, c := range map[string]net.Conn{
		"readTrackingConn":                 newReadTrackingConn(h, nil),
		"readTrackingConn, prependingConn": newReadTrackingConn(newPrependingConn(h, []byte("hello")), nil),
		"under a util.SafeConn":            newReadTrackingConn(util.NewSafeConn(h, zap.NewNop()), nil),
	} {
		h.held = nil
		pTls.PassTLSLayer(c, layer)
		if h.held != layer {
			t.Errorf("%s: the layer did not reach the connection under the relay's wrappers", name)
		}
	}
}
