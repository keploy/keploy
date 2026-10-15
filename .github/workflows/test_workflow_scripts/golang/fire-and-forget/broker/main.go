// A broker: POST /publish with "publish N" answers "ok N" and GET /ready
// answers "ready" (HTTP, 127.0.0.1:9099); on 127.0.0.1:9098 each line
// "publish N" (or "hello") on a connection gets "ok N" ("ok hello") (TCP).
package main

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
)

func main() {
	ln, err := net.Listen("tcp", "127.0.0.1:9098")
	if err != nil {
		log.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				log.Fatal(err)
			}
			go func(c net.Conn) {
				defer c.Close()
				r := bufio.NewReader(c)
				for {
					l, err := r.ReadString('\n')
					if err != nil {
						return
					}
					fmt.Fprintf(c, "ok %s\n", strings.TrimPrefix(strings.TrimSpace(l), "publish "))
				}
			}(c)
		}
	}()
	http.HandleFunc("/publish", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		fmt.Fprintf(w, "ok %s\n", strings.TrimPrefix(string(b), "publish "))
	})
	http.HandleFunc("/ready", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "ready")
	})
	log.Fatal(http.ListenAndServe("127.0.0.1:9099", nil))
}
