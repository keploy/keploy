// Command echo is the redis-skip lane's look-alike dependency: a TCP server
// that writes back every byte it reads. The app sends it a RESP command array
// whose command is not a Redis command (NOPE), the closest a client-first
// protocol comes to a Redis client without being one: OSS keploy must record
// it through the Generic parser, and the Redis skip must not take it.
package main

import (
	"io"
	"log"
	"net"
	"os"
)

func main() {
	addr := "127.0.0.1:7379"
	if len(os.Args) > 1 {
		addr = os.Args[1]
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("echo listening on %s", addr)
	for {
		c, err := ln.Accept()
		if err != nil {
			log.Fatal(err)
		}
		go func() {
			defer func() { _ = c.Close() }()
			_, _ = io.Copy(c, c)
		}()
	}
}
