// The app's dependency during recording only: replay serves its mocks. It
// listens on PAYMENTS_ADDR, 127.0.0.1:9098 by default (an app container
// reaches it on the Docker host's gateway, so the docker legs listen on
// every address).
package main

import (
	"fmt"
	"net/http"
	"os"
)

func main() {
	http.HandleFunc("/charge", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"charged":true}`)
	})
	addr := os.Getenv("PAYMENTS_ADDR")
	if addr == "" {
		addr = "127.0.0.1:9098"
	}
	_ = http.ListenAndServe(addr, nil)
}
