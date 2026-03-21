// Package main provides a multi-protocol test server for verifying Keploy network
// capture across HTTP, MySQL, Redis, and PostgreSQL protocols.
//
// Endpoints:
//
//	GET  /health            – liveness check (no external calls)
//	GET  /http-echo         – outgoing HTTP call to a local echo server
//	POST /mysql/insert      – insert a row into MySQL
//	GET  /mysql/select      – select rows from MySQL
//	POST /redis/set         – SET a key in Redis
//	GET  /redis/get?key=k   – GET a key from Redis
//	POST /postgres/insert   – insert a row into PostgreSQL
//	GET  /postgres/select   – select rows from PostgreSQL
//	GET  /all               – calls all protocols in sequence
package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/gomodule/redigo/redis"
	_ "github.com/lib/pq"
)

var (
	mysqlDB   *sql.DB
	pgDB      *sql.DB
	redisAddr string
	echoAddr  string
)

func main() {
	// Give services time to start when running under keploy
	time.Sleep(2 * time.Second)

	port := envOr("APP_PORT", "6789")
	echoAddr = envOr("ECHO_ADDR", "localhost:6790")
	redisAddr = envOr("REDIS_ADDR", "localhost:6379")
	mysqlDSN := envOr("MYSQL_DSN", "root:password@tcp(localhost:3306)/testdb")
	pgDSN := envOr("PG_DSN", "postgres://postgres:password@localhost:5432/testdb?sslmode=disable")

	// Start a local echo server for HTTP-to-HTTP testing
	go startEchoServer(echoAddr)

	// Connect MySQL
	var err error
	mysqlDB, err = sql.Open("mysql", mysqlDSN)
	if err != nil {
		log.Printf("MySQL connect error (non-fatal): %v", err)
	} else {
		mysqlDB.SetMaxOpenConns(2)
		mysqlDB.SetConnMaxLifetime(time.Minute)
		initMySQL()
	}

	// Connect PostgreSQL
	pgDB, err = sql.Open("postgres", pgDSN)
	if err != nil {
		log.Printf("PostgreSQL connect error (non-fatal): %v", err)
	} else {
		pgDB.SetMaxOpenConns(2)
		initPostgres()
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", handleHealth)
	mux.HandleFunc("/http-echo", handleHTTPEcho)
	mux.HandleFunc("/mysql/insert", handleMySQLInsert)
	mux.HandleFunc("/mysql/select", handleMySQLSelect)
	mux.HandleFunc("/redis/set", handleRedisSet)
	mux.HandleFunc("/redis/get", handleRedisGet)
	mux.HandleFunc("/postgres/insert", handlePGInsert)
	mux.HandleFunc("/postgres/select", handlePGSelect)
	mux.HandleFunc("/all", handleAll)

	log.Printf("Multi-protocol test server listening on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, mux))
}

// ── Echo Server (provides an HTTP target for outgoing HTTP calls) ──

func startEchoServer(addr string) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		body, _ := io.ReadAll(r.Body)
		resp := map[string]interface{}{
			"method":  r.Method,
			"path":    r.URL.Path,
			"headers": r.Header,
			"body":    string(body),
			"ts":      time.Now().Unix(),
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	})
	log.Printf("Echo server listening on %s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Printf("Echo server error: %v", err)
	}
}

// ── Handlers ──

func handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

func handleHTTPEcho(w http.ResponseWriter, r *http.Request) {
	msg := r.URL.Query().Get("msg")
	if msg == "" {
		msg = "hello-from-capture-test"
	}
	url := fmt.Sprintf("http://%s/echo?msg=%s", echoAddr, msg)
	resp, err := http.Get(url)
	if err != nil {
		writeJSON(w, 502, map[string]string{"error": err.Error()})
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(200)
	w.Write(body)
}

func handleMySQLInsert(w http.ResponseWriter, r *http.Request) {
	if mysqlDB == nil {
		writeJSON(w, 503, map[string]string{"error": "MySQL not connected"})
		return
	}
	var req struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	if req.Name == "" {
		req.Name = "test-key"
	}
	if req.Value == "" {
		req.Value = fmt.Sprintf("val-%d", time.Now().Unix())
	}
	result, err := mysqlDB.Exec("INSERT INTO kv_store (k, v) VALUES (?, ?)", req.Name, req.Value)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	id, _ := result.LastInsertId()
	writeJSON(w, 201, map[string]interface{}{"id": id, "name": req.Name, "value": req.Value})
}

func handleMySQLSelect(w http.ResponseWriter, _ *http.Request) {
	if mysqlDB == nil {
		writeJSON(w, 503, map[string]string{"error": "MySQL not connected"})
		return
	}
	rows, err := mysqlDB.Query("SELECT id, k, v FROM kv_store ORDER BY id DESC LIMIT 10")
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	defer rows.Close()
	var items []map[string]interface{}
	for rows.Next() {
		var id int
		var k, v string
		if err := rows.Scan(&id, &k, &v); err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		items = append(items, map[string]interface{}{"id": id, "name": k, "value": v})
	}
	if err := rows.Err(); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]interface{}{"items": items})
}

