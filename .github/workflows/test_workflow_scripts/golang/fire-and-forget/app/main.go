// A test application whose /a-publish answers 200 ms after it starts a
// publish to a broker that it does not wait for (fire and forget), with SUFFIX
// appended to the message; /b-other takes 3 s; /c-ping answers at once.
// With SHUTDOWN_PUBLISH set it publishes "shutdown" as it is stopped; with
// IGNORE_INT set it ignores SIGINT; ONE_SIGNAL and HANG_ON_STOP shape how it
// stops (below).
//
// PROTO says how it publishes:
//   - http: an HTTP POST on a connection of its own (no keep-alive);
//   - tcp: a line on a TCP connection of its own;
//   - httpkeep: an HTTP POST on a keep-alive connection dialled at startup,
//     first used then (GET /ready), and pooled since;
//   - tcpkeep: a line on a TCP connection dialled at startup, first used then
//     (a hello), and kept since.
//
// The kept connections speak at once, so the proxy never waits on them to
// tell their protocol, and each publish on them opens no connection: only its
// bytes reach the proxy.
package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptrace"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

var (
	keepMu   sync.Mutex
	keepConn net.Conn
	keepR    *bufio.Reader
	preMu    sync.Mutex
	pre      net.Conn
	keepHTTP = &http.Client{Transport: &http.Transport{
		// The first request rides the connection dialled at startup.
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			preMu.Lock()
			c := pre
			pre = nil
			preMu.Unlock()
			if c != nil {
				return c, nil
			}
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		},
		MaxIdleConnsPerHost: 4,
	}}
)

// afterPublish is a /d-after publish waiting for its /e-wait: release lets
// it go, done closes once it is over.
type afterPublish struct {
	release chan struct{}
	done    chan struct{}
}

var (
	afterMu   sync.Mutex
	afterByID = map[string]*afterPublish{}
)

func line(msg string, wrote func()) string {
	keepMu.Lock()
	defer keepMu.Unlock()
	fmt.Fprintf(keepConn, "%s\n", msg)
	wrote()
	l, err := keepR.ReadString('\n')
	return fmt.Sprintf("%q (%v)", l, err)
}

func post(c *http.Client, broker, msg string, wrote func()) string {
	req, err := http.NewRequest(http.MethodPost, "http://"+broker+"/publish", strings.NewReader(msg))
	if err != nil {
		return err.Error()
	}
	req.Header.Set("Content-Type", "text/plain")
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{WroteRequest: func(httptrace.WroteRequestInfo) { wrote() }}))
	resp, err := c.Do(req)
	if err != nil {
		return err.Error()
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return fmt.Sprintf("%q (%d)", b, resp.StatusCode)
}

// publish closes written once its bytes are in the kernel (or it failed).
func publish(broker, msg string, written chan<- struct{}) {
	var once sync.Once
	wrote := func() { once.Do(func() { close(written) }) }
	defer wrote()
	// NOPUBLISH: the app no longer makes the call (a replay of a recording
	// that has it must fail).
	if os.Getenv("NOPUBLISH") != "" {
		log.Printf("%s: not published (NOPUBLISH)", msg)
		return
	}
	switch os.Getenv("PROTO") {
	case "tcpkeep":
		log.Printf("%s: broker said %s on the kept connection", msg, line(msg, wrote))
	case "httpkeep":
		log.Printf("%s: broker said %s on the pooled connection", msg, post(keepHTTP, broker, msg, wrote))
	case "tcp":
		c, err := net.Dial("tcp", broker)
		if err != nil {
			log.Printf("%s: dial: %v", msg, err)
			return
		}
		defer c.Close()
		fmt.Fprintf(c, "%s\n", msg)
		wrote()
		l, err := bufio.NewReader(c).ReadString('\n')
		log.Printf("%s: broker said %q (%v)", msg, l, err)
	default:
		// A connection of its own (no keep-alive), as a publisher that
		// connects per message does.
		c := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
		log.Printf("%s: broker said %s", msg, post(c, broker, msg, wrote))
	}
}

