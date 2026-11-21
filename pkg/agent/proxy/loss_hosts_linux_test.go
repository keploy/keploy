//go:build linux

package proxy

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/models"
)

// lossLogged fails t if any log form of losses (keploy's CLI logs them so:
// models.CaptureLosses.LogText, NotJudgedLogText) names host whole, or does
// not name it as want: its shape beside the tag the agent's own lines name
// it by. The report's text (Text) keeps it whole.
func lossLogged(t *testing.T, what string, losses models.CaptureLosses, host, want string) {
	t.Helper()
	name := strings.TrimSuffix(strings.Split(host, ":")[0], ".")
	for form, text := range map[string]string{"LogText": losses.LogText(), "NotJudgedLogText": losses.NotJudgedLogText()} {
		if strings.Contains(text, name) {
			t.Errorf("%s: its %s names %q whole: %q", what, form, name, text)
		}
		if !strings.Contains(text, want) {
			t.Errorf("%s: its %s does not name the host as the agent's lines do, %q: %q", what, form, want, text)
		}
	}
	if !strings.Contains(losses.Text(), host) {
		t.Errorf("%s: the report's text lost %q: %q", what, host, losses.Text())
	}
}

// lossOf is p's count of kind, as the agent hands it to keploy's CLI.
func lossOf(p *Proxy, kind models.CaptureLossKind) models.CaptureLosses {
	for _, l := range p.CaptureLosses() {
		if l.Kind == kind {
			return models.CaptureLosses{l}
		}
	}
	return nil
}

// Each loss the agent counts whose What names a host, in a form real traffic
// uses, is logged by keploy's CLI with that host by its shape and the tag
// the agent's own lines name it by, never whole:
//   - a DNS question no mock answers while test workers are served
//     (sendDNSMiss), a mongodb+srv client's SRV name, labels with '_';
//   - a stopped worker's miss (stoppedWorkerMissWhat), a compose service
//     called by its Host header alone, one label and no port;
//   - a client-side TLS handshake that failed (clientTLS) to a CONNECT
//     target that is a pod's DNS name, a label of digits and dashes.
//
// Guessing hosts from the sentence kept all three whole.
func TestEachLossTheAgentCountsNamesItsHostByItsShapeAndTag(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	p, _ := storeProxy(t, ipv4Dest(t, "127.0.0.1:50213"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.StartErrorDrain(ctx)
	keys := models.NewLogKeySession()
	conn := models.WithLogKeySession(context.Background(), keys)

	srv := "_mongodb._tcp.cluster0.ab1cd.mongodb.net."
	servedWorker(t, p, 1)
	p.ContinueTestErrorCapture()
	p.resolveUncachedDNSResponse(dns.Question{Name: srv, Qtype: dns.TypeSRV, Qclass: dns.ClassINET}, models.MODE_TEST, true, time.Now(), nil)
	lossLogged(t, "a DNS miss while workers are served", lossOf(p, models.CaptureLossUnattributed), srv, p.dnsKeys().HostLogName(srv))

	servedWorker(t, p, 2)
	if err := p.StopServingInstance(2); err != nil {
		t.Fatal(err)
	}
	report := &models.MockMismatchReport{Protocol: "HTTP", ActualSummary: "GET /orders", Destination: "orders-svc"}
	p.sendMockNotFoundErrorFrom(conn, models.NewMockMismatchError(models.ErrNoMockMatched, report), 4242, 2, nil)
	lossLogged(t, "a stopped worker's miss", lossOf(p, models.CaptureLossUnattributed), "orders-svc", keys.HostLogName("orders-svc"))

	pod := "10-0-0-1.default.pod.cluster.local:443"
	(&clientTLS{p: p, ctx: conn, dst: pod}).HandshakeFailed(errors.New("remote error: tls: bad certificate"))
	lossLogged(t, "a failed TLS handshake", lossOf(p, models.CaptureLossTLSAbort), pod, keys.HostLogName(pod))
}

// A one-label host is often one of keploy's own words (a compose service
// called worker, test or process). Each loss the agent counts lists it where
// its What writes it, so the report's and the console's text tag that place
// alone, and keploy's CLI logs it by its shape and tag there alone; keploy's
// words around it ("of test worker instance 2", "the worker ended") stay as
// they are. Listed by name, each was tagged at keploy's words too.
func TestEachLossNamesAHostThatIsOneOfKeploysWordsThereAlone(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	p, _ := storeProxy(t, ipv4Dest(t, "127.0.0.1:50214"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.StartErrorDrain(ctx)
	keys := models.NewLogKeySession()
	conn := models.WithLogKeySession(context.Background(), keys)
	once := func(what string, losses models.CaptureLosses, tag string, kept ...string) {
		t.Helper()
		if len(losses) != 1 {
			t.Fatalf("%s: no loss counted", what)
		}
		for form, text := range map[string]string{"Text": losses.Text(), "NotJudgedText": losses.NotJudgedText()} {
			if n := strings.Count(text, models.HostTagLabel(tag)); n != 1 || strings.Count(text, "[host tag") != 1 {
				t.Errorf("%s: its %s tags %d places, want the one its What writes the host at: %q", what, form, strings.Count(text, "[host tag"), text)
			}
		}
		for form, text := range map[string]string{"LogText": losses.LogText(), "NotJudgedLogText": losses.NotJudgedLogText()} {
			if n := strings.Count(text, tag); n != 1 {
				t.Errorf("%s: its %s names %d places by the host's tag, want one: %q", what, form, n, text)
			}
			for _, k := range kept {
				if !strings.Contains(text, k) {
					t.Errorf("%s: its %s lost keploy's words %q: %q", what, form, k, text)
				}
			}
		}
	}

	servedWorker(t, p, 1)
	p.ContinueTestErrorCapture()
	p.resolveUncachedDNSResponse(dns.Question{Name: "worker.", Qtype: dns.TypeA, Qclass: dns.ClassINET}, models.MODE_TEST, true, time.Now(), nil)
	once("a DNS miss for worker.", lossOf(p, models.CaptureLossUnattributed), p.dnsKeys().HostTag("worker."), "asked while test workers are served", "the test worker that asked it")

	if err := p.refuseUnserved(conn, &agent.NetworkAddress{KernelPid: 9, Instance: 2}, "worker:8080", errors.New("instance 2 is not served")); err == nil {
		t.Fatal("refuseUnserved returned no error")
	}
	once("a refused connection to worker:8080", lossOf(p, models.CaptureLossUnattributed), keys.HostTag("worker"), "of test worker instance 2", "(the worker ended")

	servedWorker(t, p, 3)
	if err := p.StopServingInstance(3); err != nil {
		t.Fatal(err)
	}
	report := &models.MockMismatchReport{Protocol: "HTTP", ActualSummary: "GET /jobs", Destination: "test"}
	p.sendMockNotFoundErrorFrom(conn, models.NewMockMismatchError(models.ErrNoMockMatched, report), 4243, 3, nil)
	once("a stopped worker's miss to test", lossOf(p, models.CaptureLossUnattributed), keys.HostTag("test"), "of ended test worker 3", "serving that worker")

	(&clientTLS{p: p, ctx: conn, dst: "process:443"}).HandshakeFailed(errors.New("remote error: tls: bad certificate"))
	once("a failed TLS handshake to process:443", lossOf(p, models.CaptureLossTLSAbort), keys.HostTag("process"), "TLS handshake(s)")
}
