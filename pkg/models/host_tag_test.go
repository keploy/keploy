package models

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"reflect"
	"strings"
	"syscall"
	"testing"
)

// A host name is written by its labels' shape beside its tag under the
// session's key: one tag for a name however it is written (its case, a
// trailing dot, a port), another for another name or under another session,
// and no tag a reader can compute (it is not the process's). An IP address,
// which is no name the app chose, is written whole, with no tag.
func TestAHostIsNamedByItsShapeAndItsSessionsTag(t *testing.T) {
	session := StartLogKeySession(context.Background())
	tag := HostTag(session, "api.example.com")
	if !pathTagForm.MatchString(tag) {
		t.Fatalf("HostTag = %q, want LogKey's form", tag)
	}
	for _, same := range []string{"API.Example.com", "api.example.com.", "api.example.com:8443", "API.EXAMPLE.COM.:53"} {
		if got := HostTag(session, same); got != tag {
			t.Errorf("HostTag(%q) = %q, want %q", same, got, tag)
		}
	}
	if HostTag(session, "auth.example.com") == tag || HostTag(StartLogKeySession(context.Background()), "api.example.com") == tag ||
		HostTag(context.Background(), "api.example.com") == tag {
		t.Error("another host, or another session, or the process gave api.example.com its tag")
	}
	for in, want := range map[string]string{
		"api.example.com":      "{}.{}.{} " + tag,
		"api.example.com.":     "{}.{}.{}. " + tag,
		"api.example.com:8443": "{}.{}.{}:8443 " + tag,
		"orders":               "{} " + HostTag(session, "orders"),
		"127.0.0.1":            "127.0.0.1",
		"127.0.0.1:9096":       "127.0.0.1:9096",
		"[::1]:80":             "[::1]:80",
		"fe80::1%eth0":         "fe80::1%{}",
		"":                     "",
	} {
		if got := HostLogName(session, in); got != want {
			t.Errorf("HostLogName(%q) = %q, want %q", in, got, want)
		}
		if strings.Contains(HostShape(in), "example") || strings.Contains(HostShape(in), "orders") {
			t.Errorf("HostShape(%q) = %q names the host", in, HostShape(in))
		}
	}
}

// An error a dial or a TLS handshake returns names the host it reached for
// whole ("lookup api.example.com: no such host"), and crypto/x509's names the
// certificate's own names beside it: a log line, or a caller that logs it,
// gets it with each written by its shape and tag (HostsInError), errors.Is
// and errors.As still seeing the cause. A name inside a longer name is
// another host's and is kept; an address is kept whole.
func TestAnErrorNamesItsHostsByTheirShapeAndTag(t *testing.T) {
	keys := NewLogKeySession()
	ctx := WithLogKeySession(context.Background(), keys)
	cause := &net.DNSError{Err: "no such host", Name: "API.example.com", IsNotFound: true}
	err := fmt.Errorf("dial api.example.com:443: %w", cause)
	got := HostsInError(ctx, err, "api.example.com:443")
	want := "dial {}.{}.{}:443 " + keys.HostTag("api.example.com") + ": lookup {}.{}.{} " + keys.HostTag("api.example.com") + ": no such host"
	if got.Error() != want {
		t.Fatalf("HostsInError = %q, want %q", got, want)
	}
	var dnsErr *net.DNSError
	if !errors.As(got, &dnsErr) || !errors.Is(got, cause) {
		t.Fatalf("HostsInError lost its cause: %#v", got)
	}
	if HostsInError(ctx, errors.New("dial 10.0.0.1:443: connection refused"), "10.0.0.1:443").Error() != "dial 10.0.0.1:443: connection refused" {
		t.Fatal("an address was not kept whole")
	}
	if got := HostsInText(ctx, "x.api.example.com and api.example.com.", "api.example.com"); got != "x.api.example.com and {}.{}.{} "+keys.HostTag("api.example.com")+"." {
		t.Fatalf("HostsInText = %q: want the longer name kept and the name itself shaped", got)
	}
	if HostsInError(ctx, nil, "api.example.com") != nil {
		t.Fatal("HostsInError of nil is not nil")
	}

	cert := &x509.Certificate{DNSNames: []string{"*.cdn.example.net", "cdn.example.net"}}
	herr := fmt.Errorf("tls: failed to verify certificate: %w", x509.HostnameError{Certificate: cert, Host: "tenant.example.com"})
	got = HostsInError(ctx, herr)
	for _, name := range []string{"cdn.example.net", "tenant.example.com"} {
		if strings.Contains(got.Error(), name) {
			t.Errorf("HostsInError(%q) = %q names %s whole", herr, got, name)
		}
	}
	if !strings.Contains(got.Error(), "not {}.{}.{} "+keys.HostTag("tenant.example.com")) {
		t.Errorf("HostsInError = %q: the name checked is not named by its shape and tag", got)
	}
	var hn x509.HostnameError
	if !errors.As(got, &hn) {
		t.Fatal("HostsInError lost the x509 error")
	}
}

