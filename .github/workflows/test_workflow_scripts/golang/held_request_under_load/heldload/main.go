// Command heldload is the held-request-under-load lane's three programs:
//
//	heldload -upstream ADDR
//	    answers GET /PATH with "up:/PATH".
//	heldload -app ADDR UPSTREAM
//	    the application keploy records:
//	      GET /fast?id=N  calls the upstream for /fast/N/a and /fast/N/b and
//	                      answers with what it got;
//	      GET /held?id=N&calls=C&hold=S
//	                      calls the upstream C times (/held/N/0 ...), sends its
//	                      answer's head, then one line a second for S seconds:
//	                      a request in flight for longer than the 7 s after
//	                      which keploy drops a call no request holds, its calls
//	                      all made before its head.
//	heldload -drive APP FAST WORKERS HELD CALLS HOLD RATE
//	    sends /fast requests from WORKERS workers, RATE a second at most
//	    between them all, at least FAST of them and until the last held
//	    request has answered, and beside them HELD /held requests one after
//	    the other: held k starts once k*FAST/(HELD+1) fast ones have been sent
//	    and the one before it has answered. So fast requests are sent through
//	    the whole of every held one, past the 7 s after its first calls, and
//	    how many go depends on how long the held ones take, not on how fast
//	    the machine sends them. It prints how many of each it sent and fails
//	    on any answer that is not a 200 with the body it expects.
package main

import (
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

func main() {
	upstream := flag.String("upstream", "", "serve the upstream on this address")
	app := flag.String("app", "", "serve the application on this address")
	drive := flag.String("drive", "", "drive the application at this address")
	flag.Parse()
	switch {
	case *upstream != "":
		serveUpstream(*upstream)
	case *app != "" && flag.NArg() == 1:
		serveApp(*app, flag.Arg(0))
	case *drive != "" && flag.NArg() == 6:
		n := make([]int, 6)
		for i := range n {
			v, err := strconv.Atoi(flag.Arg(i))
			if err != nil || v < 0 || i == 5 && v == 0 {
				fmt.Fprintln(os.Stderr, "heldload -drive APP FAST WORKERS HELD CALLS HOLD RATE (RATE > 0)")
				os.Exit(2)
			}
			n[i] = v
		}
		os.Exit(driveApp(*drive, n[0], n[1], n[2], n[3], n[4], n[5]))
	default:
		fmt.Fprintln(os.Stderr, "heldload -upstream ADDR | -app ADDR UPSTREAM | -drive APP FAST WORKERS HELD CALLS HOLD RATE")
		os.Exit(2)
	}
}

func serveUpstream(addr string) {
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "up:%s", r.URL.Path)
	})
	fmt.Fprintln(os.Stderr, http.ListenAndServe(addr, nil))
	os.Exit(1)
}

func serveApp(addr, upstream string) {
	client := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{MaxIdleConnsPerHost: 64}}
	get := func(path string) (string, error) {
		resp, err := client.Get("http://" + upstream + path)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			return "", err
		}
		if resp.StatusCode != http.StatusOK {
			return "", fmt.Errorf("upstream %s: %s", path, resp.Status)
		}
		return string(b), nil
	}
	http.HandleFunc("/fast", func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("id")
		a, err := get("/fast/" + id + "/a")
		if err == nil {
			var b string
			if b, err = get("/fast/" + id + "/b"); err == nil {
				fmt.Fprintf(w, "fast %s: %s %s", id, a, b)
				return
			}
		}
		http.Error(w, err.Error(), http.StatusBadGateway)
	})
	http.HandleFunc("/held", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		id := q.Get("id")
		calls, _ := strconv.Atoi(q.Get("calls"))
		hold, _ := strconv.Atoi(q.Get("hold"))
		// Four at a time: the upstream calls are what keploy buffers for this
		// request while it is in flight.
		var failed atomic.Value
		var wg sync.WaitGroup
		next := atomic.Int64{}
		for range 4 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					i := next.Add(1) - 1
					if i >= int64(calls) {
						return
					}
					if _, err := get(fmt.Sprintf("/held/%s/%d", id, i)); err != nil {
						failed.Store(err.Error())
						return
					}
				}
			}()
		}
		wg.Wait()
		if e, ok := failed.Load().(string); ok {
			http.Error(w, e, http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		fmt.Fprintf(w, "held %s: %d calls\n", id, calls)
		for s := range hold {
			if fl != nil {
				fl.Flush()
			}
			time.Sleep(time.Second)
			fmt.Fprintf(w, "second %d\n", s+1)
		}
	})
	fmt.Fprintln(os.Stderr, http.ListenAndServe(addr, nil))
	os.Exit(1)
}

func driveApp(app string, fast, workers, held, calls, hold, rate int) int {
	client := &http.Client{Timeout: time.Duration(hold+120) * time.Second}
	var bad atomic.Int64
	check := func(path, want string, prefix bool) {
		resp, err := client.Get("http://" + app + path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", path, err)
			bad.Add(1)
			return
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		got := string(b)
		ok := err == nil && resp.StatusCode == http.StatusOK && (got == want || prefix && strings.HasPrefix(got, want))
		if !ok {
			fmt.Fprintf(os.Stderr, "%s: %s %q (read: %v), want %q\n", path, resp.Status, got, err, want)
			bad.Add(1)
		}
	}
	// Every id taken is a request sent: sent is how many went. The fast
	// requests go out at most rate a second, all workers together.
	var sent atomic.Int64
	var heldDone atomic.Bool
	pace := time.NewTicker(time.Second / time.Duration(rate))
	defer pace.Stop()
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for sent.Load() < int64(fast) || !heldDone.Load() {
				<-pace.C
				id := sent.Add(1)
				check(fmt.Sprintf("/fast?id=%d", id), fmt.Sprintf("fast %d: up:/fast/%d/a up:/fast/%d/b", id, id, id), false)
			}
		}()
	}
	// The held requests, one after the other, each once its share of the
	// fast ones has gone out. The fast ones go on until the last has
	// answered, so each is in flight beside them from its first call to its
	// answer's end.
	heldSent := 0
	for k := 1; k <= held; k++ {
		for sent.Load() < int64(k*fast/(held+1)) {
			time.Sleep(10 * time.Millisecond)
		}
		var want strings.Builder
		fmt.Fprintf(&want, "held %d: %d calls\n", k, calls)
		for s := range hold {
			fmt.Fprintf(&want, "second %d\n", s+1)
		}
		check(fmt.Sprintf("/held?id=%d&calls=%d&hold=%d", k, calls, hold), want.String(), false)
		heldSent++
	}
	heldDone.Store(true)
	wg.Wait()
	fmt.Printf("sent fast=%d held=%d bad=%d\n", sent.Load(), heldSent, bad.Load())
	if bad.Load() != 0 {
		return 1
	}
	return 0
}
