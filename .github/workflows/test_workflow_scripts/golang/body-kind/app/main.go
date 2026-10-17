// The app under test: GET /n asks its dependency (DEP_URL) for a count and
// answers its body, as JSON, with a 200 whatever the dependency answered, as
// a thin proxy or a BFF does. A replay whose dependency call gets no recorded
// answer therefore answers keploy's own plain-text "no matching mock" body in
// place of the recorded JSON, with the app's 200: only the body shows it.
package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
)

func main() {
	http.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "ok") })
	http.HandleFunc("/n", func(w http.ResponseWriter, r *http.Request) {
		var body []byte
		if resp, err := http.Get(os.Getenv("DEP_URL") + "/n"); err == nil {
			body, _ = io.ReadAll(resp.Body)
			_ = resp.Body.Close()
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	})
	// GET /t: the same for a text count, answered as text.
	http.HandleFunc("/t", func(w http.ResponseWriter, r *http.Request) {
		var body []byte
		if resp, err := http.Get(os.Getenv("DEP_URL") + "/t"); err == nil {
			body, _ = io.ReadAll(resp.Body)
			_ = resp.Body.Close()
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write(body)
	})
	// GET /upgrade: a 101 (Switching Protocols), then the protocol it switched
	// to on the connection (HTTP/2's client preface, as an h2c tunnel's server
	// sends it); a peer that does not speak it is given up on, and the
	// connection closed, as api-server's tunnel does.
	http.HandleFunc("/upgrade", func(w http.ResponseWriter, r *http.Request) {
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: keploy-e2e\r\n\r\n")
		_, _ = rw.WriteString("PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n")
		_ = rw.Flush()
	})
	if err := http.ListenAndServe(":8092", nil); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
