// Command app is the app of the stateful_prune lane (keploy: the prune keeps
// a stateful dependency's recordings whole). It runs in one of two roles:
//
//   - app counter: the dependency, on 127.0.0.1:9097, outside keploy. GET
//     /next answers {"n":N}, N counting up from 1: the same request every
//     time, a different answer each time.
//   - app tick: the app keploy records, on 127.0.0.1:8097. GET /tick answers
//     what GET /next answered; GET /healthz answers 200.
package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"sync/atomic"
)

func main() {
	role := ""
	if len(os.Args) > 1 {
		role = os.Args[1]
	}
	var err error
	switch role {
	case "counter":
		var n atomic.Int64
		http.HandleFunc("/next", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"n":%d}`, n.Add(1))
		})
		err = http.ListenAndServe("127.0.0.1:9097", nil)
	case "tick":
		http.HandleFunc("/healthz", func(http.ResponseWriter, *http.Request) {})
		http.HandleFunc("/tick", func(w http.ResponseWriter, _ *http.Request) {
			resp, err := http.Get("http://127.0.0.1:9097/next")
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadGateway)
				return
			}
			defer resp.Body.Close()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(resp.StatusCode)
			_, _ = io.Copy(w, resp.Body)
		})
		err = http.ListenAndServe("127.0.0.1:8097", nil)
	default:
		fmt.Fprintln(os.Stderr, "usage: app counter|tick")
		os.Exit(2)
	}
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
