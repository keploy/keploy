// Command egressprobe is the replay-isolation repro (DESIGN.md section 2).
//
//	egressprobe sink  -dir OUT          counts what arrives, per path, into OUT/<path>.count
//	egressprobe app   -sink IP [-sink6 IP6] [-listen :8080]
//	                                    GET /fire fires every path at the sink and answers
//	                                    a constant body (so record and replay agree); the
//	                                    per-path outcome goes to stderr only
//	egressprobe fork1|fork2 ADDR        the double-forked child path (internal)
//
// The sink owns the test addresses on its own interface, among them
// 169.254.169.254, so no path ever reaches a real cloud or metadata service.
// Nothing here reads or prints a credential.
package main

import (
	"context"
	"encoding/binary"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Ports of the sink, one per path, so a count names the path that leaked.
const (
	portTCP      = 7001 // plain TCP from the app
	portBypass   = 7002 // TCP to a port the run lists as a port-only bypass rule
	portFork     = 7003 // TCP from a double-forked (reparented) child
	portTFO      = 7004 // TCP fast open, sendto(MSG_FASTOPEN) with no connect()
	portTCP6     = 7005 // TCP over IPv6
	portIMDS     = 80   // TCP to 169.254.169.254 (an address of the sink)
	portUDP      = 9999 // UDP, connected and unconnected (payload names which)
	portQUIC     = 443  // UDP to QUIC's port
	portDNS      = 53   // DNS; the run points the resolver at the sink
	msgFastOpen  = 0x20000000
	ipprotoICMP  = 1
	ipprotoICMP6 = 58
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: egressprobe sink|app|fork1|fork2 ...")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "sink":
		sink(os.Args[2:])
	case "app":
		app(os.Args[2:])
	case "fork1": // starts fork2 and exits at once, so fork2 is reparented
		c := exec.Command("/proc/self/exe", "fork2", os.Args[2], os.Args[3])
		c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		c.Stderr = os.Stderr // fork2's outcome line goes where the app's do
		_ = c.Start()
		os.Exit(0)
	case "fork2":
		time.Sleep(300 * time.Millisecond) // until fork1 has exited
		outcome("fork-tcp", dialWrite("tcp", os.Args[2], "fork"))
		_ = os.WriteFile(os.Args[3], nil, 0o644) // tells the app it is done
	default:
		os.Exit(2)
	}
}

func outcome(path string, err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "probe %s: %v\n", path, err)
		return
	}
	fmt.Fprintf(os.Stderr, "probe %s: sent\n", path)
}

func dialWrite(network, addr, payload string) error {
	c, err := net.DialTimeout(network, addr, 3*time.Second)
	if err != nil {
		return err
	}
	defer c.Close()
	_, err = c.Write([]byte(payload))
	return err
}

