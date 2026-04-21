// Command e2e-agent-ready-harness is a minimal wrapper around the
// pkg/agent/routes MakeAgentReady HTTP handler used by the e2e test in
// tests/e2e/agent-ready-gated-on-ca. It intentionally does NOT spin up
// the full keploy agent (eBPF hooks, proxy, memory guard, ...) because
// all that machinery needs CAP_SYS_ADMIN / privileged containers and is
// orthogonal to what this regression test actually checks: that
// /agent/ready refuses to write /tmp/agent.ready until the CA-bundle
// signal has fired.
//
// The harness binds the MakeAgentReady handler on a configurable port
// and, after a configurable delay, triggers the CAReady signal via the
// tls.CloseCAReadyForTest helper — simulating SetupCA finishing.
package main

import (
	"flag"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/go-chi/chi/v5"
	"go.keploy.io/server/v3/pkg/agent/proxy/tls"
	"go.keploy.io/server/v3/pkg/agent/routes"
	"go.uber.org/zap"
)

func main() {
	addr := flag.String("addr", ":8086", "listen address")
	caDelay := flag.Duration("ca-delay", 5*time.Second, "how long to wait before signalling CAReady")
	flag.Parse()

	logger, err := zap.NewProduction()
	if err != nil {
		log.Fatalf("logger: %v", err)
	}

	// Start unlatched — the /agent/ready handler must observe the
	// "CA not ready" state until caDelay elapses.
	tls.ResetCAReadyForTest()

	go func() {
		time.Sleep(*caDelay)
		logger.Info("harness: signalling CAReady after delay", zap.Duration("delay", *caDelay))
		tls.CloseCAReadyForTest()
	}()

	r := chi.NewRouter()
	// Mount the real DefaultRoutes so we exercise the production
	// routing (and the real MakeAgentReady handler) end-to-end.
	routes.ActiveHooks.New(r, nil, logger)

	logger.Info("harness: listening", zap.String("addr", *addr))
	srv := &http.Server{
		Addr:              *addr,
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
	}
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Fatal("listen", zap.Error(err))
		os.Exit(1)
	}
}
