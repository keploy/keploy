// Command harness is every process of the IPv6-only Kubernetes e2e
// (tests/e2e/ipv6-only-kind/run.sh) but keploy's agent, by its first argument:
//
//	harness dep ADDR TOKEN   the dependency: GET /name?n=N answers TOKEN-N.
//	harness app ADDR DEP_URL the application: GET /greet?n=N calls
//	                         DEP_URL?n=N over a new connection and answers
//	                         "hello " and what the dependency said.
//	harness addrs            prints how many addresses of each kind this
//	                         network namespace's up, non-loopback interfaces
//	                         have: "ipv4 N", "ipv6-routable N",
//	                         "ipv6-link-local N".
//	harness record AGENT_IP APP_URL N FILE
//	                         keploy's client, as Kubernetes tooling drives a
//	                         sidecar agent (go.keploy.io/server/v3/pkg/platform/http):
//	                         it checks that the agent at AGENT_IP runs with
//	                         its own proven switch (KEPLOY_PROVEN) and prints
//	                         "proven on" or "proven off", starts its recording
//	                         (its incoming test cases and outgoing mocks),
//	                         calls APP_URL/greet?n=1..N, waits for a test case
//	                         and a mock for each, and writes them to FILE.
//	harness replay AGENT_IP APP_URL FILE
//	                         keploy's client again: it checks the agent's
//	                         proven switch as record does, hands the agent the
//	                         mocks in FILE, turns mocking on, sends each test
//	                         case's request to APP_URL and compares the answer
//	                         with the recorded one. It exits 1 if any differs.
//	harness idle             does nothing until it is stopped.
//
// It prints one line per fact the script checks, and nothing of keploy's own
// log but its errors.
package main

import (
	"bytes"
	"context"
	"encoding/gob"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	kphttp "go.keploy.io/server/v3/pkg/platform/http"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

// agentPort is the port the agent's API listens on (run.sh starts it with
// --port 16000).
const agentPort = "16000"

// recordWait bounds the wait for the recording of the calls made: each call
// has answered by then, so its test case and mock are on their way.
const recordWait = 2 * time.Minute

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	args := os.Args[2:]
	switch os.Args[1] {
	case "dep":
		need(args, 2)
		dep(args[0], args[1])
	case "app":
		need(args, 2)
		app(args[0], args[1])
	case "addrs":
		addrs()
	case "record":
		need(args, 4)
		n, err := strconv.Atoi(args[2])
		if err != nil || n < 1 {
			fatal("record: N must be a positive number, not %q", args[2])
		}
		record(args[0], args[1], n, args[3])
	case "replay":
		need(args, 3)
		replay(args[0], args[1], args[2])
	case "idle":
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
		<-sig
	default:
		usage()
	}
}

func usage() {
	fatal("usage: harness dep ADDR TOKEN | app ADDR DEP_URL | addrs | record AGENT_IP APP_URL N FILE | replay AGENT_IP APP_URL FILE | idle")
}

func need(args []string, n int) {
	if len(args) != n {
		usage()
	}
}

func fatal(format string, a ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", a...)
	os.Exit(2)
}

// queryN is the whole number a request's ?n= names. A request whose n is not
// one is answered 400 with a fixed text, and false returned: the dependency
// and the application answer with numbers they parsed, never the request's
// own bytes.
func queryN(w http.ResponseWriter, r *http.Request) (int, bool) {
	n, err := strconv.Atoi(r.URL.Query().Get("n"))
	if err != nil {
		http.Error(w, "n must be a whole number", http.StatusBadRequest)
		return 0, false
	}
	return n, true
}

// plainText marks w's answer as plain text that a browser never reads as
// HTML, as http.Error does for an error's.
func plainText(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
}

func dep(addr, token string) {
	http.HandleFunc("/name", func(w http.ResponseWriter, r *http.Request) {
		n, ok := queryN(w, r)
		if !ok {
			return
		}
		plainText(w)
		_, _ = io.WriteString(w, token+"-"+strconv.Itoa(n))
	})
	fatal("dep: %v", http.ListenAndServe(addr, nil))
}