// The console, the report and the misses file name a destination whole
// beside the host tag the agent's line named it by (DestinationNamed); a DNS
// question, which a miss names in its summary with no destination, gets the
// tag alone. keploy's CLI logs it by its shape beside the agent's tag
// (TaggedHostLogName).
func TestADestinationIsNamedWholeBesideItsHostTag(t *testing.T) {
	for _, c := range []struct{ dest, tag, want string }{
		{"api.example.com:443", "#3fa2c1d0e9b8", "  →  api.example.com:443 [host tag #3fa2c1d0e9b8]"},
		{"10.0.0.1:443", "", "  →  10.0.0.1:443"},
		{"", "#3fa2c1d0e9b8", " [host tag #3fa2c1d0e9b8]"},
		{"", "", ""},
	} {
		if got := DestinationNamed(c.dest, c.tag); got != c.want {
			t.Errorf("DestinationNamed(%q, %q) = %q, want %q", c.dest, c.tag, got, c.want)
		}
	}
	if got := TaggedHostLogName("api.example.com:443", "#3fa2c1d0e9b8"); got != "{}.{}.{}:443 #3fa2c1d0e9b8" {
		t.Errorf("TaggedHostLogName = %q", got)
	}
	if got := TaggedHostLogName("api.example.com:443", ""); got != "{}.{}.{}:443" {
		t.Errorf("TaggedHostLogName with no tag = %q", got)
	}
}

// A capture loss's What names a destination whole for the report; its log
// form names each host the agent listed (CaptureLoss.Hosts) by its shape and
// the agent's tag, with keploy's punctuation kept, and an address whole. The
// report's text names each whole beside the same tag. A kind whose What names
// no host keeps it: a container's name and image (postgres:16 reads like a
// host and its port), whether or not the agent listed its hosts.
func TestACaptureLossesLogTextNamesNoHost(t *testing.T) {
	keys := NewLogKeySession()
	tls := keys.LossText().Say("to ").Host("api.example.com:443").Say(": the client does not trust keploy's CA")
	dns := keys.LossText().Say("the latest: a DNS question for ").Host("tenant.example.com.").Say(" (A) that no DNS mock answers")
	losses := CaptureLosses{
		{Kind: CaptureLossTLSAbort, Count: 1, What: tls.String(), Hosts: tls.Hosts(), HostsListed: true},
		{Kind: CaptureLossUnattributed, Count: 1, What: dns.String(), Hosts: dns.Hosts(), HostsListed: true},
		{Kind: CaptureLossParserPanic, Count: 1, What: "the http parser, on a connection to 10.0.0.1:443", HostsListed: true},
	}
	for _, text := range []string{losses.LogText(), losses.NotJudgedLogText()} {
		for _, name := range []string{"api.example.com", "tenant.example.com"} {
			if strings.Contains(text, name) {
				t.Errorf("a log text names %s whole: %q", name, text)
			}
		}
	}
	if want := "(to {}.{}.{}:443 " + keys.HostTag("api.example.com") + ": the client"; !strings.Contains(losses.LogText(), want) || !strings.Contains(losses.LogText(), "on a connection to 10.0.0.1:443)") {
		t.Errorf("LogText = %q: want the host by its shape and tag (%q), the address whole", losses.LogText(), want)
	}
	if want := "a DNS question for {}.{}.{}. " + keys.HostTag("tenant.example.com") + " (A)"; !strings.Contains(losses.NotJudgedLogText(), want) {
		t.Errorf("NotJudgedLogText = %q: want the question's name by its shape and tag, %q", losses.NotJudgedLogText(), want)
	}
	if want := "api.example.com:443 [host tag " + keys.HostTag("api.example.com") + "]"; !strings.Contains(losses.Text(), want) {
		t.Errorf("Text = %q: want the destination whole beside its tag, %q", losses.Text(), want)
	}
	if want := "tenant.example.com. [host tag " + keys.HostTag("tenant.example.com") + "] (A)"; !strings.Contains(losses.NotJudgedText(), want) {
		t.Errorf("NotJudgedText = %q: want the question whole beside its tag, %q", losses.NotJudgedText(), want)
	}
	for _, listed := range []bool{false, true} {
		container := CaptureLosses{{Kind: CaptureLossContainerUnread, Count: 1, What: "container pg (postgres:16)", HostsListed: listed}}
		if got := container.NotJudgedLogText(); got != container.NotJudgedText() || !strings.Contains(got, "(container pg (postgres:16))") {
			t.Errorf("listed %v: NotJudgedLogText = %q: want a container's name and image as the report names them", listed, got)
		}
	}
}

// lossHostForms are capture losses whose What names a host in a form real
// traffic uses, as the agent writes them: a mongodb+srv client's SRV question
// (labels with '_'), a CONNECT to an account-numbered host (NetSuite, ECR), a
// TLS abort to a pod's DNS name (a label of digits and dashes), and a call
// to a compose service by its Host header alone (one label, no port).
var lossHostForms = []lossHostForm{
	{"srv-question", CaptureLossUnattributed, "the latest: a DNS question for ", "_mongodb._tcp.cluster0.ab1cd.mongodb.net.", " (SRV) that no DNS mock answers, asked while test workers are served: a DNS question carries no stamp of the test worker that asked it"},
	{"numeric-account-label", CaptureLossUnattributed, "the latest: a connection to ", "1234567.suitetalk.api.netsuite.com:443", ", from process 42, in no test worker's keploy instance"},
	{"pod-dns-name", CaptureLossTLSAbort, "to ", "10-0-0-1.default.pod.cluster.local:443", ""},
	{"single-label-host-header", CaptureLossUnattributed, "the latest: a call to ", "orders-svc", ", from process 42 of ended test worker 1, on a connection it made before keploy stopped serving that worker"},
	{"dotted-host-port", CaptureLossTLSAbort, "to ", "api.tenant.example:443", ": the client does not trust keploy's CA"},
}

