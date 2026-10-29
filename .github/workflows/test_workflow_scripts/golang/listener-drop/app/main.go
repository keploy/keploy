// A test application for keploy's listener drops lane
// (proven-abandoned-connect-linux.sh). /ping answers at once; /after calls the
// dependency (DEP, no keep-alive) and answers with what it said; /abandon
// calls the dependency, then abandons a connect to it mid-handshake, as a
// process killed in a connect does (go test's -timeout on a loaded host),
// and answers once keploy's proxy listener has counted the drop that leaves.
//
// The abandoned connect, inside /abandon's window:
//  1. a rule holds the SYN-ACKs keploy's proxy sends;
//  2. a child process connects to the dependency, which keploy redirects to
//     its proxy: the listener holds the half-open connection (SYN_RECV), its
//     SYN-ACK held, so the connect cannot finish;
//  3. the child is killed and reaped, the rule removed;
//  4. the listener's next SYN-ACK meets the reset of a client that is gone:
//     the kernel adds one to the listener's sk_drops and none to its network
//     namespace's TcpExtListenDrops (no connection was lost).
//
// Each step waits on what it needs (the half-open connection there, the
// child reaped, the half-open connection gone), never on a clock: on a loaded
// host the client's exit comes before the SYN-ACK by chance, here always. The
// app logs both counts before and after, for the lane to check that the drop
// happened and was the listener's alone.
//
// "-harness" makes it a test command for keploy mock record instead (an
// SDK-driven record): a runner that marks each test's window on keploy's
// scope API, as keploy's SDKs do, around three tests that do what the three
// routes do (harness).
//
// Run as root (keploy runs it so): it adds an iptables rule and reads the
// listener's counts with ss. "-dep ADDR" serves the dependency instead;
// "-connect ADDR" is the child. The child binds no port: keploy record
// redirects the binds of the app's processes (its ingress), so a port it
// asked for is not the one it gets.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

var (
	dep       = env("DEP", "127.0.0.1:9099")
	proxyPort = env("PROXY_PORT", "16789")
	abandonMu sync.Mutex
	client    = &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
)

