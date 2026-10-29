package http

import (
	"net/http"
	"net/url"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/identity"
	"go.keploy.io/server/v3/pkg/models"
)

func aliasFixture(t *testing.T) *identity.Rewriter {
	t.Helper()
	rw := identity.New(
		[]models.PodIdentity{{Name: "api-55f5fd9659-fzsfr", UID: "0b1c4d6e-9f2a-4c3b-8d7e-1a2b3c4d5e6f", IP: "10.244.1.17", HostIP: "192.168.1.9", Node: "worker-1"}},
		models.PodIdentity{Name: "api-rpl-1a2b3c", Hostname: "api-rpl-1a2b3c", UID: "7e6d5c4b-3a29-4817-a6f5-e4d3c2b1a0f9", IP: "10.244.2.4", HostIP: "192.168.1.10", Node: "worker-2"},
	)
	if rw == nil {
		t.Fatal("fixture identities must give a rewriter")
	}
	return rw
}

func TestAliasIdentityRewritesTheLiveRequestAndKeepsTheSentBody(t *testing.T) {
	rw := aliasFixture(t)
	u, _ := url.Parse("http://10.244.2.4:8080/api/v1/namespaces/ns/pods/api-rpl-1a2b3c?fieldSelector=spec.nodeName%3Dworker-2")
	body := []byte(`{"holderIdentity":"api-rpl-1a2b3c_1","podIP":"10.244.2.4"}`)
	input := &req{
		method: "PUT", url: u,
		header: http.Header{"Host": {"10.244.2.4:8080"}, "X-Pod": {"api-rpl-1a2b3c"}, "Accept": {"application/json"}},
		body:   body, raw: body,
	}
	aliasIdentity(rw, input)

	if got := input.url.String(); got != "http://10.244.1.17:8080/api/v1/namespaces/ns/pods/api-55f5fd9659-fzsfr?fieldSelector=spec.nodeName%3Dworker-1" {
		t.Fatalf("url = %s", got)
	}
	if got := input.header.Get("Host"); got != "10.244.1.17:8080" {
		t.Fatalf("Host = %s", got)
	}
	if got := input.header.Get("X-Pod"); got != "api-55f5fd9659-fzsfr" {
		t.Fatalf("X-Pod = %s", got)
	}
	if got := input.header.Get("Accept"); got != "application/json" {
		t.Fatalf("an untouched header moved: %s", got)
	}
	if got := string(input.body); got != `{"holderIdentity":"api-55f5fd9659-fzsfr_1","podIP":"10.244.1.17"}` {
		t.Fatalf("body = %s", got)
	}
	// The SigV4 payload hash is over what the client sent, not the rewrite.
	if got := string(input.sentBody()); got != string(body) {
		t.Fatalf("sentBody = %s", got)
	}

	// A request without the live identity is left as it came, same slices.
	plain := []byte(`{"ok":true}`)
	pu, _ := url.Parse("http://db.svc:5432/health")
	in2 := &req{method: "GET", url: pu, header: http.Header{"Host": {"db.svc:5432"}}, body: plain}
	hdr := in2.header
	aliasIdentity(rw, in2)
	if &in2.body[0] != &plain[0] || in2.sent != nil {
		t.Fatal("an unchanged body must stay the same slice and keep no copy")
	}
	if in2.header[""] != nil || len(in2.header) != len(hdr) || in2.header.Get("Host") != "db.svc:5432" {
		t.Fatal("unchanged headers must stay as they were")
	}
	// nil rewriter: no-op.
	aliasIdentity(nil, in2)
	if in2.url.String() != "http://db.svc:5432/health" {
		t.Fatal("a nil rewriter changed the request")
	}
}

