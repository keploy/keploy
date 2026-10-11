// probe is both ends of the agent-restart e2e (agent-restart-docker.sh).
//
// probe -serve PORT is the dependency: an HTTP server that answers every GET
// with the source port of the connection it came in on.
//
// probe -record AGENT_URL is the recorder, as a keploy client is: it asks the
// agent for the mocks it records (POST AGENT_URL/outgoing), prints "recording"
// once the agent has started the recording, then a line per mock: "mock KIND
// URL". It ends when the agent does.
//
// probe -call HOST:PORT is the application: one long-running process that
// calls the dependency over a new connection, again and again, and prints a
// line per call: "call N proxied" when the dependency saw another source port
// than the one this end of the connection has, so something in between (the
// keploy agent's proxy) carried it; "call N direct" when it saw the same port
// number; "call N error ..." when the call failed. "direct" is a diagnostic,
// not a proof: the proxy's connection to the dependency can get the same
// port number as this one by chance. The recorded mock is the proof.
//
// Every call is made from the process's main thread. Locked to it in init, so
// main runs there (runtime.LockOSThread), the calls come from the thread the
// process started with, older than any agent restarted after the process
// started. keploy decides whether a process predates the recording at the
// process's socket() calls, and before keploy/ebpf#108 it judged by the start
// of the calling thread, not of the process: a thread a Go program creates on
// demand after the restart could make the process look new. The main thread
// cannot, so the check below fails the same way for every agent build that
// excludes the running application.
package main

import (
	"bufio"
	"encoding/gob"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
)

func init() {
	runtime.LockOSThread()
}

func serve(port string) {
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		_, srcPort, _ := net.SplitHostPort(r.RemoteAddr)
		fmt.Println(r.URL.Path, srcPort)
		_, _ = io.WriteString(w, srcPort)
	})
	fmt.Println("listen:", http.ListenAndServe(":"+port, nil))
	os.Exit(1)
}

// mock is the part of the agent's mocks (go.keploy.io/server/v3/pkg/models.Mock,
// gob-encoded) the recorder prints; gob leaves out the fields it does not name.
type mock struct {
	Kind string
	Spec struct {
		HTTPReq *struct {
			Method string
			URL    string
		}
	}
}

func record(agent string) {
	res, err := http.Post(agent+"/outgoing", "application/json", strings.NewReader(`{"outgoingOptions":{}}`))
	if err != nil {
		fmt.Println("record:", err)
		os.Exit(1)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(res.Body)
		fmt.Printf("record: %s: %s\n", res.Status, b)
		os.Exit(1)
	}
	// The agent answers once the recording is on (Proxy.Record has run).
	fmt.Println("recording")
	dec := gob.NewDecoder(res.Body)
	for {
		var m mock
		if err := dec.Decode(&m); err != nil {
			fmt.Println("record: the agent's stream ended:", err)
			return
		}
		url := "-"
		if m.Spec.HTTPReq != nil {
			url = m.Spec.HTTPReq.URL
		}
		fmt.Println("mock", m.Kind, url)
	}
}

// call makes call n to addr and says how it got there.
func call(addr string, n int) (string, error) {
	c, err := net.DialTimeout("tcp4", addr, 5*time.Second)
	if err != nil {
		return "", err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	local := strconv.Itoa(c.LocalAddr().(*net.TCPAddr).Port)
	if _, err := fmt.Fprintf(c, "GET /call/%d HTTP/1.1\r\nHost: dependency\r\nConnection: close\r\n\r\n", n); err != nil {
		return "", err
	}
	res, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		return "", err
	}
	seen := strings.TrimSpace(string(body))
	if res.StatusCode != http.StatusOK || seen == "" {
		return "", fmt.Errorf("status %d, body %q", res.StatusCode, seen)
	}
	if seen == local {
		return "direct", nil
	}
	return "proxied", nil
}

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: probe -serve PORT | probe -record AGENT_URL | probe -call HOST:PORT")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "-serve":
		serve(os.Args[2])
	case "-record":
		record(os.Args[2])
	case "-call":
		for n := 1; ; n++ {
			how, err := call(os.Args[2], n)
			if err != nil {
				fmt.Printf("call %d error %v\n", n, err)
			} else {
				fmt.Printf("call %d %s\n", n, how)
			}
			// Paces the calls; nothing waits on it.
			time.Sleep(100 * time.Millisecond)
		}
	default:
		fmt.Fprintln(os.Stderr, "usage: probe -serve PORT | probe -record AGENT_URL | probe -call HOST:PORT")
		os.Exit(2)
	}
}
