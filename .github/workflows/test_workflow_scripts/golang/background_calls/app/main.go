// The application of the background_calls lane. It keeps API keys in a key
// service (../upstream, 127.0.0.1:9097) and stamps a key's use there, as an
// application stamps a key's last use on its own clock:
//
//	POST /keys/k1      creates the key (POST /keys);
//	POST /keys/k1/use  reads the key (GET /keys/k1), then stamps it STAMPS
//	                   times, one after the other, with the same request
//	                   (POST /keys/k1/stamp): a stamp on the application's
//	                   clock fires more or less often in a request's window.
//
// A stamp's answer never changes the use's answer, as a timer's write never
// changes a request's: the lane judges keploy's verdict on the stamps alone.
// The app logs "stamps per use: N" as it starts and "stamp: k1 stamped (i of
// N)" for each stamp the key service (or keploy, replaying) answered.
package main

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"
)

const keys = "http://127.0.0.1:9097"

var client = &http.Client{Timeout: 10 * time.Second}

// call makes one request of the key service and reads its whole answer.
func call(method, path string, body []byte) (int, []byte, error) {
	req, err := http.NewRequest(method, keys+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return resp.StatusCode, b, err
}

func main() {
	stamps, err := strconv.Atoi(os.Getenv("STAMPS"))
	if err != nil || stamps < 0 {
		log.Fatalf("STAMPS must be a count, got %q", os.Getenv("STAMPS"))
	}
	log.Printf("stamps per use: %d", stamps)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("POST /keys/k1", func(w http.ResponseWriter, r *http.Request) {
		code, _, err := call(http.MethodPost, "/keys", []byte(`{"key":"k1"}`))
		if err != nil || code != http.StatusCreated {
			http.Error(w, fmt.Sprintf("create k1: %d %v", code, err), http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"created":"k1"}`))
	})
	mux.HandleFunc("POST /keys/k1/use", func(w http.ResponseWriter, r *http.Request) {
		code, _, err := call(http.MethodGet, "/keys/k1", nil)
		if err != nil || code != http.StatusOK {
			http.Error(w, fmt.Sprintf("read k1: %d %v", code, err), http.StatusBadGateway)
			return
		}
		for i := 1; i <= stamps; i++ {
			code, _, err := call(http.MethodPost, "/keys/k1/stamp", []byte(`{"key":"k1"}`))
			if err != nil || code != http.StatusOK {
				log.Printf("stamp: k1 stamp %d of %d not answered: %d %v", i, stamps, code, err)
				continue
			}
			log.Printf("stamp: k1 stamped (%d of %d)", i, stamps)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"used":"k1"}`))
	})
	log.Fatal(http.ListenAndServe("127.0.0.1:8097", mux))
}
