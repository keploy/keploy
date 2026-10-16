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
	if err := http.ListenAndServe(":8092", nil); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
