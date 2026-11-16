package proxy

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"

	proxytls "go.keploy.io/server/v3/pkg/agent/proxy/tls"
	"go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// connectTunnelResult holds the state after handling a CONNECT tunnel handshake.
// After this, the connection has been transitioned: the CONNECT/200 exchange is
// consumed, and the caller can proceed as if this were a direct connection to
// the target host:port.
type connectTunnelResult struct {
	TargetHost string // e.g. "api.example.com"
	TargetPort string // e.g. "443"
	TargetAddr string // "host:port"
	// BufferedReader is the bufio.Reader used to parse the CONNECT request
	// from srcConn. It may contain read-ahead bytes (e.g., the TLS
	// ClientHello pipelined by the client). Callers MUST use this reader
	// for subsequent reads from srcConn.
	BufferedReader *bufio.Reader
	// DstReader is the bufio.Reader used to parse the corporate proxy's
	// 200 response on dstConn. It may hold read-ahead bytes from the
	// server side. Callers should wrap dstConn with this reader before
	// initiating the TLS handshake. Nil in test mode.
	DstReader *bufio.Reader
}

// connectRequestStart is how an HTTP CONNECT request begins: its method,
// exactly, and the space after it.
const connectRequestStart = "CONNECT "

// isConnectRequest reports whether peeked, the first bytes a client sent
// (at least 5), can begin an HTTP CONNECT request: each is the byte
// connectRequestStart has there. With fewer than its 8 they only may begin
// one (connectRequestAhead reads the rest); with 8 or more the method is
// CONNECT exactly, not one that only begins "CONNE".
func isConnectRequest(peeked []byte) bool {
	if len(peeked) < 5 {
		return false
	}
	n := min(len(peeked), len(connectRequestStart))
	return bytes.Equal(peeked[:n], []byte(connectRequestStart[:n]))
}

// connectRequestAhead reports whether r's next bytes begin an HTTP CONNECT
// request (connectRequestStart), peeked being the first of them the proxy
// has already peeked (5). Only where those may begin one does it peek the
// rest, which a client that sent them sends with them: its request line.
// A client that ends first sent no CONNECT.
func connectRequestAhead(r *bufio.Reader, peeked []byte) bool {
	if !isConnectRequest(peeked) {
		return false
	}
	start, err := r.Peek(len(connectRequestStart))
	return err == nil && isConnectRequest(start)
}