// lossHostForm is a capture loss whose What is before, host and after.
type lossHostForm struct {
	name         string
	kind         CaptureLossKind
	before, host string
	after        string
}

// older is the loss as an agent that lists no hosts sends it.
func (f lossHostForm) older() CaptureLoss {
	return CaptureLoss{Kind: f.kind, Count: 1, What: f.before + f.host + f.after}
}

// listed is the loss as an agent that lists its host sends it, the host
// tagged under keys where the agent wrote it.
func (f lossHostForm) listed(keys *LogKeySession) CaptureLoss {
	w := keys.LossText().Say(f.before).Host(f.host).Say(f.after)
	return CaptureLoss{Kind: f.kind, Count: 1, What: w.String(), Hosts: w.Hosts(), HostsListed: true}
}

// lossLogTexts are each log form of losses, by name.
func lossLogTexts(l CaptureLosses) map[string]string {
	return map[string]string{"LogText": l.LogText(), "NotJudgedLogText": l.NotJudgedLogText()}
}

// An agent that lists no hosts (one that predates CaptureLoss.Hosts) sends a
// What alone: its log forms never name it whole, whatever form its host
// takes. Guessing hosts from the sentence kept these four whole.
func TestAnOlderAgentsLossIsNeverLoggedWhole(t *testing.T) {
	for _, c := range lossHostForms {
		name, _ := splitHostPort(c.host)
		name = strings.TrimSuffix(name, ".")
		for form, text := range lossLogTexts(CaptureLosses{c.older()}) {
			if strings.Contains(text, name) {
				t.Errorf("%s %s: %q names %q whole", c.name, form, text, name)
			}
		}
	}
}

// An agent that lists the hosts each What names (CaptureLoss.Hosts) has each
// written in the log forms by its shape and its tag, whatever form it takes,
// and nothing else in What read as one: a word of keploy's that reads like a
// host (Node.js) is kept. The report's form names each whole beside the same
// tag, so the console and the report name what a log line names by its tag.
func TestAListedHostIsLoggedByItsShapeAndTagInEveryForm(t *testing.T) {
	keys := NewLogKeySession()
	for _, c := range lossHostForms {
		loss := c.listed(keys)
		name, _ := splitHostPort(c.host)
		bare := strings.TrimSuffix(name, ".")
		tag := keys.HostTag(c.host)
		for form, text := range lossLogTexts(CaptureLosses{loss}) {
			if strings.Contains(text, bare) {
				t.Errorf("%s %s: %q names %q whole", c.name, form, text, bare)
			}
			if want := HostShape(c.host) + " " + tag; !strings.Contains(text, want) {
				t.Errorf("%s %s: %q does not name the host by its shape and tag %q", c.name, form, text, want)
			}
		}
		for form, text := range map[string]string{"Text": CaptureLosses{loss}.Text(), "NotJudgedText": CaptureLosses{loss}.NotJudgedText()} {
			if want := c.host + " " + HostTagLabel(tag); !strings.Contains(text, want) {
				t.Errorf("%s %s: %q does not name the host whole beside its tag %q", c.name, form, text, want)
			}
		}
		if got := loss.LogWhat(); strings.Contains(got, bare) || !strings.Contains(got, tag) {
			t.Errorf("%s LogWhat = %q: want the host by its shape and tag", c.name, got)
		}
	}
	gate := CaptureLosses{{Kind: CaptureLossTLSAbort, Count: 1, What: "to 10.0.0.1:443: Node.js rejected keploy's certificate", HostsListed: true}}
	if got := gate.LogText(); !strings.Contains(got, "to 10.0.0.1:443: Node.js rejected") {
		t.Errorf("LogText = %q: want keploy's words and the address kept, nothing listed shaped", got)
	}
}

