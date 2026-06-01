// Same MITM proxy as ../mitm/main.go, but the hashmap is published via
// keploy's actual cbmap package — proving the integration end-to-end:
//
//   * the mapping format matches what the shim expects
//   * RFC 5929 hash-algo handling matches libpq's
//   * the path/permissions match what's wired in the webhook
//
// Run alongside the PG containers from ../pg-tls/. Verify that the shim
// (compiled by `make cbshim.so` in the parent dir) succeeds against this
// proxy the same way it did against the hand-rolled mitm/.
package main

import (
	"crypto/tls"
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"log"
	"net"

	"go.keploy.io/server/v3/pkg/agent/proxy/cbmap"
	"go.uber.org/zap"
)

const sslRequestCode uint32 = 80877103

var (
	listenAddr   = flag.String("listen", ":5432", "address to listen on")
	upstreamAddr = flag.String("upstream", "postgres:5432", "real Postgres host:port")
	mitmCert     = flag.String("cert", "/certs/mitm.crt", "MITM TLS cert")
	mitmKey      = flag.String("key", "/certs/mitm.key", "MITM TLS key")
)

func main() {
	flag.Parse()

	cert, err := tls.LoadX509KeyPair(*mitmCert, *mitmKey)
	if err != nil {
		log.Fatalf("load mitm cert/key: %v", err)
	}
	tlsConfig := &tls.Config{Certificates: []tls.Certificate{cert}}

	logger, _ := zap.NewDevelopment()
	log.Printf("MITM proxy (via keploy/cbmap) listening on %s, forwarding to %s",
		*listenAddr, *upstreamAddr)
	log.Printf("cbmap will be published to %s", cbmap.Path())

	ln, err := net.Listen("tcp", *listenAddr)
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	for {
		c, err := ln.Accept()
		if err != nil {
			log.Printf("accept: %v", err)
			continue
		}
		go func(clientConn net.Conn) {
			defer clientConn.Close()
			if err := handleConn(clientConn, tlsConfig, &cert, logger); err != nil {
				log.Printf("conn from %s ended: %v", clientConn.RemoteAddr(), err)
			}
		}(c)
	}
}

func handleConn(clientConn net.Conn, tlsCfg *tls.Config, mitmCert *tls.Certificate, logger *zap.Logger) error {
	var hdr [8]byte
	if _, err := io.ReadFull(clientConn, hdr[:]); err != nil {
		return fmt.Errorf("read SSLRequest: %w", err)
	}
	if binary.BigEndian.Uint32(hdr[0:4]) != 8 || binary.BigEndian.Uint32(hdr[4:8]) != sslRequestCode {
		return fmt.Errorf("not an SSLRequest")
	}
	if _, err := clientConn.Write([]byte{'S'}); err != nil {
		return err
	}
	appTLS := tls.Server(clientConn, tlsCfg)
	if err := appTLS.Handshake(); err != nil {
		return fmt.Errorf("app-side TLS: %w", err)
	}
	defer appTLS.Close()

	rawUp, err := net.Dial("tcp", *upstreamAddr)
	if err != nil {
		return fmt.Errorf("dial upstream: %w", err)
	}
	defer rawUp.Close()
	if _, err := rawUp.Write(hdr[:]); err != nil {
		return err
	}
	var reply [1]byte
	if _, err := io.ReadFull(rawUp, reply[:]); err != nil {
		return err
	}
	if reply[0] != 'S' {
		return fmt.Errorf("upstream refused SSL: 0x%02x", reply[0])
	}
	upTLS := tls.Client(rawUp, &tls.Config{InsecureSkipVerify: true})
	if err := upTLS.Handshake(); err != nil {
		return fmt.Errorf("upstream TLS: %w", err)
	}
	defer upTLS.Close()

	state := upTLS.ConnectionState()
	if len(state.PeerCertificates) > 0 {
		realLeaf := state.PeerCertificates[0]
		if path, err := cbmap.Publish(logger, mitmCert.Certificate[0], realLeaf.Raw, realLeaf.SignatureAlgorithm); err != nil {
			log.Printf("cbmap.Publish failed: %v", err)
		} else {
			log.Printf("cbmap published to %s (subject=%s)", path, realLeaf.Subject)
		}
	}

	pipe(appTLS, upTLS)
	return nil
}

func pipe(a, b net.Conn) {
	done := make(chan struct{}, 2)
	go func() { io.Copy(a, b); done <- struct{}{} }()
	go func() { io.Copy(b, a); done <- struct{}{} }()
	<-done
	_ = a.Close()
	_ = b.Close()
	<-done
}
