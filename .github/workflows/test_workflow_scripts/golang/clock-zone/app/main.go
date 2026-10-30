// The app under test: GET /order charges a payments service with
// {"amount":100,"day":"2006-01-02","hour":"2006-01-02T15"}, its clock read
// as it calls, written as a wall clock of no zone in the zone the process
// started with (TZ), as a date column and an hour bucket are; then it
// answers {"order":"ok"} when the charge answered {"charged":true}, else
// {"order":"unpaid"}, so a test passes only where its charge was answered
// as recorded, and only the call shows what it wrote. SKEW_HOURS (default
// 0) moves the read by that many hours: a value that is no read of the
// clock at the call.
package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"
)

func main() {
	skew, _ := strconv.Atoi(os.Getenv("SKEW_HOURS"))
	http.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok")
	})
	http.HandleFunc("/order", func(w http.ResponseWriter, r *http.Request) {
		now := time.Now().Add(time.Duration(skew) * time.Hour)
		body := fmt.Sprintf(`{"amount":100,"day":%q,"hour":%q}`, now.Format("2006-01-02"), now.Format("2006-01-02T15"))
		paid := false
		if resp, err := http.Post(os.Getenv("PAYMENTS_URL")+"/charge", "application/json", bytes.NewBufferString(body)); err == nil {
			got, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			paid = resp.StatusCode == http.StatusOK && string(got) == `{"charged":true}`
		}
		if !paid {
			fmt.Fprint(w, `{"order":"unpaid"}`)
			return
		}
		fmt.Fprint(w, `{"order":"ok"}`)
	})
	_ = http.ListenAndServe(":8092", nil)
}