// What the agent sends travels to keploy's CLI with its hosts and their tags
// (JSON, as the agent's routes send it): a current agent's loss and
// capture_incomplete evidence keep them; an older agent's, with no hosts
// field, decode as listing none, and are logged by their size alone.
func TestAListedHostTravelsFromTheAgentAndAnOlderAgentsDoesNot(t *testing.T) {
	keys := NewLogKeySession()
	host := "_mongodb._tcp.cluster0.ab1cd.mongodb.net."
	loss := lossHostForms[0].listed(keys)
	why := keys.LossText().Say("a mock.real connection to api.test:443 carried another name: its ClientHello names ").Host("canary7q.tenant.example").Say(", which mock.real does not list")
	inc := CaptureIncomplete{Class: CaptureIncompleteRealName, Destination: "10.0.0.7:443", Why: why.String(), Hosts: why.Hosts(), HostsListed: true}
	body, err := json.Marshal(struct {
		Losses CaptureLosses
		Run    RealRun
	}{CaptureLosses{loss}, RealRun{Incomplete: []CaptureIncomplete{inc}}})
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Losses CaptureLosses
		Run    RealRun
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Losses, CaptureLosses{loss}) || !reflect.DeepEqual(got.Run.Incomplete, []CaptureIncomplete{inc}) {
		t.Fatalf("decoded %+v, want %+v and %+v", got, loss, inc)
	}
	if text := got.Losses.LogText(); strings.Contains(text, "cluster0") || !strings.Contains(text, keys.HostTag(host)) {
		t.Errorf("LogText = %q: want the question by its shape and the agent's tag", text)
	}
	if why := got.Run.Incomplete[0].LogWhy(); strings.Contains(why, "canary7q") || !strings.Contains(why, keys.HostLogName("canary7q.tenant.example")) {
		t.Errorf("LogWhy = %q: want the other name by its shape and the agent's tag", why)
	}
	real := RealUncapturedLosses(got.Run.Incomplete)
	if text := real.LogText(); strings.Contains(text, "canary7q") || !strings.Contains(text, keys.HostLogName("canary7q.tenant.example")) {
		t.Errorf("RealUncapturedLosses LogText = %q: want the other name by its shape and tag", text)
	}
	if text := real.Text(); !strings.Contains(text, "canary7q.tenant.example "+HostTagLabel(keys.HostTag("canary7q.tenant.example"))) {
		t.Errorf("RealUncapturedLosses Text = %q: want the other name whole beside its tag", text)
	}

	older := []byte(`{"Losses":[{"kind":"call_unattributed","count":1,"what":"the latest: a connection to orders-svc:8080, from process 42, in no test worker's keploy instance"}],` +
		`"Run":{"incomplete":[{"class":"e","destination":"10.0.0.7:443","why":"a mock.real connection to api.test:443 carried another name: a request with Host canary7q.tenant.example, which mock.real does not list"}]}}`)
	got.Losses, got.Run = nil, RealRun{}
	if err := json.Unmarshal(older, &got); err != nil {
		t.Fatal(err)
	}
	if got.Losses[0].HostsListed || got.Run.Incomplete[0].HostsListed {
		t.Fatalf("an older agent's loss decoded as listing its hosts: %+v", got)
	}
	for form, text := range map[string]string{
		"LogText": got.Losses.LogText(), "NotJudgedLogText": got.Losses.NotJudgedLogText(), "LogWhy": got.Run.Incomplete[0].LogWhy(),
		"RealUncapturedLosses": RealUncapturedLosses(got.Run.Incomplete).LogText(),
	} {
		if strings.Contains(text, "orders-svc") || strings.Contains(text, "canary7q") {
			t.Errorf("an older agent's %s names a host whole: %q", form, text)
		}
		if !strings.Contains(text, "bytes: the agent lists none of the host names in it") {
			t.Errorf("an older agent's %s = %q: want its text by its size", form, text)
		}
	}
	if text := got.Losses.Text(); !strings.Contains(text, "orders-svc:8080") {
		t.Errorf("the report lost an older agent's destination: %q", text)
	}
}

// A loss merged with another, as keploy adds counts and joins Whats (Plus,
// JoinWhat, ChargeNotJudged), keeps the hosts each What names, listed only
// where both were; evidence for the harness and the receipts keeps each name
// with no tag (Untagged).
func TestAMergedLossKeepsTheHostsEachWhatNames(t *testing.T) {
	keys := NewLogKeySession()
	aw := keys.LossText().Say("the latest: a call to ").Host("orders-svc")
	bw := keys.LossText().Say("the latest: a connection to ").Host("1234567.suitetalk.api.netsuite.com:443")
	a := CaptureLoss{Kind: CaptureLossUnattributed, Count: 1, What: aw.String(), Hosts: aw.Hosts(), HostsListed: true}
	b := CaptureLoss{Kind: CaptureLossUnattributed, Count: 1, What: bw.String(), Hosts: bw.Hosts(), HostsListed: true}
	sum := CaptureLosses{a}.Plus(CaptureLosses{b})
	if len(sum) != 1 || sum[0].What != b.What || !reflect.DeepEqual(sum[0].Hosts, b.Hosts) || !sum[0].HostsListed {
		t.Fatalf("Plus = %+v: want b's What with its hosts", sum)
	}
	joined := a
	joined.JoinWhat(b)
	if text := (CaptureLosses{joined}).LogText(); strings.Contains(text, "orders-svc") || strings.Contains(text, "netsuite") {
		t.Errorf("a joined What's log text names a host whole: %q", text)
	}
	older := b
	older.Hosts, older.HostsListed = nil, false
	joined = a
	joined.JoinWhat(older)
	if joined.HostsListed {
		t.Error("a What joined with an older agent's says its hosts are listed")
	}
	ev := ScopeEvidence{}
	ev.ChargeNotJudged(CaptureLosses{a})
	ev.ChargeNotJudged(CaptureLosses{b})
	if text := ev.CannotCapture.NotJudgedLogText(); strings.Contains(text, "orders-svc") || strings.Contains(text, "netsuite") {
		t.Errorf("ChargeNotJudged's log text names a host whole: %q", text)
	}
	for _, c := range (CaptureLosses{joined}).Untagged() {
		for _, h := range c.Hosts {
			if h.Tag != "" || h.Name == "" {
				t.Errorf("Untagged kept %+v", h)
			}
		}
	}
	if (CaptureLosses{a}).Untagged()[0].Hosts[0].Name != "orders-svc" || a.Hosts[0].Tag == "" {
		t.Error("Untagged lost the name, or changed the loss it was given")
	}
}

