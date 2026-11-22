// A test application whose /fetch?key=K reads K from the store (store/) on
// one TCP connection it dials at its first fetch and keeps for every fetch
// after (a pooled connection), and answers with the value's length, its
// SHA-256 and its last bytes. /health answers at once and never reaches the
// store. The store's address is STORE (default 127.0.0.1:9097).
//
// A value read short, or not at all, is a 502 that says why: an answer
// recorded or replayed cut shows in the test's response.
package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	storeAddr = "127.0.0.1:9097"
	mu        sync.Mutex
	conn      net.Conn
	rd        *bufio.Reader
)

func fetch(key string) ([]byte, error) {
	mu.Lock()
	defer mu.Unlock()
	if conn == nil {
		c, err := net.DialTimeout("tcp", storeAddr, 5*time.Second)
		if err != nil {
			return nil, err
		}
		conn, rd = c, bufio.NewReader(c)
	}
	v, err := ask(key)
	if err != nil {
		conn.Close()
		conn, rd = nil, nil
	}
	return v, err
}

func ask(key string) ([]byte, error) {
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return nil, err
	}
	if _, err := fmt.Fprintf(conn, "FETCH %s\n", key); err != nil {
		return nil, err
	}
	head, err := rd.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("reading the header: %w", err)
	}
	n, err := strconv.Atoi(strings.TrimPrefix(strings.TrimSpace(head), "VAL "))
	if err != nil {
		return nil, fmt.Errorf("header %q: %w", head, err)
	}
	v := make([]byte, n)
	if got, err := io.ReadFull(rd, v); err != nil {
		return nil, fmt.Errorf("read %d of %d bytes: %w", got, n, err)
	}
	return v, nil
}

func main() {
	if a := os.Getenv("STORE"); a != "" {
		storeAddr = a
	}
	http.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "ok")
	})
	http.HandleFunc("/fetch", func(w http.ResponseWriter, r *http.Request) {
		key := r.URL.Query().Get("key")
		v, err := fetch(key)
		if err != nil {
			http.Error(w, "fetch "+key+": "+err.Error(), http.StatusBadGateway)
			return
		}
		sum := sha256.Sum256(v)
		tail := v[max(0, len(v)-16):]
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"key": key, "len": len(v), "sha256": hex.EncodeToString(sum[:]), "tail": string(tail),
		})
	})
	log.Fatal(http.ListenAndServe("127.0.0.1:8080", nil))
}
