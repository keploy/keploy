package http

import (
	"bufio"
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap/zaptest"
)

// A browser sends a CORS preflight (OPTIONS, Origin, Access-Control-Request-
// Method) before a cross-origin call when its preflight cache holds no fresh
// answer for it, so how many a test makes follows that cache's clock, not
// the app. Recorded as the test's own data, a replay that made more of them
// failed the test ("OPTIONS /query to localhost:8083 was called 9 times,
// recorded 2"), and one that made fewer failed it for the recorded calls it
// never made. A preflight is protocol-phase: the recorder tags it config, a
// replay serves it to every test and between tests, as often as the browser
// asks, and no test owns it or is judged on it. It is still compared as
// strictly as any call.

// corsPreflight is Chrome's preflight before a cross-origin POST /query
// with a JSON body.
func corsPreflight(path, requestHeaders string) []byte {
	return []byte("OPTIONS " + path + " HTTP/1.1\r\n" +
		"Host: localhost:8083\r\n" +
		"Accept: */*\r\n" +
		"Access-Control-Request-Method: POST\r\n" +
		"Access-Control-Request-Headers: " + requestHeaders + "\r\n" +
		"Origin: http://localhost:3000\r\n" +
		"Referer: http://localhost:3000/\r\n" +
		"Sec-Fetch-Mode: cors\r\n" +
		"User-Agent: Mozilla/5.0 (X11; Linux x86_64) HeadlessChrome/151.0.0.0 Safari/537.36\r\n" +
		"\r\n")
}

var corsPreflightResponse = []byte("HTTP/1.1 204 No Content\r\n" +
	"Access-Control-Allow-Credentials: true\r\n" +
	"Access-Control-Allow-Headers: content-type\r\n" +
	"Access-Control-Allow-Methods: GET,POST,OPTIONS\r\n" +
	"Access-Control-Allow-Origin: http://localhost:3000\r\n" +
	"Vary: Origin\r\n" +
	"\r\n")

// Both recorders tag a preflight config and state it session: it is
// protocol-phase, and in no test's mapping.
func TestARecordedCORSPreflightIsProtocolPhase(t *testing.T) {
	h := &HTTP{Logger: zaptest.NewLogger(t)}
	reqBuf := corsPreflight("/query", "content-type")
	v2, err := h.buildHTTPMock(&FinalHTTP{Req: reqBuf, Resp: corsPreflightResponse, ReqTimestampMock: time.Now(), ResTimestampMock: time.Now()},
		8083, "test-conn", models.OutgoingOptions{})
	if err != nil || v2 == nil {
		t.Fatalf("buildHTTPMock = (%v, %v)", v2, err)
	}
	legacy := runLegacyParseFinalHTTP(t, h, reqBuf, corsPreflightResponse, 8083, "test-conn")
	for path, m := range map[string]*models.Mock{"V2 record": v2, "legacy record": legacy} {
		if got := m.Spec.Metadata["type"]; got != "config" {
			t.Errorf("%s: type %q, want config", path, got)
		}
		if got := m.Spec.Metadata[models.MetadataLifetimeKey]; got != models.MetadataLifetimeSession {
			t.Errorf("%s: stated lifetime %q, want %q", path, got, models.MetadataLifetimeSession)
		}
		if m.TestModeInfo.Lifetime != models.LifetimeSession {
			t.Errorf("%s: lifetime %v, want session", path, m.TestModeInfo.Lifetime)
		}
		if !m.IsProtocolPhase() {
			t.Errorf("%s: a preflight is not protocol-phase", path)
		}
		if models.NewWindowedMock(m).TestMock {
			t.Errorf("%s: a test may own a preflight", path)
		}
		// Read back from disk, it derives what was stated.
		c := m.DeepCopy()
		c.TestModeInfo = models.TestModeInfo{}
		c.DeriveLifetime()
		if c.TestModeInfo.Lifetime != models.LifetimeSession || !c.IsProtocolPhase() {
			t.Errorf("%s: on load lifetime %v, protocol-phase %v; want session, true", path, c.TestModeInfo.Lifetime, c.IsProtocolPhase())
		}
	}
}

