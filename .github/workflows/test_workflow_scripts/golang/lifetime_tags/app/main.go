// Command shop is the app of the lifetime_tags lane: an HTTP API over MySQL
// that calls an HTTP auth service (../upstream).
//
// At startup it reads its configuration from the auth service (GET
// /config/boot) and creates its tables on a connection it then closes. Every
// request is served on a second pool of one connection, opened by the first
// request that needs it; the driver sets that connection up with the DSN's
// system variables (SET sql_mode, SET time_zone) as it opens it.
//
// Two things are fetched once and cached for the life of the process, as apps
// do: the auth token (POST /oauth/token) and the tax rate (SELECT from
// settings). Recorded in order, only the first request fetches them, so only
// its test records those calls; a later test that runs without it fetches
// them itself.
//
//	POST /orders        the token, the rate, an INSERT in a transaction, the
//	                    caller's profile
//	GET  /profile       the token, the caller's profile
//	GET  /quote?qty=N   the rate, a count of the orders
//	GET  /orders/count  a count of the orders
//	GET  /healthz       nothing
package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"sync"

	_ "github.com/go-sql-driver/mysql"
)

type app struct {
	db       *sql.DB
	upstream string

	mu    sync.Mutex
	token string
	rate  int
}

func main() {
	dsn := getenv("SHOP_DSN", "root:password@tcp(127.0.0.1:3306)/shop")
	upstream := getenv("SHOP_UPSTREAM", "http://127.0.0.1:9096")

	// Startup: the configuration the app runs with, from the auth service.
	var boot struct {
		Region string `json:"region"`
	}
	if err := call(http.MethodGet, upstream+"/config/boot", "", &boot); err != nil || boot.Region == "" {
		log.Fatalf("read the startup configuration: %v", err)
	}

	// Startup: the schema, on a connection of its own.
	setup, err := sql.Open("mysql", dsn)
	if err != nil {
		log.Fatalf("open: %v", err)
	}
	for _, stmt := range []string{
		`CREATE TABLE IF NOT EXISTS orders (id BIGINT AUTO_INCREMENT PRIMARY KEY, qty INT NOT NULL, total INT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS settings (k VARCHAR(32) PRIMARY KEY, v INT NOT NULL)`,
		`INSERT IGNORE INTO settings (k, v) VALUES ('tax_rate', 3)`,
	} {
		if _, err := setup.Exec(stmt); err != nil {
			log.Fatalf("schema: %v", err)
		}
	}
	_ = setup.Close()

	// Requests: one connection, opened by the first request that needs it,
	// set up by the driver with the DSN's system variables.
	db, err := sql.Open("mysql", dsn+"?sql_mode=%27STRICT_ALL_TABLES%27&time_zone=%27%2B00:00%27")
	if err != nil {
		log.Fatalf("open: %v", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	a := &app{db: db, upstream: upstream}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("POST /orders", a.createOrder)
	mux.HandleFunc("GET /profile", a.profile)
	mux.HandleFunc("GET /quote", a.quote)
	mux.HandleFunc("GET /orders/count", a.count)

	addr := getenv("SHOP_ADDR", "127.0.0.1:8096")
	log.Printf("shop listening on %s (region %s)", addr, boot.Region)
	log.Fatal(http.ListenAndServe(addr, mux))
}

// getToken is the auth token, fetched on first use.
func (a *app) getToken() (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.token != "" {
		return a.token, nil
	}
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	if err := call(http.MethodPost, a.upstream+"/oauth/token", "", &tok); err != nil {
		return "", fmt.Errorf("fetch the token: %w", err)
	}
	if tok.AccessToken == "" {
		return "", fmt.Errorf("fetch the token: no token in the answer")
	}
	a.token = tok.AccessToken
	return a.token, nil
}

// getRate is the tax rate, read on first use.
func (a *app) getRate(r *http.Request) (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.rate != 0 {
		return a.rate, nil
	}
	if err := a.db.QueryRowContext(r.Context(), `SELECT v FROM settings WHERE k = 'tax_rate'`).Scan(&a.rate); err != nil {
		return 0, fmt.Errorf("read the tax rate: %w", err)
	}
	return a.rate, nil
}

func (a *app) userOf(token string) (string, error) {
	var p struct {
		User string `json:"user"`
	}
	if err := call(http.MethodGet, a.upstream+"/profile", token, &p); err != nil {
		return "", fmt.Errorf("read the profile: %w", err)
	}
	return p.User, nil
}

func (a *app) createOrder(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Qty int `json:"qty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Qty <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad order"})
		return
	}
	token, err := a.getToken()
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	rate, err := a.getRate(r)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	// In a transaction (START TRANSACTION ... COMMIT on the wire), on the
	// pool's one connection: the rate is read first, as the transaction holds
	// the connection until it ends.
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	res, err := tx.ExecContext(r.Context(), `INSERT INTO orders (qty, total) VALUES (?, ?)`, in.Qty, in.Qty*rate)
	if err != nil {
		_ = tx.Rollback()
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	id, _ := res.LastInsertId()
	if err := tx.Commit(); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	user, err := a.userOf(token)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": id, "total": in.Qty * rate, "by": user})
}

func (a *app) profile(w http.ResponseWriter, _ *http.Request) {
	token, err := a.getToken()
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	user, err := a.userOf(token)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"user": user})
}

func (a *app) quote(w http.ResponseWriter, r *http.Request) {
	qty, err := strconv.Atoi(r.URL.Query().Get("qty"))
	if err != nil || qty <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad qty"})
		return
	}
	rate, err := a.getRate(r)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	var orders int
	if err := a.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM orders`).Scan(&orders); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"total": qty * rate, "orders": orders})
}

func (a *app) count(w http.ResponseWriter, r *http.Request) {
	var orders int
	if err := a.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM orders`).Scan(&orders); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"orders": orders})
}

// call sends a request to the auth service and decodes its JSON answer into
// out; an answer other than 200 is an error.
func call(method, url, token string, out any) error {
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		return err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s %s: %d %s", method, url, resp.StatusCode, body)
	}
	return json.Unmarshal(body, out)
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
