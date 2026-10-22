// Command store is the streaming app's backing store for the
// mock-mismatch-streaming lane: an HTTP service holding a list of messages
// in memory. The app reaches it over HTTP, a dependency every keploy records
// and mocks (the lane used Redis before; Redis is recorded and mocked only
// by Keploy Enterprise now). The lane starts it outside keploy, as a
// dependency the app calls; at replay it is down and keploy answers for it.
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"
)

type message struct {
	ID       string `json:"id"`
	Text     string `json:"text"`
	Category string `json:"category"`
}

var (
	mu       sync.Mutex
	messages []message
	next     int
)

func main() {
	addr := os.Getenv("STORE_ADDR")
	if addr == "" {
		addr = "127.0.0.1:9097"
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/store/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("/store/messages", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			var m message
			if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
				http.Error(w, "invalid JSON body", http.StatusBadRequest)
				return
			}
			mu.Lock()
			next++
			m.ID = fmt.Sprintf("m%d", next)
			messages = append(messages, m)
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(m)
		case http.MethodDelete:
			mu.Lock()
			messages, next = nil, 0
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"cleared"}`))
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
	// The read every streaming endpoint makes: the lane rewrites this path in
	// the recorded mocks so that the replay's read matches none of them.
	mux.HandleFunc("/store/messages/list", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		out := append([]message{}, messages...)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	})
	log.Printf("store listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}
