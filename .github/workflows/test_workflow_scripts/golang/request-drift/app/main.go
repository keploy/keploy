// The app under test: GET /order charges a payments service with
// {"amount": AMOUNT, "currency": "USD"} and answers {"order":"ok"} whatever
// the charge returns — it tolerates its dependency, so a changed request to
// it can never show up in the app's own response.
package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
)

func main() {
	payments := os.Getenv("PAYMENTS_URL")
	http.HandleFunc("/order", func(w http.ResponseWriter, r *http.Request) {
		body := fmt.Sprintf(`{"amount":%s,"currency":"USD"}`, os.Getenv("AMOUNT"))
		if resp, err := http.Post(payments+"/charge", "application/json", bytes.NewBufferString(body)); err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
		fmt.Fprint(w, `{"order":"ok"}`)
	})
	_ = http.ListenAndServe(":8090", nil)
}
