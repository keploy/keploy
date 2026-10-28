// tele-app is the disable-tele e2e's two tools in one static binary.
//
//	tele-app serve ADDR      answers GET /hello with a constant body: the
//	                         app keploy records.
//	tele-app dnswatch FILE   appends one line per DNS query seen on any
//	                         interface of this network namespace, as
//	                         "<id> <name> <type>", until killed; prints
//	                         "ready" on stderr once it is capturing.
//	tele-app mark NAME       sends one DNS query for NAME to 127.0.0.1:53,
//	                         for the watcher to log: a marker that every
//	                         query captured before it is already logged.
//
// dnswatch reads every packet through an AF_PACKET socket (root, or
// CAP_NET_RAW), so it sees the queries of every process of the namespace and
// those dockerd forwards for its containers, without touching resolv.conf. A
// query is a UDP datagram, on any port (keploy's DNS server is not on 53),
// whose payload parses as one, or a TCP segment to port 53 that starts one.
package main

import (
	"encoding/binary"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"syscall"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: tele-app serve ADDR | tele-app dnswatch FILE | tele-app mark NAME")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		http.HandleFunc("/hello", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("hello\n"))
		})
		fmt.Fprintln(os.Stderr, http.ListenAndServe(os.Args[2], nil))
		os.Exit(1)
	case "dnswatch":
		if err := dnswatch(os.Args[2]); err != nil {
			fmt.Fprintln(os.Stderr, "dnswatch:", err)
			os.Exit(1)
		}
	case "mark":
		if err := mark(os.Args[2]); err != nil {
			fmt.Fprintln(os.Stderr, "mark:", err)
			os.Exit(1)
		}
	default:
		fmt.Fprintln(os.Stderr, "unknown command", os.Args[1])
		os.Exit(2)
	}
}

func dnswatch(path string) error {
	const ethPAll = 0x0003 // ETH_P_ALL
	proto := int(ethPAll<<8 | ethPAll>>8)
	fd, err := syscall.Socket(syscall.AF_PACKET, syscall.SOCK_DGRAM, proto)
	if err != nil {
		return fmt.Errorf("AF_PACKET socket: %w", err)
	}
	out, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "ready")
	buf := make([]byte, 1<<16)
	for {
		n, _, err := syscall.Recvfrom(fd, buf, 0)
		if err == syscall.EINTR {
			continue
		}
		if err != nil {
			return fmt.Errorf("recvfrom: %w", err)
		}
		if id, name, qtype, ok := query(buf[:n]); ok {
			// One write per line: a reader never sees half of one.
			if _, err := fmt.Fprintf(out, "%d %s %d\n", id, name, qtype); err != nil {
				return err
			}
		}
	}
}

// mark sends a standard query (A, IN) for name to 127.0.0.1:53. Nothing need
// answer: the watcher logs the datagram as it leaves.
func mark(name string) error {
	m := []byte{0x6b, 0x70, 0x01, 0x00, 0, 1, 0, 0, 0, 0, 0, 0}
	for _, label := range strings.Split(name, ".") {
		if len(label) == 0 || len(label) > 63 {
			return fmt.Errorf("bad name %q", name)
		}
		m = append(m, byte(len(label)))
		m = append(m, label...)
	}
	m = append(m, 0, 0, 1, 0, 1)
	c, err := net.Dial("udp", "127.0.0.1:53")
	if err != nil {
		return err
	}
	defer c.Close()
	_, err = c.Write(m)
	return err
}

// query parses an IPv4 or IPv6 packet (SOCK_DGRAM: no link header) carrying a
// DNS query.
func query(p []byte) (id uint16, name string, qtype uint16, ok bool) {
	if len(p) < 1 {
		return
	}
	var l4 []byte
	var nh byte
	switch p[0] >> 4 {
	case 4:
		ihl := int(p[0]&0x0f) * 4
		if len(p) < 20 || ihl < 20 || len(p) < ihl {
			return
		}
		if binary.BigEndian.Uint16(p[6:8])&0x1fff != 0 { // a later fragment
			return
		}
		nh, l4 = p[9], p[ihl:]
	case 6:
		if len(p) < 40 {
			return
		}
		nh, l4 = p[6], p[40:]
	default:
		return
	}
	switch nh {
	case syscall.IPPROTO_UDP:
		if len(l4) < 8 {
			return
		}
		return dnsQuery(l4[8:])
	case syscall.IPPROTO_TCP:
		if len(l4) < 20 || binary.BigEndian.Uint16(l4[2:4]) != 53 {
			return
		}
		off := int(l4[12]>>4) * 4
		if off < 20 || len(l4) < off+2 {
			return
		}
		return dnsQuery(l4[off+2:])
	}
	return
}

// dnsQuery parses a DNS message that is a standard query with one question.
func dnsQuery(m []byte) (id uint16, name string, qtype uint16, ok bool) {
	if len(m) < 12 || m[2]&0x80 != 0 || m[2]&0x78 != 0 { // a response, or not QUERY
		return
	}
	if binary.BigEndian.Uint16(m[4:6]) != 1 || binary.BigEndian.Uint16(m[6:8]) != 0 || binary.BigEndian.Uint16(m[8:10]) != 0 {
		return
	}
	var labels []string
	i := 12
	for {
		if i >= len(m) {
			return
		}
		l := int(m[i])
		i++
		if l == 0 {
			break
		}
		if l > 63 || i+l > len(m) {
			return
		}
		label := m[i : i+l]
		for _, c := range label {
			if c <= ' ' || c >= 0x7f {
				return
			}
		}
		labels = append(labels, string(label))
		i += l
	}
	if len(labels) == 0 || i+4 > len(m) {
		return
	}
	return binary.BigEndian.Uint16(m[0:2]), strings.ToLower(strings.Join(labels, ".")), binary.BigEndian.Uint16(m[i : i+2]), true
}