// Only a preflight is: an OPTIONS call an app makes itself, and a call that
// carries the preflight's headers but is not an OPTIONS, are the test's data.
func TestOnlyACORSPreflightIsProtocolPhase(t *testing.T) {
	h := &HTTP{Logger: zaptest.NewLogger(t)}
	for name, reqBuf := range map[string]string{
		"an OPTIONS with no Access-Control-Request-Method": "OPTIONS /query HTTP/1.1\r\nHost: localhost:8083\r\nOrigin: http://localhost:3000\r\n\r\n",
		"an OPTIONS with no Origin":                        "OPTIONS /query HTTP/1.1\r\nHost: localhost:8083\r\nAccess-Control-Request-Method: POST\r\n\r\n",
		"a POST with the preflight's headers":              "POST /query HTTP/1.1\r\nHost: localhost:8083\r\nOrigin: http://localhost:3000\r\nAccess-Control-Request-Method: POST\r\nContent-Length: 0\r\n\r\n",
	} {
		m, err := h.buildHTTPMock(&FinalHTTP{Req: []byte(reqBuf), Resp: corsPreflightResponse, ReqTimestampMock: time.Now(), ResTimestampMock: time.Now()},
			8083, "test-conn", models.OutgoingOptions{})
		if err != nil || m == nil {
			t.Fatalf("%s: buildHTTPMock = (%v, %v)", name, m, err)
		}
		if m.Spec.Metadata["type"] != string(models.HTTPClient) || m.TestModeInfo.Lifetime != models.LifetimePerTest || m.IsProtocolPhase() {
			t.Errorf("%s: metadata %v, lifetime %v: want the test's data", name, m.Spec.Metadata, m.TestModeInfo.Lifetime)
		}
	}
}

// liveFrom is raw as decode reads a live request.
func liveFrom(t *testing.T, raw []byte) *req {
	t.Helper()
	r, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(raw)))
	if err != nil {
		t.Fatalf("parse live request: %v", err)
	}
	r.Header.Set("Host", r.Host)
	return &req{method: r.Method, url: r.URL, header: r.Header, raw: raw}
}

// Under the proven switch a recorded preflight answers only that very
// preflight: method, path and every header compared, the browser's
// Access-Control-Request-Headers and Origin included. It answers it as often
// as asked, through the reusable door (UpdateUnFilteredMock), never consumed
// as a test's mock is.
func TestACORSPreflightIsComparedStrictlyAndServedAsOftenAsAsked(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := &HTTP{Logger: zaptest.NewLogger(t)}
	rec, err := h.buildHTTPMock(&FinalHTTP{Req: corsPreflight("/query", "content-type"), Resp: corsPreflightResponse,
		ReqTimestampMock: time.Now(), ResTimestampMock: time.Now()}, 8083, "test-conn", models.OutgoingOptions{})
	if err != nil || rec == nil {
		t.Fatalf("buildHTTPMock = (%v, %v)", rec, err)
	}
	rec.Name = "preflight"

	db := &mockMemDb{mocks: []*models.Mock{rec}, updateUnFilteredReturn: true}
	for i := 0; i < 5; i++ {
		ok, served, diag, err := newHTTP().match(context.Background(), liveFrom(t, corsPreflight("/query", "content-type")), db, nil, nil, nil, true, false, false, true, true)
		if err != nil || !ok || served == nil || served.Name != "preflight" {
			t.Fatalf("preflight %d: ok %v, served %v, diag %+v, err %v", i+1, ok, served, diag, err)
		}
	}
	if db.deletedFiltered != nil || db.updateUnFilteredMockNew == nil {
		t.Fatalf("a preflight was consumed through the per-test door (%v), or not through the reusable one (%v)", db.deletedFiltered, db.updateUnFilteredMockNew)
	}

	for name, raw := range map[string][]byte{
		"other request headers": corsPreflight("/query", "content-type,authorization"),
		"another path":          corsPreflight("/upload", "content-type"),
		"another origin":        []byte(strings.Replace(string(corsPreflight("/query", "content-type")), "Origin: http://localhost:3000", "Origin: http://localhost:4000", 1)),
	} {
		ok, served, diag, err := newHTTP().match(context.Background(), liveFrom(t, raw), db, nil, nil, nil, true, false, false, true, true)
		if err != nil || ok || served != nil {
			t.Errorf("%s: ok %v, served %v, err %v; want a miss", name, ok, served, err)
			continue
		}
		if diag == nil || diag.closest != rec || len(diag.differs) == 0 {
			t.Errorf("%s: diag %+v; want the recorded preflight closest, with what differs", name, diag)
		}
	}
}
