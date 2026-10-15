// Command app is the app of the async_value_flip lane (keploy: async lanes,
// the value a poll reads changing mid-recording). It runs in one of two roles:
//
//   - app cfg: the config service, on 127.0.0.1:9098, outside keploy.
//     GET /config?watch=true answers {"value": the current value}, v1 at
//     first; GET /flip makes it v2; GET /served?value=v2 says how many watch
//     polls it has answered with v2 (counted once the answer is written).
//   - app svc: the app keploy records, on 127.0.0.1:8098. It polls the config
//     service in the background, one poll after another (pollEvery apart),
//     and keeps the last value read. GET /value answers that value once a
//     poll begun after the request arrived has returned, so the answer is
//     the value current when the request arrived, without a timing guess;
//     502 if that poll failed.
//     GET /healthz answers 200.
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"
)

const pollEvery = 100 * time.Millisecond

func main() {
	role := ""
	if len(os.Args) > 1 {
		role = os.Args[1]
	}
	switch role {
	case "cfg":
		cfg()
	case "svc":
		svc()
	default:
		fmt.Fprintln(os.Stderr, "usage: app cfg|svc")
		os.Exit(2)
	}
}

func cfg() {
	var (
		mu     sync.Mutex
		value  = "v1"
		served = map[string]int{}
	)
	http.HandleFunc("/config", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		v := value
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"value": v})
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		if r.URL.Query().Get("watch") == "true" {
			mu.Lock()
			served[v]++
			mu.Unlock()
		}
	})
	http.HandleFunc("/flip", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		value = "v2"
		mu.Unlock()
		fmt.Fprint(w, "v2")
	})
	http.HandleFunc("/served", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n := served[r.URL.Query().Get("value")]
		mu.Unlock()
		fmt.Fprint(w, n)
	})
	if err := http.ListenAndServe("127.0.0.1:9098", nil); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func svc() {
	var (
		mu   sync.Mutex
		cond = sync.NewCond(&mu)
		cur  string
		// begun counts the polls begun; done is the count when the last
		// one that returned began, and failed says it read no value.
		begun, done int
		failed      bool
	)
	poll := func() {
		mu.Lock()
		begun++
		n := begun
		mu.Unlock()
		v := ""
		if resp, err := http.Get("http://127.0.0.1:9098/config?watch=true"); err == nil {
			var c struct {
				Value string `json:"value"`
			}
			if json.NewDecoder(resp.Body).Decode(&c) == nil {
				v = c.Value
			}
			_ = resp.Body.Close()
		}
		mu.Lock()
		if v != "" {
			cur = v
		}
		failed = v == ""
		done = n
		cond.Broadcast()
		mu.Unlock()
	}
	go func() {
		for {
			poll()
			time.Sleep(pollEvery)
		}
	}()
	http.HandleFunc("/healthz", func(http.ResponseWriter, *http.Request) {})
	http.HandleFunc("/value", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		arrived := begun
		for done <= arrived {
			cond.Wait()
		}
		v, bad := cur, failed
		mu.Unlock()
		if bad {
			// cur is an older poll's value, or none, not the one current
			// when the request arrived.
			http.Error(w, "the config poll begun after this request failed", http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"value": v})
	})
	if err := http.ListenAndServe("127.0.0.1:8098", nil); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
