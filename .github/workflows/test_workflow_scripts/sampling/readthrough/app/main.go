package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"sync"
	"time"
)

type cacheStore struct {
	mu    sync.RWMutex
	items map[string][]byte
}

func newCacheStore() *cacheStore {
	return &cacheStore{items: make(map[string][]byte)}
}

func (c *cacheStore) get(k string) ([]byte, bool) {
	c.mu.RLock()
	v, ok := c.items[k]
	c.mu.RUnlock()
	return v, ok
}

func (c *cacheStore) set(k string, v []byte) {
	cp := append([]byte(nil), v...)
	c.mu.Lock()
	c.items[k] = cp
	c.mu.Unlock()
}

func fetchConfig(client *http.Client, upstreamURL, entity string) ([]byte, int, error) {
	reqURL := upstreamURL + "/config?entity=" + url.QueryEscape(entity)
	resp, err := client.Get(reqURL)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return body, resp.StatusCode, err
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "18082"
	}
	upstreamURL := os.Getenv("UPSTREAM_URL")
	if upstreamURL == "" {
		upstreamURL = "http://127.0.0.1:19091"
	}
	overrideEntity := os.Getenv("OVERRIDE_ENTITY")

	cache := newCacheStore()
	client := &http.Client{Timeout: 5 * time.Second}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	// /prime advances past StartupMockTestCaseWindow for the upstream host.
	mux.HandleFunc("/prime", func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("i")
		if id == "" {
			id = "0"
		}
		body, code, err := fetchConfig(client, upstreamURL, "prime-"+id)
		if err != nil || code != http.StatusOK {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = fmt.Fprintf(w, `{"error":"prime upstream miss","code":%d}`, code)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	})

	// /warm is excluded by record.filters (no test case recorded), fetches from
	// upstream outside any kept test window, and populates the in-process cache.
	mux.HandleFunc("/warm", func(w http.ResponseWriter, r *http.Request) {
		entity := r.URL.Query().Get("entity")
		if entity == "" {
			http.Error(w, "missing entity", http.StatusBadRequest)
			return
		}
		body, code, err := fetchConfig(client, upstreamURL, entity)
		if err != nil || code != http.StatusOK {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = fmt.Fprintf(w, `{"error":"warm upstream miss","code":%d}`, code)
			return
		}
		cache.set(entity, body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	})

	// /lookup returns the cached value without calling upstream when warm; on a
	// cold start it fetches from upstream (using OVERRIDE_ENTITY if set to test
	// that a different entity misses cleanly).
	mux.HandleFunc("/lookup", func(w http.ResponseWriter, r *http.Request) {
		entity := r.URL.Query().Get("entity")
		if entity == "" {
			http.Error(w, "missing entity", http.StatusBadRequest)
			return
		}
		if cached, ok := cache.get(entity); ok {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(cached)
			return
		}
		target := entity
		if overrideEntity != "" {
			target = overrideEntity
		}
		body, code, err := fetchConfig(client, upstreamURL, target)
		if err != nil || code != http.StatusOK {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = fmt.Fprintf(w, `{"error":"lookup upstream miss","target":%q,"code":%d}`, target, code)
			return
		}
		cache.set(entity, body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	})

	addr := "127.0.0.1:" + port
	log.Printf("app listening on %s (upstream=%s override=%q)", addr, upstreamURL, overrideEntity)
	log.Fatal(http.ListenAndServe(addr, mux))
}
