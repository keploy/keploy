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
//	GET  /pool          one prepared statement on a pool of eight
//	                    connections of its own (pool, below)
//	GET  /quote?qty=N   the rate, a count of the orders
//	GET  /orders/count  a count of the orders
//	GET  /healthz       nothing
//
// With SHOP_SETTINGS_BY_KEY=1 settings are read with one prepared statement,
// SELECT k, v FROM settings WHERE k = ?: at startup, before any request, the
// boot limit and then the boot retries, and on first use the tax rate. Each
// read checks that the row it got is its key's. Between post-orders-1 and the
// next request it reads the boot retries again: the recording lane signals
// it (SIGUSR1, to the pid the app writes to SHOP_PIDFILE) once post-orders-1
// is answered, and waits for the read before it sends the next request; with
// no signal (a replay) GET /profile makes the read first, before it checks the
// reads made so far, so a wrong startup read does not hide it. With
// SHOP_BETWEEN_ON_SIGNAL_ONLY=1 the read is made on the signal alone. A read
// that got another key's row is logged, and every request but GET /healthz is
// then answered 500, naming it.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"

	_ "github.com/go-sql-driver/mysql"
)

type app struct {
	db       *sql.DB
	upstream string
	byKey    bool // SHOP_SETTINGS_BY_KEY=1: settings read by settingByKey
	// SHOP_BETWEEN_ON_SIGNAL_ONLY=1: the between-tests read is made on
	// SIGUSR1 alone, never by GET /profile.
	betweenOnSignalOnly bool

	// wrongRead is the first settings read, by key, that got another key's
	// row (or failed); between is the read made between two tests.
	wrongMu   sync.Mutex
	wrongRead error
	between   sync.Once

	mu    sync.Mutex
	token string
	rate  int
}

func main() {
	dsn := getenv("SHOP_DSN", "root:password@tcp(127.0.0.1:3306)/shop")
	upstream := getenv("SHOP_UPSTREAM", "http://127.0.0.1:9096")
	byKey := os.Getenv("SHOP_SETTINGS_BY_KEY") == "1"

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
	schema := []string{
		`CREATE TABLE IF NOT EXISTS orders (id BIGINT AUTO_INCREMENT PRIMARY KEY, qty INT NOT NULL, total INT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS settings (k VARCHAR(32) PRIMARY KEY, v INT NOT NULL)`,
		`INSERT IGNORE INTO settings (k, v) VALUES ('tax_rate', 3)`,
	}
	if byKey {
		schema = append(schema, `INSERT IGNORE INTO settings (k, v) VALUES ('boot_limit', 7), ('boot_retries', 5)`)
	}
	for _, stmt := range schema {
		if _, err := setup.Exec(stmt); err != nil {
			log.Fatalf("schema: %v", err)
		}
	}
	a := &app{upstream: upstream, byKey: byKey, betweenOnSignalOnly: os.Getenv("SHOP_BETWEEN_ON_SIGNAL_ONLY") == "1"}
	if byKey {
		// Startup, before any request: two settings, by the statement the
		// tax rate is read with later.
		for _, key := range []string{"boot_limit", "boot_retries"} {
			v, err := setting(context.Background(), setup, key)
			if err != nil {
				a.wrong(fmt.Errorf("startup: %w", err))
				continue
			}
			log.Printf("startup setting %s = %d", key, v)
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
	a.db = db
	if byKey {
		if f := os.Getenv("SHOP_PIDFILE"); f != "" {
			if err := os.WriteFile(f, []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
				log.Fatalf("pid file: %v", err)
			}
		}
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGUSR1)
		go func() {
			for range sig {
				a.between.Do(a.betweenRead)
			}
		}()
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("POST /orders", a.checked(a.createOrder))
	mux.HandleFunc("GET /profile", a.betweenFirst(a.checked(a.profile)))
	mux.HandleFunc("GET /quote", a.checked(a.quote))
	mux.HandleFunc("GET /orders/count", a.checked(a.count))
	mux.HandleFunc("GET /pool", pool(dsn))

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
	if a.byKey {
		rate, err := setting(r.Context(), a.db, "tax_rate")
		if err != nil {
			return 0, err
		}
		a.rate = rate
		return a.rate, nil
	}
	if err := a.db.QueryRowContext(r.Context(), `SELECT v FROM settings WHERE k = 'tax_rate'`).Scan(&a.rate); err != nil {
		return 0, fmt.Errorf("read the tax rate: %w", err)
	}
	return a.rate, nil
}

// wrong records a settings read that got another key's row, or failed.
func (a *app) wrong(err error) {
	log.Printf("%v", err)
	a.wrongMu.Lock()
	defer a.wrongMu.Unlock()
	if a.wrongRead == nil {
		a.wrongRead = err
	}
}

func (a *app) wrongErr() error {
	a.wrongMu.Lock()
	defer a.wrongMu.Unlock()
	return a.wrongRead
}

// checked answers 500, naming it, once a settings read got another key's row.
func (a *app) checked(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := a.wrongErr(); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		h(w, r)
	}
}

// betweenFirst makes the between-tests read, if no signal made it (a replay),
// before h: before checked looks at the reads made so far, so the read is made,
// and logged, even after a wrong startup read.
func (a *app) betweenFirst(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.byKey && !a.betweenOnSignalOnly {
			a.between.Do(a.betweenRead)
		}
		h(w, r)
	}
}

