package main

// This example attempts to trigger the Keploy tracker warning:
// "Malformed request" when expectedRecvBytes == 0 || actualRecvBytes == 0.
//
// Approach:
// 1. We accept a TCP connection.
// 2. Immediately write an HTTP/1.1 response BEFORE reading any bytes from the client.
// 3. Sleep >2s to allow tracker inactivity logic to consider the request finished.
// 4. Close the connection.
//
// When run together with Keploy's eBPF hooks, the tracker should see
// a response direction chunk, mark lastChunkWasResp, and later attempt
// to verify the preceding request sizes which will be zero.
// This should emit the warning log line in tracker.go.
//
// NOTE: You need to run Keploy in record mode attaching to this process
// (or start this server while Keploy is globally capturing) to observe the log.
//
// Usage:
//   go run main.go
// In another terminal (after ~1s):
//   nc localhost 8087
// (Optionally send nothing or press Enter only after server already responded.)
//
// You should see the server immediately send a 200 OK header before you type anything.
//
// To intensify: Run multiple connections concurrently without sending requests.

import (
	"fmt"
	"net"
	"time"
)

func main() {
	ln, err := net.Listen("tcp", ":8087")
	if err != nil {
		panic(err)
	}
	fmt.Println("Zero-byte-request demo server listening on :8087")
	for {
		conn, err := ln.Accept()
		if err != nil {
			fmt.Println("accept error:", err)
			continue
		}
		go handle(conn)
	}
}

func handle(c net.Conn) {
	defer c.Close()
	// Immediately write a response without reading.
	// Minimal valid HTTP/1.1 response.
	resp := "HTTP/1.1 200 OK\r\nContent-Length: 13\r\nContent-Type: text/plain\r\nConnection: close\r\n\r\nHello, world!"
	_, _ = c.Write([]byte(resp))

	// Sleep >2s so tracker considers connection idle and processes completion.
	time.Sleep(3 * time.Second)
}
