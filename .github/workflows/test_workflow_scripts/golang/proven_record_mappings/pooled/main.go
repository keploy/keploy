// pooled is the application and the dependency of the pooled-connection step
// of the proven-record-mappings lane.
//
// pooled -broker ADDR is the dependency: an HTTP server that answers
// POST /publish with "ok <body>", and GET /conns with the number of
// connections it has accepted for anything but /conns.
//
// pooled -app ADDR -to BROKER is the application: at startup, before it
// serves anything, it opens one connection to the broker, as a connection
// pool warmed at startup does, and its first call to the broker goes over
// it. GET /publish?id=N publishes "publish N" to the broker and answers with
// what the broker said; GET /ping answers "pong".
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
)

func main() {
	broker := flag.String("broker", "", "serve the dependency at this address")
	app := flag.String("app", "", "serve the application at this address")
	to := flag.String("to", "", "the broker the application publishes to")
	flag.Parse()
	switch {
	case *broker != "":
		serveBroker(*broker)
	case *app != "" && *to != "":
		serveApp(*app, *to)
	default:
		log.Fatal("usage: pooled -broker ADDR | pooled -app ADDR -to BROKER")
	}
}

func serveBroker(addr string) {
	// conns counts every connection accepted; queries, the connections
	// that asked /conns (each curl opens one), which are left out.
	var conns, queries atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/publish", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		fmt.Fprintf(w, "ok %s", b)
	})
	mux.HandleFunc("/conns", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Connection", "close")
		fmt.Fprintf(w, "%d\n", conns.Load()-queries.Add(1))
	})
	srv := &http.Server{Addr: addr, Handler: mux, ConnState: func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			conns.Add(1)
		}
	}}
	log.Fatal(srv.ListenAndServe())
}

func serveApp(addr, broker string) {
	c, err := net.Dial("tcp", broker)
	if err != nil {
		log.Fatalf("dial the pooled connection: %v", err)
	}
	log.Printf("pooled connection %s -> %s opened at startup", c.LocalAddr(), c.RemoteAddr())
	var mu sync.Mutex
	pooled := c
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, a string) (net.Conn, error) {
			mu.Lock()
			pc := pooled
			pooled = nil
			mu.Unlock()
			if pc != nil {
				return pc, nil
			}
			var d net.Dialer
			return d.DialContext(ctx, network, a)
		},
	}}
	http.HandleFunc("/publish", func(w http.ResponseWriter, r *http.Request) {
		msg := "publish " + r.URL.Query().Get("id")
		resp, err := client.Post("http://"+broker+"/publish", "text/plain", strings.NewReader(msg))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		fmt.Fprintf(w, "%s\n", b)
	})
	http.HandleFunc("/ping", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "pong")
	})
	log.Fatal(http.ListenAndServe(addr, nil))
}