func app(args []string) {
	fs := flag.NewFlagSet("app", flag.ExitOnError)
	sinkIP := fs.String("sink", "198.18.0.10", "the sink's IPv4 address")
	sink6 := fs.String("sink6", "", "the sink's IPv6 address (empty: skip IPv6)")
	imds := fs.String("imds", "169.254.169.254", "the metadata address the sink owns")
	listen := fs.String("listen", ":8080", "")
	_ = fs.Parse(args)
	at := func(p int) string { return net.JoinHostPort(*sinkIP, strconv.Itoa(p)) }

	http.HandleFunc("/fire", func(w http.ResponseWriter, _ *http.Request) {
		outcome("tcp", dialWrite("tcp", at(portTCP), "tcp"))
		outcome("bypass-tcp", dialWrite("tcp", at(portBypass), "bypass"))
		outcome("udp-connected", dialWrite("udp", at(portUDP), "udp-connected"))
		outcome("udp-sendto", sendTo(at(portUDP), "udp-sendto"))
		outcome("udp-quic", dialWrite("udp", at(portQUIC), "quic"))
		outcome("icmp-raw", icmpEcho(*sinkIP, syscall.SOCK_RAW))
		outcome("icmp-dgram", icmpEcho(*sinkIP, syscall.SOCK_DGRAM))
		// As a metadata client sends it (an AWS SDK names the address it
		// dials in Host), so a replay's report can name the destination.
		outcome("imds-tcp", dialWrite("tcp", net.JoinHostPort(*imds, strconv.Itoa(portIMDS)), "GET /latest/meta-data/ HTTP/1.1\r\nHost: "+*imds+"\r\nConnection: close\r\n\r\n"))
		outcome("tfo", fastOpen(*sinkIP, portTFO))
		_, err := net.DefaultResolver.LookupHost(context.Background(), "leak-probe.invalid-replay-isolation.test")
		outcome("dns-lookup", err)
		if *sink6 != "" {
			outcome("tcp6", dialWrite("tcp6", net.JoinHostPort(*sink6, strconv.Itoa(portTCP6)), "tcp6"))
			outcome("udp6", dialWrite("udp6", net.JoinHostPort(*sink6, strconv.Itoa(portUDP)), "udp6"))
		}
		done := filepath.Join(os.TempDir(), fmt.Sprintf("egressprobe-fork-%d", time.Now().UnixNano()))
		f1 := exec.Command("/proc/self/exe", "fork1", at(portFork), done)
		f1.Stderr = os.Stderr
		outcome("fork-start", f1.Run())
		for i := 0; i < 100; i++ { // the child dials with a 3 s timeout
			if _, err := os.Stat(done); err == nil {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		_ = os.Remove(done)
		_, _ = w.Write([]byte("fired\n"))
	})
	http.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	fmt.Fprintln(os.Stderr, http.ListenAndServe(*listen, nil))
	os.Exit(1)
}

func sendTo(addr, payload string) error {
	ua, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return err
	}
	c, err := net.ListenUDP("udp", nil)
	if err != nil {
		return err
	}
	defer c.Close()
	_, err = c.WriteToUDP([]byte(payload), ua)
	return err
}

// icmpEcho sends one ICMP echo request on a raw or a ping (dgram) socket.
func icmpEcho(ip string, typ int) error {
	fd, err := syscall.Socket(syscall.AF_INET, typ, ipprotoICMP)
	if err != nil {
		return err
	}
	defer syscall.Close(fd)
	// echo, id "KP", seq 1; the payload names the path, as NAT rewrites the
	// id and a ping socket sets its own
	msg := append([]byte{8, 0, 0, 0, 0x4b, 0x50, 0, 1}, "KPROBE-"+map[int]string{syscall.SOCK_RAW: "icmp-raw", syscall.SOCK_DGRAM: "icmp-dgram"}[typ]...)
	sum := checksum(msg)
	binary.BigEndian.PutUint16(msg[2:], sum)
	var sa syscall.SockaddrInet4
	copy(sa.Addr[:], net.ParseIP(ip).To4())
	return syscall.Sendto(fd, msg, 0, &sa)
}

func checksum(b []byte) uint16 {
	var s uint32
	for i := 0; i+1 < len(b); i += 2 {
		s += uint32(b[i])<<8 | uint32(b[i+1])
	}
	if len(b)%2 == 1 {
		s += uint32(b[len(b)-1]) << 8
	}
	for s>>16 != 0 {
		s = s&0xffff + s>>16
	}
	return ^uint16(s)
}

// fastOpen sends data with the SYN, sendto(MSG_FASTOPEN), never calling connect().
func fastOpen(ip string, port int) error {
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0)
	if err != nil {
		return err
	}
	defer syscall.Close(fd)
	sa := &syscall.SockaddrInet4{Port: port}
	copy(sa.Addr[:], net.ParseIP(ip).To4())
	return syscall.Sendto(fd, []byte("tfo"), msgFastOpen, sa)
}