// A one-label host is often one of keploy's own words: a compose service
// called worker, test or process. The agent lists each host where it wrote
// it (LossText), so the report's and the console's text tag that place
// alone, and the log forms shape it there alone: keploy's words around it
// ("of test worker instance 2", "the worker ended") read as they were
// written. The texts are the agent's own (refuseUnserved,
// stoppedWorkerMissWhat, sendDNSMiss); a mock.real why names a client's
// Host "client" beside keploy's "let the client open".
func TestAHostNamedAsOneOfKeploysWordsIsReadWhereTheAgentWroteIt(t *testing.T) {
	keys := NewLogKeySession()
	cases := []struct {
		before, host, after string
	}{
		{"the latest: a connection to ", "worker:8080", ", from process 9 of test worker instance 2, which keploy does not serve (the worker ended, or keploy never began serving it)"},
		{"the latest: a call to ", "worker", ", from process 7 of ended test worker 1, on a connection it made before keploy stopped serving that worker"},
		{"the latest: a DNS question for ", "worker.", " (A) that no DNS mock answers, asked while test workers are served: a DNS question carries no stamp of the test worker that asked it"},
		{"the latest: a call to ", "test", ", from process 7 of ended test worker 1, on a connection it made before keploy stopped serving that worker"},
		{"the latest: a connection to ", "process:9000", ", from process 9, in no test worker's keploy instance"},
	}
	for _, c := range cases {
		w := keys.LossText().Say(c.before).Host(c.host).Say(c.after)
		loss := CaptureLoss{Kind: CaptureLossUnattributed, Count: 1, What: w.String(), Hosts: w.Hosts(), HostsListed: true}
		tag := keys.HostTag(c.host)
		report := c.before + WithHostTag(c.host, tag) + c.after
		logged := c.before + HostShape(c.host) + " " + tag + c.after
		for form, got := range map[string]string{
			"NotJudgedText": CaptureLosses{loss}.NotJudgedText(), "Text": CaptureLosses{loss}.Text(), "ReportWhat": loss.ReportWhat(),
		} {
			if n := strings.Count(got, "[host tag"); n != 1 || !strings.Contains(got, report) {
				t.Errorf("%s %s tags %d places, want the one the agent wrote, %q: %s", c.host, form, n, report, got)
			}
		}
		for form, got := range map[string]string{
			"NotJudgedLogText": CaptureLosses{loss}.NotJudgedLogText(), "LogText": CaptureLosses{loss}.LogText(), "LogWhat": loss.LogWhat(),
		} {
			if n := strings.Count(got, tag); n != 1 || !strings.Contains(got, logged) {
				t.Errorf("%s %s names %d places by the tag, want the one the agent wrote, %q: %s", c.host, form, n, logged, got)
			}
		}
	}
	why := keys.LossText().Say("a mock.real connection to api.test:443 carried another name: a request with Host ").Host("client").
		Say(", which mock.real does not list; keploy ended it (one connection carries one real name: list that name too, or let the client open a connection per name)")
	inc := CaptureIncomplete{Class: CaptureIncompleteRealName, Destination: "10.0.0.7:443", Why: why.String(), Hosts: why.Hosts(), HostsListed: true}
	tag := keys.HostTag("client")
	if got := inc.ReportWhy(); strings.Count(got, "[host tag") != 1 || !strings.Contains(got, "Host client "+HostTagLabel(tag)+", which") || !strings.Contains(got, "let the client open") {
		t.Errorf("ReportWhy = %q: want the client's Host tagged where the agent wrote it, and keploy's words kept", got)
	}
	if got := inc.LogWhy(); strings.Count(got, tag) != 1 || !strings.Contains(got, "Host {} "+tag+", which") || !strings.Contains(got, "let the client open") {
		t.Errorf("LogWhy = %q: want the client's Host shaped where the agent wrote it, and keploy's words kept", got)
	}
}

