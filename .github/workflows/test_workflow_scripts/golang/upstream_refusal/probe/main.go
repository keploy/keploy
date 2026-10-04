// probe connects to each address given, as an application's client would,
// and prints one line per address: what connect returned, by errno, and how
// long it took. The e2e compares these lines with and without keploy. With
// PROBE_SEND set, it also sends a line on each connection that connects and
// reads the reply.
//
// probe -serve PORT COUNTFILE is the other end: it counts each connection it
// accepts, one line in COUNTFILE, and echoes the first line back.
package main

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"
	"time"
)

func serve(port, countFile string) {
	l, err := net.Listen("tcp", ":"+port)
	if err != nil {
		fmt.Println("listen:", err)
		os.Exit(1)
	}
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		if f, err := os.OpenFile(countFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o666); err == nil {
			_, _ = f.WriteString("conn\n")
			_ = f.Close()
		}
		go func() {
			defer c.Close()
			_ = c.SetDeadline(time.Now().Add(10 * time.Second))
			if line, err := bufio.NewReader(c).ReadString('\n'); err == nil {
				_, _ = c.Write([]byte("ECHO:" + line))
			}
		}()
	}
}

func classify(err error) string {
	switch {
	case err == nil:
		return "connected"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "ECONNREFUSED"
	case errors.Is(err, syscall.EHOSTUNREACH):
		return "EHOSTUNREACH"
	case errors.Is(err, syscall.ENETUNREACH):
		return "ENETUNREACH"
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return "TIMEOUT"
	}
	return "OTHER"
}

func main() {
	if len(os.Args) == 4 && os.Args[1] == "-serve" {
		serve(os.Args[2], os.Args[3])
		return
	}
	for _, addr := range os.Args[1:] {
		start := time.Now()
		c, err := net.DialTimeout("tcp", addr, 5*time.Second)
		took := time.Since(start)
		fmt.Printf("PROBE %s %s %.1fs (%v)\n", addr, classify(err), took.Seconds(), err)
		if c != nil {
			if os.Getenv("PROBE_SEND") != "" {
				_ = c.SetDeadline(time.Now().Add(5 * time.Second))
				_, _ = c.Write([]byte("hello-from-probe\n"))
				reply, _ := bufio.NewReader(c).ReadString('\n')
				fmt.Printf("REPLY %s %q\n", addr, reply)
			}
			_ = c.Close()
		}
	}
}