// betweenRead reads the boot retries again, between two tests.
func (a *app) betweenRead() {
	v, err := setting(context.Background(), a.db, "boot_retries")
	if err != nil {
		a.wrong(fmt.Errorf("between tests: %w", err))
		return
	}
	log.Printf("between-tests setting boot_retries = %d", v)
}

// settingByKey reads one setting. With arguments the driver prepares it
// (COM_STMT_PREPARE, then COM_STMT_EXECUTE with the key, COM_STMT_CLOSE).
const settingByKey = `SELECT k, v FROM settings WHERE k = ?`

// setting is key's value, read by settingByKey on db: a row of another key
// is another read's answer, and an error.
func setting(ctx context.Context, db *sql.DB, key string) (int, error) {
	var (
		k string
		v int
	)
	if err := db.QueryRowContext(ctx, settingByKey, key).Scan(&k, &v); err != nil {
		return 0, fmt.Errorf("read the setting %s: %w", key, err)
	}
	if k != key {
		return 0, fmt.Errorf("read the setting %s: got the row of %s (value %d)", key, k, v)
	}
	return v, nil
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

// itemQuery is the statement /pool prepares; item is the row it must read
// for id, as itemRows seeds it. SHOP_ITEM_QUERY puts another statement in
// its place: the lane replays a recording with it, as an app whose query
// changed since it was recorded.
var itemQuery = getenv("SHOP_ITEM_QUERY", `SELECT id, name, price FROM items WHERE id = ?`)

func item(id int) string { return fmt.Sprintf("%d:item-%d:%d", id, id, id*100+7) }

// itemShift is added to the id the sequential read binds, not to the id
// whose row it must read (SHOP_ITEM_ID_SHIFT, 0 by default): the lane
// replays a recording with it, as an app that now sends another parameter
// to the statement it recorded. A replayer that answers such a read with a
// recording of the statement made with another parameter serves the row
// the app wants, so the test passes: a false green.
var itemShift, _ = strconv.Atoi(getenv("SHOP_ITEM_ID_SHIFT", "0"))

const poolConns = 8

// pool serves GET /pool: itemQuery, a prepared statement, on a pool of eight
// connections of its own (to dsn), opened by the request and closed with it,
// as an app over database/sql with a pool does:
//
//	seq       one connection prepares it once and executes it for ids 4,
//	          5, 6, one after the other;
//	distinct  eight connections each prepare it, then all execute it at
//	          once (a barrier releases them together), for ids 1 to 8;
//	same      the same, every connection for id 3.
//
// Every call must read its own row: the answer lists the row each call
// read, and a row that is not the id's (another call's answer) or a call
// that failed makes it 500 and names the call. So an answer served to the
// wrong call fails the test by its status and its body, whatever compares
// it.
func pool(dsn string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) { servePool(w, r, dsn) }
}

