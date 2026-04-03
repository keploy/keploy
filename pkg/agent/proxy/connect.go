package proxy

import (
	"bufio"
	"bytes"
	"fmt"
	"net"
	"net/http"
	"strings"

	"go.keploy.io/server/v3/pkg/agent/proxy/tls"
	"go.keploy.io/server/v3/pkg/agent/proxy/util"
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
	// BufferedReader is the bufio.Reader used to parse the CONNECT request.
	// It may contain read-ahead bytes (e.g., the TLS ClientHello that the
	// client sent immediately after the CONNECT headers). Callers MUST use
	// this reader for subsequent reads from srcConn instead of creating a
	// new reader, to avoid losing these buffered bytes.
	BufferedReader *bufio.Reader
}

// isConnectRequest checks whether peeked bytes look like an HTTP CONNECT request.
func isConnectRequest(peeked []byte) bool {
	return len(peeked) >= 5 && bytes.HasPrefix(peeked, []byte("CONNE"))
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
	logger *zap.Logger,
	srcConn net.Conn,
	dstConn net.Conn, // nil in test mode
	isTestMode bool,
) (*connectTunnelResult, error) {
	reader := bufio.NewReader(srcConn)
	req, err := http.ReadRequest(reader)
	if err != nil {
		return nil, fmt.Errorf("failed to parse CONNECT request: %w", err)
	}
	defer req.Body.Close()

	if req.Method != "CONNECT" {
		return nil, fmt.Errorf("expected CONNECT method, got %s", req.Method)
	}

	targetAddr := req.Host
	if targetAddr == "" {
		targetAddr = req.URL.Host
	}
	if targetAddr == "" {
		return nil, fmt.Errorf("CONNECT request has no target host")
	}

	host, port, err := net.SplitHostPort(targetAddr)
	if err != nil {
		// SplitHostPort failed — likely no port specified.
		// Strip brackets from IPv6 addresses before joining to avoid
		// double-bracketing (e.g., "[::1]" → "[[::1]]:443").
		host = strings.TrimRight(strings.TrimLeft(targetAddr, "["), "]")
		port = "443"
		targetAddr = net.JoinHostPort(host, port)
	}

	logger.Debug("CONNECT tunnel detected",
		zap.String("target", targetAddr),
		zap.Bool("testMode", isTestMode),
	)

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

		if _, err := dstConn.Write(connectBuf.Bytes()); err != nil {
			return nil, fmt.Errorf("failed to forward CONNECT to proxy: %w", err)
		}

		proxyReader := bufio.NewReader(dstConn)
		resp, err := http.ReadResponse(proxyReader, req)
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
			resp.Body = nil
			resp.ContentLength = 0
			_ = resp.Write(srcConn)
			return nil, fmt.Errorf("corporate proxy rejected CONNECT with %d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
		}
		resp.Body.Close()

		if _, err := srcConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
			return nil, fmt.Errorf("failed to forward CONNECT 200 to app: %w", err)
		}
	}

	// Store the target hostname so TLS MITM can forge the right certificate.
	if tcpAddr, ok := srcConn.RemoteAddr().(*net.TCPAddr); ok {
		tls.SrcPortToDstURL.Store(tcpAddr.Port, host)
		logger.Debug("Stored CONNECT target in SrcPortToDstURL",
			zap.Int("srcPort", tcpAddr.Port),
			zap.String("host", host),
		)
	}

	return &connectTunnelResult{
		TargetHost:     host,
		TargetPort:     port,
		TargetAddr:     targetAddr,
		BufferedReader: reader,
	}, nil
}

// stripUtilConn extracts the underlying net.Conn from a util.Conn wrapper.
func stripUtilConn(conn net.Conn) net.Conn {
	if uc, ok := conn.(*util.Conn); ok {
		return uc.Conn
	}
	return conn
}
