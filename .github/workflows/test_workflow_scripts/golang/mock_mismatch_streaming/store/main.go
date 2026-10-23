// Command store is the streaming app's backing store for the
// mock-mismatch-streaming lane: an HTTP service holding a list of messages
// in memory, and a line-protocol cache (a memcached text-protocol subset)
// holding the SSE stream's banner. The app reaches the first over HTTP and
// the second over plain TCP, which keploy records and mocks through its
// Generic parser, the parser of every protocol keploy has no parser for (the
// lane used Redis before, through Generic; Redis is recorded and mocked only
// by Keploy Enterprise now). The lane starts it outside keploy, as a
// dependency the app calls; at replay it is down and keploy answers for it.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
)

// lines is what the line-protocol cache holds.
var lines = map[string]string{"banner": "keploy streams"}

// serveLines answers a connection's "get <key>" requests as memcached does,
// "VALUE <key> 0 <len>", the value and "END", or "END" alone for a key it does
// not hold, until the connection ends.
func serveLines(c net.Conn) {
	defer func() { _ = c.Close() }()
	r := bufio.NewReader(c)
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		key, ok := strings.CutPrefix(strings.TrimRight(line, "\r\n"), "get ")
		if !ok {
			_, _ = c.Write([]byte("ERROR\r\n"))
			continue
		}
		if v, held := lines[key]; held {
			_, _ = fmt.Fprintf(c, "VALUE %s 0 %d\r\n%s\r\nEND\r\n", key, len(v), v)
			continue
		}
		_, _ = c.Write([]byte("END\r\n"))
	}
}

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
	linesAddr := os.Getenv("LINES_ADDR")
	if linesAddr == "" {
		linesAddr = "127.0.0.1:9098"
	}
	ln, err := net.Listen("tcp", linesAddr)
	if err != nil {
		log.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go serveLines(c)
		}
	}()
	log.Printf("store listening on %s (lines on %s)", addr, linesAddr)
	log.Fatal(http.ListenAndServe(addr, mux))
}