// sink counts arrivals: one TCP accept or one datagram is one count, filed
// under the path its port (and, for UDP, its payload) names.
func sink(args []string) {
	fs := flag.NewFlagSet("sink", flag.ExitOnError)
	dir := fs.String("dir", "/sink", "where <path>.count files go")
	_ = fs.Parse(args)
	var mu sync.Mutex
	counts := map[string]int{}
	bump := func(path string) {
		mu.Lock()
		defer mu.Unlock()
		counts[path]++
		_ = os.WriteFile(filepath.Join(*dir, path+".count"), []byte(strconv.Itoa(counts[path])+"\n"), 0o644)
	}
	tcp := map[int]string{portTCP: "tcp", portBypass: "bypass-tcp", portFork: "fork-tcp", portTFO: "tfo", portTCP6: "tcp6", portIMDS: "imds-tcp", portDNS: "dns-tcp"}
	for port, path := range tcp {
		ln, err := net.Listen("tcp", ":"+strconv.Itoa(port))
		if err != nil {
			fmt.Fprintf(os.Stderr, "sink: listen %d: %v\n", port, err)
			os.Exit(1)
		}
		go func(ln net.Listener, path string) {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				bump(path)
				_ = c.Close()
			}
		}(ln, path)
	}
	udp := map[int]string{portUDP: "udp", portQUIC: "udp-quic", portDNS: "dns-udp"}
	for port, path := range udp {
		pc, err := net.ListenPacket("udp", ":"+strconv.Itoa(port))
		if err != nil {
			fmt.Fprintf(os.Stderr, "sink: listen udp %d: %v\n", port, err)
			os.Exit(1)
		}
		go func(pc net.PacketConn, path string) {
			buf := make([]byte, 2048)
			for {
				n, from, err := pc.ReadFrom(buf)
				if err != nil {
					return
				}
				p := path
				if path == "udp" && n > 0 {
					p = string(buf[:n]) // udp-connected, udp-sendto, udp6
				}
				if path == "dns-udp" {
					// The probe's own name counts as dns-udp; any other name
					// (keploy's or the runtime's own lookups) as dns-other, so a
					// count names who leaked. Every name goes to dns-names.txt.
					// The sink answers NXDOMAIN, so no lookup waits on a timeout.
					name := qname(buf[:n])
					if !strings.Contains(name, "replay-isolation") {
						p = "dns-other"
					}
					appendLine(filepath.Join(*dir, "dns-names.txt"), name)
					if n >= 12 {
						r := append([]byte(nil), buf[:n]...)
						r[2] |= 0x80                // QR: a response
						r[3] = (r[3] & 0x70) | 0x83 // RA, RCODE 3 (NXDOMAIN)
						_, _ = pc.WriteTo(r, from)
					}
				}
				bump(p)
			}
		}(pc, path)
	}
	go icmpCount(bump)
	fmt.Fprintln(os.Stderr, "sink: ready")
	select {}
}

// icmpCount counts echo requests carrying the probe's id ("KP").
func icmpCount(bump func(string)) {
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_RAW, ipprotoICMP)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sink: icmp: %v\n", err)
		return
	}
	buf := make([]byte, 1500)
	for {
		n, _, err := syscall.Recvfrom(fd, buf, 0)
		if err != nil {
			return
		}
		if n < 28 {
			continue
		}
		ihl := int(buf[0]&0x0f) * 4
		if n > ihl+8 && buf[ihl] == 8 {
			if p := string(buf[ihl+8 : n]); strings.HasPrefix(p, "KPROBE-") {
				bump(strings.TrimPrefix(p, "KPROBE-"))
			}
		}
	}
}

// qname reads the first question's name from a DNS message.
func qname(m []byte) string {
	var labels []string
	for i := 12; i < len(m); {
		l := int(m[i])
		if l == 0 || l&0xc0 != 0 || i+1+l > len(m) {
			break
		}
		labels = append(labels, string(m[i+1:i+1+l]))
		i += 1 + l
	}
	return strings.Join(labels, ".")
}

func appendLine(path, line string) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(line + "\n")
}
