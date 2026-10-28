// The pass-through-ports e2e's app: GET /n posts a metric to its telemetry
// service (TEL_URL), then answers its dependency's (DEP_URL) GET /n body with
// the telemetry call's status: {"dep":{"n":7},"telemetry":"200"}. With
// RAW_ADDR, it also sends "ping" to that plain TCP service, which is not HTTP,
// and adds its answer: {"dep":{"n":7},"telemetry":"200","raw":"pong:ping"}.
package main

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
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
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"dep":%s,"telemetry":"%s"%s}`, body, status, rawPart)
	})
	if err := http.ListenAndServe(":8094", nil); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
