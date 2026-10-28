// The pass-through-ports e2e's app: GET /n posts a metric to its telemetry
// service (TEL_URL), then answers its dependency's (DEP_URL) GET /n body with
// the telemetry call's status: {"dep":{"n":7},"telemetry":"200"}.
package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

func main() {
	dep, tel := os.Getenv("DEP_URL"), os.Getenv("TEL_URL")
	http.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "ok") })
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
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"dep":%s,"telemetry":"%s"}`, body, status)
	})
	if err := http.ListenAndServe(":8094", nil); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
