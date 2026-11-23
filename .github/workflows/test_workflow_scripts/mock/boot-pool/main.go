// Command bootpool is the fixture of the mock parallel lane's boot-pool leg:
// an app that opens its connection to a dependency at boot, before any test
// begins, as a database pool does, and makes its calls over it. It serves no
// port: each line on stdin is a path to GET from the dependency, and each
// answer is one line on stdout, <path>=SERVED:<body>:<conn> or
// <path>=MISS:<conn>, where <conn> is "boot" when the call went over the
// connection opened at boot and "new" otherwise. Its first line is
// "READY <pid>", once the boot connection is open.
//
// Usage: bootpool HOST:PORT
package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"os"
	"sync"
	"time"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: bootpool HOST:PORT")
		os.Exit(2)
	}
	dep := os.Args[1]
	boot, err := net.DialTimeout("tcp", dep, 10*time.Second)
	if err != nil {
		fmt.Printf("BOOT_FAILED %v\n", err)
		os.Exit(1)
	}
	var (
		mu    sync.Mutex
		ready = boot
	)
	// The pool's first connection is the one opened at boot; one is ever
	// open, so every call goes over it while it lives.
	tr := &http.Transport{
		MaxConnsPerHost:     1,
		MaxIdleConnsPerHost: 1,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			mu.Lock()
			c := ready
			ready = nil
			mu.Unlock()
			if c != nil {
				return c, nil
			}
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		},
	}
	client := &http.Client{Transport: tr, Timeout: 15 * time.Second}
	fmt.Printf("READY %d\n", os.Getpid())

	in := bufio.NewScanner(os.Stdin)
	for in.Scan() {
		path := in.Text()
		conn := "new"
		trace := &httptrace.ClientTrace{GotConn: func(i httptrace.GotConnInfo) {
			if i.Conn == boot {
				conn = "boot"
			}
		}}
		req, err := http.NewRequestWithContext(httptrace.WithClientTrace(context.Background(), trace), http.MethodGet, "http://"+dep+path, nil)
		if err != nil {
			fmt.Printf("%s=MISS:%s\n", path, conn)
			continue
		}
		resp, err := client.Do(req)
		if err != nil {
			fmt.Printf("%s=MISS:%s\n", path, conn)
			continue
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil || resp.StatusCode != http.StatusOK {
			fmt.Printf("%s=MISS:%s\n", path, conn)
			continue
		}
		fmt.Printf("%s=SERVED:%s:%s\n", path, body, conn)
	}
}
