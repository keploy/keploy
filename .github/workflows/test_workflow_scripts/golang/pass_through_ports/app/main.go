// The pass-through-ports e2e's app: GET /n posts a metric to its telemetry
// service (TEL_URL), then answers its dependency's (DEP_URL) GET /n body with
// the telemetry call's status: {"dep":{"n":7},"telemetry":"200"}. With
// RAW_ADDR, it also sends "ping" to that plain TCP service, which is not HTTP,
// and adds its answer: {"dep":{"n":7},"telemetry":"200","raw":"pong:ping"}.
// With MTLS_ADDR, it also sends "ping" over TLS to that service, asking for
// the server MTLS_NAME, with the client certificate in MTLS_DIR and trusting
// only the server's own certificate there (a pin), and adds its answer:
// ..."mtls":"pong:ptp-client"}.
package main

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// rawCall sends "ping" to addr and returns its one-line answer. With greet, it
// first reads the line the service greets it with, as a client of a
// server-first protocol (SMTP, MySQL) waits to be greeted before it speaks.
func rawCall(addr string, greet bool) (string, error) {
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return "", err
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return "", err
	}
	r := bufio.NewReader(conn)
	if greet {
		if _, err := r.ReadString('\n'); err != nil {
			return "", err
		}
	}
	if _, err := io.WriteString(conn, "ping\n"); err != nil {
		return "", err
	}
	line, err := r.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

// mtlsCall sends "ping" over TLS to addr, asking for the server name, with the
// client certificate in dir (client.pem, client-key.pem), and trusting only
// the server certificate there (server.pem): keploy's MITM leaf is refused, and
// the server refuses a client without its certificate.
func mtlsCall(addr, name, dir string) (string, error) {
	cert, err := tls.LoadX509KeyPair(filepath.Join(dir, "client.pem"), filepath.Join(dir, "client-key.pem"))
	if err != nil {
		return "", err
	}
	pin, err := os.ReadFile(filepath.Join(dir, "server.pem"))
	if err != nil {
		return "", err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pin) {
		return "", errors.New("server.pem holds no certificate")
	}
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", addr,
		&tls.Config{ServerName: name, RootCAs: roots, Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	if err != nil {
		return "", err
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return "", err
	}
	if _, err := io.WriteString(conn, "ping\n"); err != nil {
		return "", err
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

func main() {
	dep, tel, raw := os.Getenv("DEP_URL"), os.Getenv("TEL_URL"), os.Getenv("RAW_ADDR")
	http.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprint(w, "ok") })
	http.HandleFunc("/n", func(w http.ResponseWriter, _ *http.Request) {
		status := "error"
		if resp, err := http.Post(tel+"/metrics", "application/json", strings.NewReader(`{"m":1}`)); err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			status = fmt.Sprint(resp.StatusCode)
		}
		resp, err := http.Get(dep + "/n")
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		rawPart := ""
		if raw != "" {
			line, err := rawCall(raw, os.Getenv("RAW_GREET") == "1")
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadGateway)
				return
			}
			rawPart = fmt.Sprintf(`,"raw":%q`, line)
		}
		if addr := os.Getenv("MTLS_ADDR"); addr != "" {
			line, err := mtlsCall(addr, os.Getenv("MTLS_NAME"), os.Getenv("MTLS_DIR"))
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadGateway)
				return
			}
			rawPart += fmt.Sprintf(`,"mtls":%q`, line)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"dep":%s,"telemetry":"%s"%s}`, body, status, rawPart)
	})
	if err := http.ListenAndServe(":8094", nil); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
