package http

import (
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap/zaptest"
)

// preflightReq is Chromium's preflight before a cross-origin POST /query
// carrying a Content-Type: application/json (enterprise-ui's dashboard to
// api-server on :8083).
const preflightReq = "OPTIONS /query HTTP/1.1\r\nHost: localhost:8083\r\n" +
	"Origin: http://localhost:3000\r\n" +
	"Access-Control-Request-Method: POST\r\n" +
	"Access-Control-Request-Headers: content-type\r\n" +
	"Sec-Fetch-Mode: cors\r\n\r\n"

const preflightAllowed = "HTTP/1.1 204 No Content\r\n" +
	"Access-Control-Allow-Origin: http://localhost:3000\r\n" +
	"Access-Control-Allow-Methods: POST\r\n" +
	"Access-Control-Allow-Headers: content-type\r\n" +
	"Vary: Origin\r\n\r\n"

const preflightRefused = "HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\n\r\n"

// guardedCall is the call the preflight guards.
const guardedCall = "POST /query HTTP/1.1\r\nHost: localhost:8083\r\n" +
	"Origin: http://localhost:3000\r\nContent-Type: application/json\r\n" +
	"Content-Length: 2\r\n\r\n{}"

const guardedAnswer = "HTTP/1.1 200 OK\r\nAccess-Control-Allow-Origin: http://localhost:3000\r\n" +
	"Content-Type: application/json\r\nContent-Length: 2\r\n\r\n{}"

// recordBoth records one exchange through each HTTP recorder, the V2 one
// (buildHTTPMock) and the legacy one (parseFinalHTTP), which must agree.
func recordBoth(t *testing.T, req, resp string) map[string]*models.Mock {
	t.Helper()
	h := &HTTP{Logger: zaptest.NewLogger(t)}
	at := time.Unix(1_700_002_000, 0)
	v2, err := h.buildHTTPMock(&FinalHTTP{Req: []byte(req), Resp: []byte(resp), ReqTimestampMock: at, ResTimestampMock: at.Add(time.Millisecond)},
		8083, "conn-1", models.OutgoingOptions{})
	if err != nil || v2 == nil {
		t.Fatalf("V2 recorder: mock %v, err %v", v2, err)
	}
	return map[string]*models.Mock{"v2": v2, "legacy": runLegacyParseFinalHTTP(t, h, []byte(req), []byte(resp), 8083, "conn-1")}
}

// isPhase reports whether m is recorded as a phase (config, stated session,
// protocol-phase), and fails the row when it is half one.
func isPhase(t *testing.T, where string, m *models.Mock) bool {
	t.Helper()
	tag := m.Spec.Metadata["type"] == "config"
	stated := m.Spec.Metadata[models.MetadataLifetimeKey] == models.MetadataLifetimeSession
	held := m.TestModeInfo.Lifetime == models.LifetimeSession && m.TestModeInfo.LifetimeDerived
	phase := m.IsProtocolPhase()
	if tag != stated || tag != held || tag != phase {
		t.Fatalf("%s: half a phase: type %q, stated %q, held %v, protocol-phase %v", where,
			m.Spec.Metadata["type"], m.Spec.Metadata[models.MetadataLifetimeKey], m.TestModeInfo.Lifetime, phase)
	}
	if !tag && (m.Spec.Metadata["type"] != models.HTTPClient || m.Spec.Metadata[models.MetadataLifetimeKey] != models.MetadataLifetimePerTest) {
		t.Fatalf("%s: not a phase, yet not recorded as an exchange is: %v", where, m.Spec.Metadata)
	}
	return tag
}

// Under the proven switch a CORS preflight the server allowed is recorded as
// a phase of the call it guards (protocol-phase: served to any test as often
// as asked, owned by none), by both recorders; the call it guards is the
// test's data, as before.
func TestAnAllowedCORSPreflightIsRecordedAsAPhaseOfTheCallItGuards(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for path, m := range recordBoth(t, preflightReq, preflightAllowed) {
		if !isPhase(t, path+" preflight", m) {
			t.Errorf("%s: an allowed CORS preflight is recorded as the test's data", path)
		}
	}
	for path, m := range recordBoth(t, guardedCall, guardedAnswer) {
		if isPhase(t, path+" guarded call", m) {
			t.Errorf("%s: the call a preflight guards is recorded as a phase", path)
		}
	}
}

// Only an allowed CORS preflight is a phase: an OPTIONS the app sends itself,
// one with no Origin, a call carrying the preflight's headers that is no
// OPTIONS, and a preflight the server refused (the browser never made the
// call: the refusal is what the test got) stay the test's data.
func TestOnlyAnAllowedCORSPreflightIsAPhase(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for name, ex := range map[string][2]string{
		"an app's own OPTIONS":                {strings.Replace(preflightReq, "Access-Control-Request-Method: POST\r\n", "", 1), preflightAllowed},
		"an OPTIONS with no Origin":           {strings.Replace(preflightReq, "Origin: http://localhost:3000\r\n", "", 1), preflightAllowed},
		"a POST with the preflight's headers": {strings.Replace(strings.Replace(preflightReq, "OPTIONS", "POST", 1), "\r\n\r\n", "\r\nContent-Length: 0\r\n\r\n", 1), preflightAllowed},
		"a refused preflight":                 {preflightReq, preflightRefused},
	} {
		for path, m := range recordBoth(t, ex[0], ex[1]) {
			if isPhase(t, name+", "+path, m) {
				t.Errorf("%s (%s): recorded as a phase", name, path)
			}
		}
	}
}

// Without the proven switch a preflight is recorded as every exchange was.
func TestWithoutTheSwitchACORSPreflightIsRecordedAsBefore(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	for path, m := range recordBoth(t, preflightReq, preflightAllowed) {
		if isPhase(t, path, m) {
			t.Errorf("%s: a preflight is recorded as a phase without the switch", path)
		}
	}
}
