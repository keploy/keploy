// Command upstream is the HTTP dependency of the lifetime_tags lane's app: an
// auth and configuration service whose answers never change.
//
//	GET  /config/boot   the app's startup configuration, read once at boot
//	POST /oauth/token   a token, which the app fetches once and caches
//	GET  /profile       the caller's profile, for a request with that token
//
// The token, which the app sends back in its Authorization header, and the
// profile's note are the application's content, which the lane checks keploy's
// logs never carry; its script spells them too.
package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
)

const (
	token = "kp-e2e-canary-auth-5d2f9a"
	note  = "kp-e2e-canary-body-8b3e17"
)

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /config/boot", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"region": "eu-west", "currency": "EUR"})
	})
	mux.HandleFunc("POST /oauth/token", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"access_token": token, "token_type": "Bearer"})
	})
	mux.HandleFunc("GET /profile", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "bad token"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"user": "ada", "plan": "pro", "note": note})
	})
	addr := os.Getenv("UPSTREAM_ADDR")
	if addr == "" {
		addr = "127.0.0.1:9096"
	}
	log.Printf("upstream listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
