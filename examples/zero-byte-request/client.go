//go:build never

// Deprecated: use ./client/main.go instead. This file is excluded from builds.
package main

// A client that connects to the demo server and purposefully does not send
// any request bytes for several seconds, then optionally sends a minimal GET.
// Running multiple instances increases chances of hitting the warning.
//
// Usage:
//   go run client.go (then maybe run again in parallel)
//
// Press Ctrl+C to exit.

import (
	"fmt"
	"net"
	"time"
)

func main() {
	fmt.Println("Client: connecting to :8087 and idling before sending request")
	c, err := net.Dial("tcp", "127.0.0.1:8087")
	if err != nil {
		panic(err)
	}
	defer c.Close()

	// Wait so server sends response first.
	time.Sleep(4 * time.Second)

	// Optionally send a simple request after response already came (out of order scenario).
	_, _ = c.Write([]byte("GET /late HTTP/1.1\r\nHost: localhost\r\n\r\n"))

	buf := make([]byte, 1024)
	n, _ := c.Read(buf)
	fmt.Printf("Client received (%d bytes):\n%s\n", n, string(buf[:n]))

	time.Sleep(1 * time.Second)
}