func TestAliasResponsePutsTheLiveIdentityBackIntoText(t *testing.T) {
	rev := aliasFixture(t).Reverse()
	header := map[string][]string{"Content-Type": {"application/json"}, "X-Served-By": {"api-55f5fd9659-fzsfr"}}
	body := `{"metadata":{"name":"api-55f5fd9659-fzsfr"},"status":{"podIP":"10.244.1.17","hostIP":"192.168.1.9"}}`
	gotBody, gotHeader := aliasResponse(rev, header, body)
	if gotBody != `{"metadata":{"name":"api-rpl-1a2b3c"},"status":{"podIP":"10.244.2.4","hostIP":"192.168.1.10"}}` {
		t.Fatalf("body = %s", gotBody)
	}
	if gotHeader["X-Served-By"][0] != "api-rpl-1a2b3c" {
		t.Fatalf("header = %v", gotHeader)
	}
	if header["X-Served-By"][0] != "api-55f5fd9659-fzsfr" {
		t.Fatal("the recorded header map was written through")
	}

	// A binary body is served as recorded; its headers are still put back.
	bin := map[string][]string{"Content-Type": {"application/x-protobuf"}, "X-Served-By": {"api-55f5fd9659-fzsfr"}}
	gotBody, gotHeader = aliasResponse(rev, bin, "\x0a\x14api-55f5fd9659-fzsfr")
	if gotBody != "\x0a\x14api-55f5fd9659-fzsfr" {
		t.Fatalf("a protobuf body was rewritten: %q", gotBody)
	}
	if gotHeader["X-Served-By"][0] != "api-rpl-1a2b3c" {
		t.Fatalf("header = %v", gotHeader)
	}

	// No reverse: nothing moves.
	b, h := aliasResponse(nil, header, body)
	if b != body || h["X-Served-By"][0] != "api-55f5fd9659-fzsfr" {
		t.Fatal("a nil rewriter changed the response")
	}

	// An Office document names xml in its type but is a ZIP: a stored entry
	// holding the name keeps its length, so the archive stays readable.
	xlsx := map[string][]string{"Content-Type": {"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"}}
	zipBody := "PK\x03\x04\x0a\x00\x00\x00\x00\x00<c>api-55f5fd9659-fzsfr</c>"
	if got, _ := aliasResponse(rev, xlsx, zipBody); got != zipBody {
		t.Fatalf("an xlsx body was rewritten: %q", got)
	}
	// A body that names no Content-Type may be anything: served as recorded,
	// its headers still put back.
	none := map[string][]string{"X-Served-By": {"api-55f5fd9659-fzsfr"}}
	gotBody, gotHeader = aliasResponse(rev, none, "api-55f5fd9659-fzsfr")
	if gotBody != "api-55f5fd9659-fzsfr" || gotHeader["X-Served-By"][0] != "api-rpl-1a2b3c" {
		t.Fatalf("no Content-Type: body %q, header %v", gotBody, gotHeader)
	}
	// A textual type over bytes that are not UTF-8 (a mislabelled or still
	// compressed body) is served as recorded.
	notUTF8 := "\xff\xfe" + `{"name":"api-55f5fd9659-fzsfr"}`
	if got, _ := aliasResponse(rev, header, notUTF8); got != notUTF8 {
		t.Fatalf("a body that is not UTF-8 was rewritten: %q", got)
	}
}

func TestTextualContentType(t *testing.T) {
	for ct, want := range map[string]bool{
		"": false, "application/json": true, "application/json; charset=utf-8": true, "text/plain": true,
		"application/vnd.kubernetes.protobuf": false, "application/x-protobuf": false, "image/png": false,
		"application/octet-stream": false, "application/xml": true, "application/x-www-form-urlencoded": true,
		"application/x-ndjson": true, "application/gzip": false, "Application/JSON": true,
		"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":       false,
		"application/vnd.openxmlformats-officedocument.wordprocessingml.document": false,
		"application/zip": false, "application/vnd.api+json": true, "application/problem+xml": true,
		"application/yaml": true, "text/html; charset=utf-8": true, "json": false, "not a type": false,
	} {
		h := map[string][]string{}
		if ct != "" {
			h["Content-Type"] = []string{ct}
		}
		if got := textualContentType(h); got != want {
			t.Errorf("textualContentType(%q) = %v, want %v", ct, got, want)
		}
	}
}