func servePool(w http.ResponseWriter, r *http.Request, dsn string) {
	ctx := r.Context()
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	defer db.Close()
	db.SetMaxOpenConns(poolConns)
	db.SetMaxIdleConns(poolConns)
	for _, stmt := range []string{
		`CREATE TABLE IF NOT EXISTS items (id INT PRIMARY KEY, name VARCHAR(32) NOT NULL, price INT NOT NULL)`,
		itemRows(),
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "seed: " + err.Error()})
			return
		}
	}
	out := map[string][]string{}
	var wrong []string
	check := func(run string, ids []int, got []string, errs []error) {
		out[run] = got
		for i, id := range ids {
			switch {
			case errs[i] != nil:
				wrong = append(wrong, fmt.Sprintf("%s call %d (id %d) failed: %v", run, i, id, errs[i]))
			case got[i] != item(id):
				wrong = append(wrong, fmt.Sprintf("%s call %d (id %d) read %s, want %s", run, i, id, got[i], item(id)))
			}
		}
	}
	seqIDs := []int{4, 5, 6}
	got, errs := sequential(ctx, db, seqIDs)
	check("seq", seqIDs, got, errs)
	distinct := make([]int, poolConns)
	same := make([]int, poolConns)
	for i := range distinct {
		distinct[i], same[i] = i+1, 3
	}
	got, errs = together(ctx, db, distinct)
	check("distinct", distinct, got, errs)
	got, errs = together(ctx, db, same)
	check("same", same, got, errs)
	if len(wrong) > 0 {
		log.Printf("pool: %s", strings.Join(wrong, "; "))
		writeJSON(w, http.StatusInternalServerError, map[string]any{"wrong": wrong, "read": out})
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// itemRows is the INSERT that seeds ids 1 to 8.
func itemRows() string {
	vals := make([]string, poolConns)
	for i := range vals {
		id := i + 1
		vals[i] = fmt.Sprintf("(%d, 'item-%d', %d)", id, id, id*100+7)
	}
	return "INSERT IGNORE INTO items (id, name, price) VALUES " + strings.Join(vals, ", ")
}

func scanItem(row *sql.Row) (string, error) {
	var id, price int
	var name string
	if err := row.Scan(&id, &name, &price); err != nil {
		return "", err
	}
	return fmt.Sprintf("%d:%s:%d", id, name, price), nil
}

// sequential executes itemQuery for each id in turn, on one connection that
// prepares it once.
func sequential(ctx context.Context, db *sql.DB, ids []int) ([]string, []error) {
	got, errs := make([]string, len(ids)), make([]error, len(ids))
	fill := func(err error) ([]string, []error) {
		for i := range errs {
			errs[i] = err
		}
		return got, errs
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return fill(err)
	}
	defer conn.Close()
	stmt, err := conn.PrepareContext(ctx, itemQuery)
	if err != nil {
		return fill(err)
	}
	defer stmt.Close()
	for i, id := range ids {
		got[i], errs[i] = scanItem(stmt.QueryRowContext(ctx, id+itemShift))
	}
	return got, errs
}

// together takes one connection per id, all held at once (so each is a
// connection of its own), prepares itemQuery on each, and then, released
// together, executes it on every connection at once, each for its id.
func together(ctx context.Context, db *sql.DB, ids []int) ([]string, []error) {
	got, errs := make([]string, len(ids)), make([]error, len(ids))
	fill := func(err error) ([]string, []error) {
		for i := range errs {
			errs[i] = err
		}
		return got, errs
	}
	stmts := make([]*sql.Stmt, len(ids))
	for i := range ids {
		conn, err := db.Conn(ctx)
		if err != nil {
			return fill(err)
		}
		defer conn.Close()
		stmt, err := conn.PrepareContext(ctx, itemQuery)
		if err != nil {
			return fill(err)
		}
		defer stmt.Close()
		stmts[i] = stmt
	}
	start := make(chan struct{})
	var done sync.WaitGroup
	for i, id := range ids {
		done.Add(1)
		go func() {
			defer done.Done()
			<-start
			got[i], errs[i] = scanItem(stmts[i].QueryRowContext(ctx, id))
		}()
	}
	close(start)
	done.Wait()
	return got, errs
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