func main() {
	log.SetFlags(0)
	if len(os.Args) > 2 && os.Args[1] == "-dep" {
		http.HandleFunc("/v", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "dep-ok") })
		log.Fatal(http.ListenAndServe(os.Args[2], nil))
	}
	if len(os.Args) > 2 && os.Args[1] == "-connect" {
		connectAndHang(os.Args[2])
	}
	if len(os.Args) > 1 && os.Args[1] == "-harness" {
		os.Exit(harness())
	}
	http.HandleFunc("/ping", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "pong") })
	http.HandleFunc("/after", func(w http.ResponseWriter, r *http.Request) {
		said, err := callDep()
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		fmt.Fprintf(w, "after, dep said %s", said)
	})
	http.HandleFunc("/abandon", func(w http.ResponseWriter, r *http.Request) {
		said, err := callDep()
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		if err := abandon(); err != nil {
			log.Printf("abandoned connect: %v", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		fmt.Fprintf(w, "abandoned, dep said %s", said)
	})
	log.Fatal(http.ListenAndServe(":8080", nil))
}

// harness runs TestPing (no call), TestAbandon (a call to the dependency,
// then an abandoned connect: abandon) and TestAfter (a call to the
// dependency), each inside its window on keploy's scope API
// ($KEPLOY_MOCK_AGENT, which keploy mock record exports to its test command
// with the token the API takes), after claiming the run as the proven wire
// asks. Then it writes the windows the agent closed, each with the evidence
// keploy charged it, to windows.json for the lane to read. It exits 1 when a
// test failed or the agent refused a call.
func harness() int {
	agent := os.Getenv("KEPLOY_MOCK_AGENT")
	if agent == "" {
		log.Print("harness: KEPLOY_MOCK_AGENT is not set: run it as keploy mock record's test command")
		return 1
	}
	wd, err := os.Getwd()
	if err != nil {
		log.Printf("harness: %v", err)
		return 1
	}
	if err := agentCall(http.MethodPost, agent+"/agent/instance", map[string]string{"key": ".", "dir": wd}, nil); err != nil {
		log.Printf("harness: claim the run: %v", err)
		return 1
	}
	callsDep := func() error {
		said, err := callDep()
		if err == nil && said != "dep-ok" {
			err = fmt.Errorf("the dependency said %q", said)
		}
		return err
	}
	tests := []struct {
		name string
		run  func() error
	}{
		{"TestPing", func() error { return nil }},
		{"TestAbandon", func() error {
			if err := callsDep(); err != nil {
				return err
			}
			if err := abandon(); err != nil {
				log.Printf("abandoned connect: %v", err)
				return err
			}
			return nil
		}},
		{"TestAfter", callsDep},
	}
	failed := 0
	for _, t := range tests {
		var begin struct{ Action, Reason string }
		// canSkip: a harness of the scope protocol's version 1 (keploy
		// refuses one that cannot skip a test it quarantines).
		if err := agentCall(http.MethodPost, agent+"/agent/scope/begin", map[string]any{"name": t.name, "canSkip": true}, &begin); err != nil {
			log.Printf("%s: begin: %v", t.name, err)
			failed++
			continue
		}
		if begin.Action != "" {
			// A record runs every test: a skip here is keploy's refusal.
			log.Printf("%s: keploy answered its begin %q (%s)", t.name, begin.Action, begin.Reason)
			failed++
			continue
		}
		outcome := "passed"
		if err := t.run(); err != nil {
			log.Printf("%s: %v", t.name, err)
			outcome = "failed"
			failed++
		}
		if err := agentCall(http.MethodPost, agent+"/agent/scope/end", map[string]string{"name": t.name, "outcome": outcome}, nil); err != nil {
			log.Printf("%s: end: %v", t.name, err)
			failed++
		}
		log.Printf("%s: %s", t.name, outcome)
	}
	var windows json.RawMessage
	if err := agentCall(http.MethodGet, agent+"/agent/scope/windows", nil, &windows); err != nil {
		log.Printf("harness: read the windows: %v", err)
		return 1
	}
	if err := os.WriteFile("windows.json", windows, 0o644); err != nil {
		log.Printf("harness: %v", err)
		return 1
	}
	if failed > 0 {
		log.Printf("harness: %d failure(s)", failed)
		return 1
	}
	return 0
}

// agentCall sends one call of keploy's scope API, with the token keploy
// exported, and reads its JSON answer into out (when not nil).
func agentCall(method, url string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if tok := os.Getenv("KEPLOY_MOCK_AGENT_TOKEN"); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%s %s: %s: %s", method, url, resp.Status, bytes.TrimSpace(b))
	}
	if out != nil && len(bytes.TrimSpace(b)) > 0 {
		return json.Unmarshal(b, out)
	}
	return nil
}

func callDep() (string, error) {
	resp, err := client.Get("http://" + dep + "/v")
	if err != nil {
		return "", fmt.Errorf("call the dependency: %v", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read the dependency's answer: %v", err)
	}
	return string(b), nil
}

// connectAndHang is the child: a blocking connect to ADDR. It returns only if
// the connect ended (refused: it was not redirected to keploy's proxy; or
// completed: its SYN-ACK was not held).
func connectAndHang(addr string) {
	ta, err := net.ResolveTCPAddr("tcp4", addr)
	if err != nil {
		log.Fatalf("connect: %v", err)
	}
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0)
	if err != nil {
		log.Fatalf("connect: socket: %v", err)
	}
	var to [4]byte
	copy(to[:], ta.IP.To4())
	err = syscall.Connect(fd, &syscall.SockaddrInet4{Port: ta.Port, Addr: to})
	log.Fatalf("connect: the connect ended (%v): it must hang, its SYN-ACK held", err)
}

