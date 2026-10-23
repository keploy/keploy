// Command app is the redis-skip lane's application: an HTTP API over two
// Redis servers (go-redis), a memcached (its text protocol, by hand) and an
// echo server (../echo) it sends a RESP array naming no Redis command.
//
// OSS keploy records and mocks no Redis: it relays every Redis connection to
// the real server and says so once per destination. memcached and the echo
// server are TCP dependencies OSS keploy records through its Generic parser:
// memcached's text protocol ("get k\r\n") looks like an inline Redis command,
// and the echo server is sent what a Redis client sends but for its command
// name, so the lane checks on them that the Redis skip claims nothing else.
//
// Every answer is a function of the request alone, given the dependencies
// answer as they did when recorded: a replay with Redis live (and memcached
// mocked) answers each test as recorded.
package main

import (
	"bufio"
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

	"github.com/redis/go-redis/v9"
)

// The clients wait on Redis as long as it takes (no read or write timeout):
// keploy sits between the app and Redis, and a loaded runner must slow the
// lane down, never fail it.
var (
	redisA = redis.NewClient(&redis.Options{Addr: envOr("REDIS_A", "127.0.0.1:6379"), PoolSize: 8, ReadTimeout: -1, WriteTimeout: -1})
	redisB = redis.NewClient(&redis.Options{Addr: envOr("REDIS_B", "127.0.0.1:6380"), PoolSize: 8, ReadTimeout: -1, WriteTimeout: -1})
	cache  = envOr("CACHE_ADDR", "127.0.0.1:11211")
	echo   = envOr("ECHO_ADDR", "127.0.0.1:7379")
)

func main() {
	ctx := context.Background()
	// A pool's connections: four at once to each Redis, each opened (HELLO,
	// CLIENT SETINFO) and used before any is given back, so every Redis is
	// reached over four connections before the first request.
	for _, c := range []*redis.Client{redisA, redisB} {
		conns := make([]*redis.Conn, 4)
		for i := range conns {
			conns[i] = c.Conn()
			if err := conns[i].Ping(ctx).Err(); err != nil {
				log.Fatalf("cannot reach Redis at %s: %v", c.Options().Addr, err)
			}
		}
		for _, cn := range conns {
			_ = cn.Close()
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) { reply(w, map[string]string{"status": "ok"}) })
	mux.HandleFunc("/kv/", handleKV)
	mux.HandleFunc("/pipeline/", handlePipeline)
	mux.HandleFunc("/cache/", handleCache)
	mux.HandleFunc("/resp/", handleResp)
	srv := &http.Server{Addr: envOr("LISTEN_ADDR", ":8098"), Handler: mux}
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		sctx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer scancel()
		_ = srv.Shutdown(sctx)
	}()
	// Said once the port is bound, not before: the compose cell waits for
	// this line, and for keploy's agent to take the port over, before its
	// first request (../golang-compose-linux.sh).
	ln, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("listening on %s", srv.Addr)
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

