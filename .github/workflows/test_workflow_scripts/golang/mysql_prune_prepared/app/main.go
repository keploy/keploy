// Command orders is the app of the mysql_prune_prepared lane (keploy #4810).
//
// It talks to MySQL through database/sql and go-sql-driver/mysql with the
// driver's default DSN (no interpolateParams), so every parameterised
// statement goes over the wire as COM_STMT_PREPARE, COM_STMT_EXECUTE and
// COM_STMT_CLOSE. The pool holds one connection, so the same INSERT is
// prepared again on that connection for every order, each time under a new
// statement ID.
//
// POST /orders inserts an order keyed by its idempotency key. A second order
// with a key already used fails on the table's unique key (ERR 1062), and the
// handler answers 500 with the driver's error, as the app in the issue did.
// The INSERT binds the time it runs, so a replayed INSERT never carries the
// recorded parameters: only the statement's query tells its mocks apart.
//
// GET /orders/{key} reads an order back by its key.
package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
)

type order struct {
	Key  string `json:"key"`
	Item string `json:"item"`
	Qty  int    `json:"qty"`
}

func main() {
	dsn := os.Getenv("ORDERS_DSN")
	if dsn == "" {
		dsn = "root:password@tcp(127.0.0.1:3306)/orders?parseTime=true"
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		log.Fatalf("open: %v", err)
	}
	// One connection: every statement is prepared again on it.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	// No arguments: sent as COM_QUERY, never prepared.
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS orders (
		id BIGINT AUTO_INCREMENT PRIMARY KEY,
		idmp_key VARCHAR(64) NOT NULL,
		item VARCHAR(64) NOT NULL,
		qty INT NOT NULL,
		status VARCHAR(16) NOT NULL,
		created_at DATETIME(6) NOT NULL,
		UNIQUE KEY uq_orders_idmp (idmp_key))`); err != nil {
		log.Fatalf("create table: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("POST /orders", func(w http.ResponseWriter, r *http.Request) {
		var o order
		if err := json.NewDecoder(r.Body).Decode(&o); err != nil || o.Key == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad order"})
			return
		}
		_, err := db.ExecContext(r.Context(),
			"INSERT INTO orders (idmp_key, item, qty, status, created_at) VALUES (?, ?, ?, ?, ?)",
			o.Key, o.Item, o.Qty, "PENDING", time.Now().UTC())
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Failed to create order: " + err.Error()})
			return
		}
		writeJSON(w, http.StatusCreated, map[string]string{"key": o.Key, "status": "PENDING"})
	})
	mux.HandleFunc("GET /orders/{key}", func(w http.ResponseWriter, r *http.Request) {
		var o order
		var status string
		err := db.QueryRowContext(r.Context(),
			"SELECT idmp_key, item, qty, status FROM orders WHERE idmp_key = ?",
			r.PathValue("key")).Scan(&o.Key, &o.Item, &o.Qty, &status)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such order"})
		case err != nil:
			var me *mysql.MySQLError
			if errors.As(err, &me) {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": me.Error()})
				return
			}
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		default:
			writeJSON(w, http.StatusOK, map[string]any{"key": o.Key, "item": o.Item, "qty": o.Qty, "status": strings.ToUpper(status)})
		}
	})

	addr := os.Getenv("ORDERS_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8095"
	}
	log.Printf("orders listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