// abandon abandons one connect mid-handshake (the steps in the file's
// comment) and says what the kernel counted.
func abandon() error {
	abandonMu.Lock()
	defer abandonMu.Unlock()
	if p := halfOpen(""); p != "" {
		return fmt.Errorf("a half-open connection (from port %s) is at keploy's proxy already", p)
	}
	lb, err := listenerDrops()
	if err != nil {
		return err
	}
	nb, err := netListenDrops()
	if err != nil {
		return err
	}
	rule := []string{"OUTPUT", "-p", "tcp", "--sport", proxyPort,
		"--tcp-flags", "SYN,ACK", "SYN,ACK", "-j", "DROP"}
	if err := run("iptables", append([]string{"-w", "-I"}, rule...)...); err != nil {
		return fmt.Errorf("hold the proxy's SYN-ACKs: %v", err)
	}
	held := true
	release := func() error {
		if !held {
			return nil
		}
		held = false
		return run("iptables", append([]string{"-w", "-D"}, rule...)...)
	}
	defer func() { _ = release() }()

	var out bytes.Buffer
	cmd := exec.Command(os.Args[0], "-connect", dep)
	cmd.Stdout, cmd.Stderr = &out, &out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start the client: %v", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	// The connect reached keploy's proxy: its listener holds the half-open
	// connection (the only one: abandonMu keeps the app's other calls out,
	// and the rule anyone else's handshake).
	var from string
	for from = halfOpen(""); from == ""; from = halfOpen("") {
		select {
		case err := <-exited:
			return fmt.Errorf("the client's connect ended before it reached keploy's proxy (%v): %s", err, strings.TrimSpace(out.String()))
		case <-time.After(5 * time.Millisecond):
		}
	}
	// The client goes mid-handshake, as a process killed in a connect does.
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	<-exited
	if err := release(); err != nil {
		return fmt.Errorf("let the proxy's SYN-ACKs through again: %v", err)
	}
	// The listener's next SYN-ACK meets the client's reset: the half-open
	// connection goes, and the listener counts the reset it discarded.
	var la uint64
	for {
		if la, err = listenerDrops(); err != nil {
			return err
		}
		if la > lb {
			break
		}
		if halfOpen(from) == "" {
			if la, err = listenerDrops(); err != nil {
				return err
			}
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	na, err := netListenDrops()
	if err != nil {
		return err
	}
	log.Printf("abandoned connect: listener drops %d -> %d, namespace ListenDrops %d -> %d (from port %s)", lb, la, nb, na, from)
	if la <= lb {
		return fmt.Errorf("the half-open connection went without a drop at the listener (%d -> %d)", lb, la)
	}
	return nil
}

func run(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// halfOpen is the source port, as /proc/net/tcp writes it, of a half-open
// connection (SYN_RECV, 03) keploy's proxy listener holds, from port from
// ("" for any), in /proc/net/tcp or tcp6 (a listener on [::] holds IPv4 ones
// as v4-mapped); "" if there is none.
func halfOpen(from string) string {
	pp, _ := strconv.Atoi(proxyPort)
	local := fmt.Sprintf(":%04X", pp)
	for _, f := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(b), "\n") {
			fs := strings.Fields(line)
			if len(fs) <= 3 || !strings.HasSuffix(fs[1], local) || fs[3] != "03" {
				continue
			}
			p := fs[2][strings.LastIndexByte(fs[2], ':')+1:]
			if from == "" || p == from {
				return p
			}
		}
	}
	return ""
}

var skmemDrops = regexp.MustCompile(`skmem:\([^)]*\bd(\d+)\)`)

// listenerDrops is the sk_drops of keploy's proxy listener (ss's skmem d).
func listenerDrops() (uint64, error) {
	out, err := exec.Command("ss", "-Hltnm", "sport = :"+proxyPort).CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("ss: %v: %s", err, strings.TrimSpace(string(out)))
	}
	m := skmemDrops.FindAllSubmatch(out, -1)
	if len(m) == 0 {
		return 0, fmt.Errorf("no listener on port %s with a drop count in ss: %q", proxyPort, out)
	}
	var n uint64
	for _, s := range m {
		v, _ := strconv.ParseUint(string(s[1]), 10, 64)
		n += v
	}
	return n, nil
}

// netListenDrops is the network namespace's TcpExtListenDrops.
func netListenDrops() (uint64, error) {
	b, err := os.ReadFile("/proc/net/netstat")
	if err != nil {
		return 0, err
	}
	var names []string
	for _, line := range strings.Split(string(b), "\n") {
		fs := strings.Fields(line)
		if len(fs) == 0 || fs[0] != "TcpExt:" {
			continue
		}
		if names == nil {
			names = fs
			continue
		}
		for i, n := range names {
			if n == "ListenDrops" && i < len(fs) {
				return strconv.ParseUint(fs[i], 10, 64)
			}
		}
	}
	return 0, errors.New("no TcpExt ListenDrops in /proc/net/netstat")
}