// A loss whose What joins two (JoinWhat), and the capture loss keploy makes
// of mock.real's evidence (RealUncapturedLosses, which writes each
// destination before its Why), keep each host at its place in the text they
// make: each is tagged there, once, in every form.
func TestAJoinedTextKeepsEachHostAtItsPlace(t *testing.T) {
	keys := NewLogKeySession()
	aw := keys.LossText().Say("the latest: a call to ").Host("worker").Say(", from process 7 of ended test worker 1")
	bw := keys.LossText().Say("the latest: a connection to ").Host("test:8080").Say(", from process 9, in no test worker's keploy instance")
	joined := CaptureLoss{Kind: CaptureLossUnattributed, Count: 1, What: aw.String(), Hosts: aw.Hosts(), HostsListed: true}
	joined.JoinWhat(CaptureLoss{Kind: CaptureLossUnattributed, Count: 1, What: bw.String(), Hosts: bw.Hosts(), HostsListed: true})
	wantReport := "the latest: a call to " + WithHostTag("worker", keys.HostTag("worker")) + ", from process 7 of ended test worker 1; the latest: a connection to " +
		WithHostTag("test:8080", keys.HostTag("test")) + ", from process 9, in no test worker's keploy instance"
	if got := joined.ReportWhat(); got != wantReport {
		t.Errorf("a joined What's report form = %q, want %q", got, wantReport)
	}
	wantLog := "the latest: a call to {} " + keys.HostTag("worker") + ", from process 7 of ended test worker 1; the latest: a connection to {}:8080 " +
		keys.HostTag("test") + ", from process 9, in no test worker's keploy instance"
	if got := joined.LogWhat(); got != wantLog {
		t.Errorf("a joined What's log form = %q, want %q", got, wantLog)
	}

	var inc []CaptureIncomplete
	for _, h := range []string{"worker", "client"} {
		w := keys.LossText().Say("a mock.real connection to api.test:443 carried another name: a request with Host ").Host(h).Say(", which mock.real does not list; let the client open a connection per name")
		inc = append(inc, CaptureIncomplete{Class: CaptureIncompleteRealName, Destination: "10.0.0.7:443", Why: w.String(), Hosts: w.Hosts(), HostsListed: true})
	}
	real := RealUncapturedLosses(inc)
	for _, h := range []string{"worker", "client"} {
		tag := keys.HostTag(h)
		if got := real.Text(); strings.Count(got, HostTagLabel(tag)) != 1 || !strings.Contains(got, "Host "+h+" "+HostTagLabel(tag)+", which") {
			t.Errorf("RealUncapturedLosses Text = %q: want %s tagged once, where its Why wrote it", got, h)
		}
		if got := real.LogText(); strings.Count(got, tag) != 1 || !strings.Contains(got, "Host {} "+tag+", which") {
			t.Errorf("RealUncapturedLosses LogText = %q: want %s by its shape and tag once, where its Why wrote it", got, h)
		}
	}
	if got := real.Text(); strings.Count(got, "let the client open") != 2 {
		t.Errorf("RealUncapturedLosses Text = %q: keploy's words were read as a host", got)
	}
}

// A list whose places the text does not hold (a text changed after its
// hosts were listed, without moving them) is no list keploy can trust: the
// log forms write the text by its size alone, never whole, and the report
// names it whole with no tag at a place it does not hold.
func TestAListAtPlacesTheTextDoesNotHoldIsLoggedBySize(t *testing.T) {
	keys := NewLogKeySession()
	w := keys.LossText().Say("to ").Host("orders-svc:443")
	for name, hosts := range map[string]LossHosts{
		"moved":       w.Hosts().Shifted(5),
		"beyond":      w.Hosts().Shifted(400),
		"another":     {{Name: "other-svc:443", Tag: keys.HostTag("other-svc"), At: 3}},
		"overlapping": append(w.Hosts(), LossHost{Name: "orders-svc", Tag: keys.HostTag("orders-svc"), At: 3}),
	} {
		loss := CaptureLoss{Kind: CaptureLossTLSAbort, Count: 1, What: w.String(), Hosts: hosts, HostsListed: true}
		inc := CaptureIncomplete{Class: CaptureIncompleteRealName, Destination: "10.0.0.7:443", Why: w.String(), Hosts: hosts, HostsListed: true}
		for form, got := range map[string]string{
			"LogText": CaptureLosses{loss}.LogText(), "NotJudgedLogText": CaptureLosses{loss}.NotJudgedLogText(), "LogWhat": loss.LogWhat(), "LogWhy": inc.LogWhy(),
		} {
			if strings.Contains(got, "orders-svc") || !strings.Contains(got, "the host names listed in it are not where the list says") {
				t.Errorf("%s %s = %q: want the text by its size", name, form, got)
			}
		}
		if got := loss.ReportWhat(); got != "to orders-svc:443" {
			t.Errorf("%s ReportWhat = %q: want the text whole, with no tag", name, got)
		}
	}
}

// A port that is no number is the client's text (a CONNECT target's or a
// Host header's): a log line writes it {}, beside a name's labels or an IP
// address alike.
func TestAPortThatIsNoNumberIsNotWritten(t *testing.T) {
	keys := NewLogKeySession()
	for in, want := range map[string]string{
		"api.example.com:s3cr3t": "{}.{}.{}:{} " + keys.HostTag("api.example.com"),
		"api.example.com:443":    "{}.{}.{}:443 " + keys.HostTag("api.example.com"),
		"10.0.0.1:s3cr3t":        "10.0.0.1:{}",
		"[::1]:s3cr3t":           "[::1]:{}",
		"10.0.0.1:443":           "10.0.0.1:443",
		":s3cr3t":                ":{}",
	} {
		if got := keys.HostLogName(in); got != want {
			t.Errorf("HostLogName(%q) = %q, want %q", in, got, want)
		}
	}
}

