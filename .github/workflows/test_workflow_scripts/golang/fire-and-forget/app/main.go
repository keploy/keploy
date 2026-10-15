// A test application whose /a-publish answers 200 ms after it starts a
// publish to a broker that it does not wait for (fire and forget), with SUFFIX
// appended to the message; /b-other takes 3 s; /c-ping answers at once.
//
// PROTO says how it publishes:
//   - http: an HTTP POST on a connection of its own (no keep-alive);
//   - tcp: a line on a TCP connection of its own;
//   - httpkeep: an HTTP POST on a keep-alive connection dialled at startup,
//     first used then (GET /ready), and pooled since;
//   - tcpkeep: a line on a TCP connection dialled at startup, first used then
//     (a hello), and kept since.
//
// The kept connections speak at once, so the proxy never waits on them to
// tell their protocol, and each publish on them opens no connection: only its
// bytes reach the proxy.
package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptrace"
	"os"
	"strings"
	"sync"
	"time"
)

var (
	keepMu   sync.Mutex
	keepConn net.Conn
	keepR    *bufio.Reader
	preMu    sync.Mutex
	pre      net.Conn
	keepHTTP = &http.Client{Transport: &http.Transport{
		// The first request rides the connection dialled at startup.
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			preMu.Lock()
			c := pre
			pre = nil
			preMu.Unlock()
			if c != nil {
				return c, nil
			}
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		},
		MaxIdleConnsPerHost: 4,
	}}
)

func line(msg string, wrote func()) string {
	keepMu.Lock()
	defer keepMu.Unlock()
	fmt.Fprintf(keepConn, "%s\n", msg)
	wrote()
	l, err := keepR.ReadString('\n')
	return fmt.Sprintf("%q (%v)", l, err)
}

func post(c *http.Client, broker, msg string, wrote func()) string {
	req, err := http.NewRequest(http.MethodPost, "http://"+broker+"/publish", strings.NewReader(msg))
	if err != nil {
		return err.Error()
	}
	req.Header.Set("Content-Type", "text/plain")
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{WroteRequest: func(httptrace.WroteRequestInfo) { wrote() }}))
	resp, err := c.Do(req)
	if err != nil {
		return err.Error()
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return fmt.Sprintf("%q (%d)", b, resp.StatusCode)
}

// publish closes written once its bytes are in the kernel (or it failed).
func publish(broker, msg string, written chan<- struct{}) {
	var once sync.Once
	wrote := func() { once.Do(func() { close(written) }) }
	defer wrote()
	switch os.Getenv("PROTO") {
	case "tcpkeep":
		log.Printf("%s: broker said %s on the kept connection", msg, line(msg, wrote))
	case "httpkeep":
		log.Printf("%s: broker said %s on the pooled connection", msg, post(keepHTTP, broker, msg, wrote))
	case "tcp":
		c, err := net.Dial("tcp", broker)
		if err != nil {
			log.Printf("%s: dial: %v", msg, err)
			return
		}
		defer c.Close()
		fmt.Fprintf(c, "%s\n", msg)
		wrote()
		l, err := bufio.NewReader(c).ReadString('\n')
		log.Printf("%s: broker said %q (%v)", msg, l, err)
	default:
		// A connection of its own (no keep-alive), as a publisher that
		// connects per message does.
		c := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
		log.Printf("%s: broker said %s", msg, post(c, broker, msg, wrote))
	}
}

func main() {
	broker := os.Getenv("BROKER")
	switch os.Getenv("PROTO") {
	case "tcpkeep", "httpkeep":
		c, err := net.Dial("tcp", broker)
		if err != nil {
			log.Fatalf("dial the kept connection: %v", err)
		}
		if os.Getenv("PROTO") == "tcpkeep" {
			keepConn, keepR = c, bufio.NewReader(c)
			log.Printf("hello: broker said %s on the kept connection", line("hello", func() {}))
			break
		}
		pre = c
		resp, err := keepHTTP.Get("http://" + broker + "/ready")
		if err != nil {
			log.Fatalf("ready: %v", err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		log.Printf("ready: broker said %d on the pooled connection", resp.StatusCode)
	}
	pub := func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("id")
		written := make(chan struct{})
		go publish(broker, "publish "+id+os.Getenv("SUFFIX"), written)
		// Its bytes are in the kernel before the answer leaves, however late
		// the scheduler runs it; its reply is still not waited for.
		<-written
		// hold: the response waits this long after the publish began, so a
		// recording finds the publish inside the test.
		if d, err := time.ParseDuration(r.URL.Query().Get("hold")); err == nil {
			time.Sleep(d)
		}
		fmt.Fprintf(w, "queued %s\n", id)
	}
	http.HandleFunc("/a-publish", pub)
	http.HandleFunc("/b-other", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(3 * time.Second)
		fmt.Fprintln(w, "other")
	})
	http.HandleFunc("/c-ping", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "pong")
	})
	log.Fatal(http.ListenAndServe(":8080", nil))
}