func app(addr, depURL string) {
	// A connection per call: each call is a connection of its own to
	// redirect, record and replay.
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	http.HandleFunc("/greet", func(w http.ResponseWriter, r *http.Request) {
		n, ok := queryN(w, r)
		if !ok {
			return
		}
		res, err := client.Get(depURL + "?n=" + strconv.Itoa(n))
		if err != nil {
			http.Error(w, "the dependency did not answer: "+err.Error(), http.StatusBadGateway)
			return
		}
		defer func() { _ = res.Body.Close() }()
		body, err := io.ReadAll(res.Body)
		if err != nil || res.StatusCode != http.StatusOK {
			http.Error(w, fmt.Sprintf("the dependency answered %d: %s %v", res.StatusCode, body, err), http.StatusBadGateway)
			return
		}
		plainText(w)
		_, _ = io.WriteString(w, "hello "+string(body))
	})
	fatal("app: %v", http.ListenAndServe(addr, nil))
}

func addrs() {
	ifaces, err := net.Interfaces()
	if err != nil {
		fatal("addrs: %v", err)
	}
	var v4, v6, ll int
	for _, i := range ifaces {
		if i.Flags&net.FlagUp == 0 || i.Flags&net.FlagLoopback != 0 {
			continue
		}
		as, err := i.Addrs()
		if err != nil {
			fatal("addrs: %v", err)
		}
		for _, a := range as {
			ipn, ok := a.(*net.IPNet)
			switch {
			case !ok:
			case ipn.IP.To4() != nil:
				v4++
			case ipn.IP.IsLinkLocalUnicast():
				ll++
			case ipn.IP.IsGlobalUnicast():
				v6++
			}
		}
	}
	fmt.Printf("ipv4 %d\nipv6-routable %d\nipv6-link-local %d\n", v4, v6, ll)
}

// listenWait bounds the wait for the application to take connections.
const listenWait = time.Minute