// handleConnectTunnel handles the HTTP CONNECT tunnel handshake.
//
// In RECORD mode: forwards CONNECT to the corporate proxy, relays the 200 back,
// and returns the target host:port so the caller can set up TLS MITM on the
// now-established tunnel.
//
// In TEST mode: sends a synthetic "200 Connection Established" directly to the
// app without contacting the corporate proxy, enabling fully isolated replay.
//
// After return, srcConn is positioned right after the CONNECT handshake — the
// next bytes will be a TLS ClientHello (or plaintext for non-TLS targets).
func handleConnectTunnel(
	ctx context.Context,
	logger *zap.Logger,
	srcConn net.Conn,
	dstConn net.Conn, // nil in test mode
	isTestMode bool,
) (*connectTunnelResult, error) {
	// A new bufio.Reader is intentionally created here even though srcConn
	// may already be wrapped with one. http.ReadRequest requires a
	// bufio.Reader, and we return this reader as BufferedReader so the
	// caller can reuse it for subsequent reads (preserving any bytes
	// read ahead past the CONNECT headers, like a pipelined TLS ClientHello).
	reader := bufio.NewReader(srcConn)
	req, err := http.ReadRequest(reader)
	if err != nil {
		// The parser's error quotes the head (a Proxy-Authorization
		// line among it): withheld.
		return nil, fmt.Errorf("failed to parse CONNECT request: %w", utils.WithholdInput(err))
	}
	defer req.Body.Close()

	// The method is the client's text (ReadRequest takes any token): named
	// by its size. The proxy sends here only a request that begins
	// "CONNECT " (connectRequestAhead).
	if req.Method != "CONNECT" {
		return nil, fmt.Errorf("expected the CONNECT method, got a %d-byte method that is not CONNECT", len(req.Method))
	}

	// For CONNECT, Go's http.ReadRequest puts the authority in different
	// fields depending on the request form. Try all sources.
	targetAddr := req.Host
	if targetAddr == "" {
		targetAddr = req.URL.Host
	}
	if targetAddr == "" {
		targetAddr = req.RequestURI
	}
	if targetAddr == "" {
		return nil, fmt.Errorf("CONNECT request has no target host")
	}

	host, port, err := net.SplitHostPort(targetAddr)
	if err != nil {
		// SplitHostPort failed — no port in address. Default to 443.
		// Strip a single pair of IPv6 brackets to avoid double-bracketing.
		host = targetAddr
		if len(host) >= 2 && host[0] == '[' && host[len(host)-1] == ']' {
			host = host[1 : len(host)-1]
		}
		port = "443"
		targetAddr = net.JoinHostPort(host, port)
	}
	// Reject an empty host (e.g. ":443"). net.SplitHostPort accepts it and
	// http.ReadRequest accepts `CONNECT :443 HTTP/1.1`, so without this check
	// an authority with no host at all flows on as a real target: it names
	// the connection's MITM leaf (a cert with an empty CN), it overwrites
	// dstAddr with ":443", and with upstream verification on it leaves
	// keploy with no name to verify against. There is no destination in a
	// host-less CONNECT — reject it the same way the empty port below is
	// rejected.
	if host == "" {
		return nil, fmt.Errorf("empty host in CONNECT target %s", models.HostLogName(ctx, targetAddr))
	}
	// Reject empty port (e.g., "example.com:"), non-numeric, out-of-range.
	if port == "" {
		return nil, fmt.Errorf("empty port in CONNECT target %s", models.HostLogName(ctx, targetAddr))
	}
	// The port is not quoted: for an origin-form CONNECT (`CONNECT /x`)
	// Go takes the authority from the Host header, whose port it never
	// checks, so it is any text the client sent; the proxy logs this error
	// at ERROR. The target's shape writes a port that is no number {}.
	portNum, parseErr := strconv.ParseUint(port, 10, 16)
	if parseErr != nil || portNum == 0 {
		return nil, fmt.Errorf("invalid port in CONNECT target %s: a %d-byte value that is no port from 1 to 65535", models.HostLogName(ctx, targetAddr), len(port))
	}

	logger.Debug("CONNECT tunnel detected",
		zap.String("target", models.HostLogName(ctx, targetAddr)),
		zap.Bool("testMode", isTestMode),
	)

	var proxyReader *bufio.Reader
	if isTestMode {
		if _, err := srcConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
			return nil, fmt.Errorf("failed to send CONNECT 200 to app: %w", err)
		}
	} else {
		if dstConn == nil {
			return nil, fmt.Errorf("dstConn is nil in record mode — cannot forward CONNECT")
		}

		// Reconstruct and forward the CONNECT request to the corporate proxy.
		var connectBuf bytes.Buffer
		fmt.Fprintf(&connectBuf, "CONNECT %s HTTP/1.1\r\n", targetAddr)
		fmt.Fprintf(&connectBuf, "Host: %s\r\n", targetAddr)
		for key, vals := range req.Header {
			// Skip Host — already written above to avoid duplicates.
			if key == "Host" {
				continue
			}
			for _, v := range vals {
				fmt.Fprintf(&connectBuf, "%s: %s\r\n", key, v)
			}
		}
		connectBuf.WriteString("\r\n")

		if _, err := io.Copy(dstConn, &connectBuf); err != nil {
			return nil, fmt.Errorf("failed to forward CONNECT to proxy: %w", err)
		}

		proxyReader = bufio.NewReader(dstConn)
		// The proxy's final response: a client takes any interim (1xx) ones
		// before it (RFC 9110 15.2).
		resp, err := util.ReadFinalResponse(proxyReader, req, nil)
		if err != nil {
			return nil, fmt.Errorf("failed to read CONNECT response from proxy: %w", err)
		}

		if resp.StatusCode != http.StatusOK {
			// Forward the proxy's error response (status + headers) to the
			// client. Note: multi-round-trip proxy auth (407 → credentials →
			// retry on the same connection) is not supported because Keploy's
			// transparent proxy processes each intercepted connection as a
			// single CONNECT attempt. Clients that need proxy auth should
			// include credentials in the initial CONNECT request.
			resp.Body.Close()
			resp.Body = http.NoBody
			resp.ContentLength = 0
			if writeErr := resp.Write(srcConn); writeErr != nil {
				logger.Debug("failed to forward proxy error response to client", zap.Error(writeErr))
			}
			return nil, fmt.Errorf("corporate proxy rejected CONNECT with %d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
		}
		// Do NOT close resp.Body here. For a CONNECT 200, the "body" is the
		// start of the tunneled byte stream. Closing it would drain/consume
		// bytes needed for the subsequent TLS handshake over the tunnel.

		// Send a clean 200 to the app. CONNECT tunnel 200 responses have no
		// meaningful headers per RFC 7231 §4.3.6, so a synthetic response
		// is correct and avoids Transfer-Encoding issues with resp.Write.
		if _, err := srcConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
			return nil, fmt.Errorf("failed to forward CONNECT 200 to app: %w", err)
		}
	}

	// TargetHost is what the TLS MITM names its leaf for when the client's
	// ClientHello carries no SNI: handleConnection hands it to the handshake
	// (pTls.WithConnectTarget), on this connection alone. It is filed under
	// the client's source port too, as it always was, only for the readers
	// of SrcPortToDstURL outside this module (see its doc).
	if tcpAddr, ok := srcConn.RemoteAddr().(*net.TCPAddr); ok {
		proxytls.SrcPortToDstURL.Store(tcpAddr.Port, host)
		logger.Debug("Stored CONNECT target in SrcPortToDstURL",
			zap.Int("srcPort", tcpAddr.Port),
			zap.String("host", models.HostLogName(ctx, host)),
		)
	}

	result := &connectTunnelResult{
		TargetHost:     host,
		TargetPort:     port,
		TargetAddr:     targetAddr,
		BufferedReader: reader,
	}

	// In record mode, the proxyReader (bufio.Reader on dstConn) may have
	// buffered bytes beyond the 200 response (e.g., server greeting). Wrap
	// dstConn so those bytes aren't lost during the subsequent TLS handshake.
	if !isTestMode && dstConn != nil {
		result.DstReader = proxyReader
	}

	return result, nil
}

// stripUtilConn extracts the underlying net.Conn from a util.Conn wrapper.
func stripUtilConn(conn net.Conn) net.Conn {
	if uc, ok := conn.(*util.Conn); ok {
		return uc.Conn
	}
	return conn
}