func handleRedisSet(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	if req.Key == "" {
		req.Key = "test-key"
	}
	if req.Value == "" {
		req.Value = fmt.Sprintf("val-%d", time.Now().Unix())
	}
	conn, err := redis.Dial("tcp", redisAddr)
	if err != nil {
		writeJSON(w, 503, map[string]string{"error": fmt.Sprintf("Redis connect: %v", err)})
		return
	}
	defer conn.Close()
	_, err = conn.Do("SET", req.Key, req.Value, "EX", 300)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ok", "key": req.Key})
}

func handleRedisGet(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("key")
	if key == "" {
		key = "test-key"
	}
	conn, err := redis.Dial("tcp", redisAddr)
	if err != nil {
		writeJSON(w, 503, map[string]string{"error": fmt.Sprintf("Redis connect: %v", err)})
		return
	}
	defer conn.Close()
	val, err := redis.String(conn.Do("GET", key))
	if err != nil {
		writeJSON(w, 404, map[string]string{"error": err.Error(), "key": key})
		return
	}
	writeJSON(w, 200, map[string]string{"key": key, "value": val})
}

func handlePGInsert(w http.ResponseWriter, r *http.Request) {
	if pgDB == nil {
		writeJSON(w, 503, map[string]string{"error": "PostgreSQL not connected"})
		return
	}
	var req struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	if req.Name == "" {
		req.Name = "pg-key"
	}
	if req.Value == "" {
		req.Value = fmt.Sprintf("pg-val-%d", time.Now().Unix())
	}
	var id int
	err := pgDB.QueryRow("INSERT INTO kv_store (k, v) VALUES ($1, $2) RETURNING id", req.Name, req.Value).Scan(&id)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 201, map[string]interface{}{"id": id, "name": req.Name, "value": req.Value})
}

func handlePGSelect(w http.ResponseWriter, _ *http.Request) {
	if pgDB == nil {
		writeJSON(w, 503, map[string]string{"error": "PostgreSQL not connected"})
		return
	}
	rows, err := pgDB.Query("SELECT id, k, v FROM kv_store ORDER BY id DESC LIMIT 10")
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	defer rows.Close()
	var items []map[string]interface{}
	for rows.Next() {
		var id int
		var k, v string
		if err := rows.Scan(&id, &k, &v); err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		items = append(items, map[string]interface{}{"id": id, "name": k, "value": v})
	}
	if err := rows.Err(); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]interface{}{"items": items})
}

// handleAll calls every protocol in sequence and aggregates the results.
func handleAll(w http.ResponseWriter, _ *http.Request) {
	results := map[string]interface{}{}

	// 1. HTTP echo
	resp, err := http.Get(fmt.Sprintf("http://%s/echo?msg=all-test", echoAddr))
	if err != nil {
		results["http"] = map[string]string{"error": err.Error()}
	} else {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		results["http"] = json.RawMessage(body)
	}

	// 2. MySQL
	if mysqlDB != nil {
		_, err := mysqlDB.Exec("INSERT INTO kv_store (k, v) VALUES (?, ?)", "all-test", "all-val")
		if err != nil {
			results["mysql_insert"] = map[string]string{"error": err.Error()}
		} else {
			results["mysql_insert"] = "ok"
		}
		var count int
		mysqlDB.QueryRow("SELECT COUNT(*) FROM kv_store").Scan(&count)
		results["mysql_count"] = count
	} else {
		results["mysql"] = "not connected"
	}

	// 3. Redis
	conn, err := redis.Dial("tcp", redisAddr)
	if err != nil {
		results["redis"] = map[string]string{"error": err.Error()}
	} else {
		conn.Do("SET", "all-test", "all-val", "EX", 60)
		val, _ := redis.String(conn.Do("GET", "all-test"))
		conn.Close()
		results["redis"] = val
	}

	// 4. PostgreSQL
	if pgDB != nil {
		_, err := pgDB.Exec("INSERT INTO kv_store (k, v) VALUES ($1, $2)", "all-test", "all-pg-val")
		if err != nil {
			results["postgres_insert"] = map[string]string{"error": err.Error()}
		} else {
			results["postgres_insert"] = "ok"
		}
		var count int
		pgDB.QueryRow("SELECT COUNT(*) FROM kv_store").Scan(&count)
		results["postgres_count"] = count
	} else {
		results["postgres"] = "not connected"
	}

	writeJSON(w, 200, results)
}

// ── Helpers ──

func initMySQL() {
	for i := 0; i < 10; i++ {
		if err := mysqlDB.Ping(); err == nil {
			break
		}
		time.Sleep(time.Second)
	}
	mysqlDB.Exec("CREATE TABLE IF NOT EXISTS kv_store (id INT AUTO_INCREMENT PRIMARY KEY, k VARCHAR(255), v TEXT)")
}

func initPostgres() {
	for i := 0; i < 10; i++ {
		if err := pgDB.Ping(); err == nil {
			break
		}
		time.Sleep(time.Second)
	}
	pgDB.Exec("CREATE TABLE IF NOT EXISTS kv_store (id SERIAL PRIMARY KEY, k VARCHAR(255), v TEXT)")
}

func writeJSON(w http.ResponseWriter, code int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(data)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