// An IP address with a port that is no number, written into a loss's text
// (LossText.Host), is listed there with no tag, as an IP address has none,
// so every log form writes it by its shape (10.0.0.1:{}), as HostLogName
// does, and the report's forms keep it whole. LossText listed no IP, so
// every log form kept the port's text whole. An IP address a line writes
// whole (its port a number) is still not listed.
func TestAnIPWithAPortThatIsNoNumberInALossTextIsLoggedByItsShape(t *testing.T) {
	const canary = "kpLogCanary7Qx3vZ9mW2"
	keys := NewLogKeySession()
	for host, shape := range map[string]string{"10.0.0.1:" + canary: "10.0.0.1:{}", "[::1]:" + canary: "[::1]:{}"} {
		w := keys.LossText().Say("the latest: a call to ").Host(host).Sayf(", from process %d of ended test worker %s, on a connection it made before keploy stopped serving that worker", 9, "1")
		if want := (LossHosts{{Name: host, At: len("the latest: a call to ")}}); !reflect.DeepEqual(w.Hosts(), want) {
			t.Errorf("%s: Hosts = %+v, want %+v: listed where it stands, with no tag", host, w.Hosts(), want)
		}
		loss := CaptureLoss{Kind: CaptureLossUnattributed, Count: 1, What: w.String(), Hosts: w.Hosts(), HostsListed: true}
		inc := CaptureIncomplete{Class: CaptureIncompleteRealName, Destination: "10.0.0.7:443", Why: w.String(), Hosts: w.Hosts(), HostsListed: true}
		for form, text := range map[string]string{
			"HostLogName":      keys.HostLogName(host),
			"LogWhat":          loss.LogWhat(),
			"LogText":          CaptureLosses{loss}.LogText(),
			"NotJudgedLogText": CaptureLosses{loss}.NotJudgedLogText(),
			"EvidenceLogText":  CaptureLosses{loss}.EvidenceLogText(),
			"LogWhy":           inc.LogWhy(),
		} {
			if strings.Contains(text, canary) {
				t.Errorf("%s: %s holds the port's text whole: %q", host, form, text)
			}
			// EvidenceLogText names only the kernel's skips: "" here.
			if form != "EvidenceLogText" && !strings.Contains(text, shape) {
				t.Errorf("%s: %s does not name it by its shape %q: %q", host, form, shape, text)
			}
		}
		for form, text := range map[string]string{"Text": CaptureLosses{loss}.Text(), "ReportWhat": loss.ReportWhat(), "ReportWhy": inc.ReportWhy()} {
			if !strings.Contains(text, "a call to "+host+",") {
				t.Errorf("%s: %s does not name it whole: %q", host, form, text)
			}
		}
	}
	if w := keys.LossText().Say("a call to ").Host("10.0.0.1:443"); w.Hosts() != nil {
		t.Errorf("an IP address a line writes whole was listed: %+v", w.Hosts())
	}
}

// An IPv6 address's zone is any text a client sends (Go checks nothing in a
// Host header, and a CONNECT target's %25-escaped zone is any text too), so
// a line writes it {}, with the address and the port rule kept; a loss's
// text then lists the host with no tag, and every log form writes it by
// that shape. HostShape wrote the zone whole where the port is a number,
// and LossText left the host unlisted, so every log form kept it whole.
func TestAnIPv6ZoneIsNotWritten(t *testing.T) {
	const canary = "kpLogCanary7Qx3vZ9mW2"
	keys := NewLogKeySession()
	for in, want := range map[string]string{
		"[fe80::1%" + canary + "]:443":   "[fe80::1%{}]:443",
		"fe80::1%" + canary:              "fe80::1%{}",
		"[fe80::1%" + canary + "]":       "[fe80::1%{}]",
		"[fe80::1%" + canary + "]:s3cr3": "[fe80::1%{}]:{}",
		"[fe80::1%" + canary + "]:99999": "[fe80::1%{}]:99999",
		"[fe80::1]:443":                  "[fe80::1]:443",
	} {
		if got := HostShape(in); got != want {
			t.Errorf("HostShape(%q) = %q, want %q", in, got, want)
		}
		if got := keys.HostLogName(in); got != want {
			t.Errorf("HostLogName(%q) = %q, want %q", in, got, want)
		}
	}
	for host, shape := range map[string]string{"[fe80::1%" + canary + "]:443": "[fe80::1%{}]:443", "fe80::1%" + canary: "fe80::1%{}"} {
		w := keys.LossText().Say("the latest: a call to ").Host(host).Sayf(", from process %d", 9)
		if want := (LossHosts{{Name: host, At: len("the latest: a call to ")}}); !reflect.DeepEqual(w.Hosts(), want) {
			t.Errorf("%s: Hosts = %+v, want %+v: listed where it stands, with no tag", host, w.Hosts(), want)
		}
		loss := CaptureLoss{Kind: CaptureLossUnattributed, Count: 1, What: w.String(), Hosts: w.Hosts(), HostsListed: true}
		for form, text := range map[string]string{
			"HostLogName": keys.HostLogName(host),
			"HostShape":   HostShape(host),
			"LogWhat":     loss.LogWhat(),
			"LogText":     CaptureLosses{loss}.LogText(),
		} {
			if strings.Contains(text, canary) {
				t.Errorf("%s: %s holds the zone's text whole: %q", host, form, text)
			}
			if !strings.Contains(text, shape) {
				t.Errorf("%s: %s does not name it by its shape %q: %q", host, form, shape, text)
			}
		}
		if text := (CaptureLosses{loss}).Text(); !strings.Contains(text, "a call to "+host+",") {
			t.Errorf("%s: the report's text does not name it whole: %q", host, text)
		}
	}
}

