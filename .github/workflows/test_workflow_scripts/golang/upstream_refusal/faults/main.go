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
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
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
	if err := http.ListenAndServe(addr, nil); err != nil {
		fmt.Println("listen:", err)
		os.Exit(1)
	}
}

func main() {
	switch {
	case len(os.Args) == 3 && os.Args[1] == "-upstream":
		upstream(os.Args[2])
	case len(os.Args) == 4 && os.Args[1] == "-app":
		app(os.Args[2], os.Args[3])
	default:
		fmt.Println("usage: faults -upstream ADDR | faults -app ADDR UPSTREAM")
		os.Exit(2)
	}
}
