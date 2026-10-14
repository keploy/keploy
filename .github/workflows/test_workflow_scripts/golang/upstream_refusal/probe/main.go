// probe connects to each address given, as an application's client would,
// and prints one line per address: what connect returned, by errno, and how
// long it took. The e2e compares these lines with and without keploy. With
// PROBE_SEND set, it also sends a line on each connection that connects and
// reads the reply.
//
// An address written udp:HOST:PORT is a connected UDP client instead (statsd,
// syslog, an OpenTelemetry exporter, QUIC): connect, send a datagram, read the
// reply. Its class is connect's errno when connect fails -- a UDP connect to an
// address with no route is how glibc's getaddrinfo finds the addresses it
// cannot use, to sort them last -- else "echoed", or what the read returned.
//
// probe -serve PORT COUNTFILE is the other end: it counts each connection it
// accepts, one line in COUNTFILE, and echoes the first line back.
// probe -udpecho HOST:PORT echoes each datagram back, prefixed "ECHO:".
package main

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
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

func udpEcho(addr string) {
	c, err := net.ListenPacket("udp", addr)
	if err != nil {
		fmt.Println("listen:", err)
		os.Exit(1)
	}
	buf := make([]byte, 1500)
	for {
		n, from, err := c.ReadFrom(buf)
		if err != nil {
			return
		}
		_, _ = c.WriteTo(append([]byte("ECHO:"), buf[:n]...), from)
	}
}

// probeUDP is one connected UDP client's exchange with addr.
func probeUDP(addr string) (string, error) {
	c, err := net.DialTimeout("udp", addr, 5*time.Second)
	if err != nil {
		return classify(err), err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Write([]byte("hello-from-probe\n")); err != nil {
		return classify(err), err
	}
	buf := make([]byte, 1500)
	n, err := c.Read(buf)
	if err != nil {
		return classify(err), err
	}
	if !strings.HasPrefix(string(buf[:n]), "ECHO:") {
		return "OTHER", fmt.Errorf("reply %q", buf[:n])
	}
	return "echoed", nil
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
	if len(os.Args) == 3 && os.Args[1] == "-udpecho" {
		udpEcho(os.Args[2])
		return
	}
	for _, addr := range os.Args[1:] {
		if udp, ok := strings.CutPrefix(addr, "udp:"); ok {
			start := time.Now()
			class, err := probeUDP(udp)
			fmt.Printf("PROBE %s %s %.1fs (%v)\n", addr, class, time.Since(start).Seconds(), err)
			continue
		}
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
