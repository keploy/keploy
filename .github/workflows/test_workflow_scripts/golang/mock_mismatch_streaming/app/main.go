// Command app serves the streaming responses of the mock-mismatch-streaming
// lane (SSE, NDJSON, multipart and plain text), each built from what it reads
// from the store (../store) over HTTP; the SSE stream also carries a banner
// it reads from the store's line-protocol cache over plain TCP, which keploy
// records and mocks through its Generic parser. It is the samples-go
// sse-redis sample with its Redis replaced by that store.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
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

var (
	store = envOr("STORE_URL", "http://127.0.0.1:9097")
	cache = envOr("LINES_ADDR", "127.0.0.1:9098")
)

// readBanner reads the SSE stream's banner from the store's line-protocol
// cache: "get banner", answered "VALUE banner 0 <len>", the value and "END",
// on a connection of its own. A connection that ends before the answer (a
// replay whose recorded get matches nothing: keploy closes it) is an error.
func readBanner() (string, error) {
	c, err := net.Dial("tcp", cache)
	if err != nil {
		return "", err
	}
	defer func() { _ = c.Close() }()
	if _, err := c.Write([]byte("get banner\r\n")); err != nil {
		return "", err
	}
	r := bufio.NewReader(c)
	head, err := r.ReadString('\n')
	if err != nil {
		return "", err
	}
	var key string
	var flags, n int
	if _, err := fmt.Sscanf(head, "VALUE %s %d %d", &key, &flags, &n); err != nil {
		return "", fmt.Errorf("the cache answered %q: %w", head, err)
	}
	v := make([]byte, n+2)
	if _, err := io.ReadFull(r, v); err != nil {
		return "", err
	}
	if end, err := r.ReadString('\n'); err != nil || strings.TrimRight(end, "\r\n") != "END" {
		return "", fmt.Errorf("the cache did not end its answer: %q, %v", end, err)
	}
	return string(v[:n]), nil
}

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
	// The banner first, then the messages: a banner the cache does not
	// answer is said in the stream, and the messages are read all the same.
	banner, err := readBanner()
	if err != nil {
		log.Printf("banner read failed: %v", err)
		banner = "unavailable"
	}
	ms, flusher, ok := streamStart(w, r)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	ticker, _ := json.Marshal([]map[string]string{{"message_type": "TICKER", "banner": banner}})
	_, _ = fmt.Fprintf(w, "event:TICKER\ndata:%s\n\n", ticker)
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
