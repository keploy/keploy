package proxy

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
)

func TestNoInterceptSet_SeedsFromKubernetesServiceHost(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.96.0.1")
	t.Setenv("KEPLOY_NO_INTERCEPT_HOSTS", "")
	t.Setenv("KEPLOY_DISABLE_APISERVER_BYPASS", "")

	s := newNoInterceptSet(zap.NewNop())
	if !s.matches("10.96.0.1") {
		t.Error("the API server ClusterIP must be relayed without interception")
	}
	if s.matches("10.96.0.2") {
		t.Error("an unrelated address must still be intercepted")
	}
}

// The kill switch has to work, because an operator who wants API-server mocks
// should be able to ask for them.
func TestNoInterceptSet_KillSwitch(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.96.0.1")
	t.Setenv("KEPLOY_NO_INTERCEPT_HOSTS", "")
	t.Setenv("KEPLOY_DISABLE_APISERVER_BYPASS", "true")

	if newNoInterceptSet(zap.NewNop()).matches("10.96.0.1") {
		t.Error("KEPLOY_DISABLE_APISERVER_BYPASS=true must re-enable interception")
	}
}

func TestNoInterceptSet_ExtraHostsAndCIDRs(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KEPLOY_DISABLE_APISERVER_BYPASS", "")
	t.Setenv("KEPLOY_NO_INTERCEPT_HOSTS", "192.168.5.5, 172.20.0.0/16 , fd00::1")

	s := newNoInterceptSet(zap.NewNop())
	for _, in := range []string{"192.168.5.5", "172.20.3.9", "fd00::1"} {
		if !s.matches(in) {
			t.Errorf("%s should be relayed without interception", in)
		}
	}
	for _, out := range []string{"192.168.5.6", "172.21.0.1", "fd00::2"} {
		if s.matches(out) {
			t.Errorf("%s should still be intercepted", out)
		}
	}
}

// Nothing configured must leave every destination intercepted — the fix must
// not quietly widen into a general capture hole.
func TestNoInterceptSet_EmptyMatchesNothing(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KEPLOY_NO_INTERCEPT_HOSTS", "")
	t.Setenv("KEPLOY_DISABLE_APISERVER_BYPASS", "")

	s := newNoInterceptSet(zap.NewNop())
	for _, h := range []string{"10.96.0.1", "1.2.3.4", "example.com", ""} {
		if s.matches(h) {
			t.Errorf("empty set must not match %q", h)
		}
	}
	var nilSet *noInterceptSet
	if nilSet.matches("10.96.0.1") {
		t.Error("a nil set must not match")
	}
}

// A hostname is never matched: the set is IP-only by design, and the value it
// guards is reached by IP. Pinning this stops a later "helpful" DNS lookup
// per connection.
func TestNoInterceptSet_IgnoresHostnames(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KEPLOY_DISABLE_APISERVER_BYPASS", "")
	t.Setenv("KEPLOY_NO_INTERCEPT_HOSTS", "kubernetes.default.svc")

	s := newNoInterceptSet(zap.NewNop())
	if s.matches("kubernetes.default.svc") {
		t.Error("hostnames must not be matched")
	}
}

// hostFromAddr feeds matches(); it must hand over a bare host for both families
// or the lookup silently never fires.
func TestHostFromAddr_StripsPortAndBrackets(t *testing.T) {
	for addr, want := range map[string]string{
		"10.96.0.1:443": "10.96.0.1",
		"[fd00::1]:443": "fd00::1",
	} {
		if got := hostFromAddr(addr); got != want {
			t.Errorf("hostFromAddr(%q) = %q, want %q", addr, got, want)
		}
	}
}

// A no-intercept destination that blackholes its SYN must not pin the
// connection goroutine indefinitely. This is exactly what happens when the
// pod runs under an egress-deny NetworkPolicy — keploy's own replay sandbox
// applies one — and before the bound, the dial sat in kernel SYN-retry with
// no log line before it and the error line after it unreachable, so the hang
// produced zero evidence.
func TestNoInterceptDial_IsBounded(t *testing.T) {
	blackhole := func(ctx context.Context, addr string) (net.Conn, error) {
		<-ctx.Done() // a dropped SYN returns only when the dial context dies
		return nil, ctx.Err()
	}
	done := make(chan error, 1)
	go func() {
		_, err := dialNoIntercept(context.Background(), zap.NewNop(), "203.0.113.9:443", blackhole)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a blackholed dial returned no error")
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("expected a deadline error naming the bound, got: %v", err)
		}
	case <-time.After(noInterceptDialTimeout + 5*time.Second):
		t.Fatal("the no-intercept dial is not bounded: it outlived its own timeout budget")
	}
}

// The bound must cost a healthy destination nothing: an immediately-accepting
// dial comes back with its connection and no error.
func TestNoInterceptDial_HealthyDestinationUnaffected(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	conn, err := dialNoIntercept(context.Background(), zap.NewNop(), "10.96.0.1:443", func(ctx context.Context, addr string) (net.Conn, error) {
		return c1, nil
	})
	if err != nil {
		t.Fatalf("healthy dial failed: %v", err)
	}
	if conn != c1 {
		t.Fatal("helper did not hand back the dialed connection")
	}
}

// A dial failure that merely RACES the budget must come back as itself, not as
// the egress-policy hint: a refused connection at second 15 has nothing to do
// with NetworkPolicy, and the hint would send the operator to the wrong system.
func TestNoInterceptDial_DoesNotRelabelForeignErrorsAsTimeout(t *testing.T) {
	refused := errors.New("connect: connection refused")
	parent, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	dial := func(ctx context.Context, addr string) (net.Conn, error) {
		<-ctx.Done() // the budget has expired by the time the error surfaces
		return nil, refused
	}
	_, err := dialNoIntercept(parent, zap.NewNop(), "10.96.0.1:443", dial)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !errors.Is(err, refused) {
		t.Fatalf("the dial's own error must survive, got: %v", err)
	}
	if strings.Contains(err.Error(), "egress policy") {
		t.Fatalf("a non-deadline failure must not carry the egress-policy hint: %v", err)
	}
}
