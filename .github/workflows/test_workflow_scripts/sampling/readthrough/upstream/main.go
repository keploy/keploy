package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
)

func main() {
	port := os.Getenv("UPSTREAM_PORT")
	if port == "" {
		port = "19091"
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/config", func(w http.ResponseWriter, r *http.Request) {
		entity := r.URL.Query().Get("entity")
		if entity == "" {
			http.Error(w, "missing entity", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"entity": entity,
			"owner":  "owner-" + entity,
		})
	})
	addr := "127.0.0.1:" + port
	log.Printf("upstream listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}
