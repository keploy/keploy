// Command egressprobe is the replay-isolation repro (DESIGN.md section 2).
//
//	egressprobe sink  -dir OUT          counts what arrives, per path, into OUT/<path>.count
//	egressprobe app   -sink IP [-sink6 IP6] [-imds IP] [-peer NAME] [-listen :8080]
//	                                    GET /fire fires every path at the sink and answers
//	                                    a constant body (so record and replay agree); the
//	                                    per-path outcome goes to stderr only
//	egressprobe ask   IP NAME           asks the DNS server at IP:53 for NAME
//	                                    (A, over UDP) and prints "answered
//	                                    rcode=N", or why it got no answer (exit 1)
//	egressprobe runner -url URL [-n N]  a compose stack's test driver: waits for URL/health,
//	                                    then GETs URL/health?n=1..N and exits 0
//	egressprobe fork1|fork2 ADDR        the double-forked child path (internal)
//
// The sink owns the test addresses on its own interface, among them the one
// the app is given as -imds (the metadata address; with none it makes no
// metadata call), so no path ever reaches a real cloud or metadata service.
// Nothing here reads or prints a credential.
package main

import (
	"context"
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
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
	portIMDS     = 80   // TCP to the metadata address (-imds, an address of the sink)
	portUDP      = 9999 // UDP, connected and unconnected (payload names which)
	portQUIC     = 443  // UDP to QUIC's port
	portDNS      = 53   // DNS; the run points the resolver at the sink
	msgFastOpen  = 0x20000000
	ipprotoICMP  = 1
	ipprotoICMP6 = 58
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: egressprobe sink|app|oneshot|early|runner|fork1|fork2 ...")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "sink":
		sink(os.Args[2:])
	case "app":
		app(os.Args[2:])
	case "ask":
		if len(os.Args) != 4 {
			fmt.Fprintln(os.Stderr, "usage: egressprobe ask IP NAME")
			os.Exit(2)
		}
		rcode, err := ask(os.Args[2], os.Args[3])
		if err != nil {
			fmt.Printf("no answer: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("answered rcode=%d\n", rcode)
	case "oneshot": // exits 0 at once, as a compose stack's one-shot migration does
		os.Exit(0)
	case "early": // exits 3 at once, as a test driver that fails before the app is up does
		fmt.Println("EARLY EXITS 3")
		os.Exit(3)
	case "runner":
		runner(os.Args[2:])
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

// runner drives the app as a compose stack's test-driver service does (the
// service `docker compose up --exit-code-from` names): it waits until
// URL/health answers, makes N more calls, each with its own query so each is
// its own test, says so with the time on stdout ("RUNNER DONE <unix
// seconds>", which compose shows in keploy's log) and exits 0. It exits 2 if
// the app never answers and 3 if a call fails.
func runner(args []string) {
	fs := flag.NewFlagSet("runner", flag.ExitOnError)
	base := fs.String("url", "", "the app's base URL")
	n := fs.Int("n", 5, "the calls made once the app answers")
	_ = fs.Parse(args)
	c := &http.Client{Timeout: 10 * time.Second}
	get := func(u string) error {
		resp, err := c.Get(u)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("status %d", resp.StatusCode)
		}
		return nil
	}
	up := false
	for i := 0; i < 240 && !up; i++ {
		if up = get(*base+"/health") == nil; !up {
			time.Sleep(time.Second)
		}
	}
	if !up {
		fmt.Fprintln(os.Stderr, "runner: the app never answered")
		os.Exit(2)
	}
	for i := 1; i <= *n; i++ {
		if err := get(fmt.Sprintf("%s/health?n=%d", *base, i)); err != nil {
			fmt.Fprintf(os.Stderr, "runner: call %d: %v\n", i, err)
			os.Exit(3)
		}
	}
	fmt.Printf("RUNNER DONE %d\n", time.Now().Unix())
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
	imds := fs.String("imds", "", "the metadata address the sink owns (empty: the app makes no metadata call)")
	peer := fs.String("peer", "", "a container name the network holds, resolved through it (empty: skip)")
	unrecorded := fs.Bool("unrecorded", false, "also look up the names a recording must not hold (the replay's and the baselines' args; the recording's leave it off)")
	stopLookup := fs.Bool("stop-lookup", false, "look a name up as the app stops (SIGINT or SIGTERM), after its last test and in no recording, then exit (the replay's args: keploy test reads the calls made after a set's last test once its app has stopped)")
	stopRecorded := fs.Bool("stop-recorded", false, "look a name up as the app stops (SIGINT or SIGTERM), after its last test, at record and at replay, then exit (the recording's args too: keploy record stops the app before it saves what the agent captured, so the recording holds it and the replay serves it)")
	crashOnFire := fs.Int("crash-on-fire", 0, "exit with this code as /fire is asked, before it answers (0: never): a crash during a test")
	listen := fs.String("listen", ":8080", "")
	_ = fs.Parse(args)
	at := func(p int) string { return net.JoinHostPort(*sinkIP, strconv.Itoa(p)) }

	http.HandleFunc("/fire", func(w http.ResponseWriter, _ *http.Request) {
		if *crashOnFire != 0 {
			fmt.Fprintf(os.Stderr, "probe crash: exit %d\n", *crashOnFire)
			os.Exit(*crashOnFire)
		}
		outcome("tcp", dialWrite("tcp", at(portTCP), "tcp"))
		outcome("bypass-tcp", dialWrite("tcp", at(portBypass), "bypass"))
		outcome("udp-connected", dialWrite("udp", at(portUDP), "udp-connected"))
		outcome("udp-sendto", sendTo(at(portUDP), "udp-sendto"))
		outcome("udp-quic", dialWrite("udp", at(portQUIC), "quic"))
		outcome("icmp-raw", icmpEcho(*sinkIP, syscall.SOCK_RAW))
		outcome("icmp-dgram", icmpEcho(*sinkIP, syscall.SOCK_DGRAM))
		// As a metadata client sends it (an AWS SDK names the address it
		// dials in Host), so a replay's report can name the destination.
		if *imds != "" {
			outcome("imds-tcp", dialWrite("tcp", net.JoinHostPort(*imds, strconv.Itoa(portIMDS)), "GET /latest/meta-data/ HTTP/1.1\r\nHost: "+*imds+"\r\nConnection: close\r\n\r\n"))
		}
		outcome("tfo", fastOpen(*sinkIP, portTFO))
		// A name that does not exist, looked up at record and at replay: the
		// recording holds its NXDOMAIN, which the replay serves itself, so it
		// fails as it did while recording and never reaches the sink there
		// (dns-recorded).
		_, err := net.DefaultResolver.LookupHost(context.Background(), "recorded-nx.invalid-replay-isolation.test")
		outcome("dns-recorded-lookup", err)
		// The names below are looked up only with -unrecorded, which the
		// recording leaves off, so no mock answers them at replay.
		if *unrecorded {
			_, err = net.DefaultResolver.LookupHost(context.Background(), "leak-probe.invalid-replay-isolation.test")
			outcome("dns-lookup", err)
			// An unrecorded single-label name: docker's 127.0.0.11 recurses
			// one it does not hold to its upstream like any other name.
			_, err = net.DefaultResolver.LookupHost(context.Background(), "leak-probe-bare-replay-isolation")
			outcome("dns-bare-lookup", err)
		}
		// A container the network holds: its address, which only docker's
		// resolver knows, says that resolver still answers the network's
		// own names. The recording looks it up too, before the peer is up,
		// so it holds the name's NXDOMAIN, and the replay must still ask
		// docker's resolver.
		if *peer != "" {
			// Its name's other types (TXT, SRV, and what CNAME asks), which
			// docker's resolver does not hold and recurses to its upstream:
			// the sink counts each as dns-owned.
			if *unrecorded {
				_, _ = net.DefaultResolver.LookupTXT(context.Background(), *peer)
				_, _, _ = net.DefaultResolver.LookupSRV(context.Background(), "", "", *peer)
				_, _ = net.DefaultResolver.LookupCNAME(context.Background(), *peer)
			}
			addrs, err := net.DefaultResolver.LookupHost(context.Background(), *peer)
			if err != nil {
				outcome("dns-peer", err)
			} else {
				fmt.Fprintf(os.Stderr, "probe dns-peer: %s\n", strings.Join(addrs, ","))
			}
		}
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
	// A name looked up as the app starts, before it serves, so outside every
	// test, and only with -unrecorded: a start-up lookup made only in a
	// replay (a configuration that differs from the recording's), which no
	// mock answers. keploy test answers it itself; its report names it by its
	// shape and its misses file names it whole (the startup rows).
	if *unrecorded {
		_, err := net.DefaultResolver.LookupHost(context.Background(), "startup-probe.invalid-replay-isolation.test")
		outcome("dns-startup-lookup", err)
	}
	// A name looked up as the app stops, so after its last test, which no
	// recording holds (the recording's args leave -stop-lookup off). keploy
	// test stops the app before it reads the calls made after the set's last
	// test (with the switch off after the set's post-script, before its
	// report), so that read takes it: the report names it as made after the
	// last test by its shape, and the misses file names it whole (the
	// after-last rows). Bounded, so the stop is prompt.
	// With -stop-recorded it first looks up a name the recording holds, made
	// as the app stops at record too: keploy record stops the app before it
	// saves what the agent captured, so the recording holds that NXDOMAIN, the
	// replay serves it, and no report or misses file names it (the
	// stop-recorded rows).
	if *stopLookup || *stopRecorded {
		stop := make(chan os.Signal, 1)
		signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
		go func() {
			<-stop
			if *stopRecorded {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				_, err := net.DefaultResolver.LookupHost(ctx, "stop-recorded.invalid-replay-isolation.test")
				cancel()
				outcome("dns-stop-recorded-lookup", err)
			}
			if *stopLookup {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				_, err := net.DefaultResolver.LookupHost(ctx, "stop-probe.invalid-replay-isolation.test")
				cancel()
				outcome("dns-stop-lookup", err)
			}
			os.Exit(0)
		}()
	}
	fmt.Fprintln(os.Stderr, http.ListenAndServe(*listen, nil))
	os.Exit(1)
}

// ask sends one DNS question for name (type A) to ip:53 over UDP and returns
// the answer's RCODE. The lane asks the sink so at the start of each phase:
// an answer says the sink was receiving DNS then, so a DNS row with nothing
// at the sink failed on the resolver's side, not the sink's.
func ask(ip, name string) (int, error) {
	q := []byte{0x52, 0x49, 0x01, 0x00, 0, 1, 0, 0, 0, 0, 0, 0} // id "RI", RD, one question
	for _, l := range strings.Split(strings.TrimSuffix(name, "."), ".") {
		if l == "" || len(l) > 63 {
			return 0, fmt.Errorf("bad name %q", name)
		}
		q = append(append(q, byte(len(l))), l...)
	}
	q = append(q, 0, 0, 1, 0, 1) // root, type A, class IN
	c, err := net.Dial("udp", net.JoinHostPort(ip, strconv.Itoa(portDNS)))
	if err != nil {
		return 0, err
	}
	defer c.Close()
	if err := c.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		return 0, err
	}
	if _, err := c.Write(q); err != nil {
		return 0, err
	}
	r := make([]byte, 512)
	n, err := c.Read(r)
	if err != nil {
		return 0, err
	}
	if n < 12 || r[0] != q[0] || r[1] != q[1] || r[2]&0x80 == 0 {
		return 0, fmt.Errorf("not an answer to the question (%d bytes)", n)
	}
	return int(r[3] & 0x0f), nil
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
					// The probe's own dotted name counts as dns-udp, the ones
					// its recording holds (recorded-nx.*, and stop-recorded.*,
					// looked up as it stops) as dns-recorded, its single-label one
					// as dns-bare; any other name
					// (keploy's or the runtime's own lookups) as dns-other, so a
					// count names who leaked. Every name goes to dns-names.txt.
					// The sink answers the probe's names NXDOMAIN and every
					// other name REFUSED, so no lookup waits on a timeout.
					// The probe's names include the peer's: before the peer
					// is up its name does not exist, and the sink is the only
					// upstream of the docker phases' 127.0.0.11, so it must
					// say so (with no other upstream to move on to, a REFUSED
					// would reach the app as a server failure). A peer name's
					// lookup of a type other than A, AAAA and MX counts as
					// dns-owned, each with its type in dns-owned-types.txt;
					// one of those types as dns-other. The lane's own question
					// at the start of a phase (sink-check.*) counts as
					// dns-check, so it moves no other count.
					name := qname(buf[:n])
					rcode := byte(3) // NXDOMAIN
					qtype := 0
					if off := 12 + len(name) + 2; name != "" && off+2 <= n {
						qtype = int(buf[off])<<8 | int(buf[off+1])
					}
					peer := strings.HasPrefix(name, "riso-peer")
					switch {
					case strings.HasPrefix(name, "sink-check."):
						p = "dns-check"
					case strings.HasPrefix(name, "recorded-nx."), strings.HasPrefix(name, "stop-recorded."):
						p = "dns-recorded"
					case peer && qtype != 1 && qtype != 28 && qtype != 15:
						p = "dns-owned"
						appendLine(filepath.Join(*dir, "dns-owned-types.txt"), name+" "+strconv.Itoa(qtype))
					case peer:
						p = "dns-other"
					case !strings.Contains(name, "replay-isolation"):
						p = "dns-other"
						rcode = 5 // REFUSED
					case !strings.Contains(strings.TrimSuffix(name, "."), "."):
						p = "dns-bare"
					}
					appendLine(filepath.Join(*dir, "dns-names.txt"), name)
					if n >= 12 {
						r := append([]byte(nil), buf[:n]...)
						r[2] |= 0x80                        // QR: a response
						r[3] = (r[3] & 0x70) | 0x80 | rcode // RA and the RCODE
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
