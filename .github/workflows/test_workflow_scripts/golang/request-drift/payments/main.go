// The app's dependency during recording only: replay serves its mocks.
package main

import (
	"fmt"
	"net/http"
)

func main() {
	http.HandleFunc("/charge", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"charged":true}`)
	})
	_ = http.ListenAndServe("127.0.0.1:9099", nil)
}
