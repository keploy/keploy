// Command app serves the streaming responses of the mock-mismatch-streaming
// lane (SSE, NDJSON, multipart and plain text), each built from what it reads
// from the store (../store) over HTTP. It is the samples-go sse-redis sample
// with its Redis replaced by that store.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

const (
	flushDelay = 120 * time.Millisecond
	boundary   = "keploy-stream-boundary"
)

var store = envOr("STORE_URL", "http://127.0.0.1:9097")

type message struct {
	ID       string `json:"id"`
	Text     string `json:"text"`
	Category string `json:"category"`
}

func main() {
	listen := envOr("LISTEN_ADDR", ":8097")
	// The store must answer before the app serves: its first call, recorded
	// before any test.
	if _, err := call(context.Background(), http.MethodGet, "/store/health", nil); err != nil {
		log.Fatalf("cannot reach the store at %s: %v", store, err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/messages", routeMessages)
	mux.HandleFunc("/events/sse", handleSSE)
	mux.HandleFunc("/events/ndjson", handleNDJSON)
	mux.HandleFunc("/events/multipart", handleMultipart)
	mux.HandleFunc("/events/plain", handlePlain)
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	srv := &http.Server{Addr: listen, Handler: mux}
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()
	log.Printf("listening on %s (store %s)", listen, store)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

// call makes one request of the store and returns its body, or an error for
// a failed call or a non-2xx answer.
func call(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, store+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = res.Body.Close() }()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}
	if res.StatusCode/100 != 2 {
		return nil, fmt.Errorf("store answered %d: %s", res.StatusCode, b)
	}
	return b, nil
}

func routeMessages(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		out, err := call(r.Context(), http.MethodPost, "/store/messages", body)
		if err != nil {
			http.Error(w, "store write failed: "+err.Error(), http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(out)
	case http.MethodDelete:
		out, err := call(r.Context(), http.MethodDelete, "/store/messages", nil)
		if err != nil {
			http.Error(w, "store delete failed: "+err.Error(), http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(out)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// fetch is the read every streaming endpoint makes.
func fetch(ctx context.Context) ([]message, error) {
	b, err := call(ctx, http.MethodGet, "/store/messages/list", nil)
	if err != nil {
		return nil, err
	}
	var ms []message
	if err := json.Unmarshal(b, &ms); err != nil {
		return nil, err
	}
	return ms, nil
}

// streamStart reads the messages and readies w to stream them, or answers
// the error itself and returns false.
func streamStart(w http.ResponseWriter, r *http.Request) ([]message, http.Flusher, bool) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return nil, nil, false
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return nil, nil, false
	}
	ms, err := fetch(r.Context())
	if err != nil {
		http.Error(w, "store read failed: "+err.Error(), http.StatusBadGateway)
		return nil, nil, false
	}
	return ms, flusher, true
}

func handleSSE(w http.ResponseWriter, r *http.Request) {
	ms, flusher, ok := streamStart(w, r)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	_, _ = fmt.Fprint(w, "event:TICKER\ndata:[{\"message_type\":\"TICKER\"}]\n\n")
	flusher.Flush()
	for _, m := range ms {
		data, _ := json.Marshal([]map[string]any{{"message_id": m.ID, "message_type": "SYSTEM", "title": m.Text, "category": m.Category}})
		_, _ = fmt.Fprintf(w, "event:message\ndata:%s\n\n", data)
		flusher.Flush()
		time.Sleep(flushDelay)
	}
	_, _ = fmt.Fprint(w, "event:message\ndata:[{\"message_subtype\":\"CLOSE_SSE_CONNECTION\"}]\n\n")
	flusher.Flush()
}

func handleNDJSON(w http.ResponseWriter, r *http.Request) {
	ms, flusher, ok := streamStart(w, r)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	for i, m := range ms {
		data, _ := json.Marshal(m)
		_, _ = w.Write(append(data, '\n'))
		flusher.Flush()
		if i < len(ms)-1 {
			time.Sleep(flushDelay)
		}
	}
}

func handleMultipart(w http.ResponseWriter, r *http.Request) {
	ms, flusher, ok := streamStart(w, r)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "multipart/x-mixed-replace; boundary="+boundary)
	for i, m := range ms {
		data, _ := json.Marshal(m)
		_, _ = fmt.Fprintf(w, "--%s\r\nContent-Type: application/json\r\n\r\n%s\r\n", boundary, data)
		flusher.Flush()
		if i < len(ms)-1 {
			time.Sleep(flushDelay)
		}
	}
	_, _ = fmt.Fprintf(w, "--%s--\r\n", boundary)
	flusher.Flush()
}

func handlePlain(w http.ResponseWriter, r *http.Request) {
	ms, flusher, ok := streamStart(w, r)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	for i, m := range ms {
		_, _ = fmt.Fprintf(w, "[%s] %s: %s\n", strings.ToUpper(m.Category), m.ID, m.Text)
		flusher.Flush()
		if i < len(ms)-1 {
			time.Sleep(flushDelay)
		}
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