// handleKV: PUT /kv/<key> stores the body in Redis A; GET /kv/<key> reads it.
func handleKV(w http.ResponseWriter, r *http.Request) {
	key := "kv:" + strings.TrimPrefix(r.URL.Path, "/kv/")
	switch r.Method {
	case http.MethodPut:
		body, _ := io.ReadAll(r.Body)
		if err := redisA.Set(r.Context(), key, string(body), 0).Err(); err != nil {
			http.Error(w, "redis: "+err.Error(), http.StatusBadGateway)
			return
		}
		reply(w, map[string]any{"key": key, "stored": true})
	case http.MethodGet:
		v, err := redisA.Get(r.Context(), key).Result()
		if err != nil {
			http.Error(w, "redis: "+err.Error(), http.StatusBadGateway)
			return
		}
		reply(w, map[string]any{"key": key, "value": v})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handlePipeline: GET /pipeline/<key> sends SET, GET and DEL of the key to
// Redis B in one pipeline (one write, three replies).
func handlePipeline(w http.ResponseWriter, r *http.Request) {
	key := "pipe:" + strings.TrimPrefix(r.URL.Path, "/pipeline/")
	var get *redis.StringCmd
	var del *redis.IntCmd
	_, err := redisB.Pipelined(r.Context(), func(p redis.Pipeliner) error {
		p.Set(r.Context(), key, "v-"+key, 0)
		get = p.Get(r.Context(), key)
		del = p.Del(r.Context(), key)
		return nil
	})
	if err != nil {
		http.Error(w, "redis: "+err.Error(), http.StatusBadGateway)
		return
	}
	reply(w, map[string]any{"key": key, "got": get.Val(), "deleted": del.Val()})
}

// handleCache: PUT /cache/<key> sets the body in memcached and in Redis A;
// GET reads both. A test's window so holds a Redis call beside the memcached
// one, and the Generic parser's pool, which the lane reads at replay, would
// list that Redis call's mock were it not left out.
func handleCache(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimPrefix(r.URL.Path, "/cache/")
	var req string
	var stored string
	switch r.Method {
	case http.MethodPut:
		body, _ := io.ReadAll(r.Body)
		req = fmt.Sprintf("set %s 0 0 %d\r\n%s\r\n", key, len(body), body)
		if err := redisA.Set(r.Context(), "cache:"+key, string(body), 0).Err(); err != nil {
			http.Error(w, "redis: "+err.Error(), http.StatusBadGateway)
			return
		}
		stored = string(body)
	case http.MethodGet:
		req = "get " + key + "\r\n"
		v, err := redisA.Get(r.Context(), "cache:"+key).Result()
		if err != nil {
			http.Error(w, "redis: "+err.Error(), http.StatusBadGateway)
			return
		}
		stored = v
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	lines, err := memcached(r.Context(), req)
	if err != nil {
		http.Error(w, "memcached: "+err.Error(), http.StatusBadGateway)
		return
	}
	reply(w, map[string]any{"key": key, "answer": lines, "redis": stored})
}

// handleResp: GET /resp/<key> sends the echo server a RESP command array whose
// command is NOPE, on a connection of its own, and answers with what came
// back.
func handleResp(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimPrefix(r.URL.Path, "/resp/")
	req := fmt.Sprintf("*2\r\n$4\r\nNOPE\r\n$%d\r\n%s\r\n", len(key), key)
	var d net.Dialer
	c, err := d.DialContext(r.Context(), "tcp", echo)
	if err != nil {
		http.Error(w, "echo: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer func() { _ = c.Close() }()
	if _, err := io.WriteString(c, req); err != nil {
		http.Error(w, "echo: "+err.Error(), http.StatusBadGateway)
		return
	}
	got := make([]byte, len(req))
	if _, err := io.ReadFull(c, got); err != nil {
		http.Error(w, "echo: "+err.Error(), http.StatusBadGateway)
		return
	}
	reply(w, map[string]any{"key": key, "echoed": string(got) == req})
}

// memcached sends one request on a connection of its own and returns the
// lines of its answer, up to STORED, END or an error line.
func memcached(ctx context.Context, req string) ([]string, error) {
	var d net.Dialer
	c, err := d.DialContext(ctx, "tcp", cache)
	if err != nil {
		return nil, err
	}
	defer func() { _ = c.Close() }()
	if _, err := io.WriteString(c, req); err != nil {
		return nil, err
	}
	br := bufio.NewReader(c)
	var lines []string
	for {
		l, err := br.ReadString('\n')
		if err != nil {
			return lines, err
		}
		l = strings.TrimRight(l, "\r\n")
		lines = append(lines, l)
		switch {
		case l == "STORED", l == "END", l == "NOT_STORED", strings.HasSuffix(l, "ERROR"):
			return lines, nil
		}
	}
}

func reply(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
