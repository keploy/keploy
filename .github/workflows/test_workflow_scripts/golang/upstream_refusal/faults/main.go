// faults is the application and the upstream of the upstream-faults e2e.
//
// faults -upstream ADDR is an HTTP upstream that answers each path with one
// fault, on a raw socket so that it can give faults net/http never writes:
//
//	/ok        a whole 200
//	/reset     a reset (SO_LINGER 0) before any answer
//	/eof       a close with no answer
//	/hang      no answer; it holds the connection until the client closes it
//	/truncate  a 200 whose Content-Length is 20, then 10 body bytes and a close
//	/reason    a 402 with the reason phrase "Pay Up Now"
//
// faults -app ADDR UPSTREAM is the application: GET /call/SHAPE calls
// UPSTREAM's /SHAPE on a new connection, with a 2s client timeout, and
// answers 200 with what its own error handling saw, as JSON (saw reset, eof
// or timeout; else the status line and how much of the body it read). The e2e
// asserts these answers are the same without keploy, under keploy record,
// and under keploy test with the upstream gone.
//
// faults -get URL is a client that gives up on URL by its own 1s timeout and
// exits at once after it gave up, as a test's worker process that ends right
// after its call failed.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

func upstream(addr string) {
	l, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Println("listen:", err)
		os.Exit(1)
	}
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		go answer(c.(*net.TCPConn))
	}
}

func answer(c *net.TCPConn) {
	defer c.Close()
	req, err := http.ReadRequest(bufio.NewReader(c))
	if err != nil {
		return
	}
	switch req.URL.Path {
	case "/ok":
		_, _ = c.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok"))
	case "/reset":
		_ = c.SetLinger(0)
	case "/eof":
	case "/hang":
		// Held until the client closes: its read then ends.
		_, _ = io.Copy(io.Discard, c)
	case "/held":
		// A long poll's: held until the client closes, said as it begins,
		// so a stop can be sent while it is in flight.
		fmt.Println("upstream: holding /held")
		_, _ = io.Copy(io.Discard, c)
	case "/truncate":
		_, _ = c.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 20\r\nConnection: close\r\n\r\n0123456789"))
	case "/reason":
		_, _ = c.Write([]byte("HTTP/1.1 402 Pay Up Now\r\nContent-Length: 4\r\nConnection: close\r\n\r\npay!"))
	default:
		_, _ = c.Write([]byte("HTTP/1.1 404 Not Found\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"))
	}
}

// saw is what the application's error handling makes of err.
func saw(err error) string {
	var ne net.Error
	switch {
	case errors.As(err, &ne) && ne.Timeout():
		return "timeout"
	case errors.Is(err, syscall.ECONNRESET):
		return "reset"
	case errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.EOF):
		return "eof"
	}
	return "other"
}

// outcome is the application's answer: JSON, so that keploy test compares
// it whole in every mode (keploy compares a non-JSON body only under
// --compare-all or the proven switch).
type outcome struct {
	Shape  string `json:"shape"`
	Saw    string `json:"saw,omitempty"`
	Status string `json:"status,omitempty"`
	Body   string `json:"body,omitempty"`
	Bytes  int    `json:"bytes,omitempty"`
	Length int64  `json:"length,omitempty"`
	Read   string `json:"read,omitempty"`
}

func app(addr, up string) {
	client := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	http.HandleFunc("/call/", func(w http.ResponseWriter, r *http.Request) {
		o := outcome{Shape: strings.TrimPrefix(r.URL.Path, "/call/")}
		w.Header().Set("Content-Type", "application/json")
		defer func() { _ = json.NewEncoder(w).Encode(o) }()
		resp, err := client.Get("http://" + up + "/" + o.Shape)
		if err != nil {
			o.Saw = saw(err)
			return
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		o.Status, o.Body, o.Bytes, o.Length, o.Read = resp.Status, string(body), len(body), resp.ContentLength, "whole"
		if err != nil {
			o.Read = saw(err)
		}
	})
	// The application's own exit, as a test command's when its tests are
	// done: whatever it still has in flight (the held call) is cut by the
	// exit, not given up on.
	http.HandleFunc("/exit", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Println("app: exiting on its own")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		os.Exit(0)
	})
	if os.Getenv("HELD_CALL") == "1" {
		// A background long poll, as a queue consumer's: in flight until it
		// is answered or its client's 120s timeout, which no run here
		// reaches; what ends it is said.
		//
		// GRACEFUL=1 is a graceful application's: the stop signal cancels the
		// call, so its client closes the connection while the process still
		// runs, and the process exits once that call has ended. Only keploy's
		// notice that the stop began tells the agent that close is the
		// stop's, not the application giving up.
		held := context.Background()
		graceful := os.Getenv("GRACEFUL") == "1"
		if graceful {
			held, _ = signal.NotifyContext(held, syscall.SIGINT, syscall.SIGTERM)
		}
		go func() {
			pc := &http.Client{Timeout: 120 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
			req, err := http.NewRequestWithContext(held, http.MethodGet, "http://"+up+"/held", nil)
			if err != nil {
				fmt.Println("held call:", err)
				return
			}
			resp, err := pc.Do(req)
			if err != nil {
				if graceful && held.Err() != nil {
					fmt.Println("app: closed its held call on the stop signal; exiting")
					os.Exit(0)
				}
				fmt.Println("held call:", saw(err))
				return
			}
			_ = resp.Body.Close()
			fmt.Println("held call: answered", resp.Status)
		}()
	}
	if err := http.ListenAndServe(addr, nil); err != nil {
		fmt.Println("listen:", err)
		os.Exit(1)
	}
}

func get(url string) {
	resp, err := (&http.Client{Timeout: time.Second}).Get(url)
	if err != nil {
		fmt.Println("get:", saw(err))
		os.Exit(0)
	}
	_ = resp.Body.Close()
	fmt.Println("get: answered", resp.Status)
	os.Exit(0)
}

func main() {
	switch {
	case len(os.Args) == 3 && os.Args[1] == "-get":
		get(os.Args[2])
	case len(os.Args) == 3 && os.Args[1] == "-upstream":
		upstream(os.Args[2])
	case len(os.Args) == 4 && os.Args[1] == "-app":
		app(os.Args[2], os.Args[3])
	default:
		fmt.Println("usage: faults -upstream ADDR | faults -app ADDR UPSTREAM | faults -get URL")
		os.Exit(2)
	}
}
