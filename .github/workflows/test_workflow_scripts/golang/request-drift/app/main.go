// The app under test: GET /order charges a payments service with
// {"amount": AMOUNT, "currency": "USD"} and answers {"order":"ok"} whatever
// the charge returns — it tolerates its dependency, so a changed request to
// it can never show up in the app's own response.
//
// The payments service is at PAYMENTS_URL, or, with PAYMENTS_URL_FILE, at the
// URL that file holds when a request comes: a dependency started after the
// app, as a test starts a Testcontainers container. GET /healthz answers 200
// once the app knows where it is, 503 before.
package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

func paymentsURL() string {
	if f := os.Getenv("PAYMENTS_URL_FILE"); f != "" {
		b, _ := os.ReadFile(f)
		return strings.TrimSpace(string(b))
	}
	return os.Getenv("PAYMENTS_URL")
}

func main() {
	http.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if paymentsURL() == "" {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		fmt.Fprint(w, "ok")
	})
	http.HandleFunc("/order", func(w http.ResponseWriter, r *http.Request) {
		body := fmt.Sprintf(`{"amount":%s,"currency":"USD"}`, os.Getenv("AMOUNT"))
		if resp, err := http.Post(paymentsURL()+"/charge", "application/json", bytes.NewBufferString(body)); err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
		fmt.Fprint(w, `{"order":"ok"}`)
	})
	_ = http.ListenAndServe(":8090", nil)
}