// waitListening waits until the application at appURL answers a request,
// whatever it answers: a Pod that is ready may not listen yet.
func waitListening(appURL string) {
	hc := &http.Client{Timeout: 5 * time.Second}
	deadline := time.Now().Add(listenWait)
	for {
		res, err := hc.Get(appURL + "/listening")
		if err == nil {
			_ = res.Body.Close()
			return
		}
		if time.Now().After(deadline) {
			fatal("the application did not take a connection within %s: %v", listenWait, err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// recording is what record writes and replay reads.
type recording struct {
	TestCases []*models.TestCase
	Mocks     []*models.Mock
}

// agentClient is keploy's client of the agent whose Pod IP is ip.
func agentClient(ip string) *kphttp.AgentClient {
	logger, err := zap.NewProduction(zap.IncreaseLevel(zap.ErrorLevel))
	if err != nil {
		fatal("logger: %v", err)
	}
	cfg := config.New()
	cfg.Agent.AgentURI = "http://" + net.JoinHostPort(ip, agentPort) + "/agent"
	return kphttp.New(logger, nil, cfg)
}

// checkSwitch fails unless the agent whose Pod IP is ip runs with this
// process's proven switch, as keploy's client refuses an agent that does not
// (provenswitch.CheckAgent, from the switch the agent reports on its health
// check), and prints it: "proven on" or "proven off".
func checkSwitch(ip string) {
	hc := &http.Client{Timeout: 10 * time.Second}
	res, err := hc.Get("http://" + net.JoinHostPort(ip, agentPort) + "/agent/health")
	if err != nil {
		fatal("the agent's health check: %v", err)
	}
	_, _ = io.Copy(io.Discard, res.Body)
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK {
		fatal("the agent's health check answered %d", res.StatusCode)
	}
	if err := provenswitch.CheckAgent(res.Header.Get(provenswitch.HealthHeader)); err != nil {
		fatal("%v", err)
	}
	fmt.Println("proven", provenswitch.State())
}

// callNumber is the n of a request URL's ?n=, -1 for none.
func callNumber(raw string) int {
	u, err := url.Parse(raw)
	if err != nil {
		return -1
	}
	n, err := strconv.Atoi(u.Query().Get("n"))
	if err != nil {
		return -1
	}
	return n
}

func record(agentIP, appURL string, n int, file string) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	g, gctx := errgroup.WithContext(ctx)
	sctx := context.WithValue(gctx, models.ErrGroupKey, g)
	client := agentClient(agentIP)
	checkSwitch(agentIP)

	// The recording first: until it starts, the agent serves none of the
	// application's incoming calls (they wait for it).
	tcs, err := client.GetIncoming(sctx, models.IncomingOptions{})
	if err != nil {
		fatal("record: the agent did not start recording test cases: %v", err)
	}
	mocks, err := client.GetOutgoing(sctx, models.OutgoingOptions{})
	if err != nil {
		fatal("record: the agent did not start recording mocks: %v", err)
	}
	fmt.Println("recording")

	type got struct {
		tc   *models.TestCase
		mock *models.Mock
	}
	ch := make(chan got, 1024) // read once the calls are made
	go func() {
		for tc := range tcs {
			ch <- got{tc: tc}
		}
	}()
	go func() {
		for m := range mocks {
			ch <- got{mock: m}
		}
	}()

	waitListening(appURL)
	hc := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	for i := 1; i <= n; i++ {
		res, err := hc.Get(fmt.Sprintf("%s/greet?n=%d", appURL, i))
		if err != nil {
			fmt.Printf("call %d error %v\n", i, err)
			continue
		}
		body, _ := io.ReadAll(res.Body)
		_ = res.Body.Close()
		fmt.Printf("call %d %d %s\n", i, res.StatusCode, body)
	}

	var rec recording
	testOf, mockOf := map[int]bool{}, map[int]bool{}
	deadline := time.After(recordWait)
	for len(testOf) < n || len(mockOf) < n {
		select {
		case <-deadline:
			fmt.Printf("record: gave up after %s with %d of %d test cases and %d of %d mocks of the dependency's call\n", recordWait, len(testOf), n, len(mockOf), n)
			write(file, rec)
			os.Exit(1)
		case x := <-ch:
			switch {
			case x.tc != nil:
				rec.TestCases = append(rec.TestCases, x.tc)
				k := callNumber(x.tc.HTTPReq.URL)
				if k >= 1 && k <= n && !testOf[k] {
					testOf[k] = true
					fmt.Printf("testcase %d %s %d %s\n", k, x.tc.HTTPReq.Method, x.tc.HTTPResp.StatusCode, x.tc.HTTPResp.Body)
				}
			case x.mock != nil:
				rec.Mocks = append(rec.Mocks, x.mock)
				k := -1
				if x.mock.Kind == models.HTTP && x.mock.Spec.HTTPReq != nil && strings.HasPrefix(pathOf(x.mock.Spec.HTTPReq.URL), "/name") {
					k = callNumber(x.mock.Spec.HTTPReq.URL)
				}
				if k >= 1 && k <= n && !mockOf[k] && x.mock.Spec.HTTPResp != nil {
					mockOf[k] = true
					fmt.Printf("mock %d Http %s\n", k, x.mock.Spec.HTTPResp.Body)
				} else {
					fmt.Printf("mock - %s\n", x.mock.Kind)
				}
			}
		}
	}
	write(file, rec)
	fmt.Printf("recorded %d test cases and %d mocks\n", len(rec.TestCases), len(rec.Mocks))
}

func pathOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Path
}

func write(file string, rec recording) {
	var b bytes.Buffer
	if err := gob.NewEncoder(&b).Encode(rec); err != nil {
		fatal("record: encode the recording: %v", err)
	}
	if err := os.WriteFile(file, b.Bytes(), 0o600); err != nil {
		fatal("record: write the recording: %v", err)
	}
}

func replay(agentIP, appURL, file string) {
	raw, err := os.ReadFile(file)
	if err != nil {
		fatal("replay: read the recording: %v", err)
	}
	var rec recording
	if err := gob.NewDecoder(bytes.NewReader(raw)).Decode(&rec); err != nil {
		fatal("replay: decode the recording: %v", err)
	}
	ctx := context.Background()
	client := agentClient(agentIP)
	checkSwitch(agentIP)

	// As keploy's replay hands a test set's mocks to its agent: the session
	// and connection mocks unfiltered, every other one per test.
	var filtered, unfiltered []*models.Mock
	for i, m := range rec.Mocks {
		// Named as keploy's mock store names them as it writes them: the
		// agent keeps one mock per name.
		m.Name = fmt.Sprint("mock-", i+1)
		m.DeriveLifetime()
		switch m.TestModeInfo.Lifetime {
		case models.LifetimeSession, models.LifetimeConnection:
			unfiltered = append(unfiltered, m)
		default:
			filtered = append(filtered, m)
		}
	}
	if err := client.MockOutgoing(ctx, models.OutgoingOptions{StartsTestSet: true, Mocking: true}); err != nil {
		fatal("replay: the agent did not turn mocking on: %v", err)
	}
	if err := client.StoreMocks(ctx, filtered, unfiltered); err != nil {
		fatal("replay: the agent did not take the mocks: %v", err)
	}
	// keploy test's first, wide window (the set's start-up), then one per
	// test case: its recorded request and response times, which bound the
	// mocks recorded for it.
	if err := client.UpdateMockParams(ctx, models.MockFilterParams{AfterTime: models.BaseTime, BeforeTime: time.Now(), MockMapping: []string{}, TotalConsumedMocks: map[string]models.MockState{}}); err != nil {
		fatal("replay: the agent did not arm the mocks: %v", err)
	}
	fmt.Printf("replaying %d test cases against %d mocks\n", len(rec.TestCases), len(rec.Mocks))
	waitListening(appURL)

	sort.Slice(rec.TestCases, func(i, j int) bool {
		return callNumber(rec.TestCases[i].HTTPReq.URL) < callNumber(rec.TestCases[j].HTTPReq.URL)
	})
	hc := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	failed := 0
	for _, tc := range rec.TestCases {
		k := callNumber(tc.HTTPReq.URL)
		u, err := url.Parse(tc.HTTPReq.URL)
		if err != nil || k < 1 {
			continue // not one of the calls made (none is expected)
		}
		if err := client.UpdateMockParams(ctx, models.MockFilterParams{AfterTime: tc.HTTPReq.Timestamp, BeforeTime: tc.HTTPResp.Timestamp, TotalConsumedMocks: map[string]models.MockState{}}); err != nil {
			fatal("replay: the agent did not arm test %d's mocks: %v", k, err)
		}
		req, err := http.NewRequest(string(tc.HTTPReq.Method), appURL+u.RequestURI(), strings.NewReader(tc.HTTPReq.Body))
		if err != nil {
			fatal("replay: build test %d's request: %v", k, err)
		}
		res, err := hc.Do(req)
		if err != nil {
			fmt.Printf("test %d fail: %v\n", k, err)
			failed++
			continue
		}
		body, _ := io.ReadAll(res.Body)
		_ = res.Body.Close()
		if res.StatusCode != tc.HTTPResp.StatusCode || string(body) != tc.HTTPResp.Body {
			fmt.Printf("test %d fail: got %d %q, recorded %d %q\n", k, res.StatusCode, body, tc.HTTPResp.StatusCode, tc.HTTPResp.Body)
			failed++
			continue
		}
		fmt.Printf("test %d pass %d %s\n", k, res.StatusCode, body)
	}
	if consumed, err := client.GetConsumedMocks(ctx); err == nil {
		fmt.Printf("consumed %d mocks\n", len(consumed))
	} else {
		fmt.Printf("consumed unknown: %v\n", err)
	}
	if failed > 0 {
		os.Exit(1)
	}
}