// A client's TLS server name may be a zoned IPv6 address whose zone is any
// text, and keploy record dials it: the dial's error, logged and returned,
// writes the address in Go's canonical form ("FE80:0::1" as "fe80::1", a
// v4-mapped address as IPv4) and the zone as it is. HostsInError, given the
// hosts as the proxy gives them (the address it dialled, the server name
// twice), writes the zone {} wherever the error writes it, a second '%'
// being the zone's, and keeps the address. An address with no zone is kept
// whole, and its error is returned as it is.
func TestAZonedIPv6InAnErrorIsWrittenByItsShape(t *testing.T) {
	const canary = "kpLogCanary7Qx3vZ9mW2"
	ctx := WithLogKeySession(context.Background(), NewLogKeySession())
	invalid := os.NewSyscallError("connect", syscall.EINVAL)
	for _, c := range []struct{ server, want string }{
		{"fe80::1%" + canary + "%x", "dial [fe80::1%{}]:443: dial tcp [fe80::1%{}]:443: connect: invalid argument"},
		{"fe80::1%" + canary, "dial [fe80::1%{}]:443: dial tcp [fe80::1%{}]:443: connect: invalid argument"},
		{"FE80:0::1%" + canary, "dial [FE80:0::1%{}]:443: dial tcp [fe80::1%{}]:443: connect: invalid argument"},
		{"fe80:0000:0000:0000:0000:0000:0000:0001%" + canary, "dial [fe80:0000:0000:0000:0000:0000:0000:0001%{}]:443: dial tcp [fe80::1%{}]:443: connect: invalid argument"},
		{"::ffff:1.2.3.4%" + canary, "dial [::ffff:1.2.3.4%{}]:443: dial tcp 1.2.3.4%{}:443: connect: invalid argument"},
		{"fe80::1%" + canary + ".", "dial [fe80::1%{}]:443: dial tcp [fe80::1%{}]:443: connect: invalid argument"},
	} {
		addr := net.JoinHostPort(c.server, "443")
		to, err := net.ResolveTCPAddr("tcp", addr) // an address: nothing is looked up
		if err != nil {
			t.Fatalf("%s: %v", addr, err)
		}
		dial := &net.OpError{Op: "dial", Net: "tcp", Addr: to, Err: invalid}
		// As the HTTP parser's pass-through writes it, then as net.Dial does.
		err = fmt.Errorf("dial %s: %w", addr, dial)
		got := HostsInError(ctx, err, addr, c.server, c.server)
		if got.Error() != c.want {
			t.Errorf("%s: HostsInError(%q) = %q, want %q", c.server, err, got, c.want)
		}
		if !errors.Is(got, syscall.EINVAL) {
			t.Errorf("%s: HostsInError lost its cause: %#v", c.server, got)
		}
	}

	// A zone that holds ']' and ':' makes an address net.Dial cannot split:
	// its error quotes the address as given.
	server := "fe80::1%" + canary + "]:x"
	addr := net.JoinHostPort(server, "443")
	err := &net.OpError{Op: "dial", Net: "tcp", Err: &net.AddrError{Err: "too many colons in address", Addr: addr}}
	if got, want := HostsInError(ctx, err, addr, server, server).Error(), "dial tcp: address [fe80::1%{}: too many colons in address"; got != want {
		t.Errorf("HostsInError(%q) = %q, want %q", err, got, want)
	}

	plain := &net.OpError{Op: "dial", Net: "tcp", Addr: &net.TCPAddr{IP: net.ParseIP("fe80::1"), Port: 443}, Err: invalid}
	if got := HostsInError(ctx, plain, "[fe80::1]:443", "fe80::1"); got != error(plain) {
		t.Errorf("HostsInError rewrote an address with no zone: %q", got)
	}
	if got := HostsInText(ctx, "a %eth0 and fe80::1%eth01", "fe80::1%eth0"); got != "a %{} and fe80::1%eth01" {
		t.Errorf("HostsInText = %q: want the zone written {} where it stands whole, and a longer one kept", got)
	}
}

// A What charged again that the test's sentence already names (ChargeNotJudged:
// c's What holds it) is named there, and the hosts keep the places c's What
// holds them at: each is tagged once, where it stands, and the log form is
// no list keploy distrusts.
func TestAChargedWhatAlreadyNamedKeepsItsPlaces(t *testing.T) {
	keys := NewLogKeySession()
	aw := keys.LossText().Say("the latest: a call to ").Host("worker")
	bw := keys.LossText().Say("the latest: a connection to ").Host("test:8080")
	a := CaptureLoss{Kind: CaptureLossUnattributed, Count: 1, What: aw.String(), Hosts: aw.Hosts(), HostsListed: true}
	b := CaptureLoss{Kind: CaptureLossUnattributed, Count: 1, What: bw.String(), Hosts: bw.Hosts(), HostsListed: true}
	ev := ScopeEvidence{}
	ev.ChargeNotJudged(CaptureLosses{a})
	ev.ChargeNotJudged(CaptureLosses{b})
	ev.ChargeNotJudged(CaptureLosses{b})
	if len(ev.CannotCapture) != 1 {
		t.Fatalf("CannotCapture = %+v", ev.CannotCapture)
	}
	got := ev.CannotCapture.NotJudgedLogText()
	for _, h := range []string{"worker", "test"} {
		if n := strings.Count(got, keys.HostTag(h)); n != 1 {
			t.Errorf("NotJudgedLogText names %s by its tag %d times, want once where it stands: %q", h, n, got)
		}
	}
	if !strings.Contains(got, "a call to {} "+keys.HostTag("worker")+"; the latest: a connection to {}:8080 "+keys.HostTag("test")) {
		t.Errorf("NotJudgedLogText = %q: want each host by its shape and tag at its place", got)
	}
}