func main() {
	broker := os.Getenv("BROKER")
	switch os.Getenv("PROTO") {
	case "tcpkeep", "httpkeep":
		c, err := net.Dial("tcp", broker)
		if err != nil {
			log.Fatalf("dial the kept connection: %v", err)
		}
		if os.Getenv("PROTO") == "tcpkeep" {
			keepConn, keepR = c, bufio.NewReader(c)
			log.Printf("hello: broker said %s on the kept connection", line("hello", func() {}))
			break
		}
		pre = c
		resp, err := keepHTTP.Get("http://" + broker + "/ready")
		if err != nil {
			log.Fatalf("ready: %v", err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		log.Printf("ready: broker said %d on the pooled connection", resp.StatusCode)
	}
	pub := func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("id")
		written := make(chan struct{})
		go publish(broker, "publish "+id+os.Getenv("SUFFIX"), written)
		// Its bytes are in the kernel before the answer leaves, however late
		// the scheduler runs it; its reply is still not waited for.
		<-written
		// hold: the response waits this long after the publish began, so a
		// recording finds the publish inside the test.
		if d, err := time.ParseDuration(r.URL.Query().Get("hold")); err == nil {
			time.Sleep(d)
		}
		fmt.Fprintf(w, "queued %s\n", id)
	}
	http.HandleFunc("/a-publish", pub)
	// /d-after answers first, and publishes only once the answer has left:
	// from a goroutine of its own, never waiting for the broker's reply,
	// when /e-wait for the same id releases it. The publish is made after
	// the test has its answer, inside the test /e-wait is: in a recording
	// and in a replay alike, whatever the scheduler does. (keploy replays a
	// set's tests in name order, so e-wait follows d-after.)
	http.HandleFunc("/d-after", func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("id")
		p := &afterPublish{release: make(chan struct{}), done: make(chan struct{})}
		afterMu.Lock()
		afterByID[id] = p
		afterMu.Unlock()
		go func() {
			defer close(p.done)
			<-p.release
			publish(broker, "after "+id+os.Getenv("SUFFIX"), make(chan struct{}))
		}()
		fmt.Fprintf(w, "accepted %s\n", id)
	})
	// /e-wait releases /d-after's publish for its id and answers once the
	// publish is over (the broker's reply read, or the publish failed), so a
	// recording finds the whole call inside this test. An id with no publish
	// pending answers at once.
	http.HandleFunc("/e-wait", func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("id")
		afterMu.Lock()
		p := afterByID[id]
		delete(afterByID, id)
		afterMu.Unlock()
		if p != nil {
			close(p.release)
			<-p.done
		}
		fmt.Fprintf(w, "done %s\n", id)
	})
	http.HandleFunc("/b-other", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(3 * time.Second)
		fmt.Fprintln(w, "other")
	})
	http.HandleFunc("/c-ping", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "pong")
	})
	// SHUTDOWN_PUBLISH: as it is stopped (SIGINT, SIGTERM), after its last
	// answer, the app publishes "shutdown" (with SUFFIX), waits for the
	// broker's reply and exits 0, as an app whose shutdown flushes a queue
	// does. The call is made only once keploy stops the app.
	// IGNORE_INT: the app ignores SIGINT, as one with no graceful stop does
	// (a container's init with no handler for it): only a kill ends it.
	if os.Getenv("IGNORE_INT") != "" {
		signal.Ignore(syscall.SIGINT)
	}
	if os.Getenv("SHUTDOWN_PUBLISH") == "" {
		log.Fatal(http.ListenAndServe(":8080", nil))
	}
	stop, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	srv := &http.Server{Addr: ":8080"}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()
	<-stop.Done()
	// ONE_SIGNAL: once stopped, the app lets a second signal end it, as Go's
	// signal.NotifyContext documents (its stop called): a second signal cuts
	// its shutdown.
	if os.Getenv("ONE_SIGNAL") != "" {
		cancel()
	}
	_ = srv.Shutdown(context.Background())
	// HANG_ON_STOP: once stopped, the app never ends, and makes no progress
	// on its way out.
	if os.Getenv("HANG_ON_STOP") != "" {
		time.Sleep(24 * time.Hour)
	}
	// SHUTDOWN_DELAY: its shutdown takes this long before it publishes.
	if d, err := time.ParseDuration(os.Getenv("SHUTDOWN_DELAY")); err == nil {
		time.Sleep(d)
	}
	done := make(chan struct{})
	publish(broker, "shutdown"+os.Getenv("SUFFIX"), done)
	log.Printf("shutdown published")
}
