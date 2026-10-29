// The key service the app (../app) calls: it creates a key, reads it and
// takes a stamp of its use. Every answer is the same bytes for the same
// request, so a recording of it replays as recorded.
package main

import (
	"log"
	"net/http"
)

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /keys", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"created":"k1"}`))
	})
	mux.HandleFunc("GET /keys/k1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"key":"k1"}`))
	})
	mux.HandleFunc("POST /keys/k1/stamp", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"stamped":"k1"}`))
	})
	log.Fatal(http.ListenAndServe("127.0.0.1:9097", mux))
}
