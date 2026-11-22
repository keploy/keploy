// A key-value store that speaks a plain TCP line protocol keploy has no
// parser for, so keploy records it with its Generic recorder: each line
// "FETCH <key>" on a connection gets "VAL <n>\n" and n bytes of the key's
// value (127.0.0.1:9097, or the address in STORE).
//
// It answers in more than one chunk, as real servers do:
//   - a key that starts with "p" (parts): its header and its value written
//     apart, 150 ms between them; any other key: in one write;
//   - a key with "big" in it: a 96 KiB value, more than a relay read of
//     32 KiB takes at once, so its answer is in more than one chunk however
//     the writes are scheduled; one that starts with "p": 4 KiB; any other:
//     64 bytes.
//
// A value is the key's own bytes, repeated, and ends with "|END <key>", so a
// cut answer shows in its length and in its last bytes.
//
// It keeps its side of a connection open after the client's FIN, as a server
// that notices a closed peer only at its next write does, until it is
// stopped: the connection, its client gone, ends only when keploy's relay
// gives up on it.
package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strings"
	"time"
)

func value(key string) []byte {
	n := 64
	switch {
	case strings.Contains(key, "big"):
		n = 96 << 10
	case strings.HasPrefix(key, "p"):
		n = 4 << 10
	}
	end := "|END " + key
	v := []byte(strings.Repeat(key+"-", n/(len(key)+1)+1))[:n-len(end)]
	return append(v, end...)
}

func serve(c net.Conn) {
	defer c.Close()
	r := bufio.NewReader(c)
	for {
		l, err := r.ReadString('\n')
		if errors.Is(err, io.EOF) {
			// The client's FIN: no more requests, but this side stays open.
			select {}
		}
		if err != nil {
			return
		}
		key, ok := strings.CutPrefix(strings.TrimSpace(l), "FETCH ")
		if !ok {
			fmt.Fprintf(c, "ERR unknown command\n")
			continue
		}
		v := value(key)
		head := fmt.Sprintf("VAL %d\n", len(v))
		if strings.HasPrefix(key, "p") {
			if _, err := c.Write([]byte(head)); err != nil {
				return
			}
			time.Sleep(150 * time.Millisecond)
			if _, err := c.Write(v); err != nil {
				return
			}
			continue
		}
		if _, err := c.Write(append([]byte(head), v...)); err != nil {
			return
		}
	}
}

func main() {
	addr := os.Getenv("STORE")
	if addr == "" {
		addr = "127.0.0.1:9097"
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("store listening on %s", addr)
	for {
		c, err := ln.Accept()
		if err != nil {
			log.Fatal(err)
		}
		go serve(c)
	}
}
