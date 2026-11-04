package http

import (
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap/zaptest"
)

// The recorder states each HTTP mock's lifetime (models.MetadataLifetimeKey):
// an exchange is the data of its test, a recordOne passthrough representative
// is served to every test. The type tag older replayers read is unchanged.
func TestRecorderStatesHTTPLifetimes(t *testing.T) {
	h := &HTTP{Logger: zaptest.NewLogger(t)}
	reqTs := time.Unix(1_700_002_000, 0)
	build := func(opts models.OutgoingOptions) *models.Mock {
		t.Helper()
		m, err := h.buildHTTPMock(&FinalHTTP{
			Req:              canonicalRequest,
			Resp:             canonicalResponse,
			ReqTimestampMock: reqTs,
			ResTimestampMock: reqTs.Add(5 * time.Millisecond),
		}, 80, "test-conn", opts)
		if err != nil || m == nil {
			t.Fatalf("buildHTTPMock: mock %v, err %v", m, err)
		}
		return m
	}
	for _, tc := range []struct {
		name     string
		opts     models.OutgoingOptions
		typ      string
		stated   string
		lifetime models.Lifetime
	}{
		{"an exchange", models.OutgoingOptions{}, string(models.HTTPClient), models.MetadataLifetimePerTest, models.LifetimePerTest},
		{
			"a recordOne representative",
			models.OutgoingOptions{PassThroughPorts: []models.PassThroughRule{{Port: 80, Mode: models.PassThroughRecordOne}}},
			"config", models.MetadataLifetimeSession, models.LifetimeSession,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := build(tc.opts)
			if got := m.Spec.Metadata["type"]; got != tc.typ {
				t.Fatalf("type %q, want %q", got, tc.typ)
			}
			if got := m.Spec.Metadata[models.MetadataLifetimeKey]; got != tc.stated {
				t.Fatalf("stated lifetime %q, want %q", got, tc.stated)
			}
			if m.TestModeInfo.Lifetime != tc.lifetime {
				t.Fatalf("lifetime %v, want %v", m.TestModeInfo.Lifetime, tc.lifetime)
			}
			// Read back from disk, the mock derives what was stated.
			c := m.DeepCopy()
			c.TestModeInfo = models.TestModeInfo{}
			c.DeriveLifetime()
			if c.TestModeInfo.Lifetime != tc.lifetime {
				t.Fatalf("derived %v on load, want %v", c.TestModeInfo.Lifetime, tc.lifetime)
			}
		})
	}
}

// A miss on a recording that serves each test its own HTTP calls says how to
// serve a call to every test (keploy.yaml sessionCalls, or recording again):
// the call may be one another test recorded or, under the proven switch, one
// the application made outside every test, which is what a declaration serves
// there; another test's recording stays that test's. Under the switch a call
// keploy.yaml declares already is told what the declaration serves, not to
// declare it. A miss on a recording made before says nothing of it, as every
// recorded call is served to every test.
func TestMissOnAStatedRecordingTellsHowToShareACall(t *testing.T) {
	h := newHTTP()
	stated := httpMockOnHost("mock-1", "GET", "/api/orders?id=90", "192.0.2.10")
	stated.Spec.Metadata = map[string]string{"type": string(models.HTTPClient), models.MetadataLifetimeKey: models.MetadataLifetimePerTest}
	legacy := httpMockOnHost("mock-2", "GET", "/api/orders?id=90", "192.0.2.10")
	legacy.Spec.Metadata = map[string]string{"type": string(models.HTTPClient)}
	const (
		none     = ""
		off      = "If another test recorded this call"
		on       = "If the application made this call outside every test (in TestMain or at start-up)"
		declared = "keploy.yaml declares this call a session call, which serves a recording of it made outside every test"
	)
	decl := []models.SessionCallRule{{Host: "auth.example.com", Method: "POST", PathPrefix: "/oauth"}}
	for _, tc := range []struct {
		name string
		pool []*models.Mock
		// recording: what the mock db says of the set's recording
		// (integrations.LifetimeStater), when it says anything.
		recording *bool
		proven    bool
		rules     []models.SessionCallRule
		want      string
	}{
		{"stated recording", []*models.Mock{stated}, nil, false, nil, off},
		{"stated recording, proven switch on", []*models.Mock{stated}, nil, true, nil, on},
		{"stated recording, call declared", []*models.Mock{stated}, nil, false, decl, off},
		{"stated recording, call declared, proven switch on", []*models.Mock{stated}, nil, true, decl, declared},
		{"stated recording, another call declared, proven switch on", []*models.Mock{stated}, nil, true,
			[]models.SessionCallRule{{Host: "auth.example.com", Method: "GET"}}, on},
		{"recording made before", []*models.Mock{legacy}, nil, false, nil, none},
		{"recording made before, proven switch on", []*models.Mock{legacy}, nil, true, decl, none},
		// The test's own calls are consumed, and it holds no other HTTP mock:
		// the mock db knows the set's recording states its lifetimes.
		{"stated recording, nothing left to the test", nil, ptrTo(true), false, nil, off},
		{"stated recording, nothing left to the test, proven switch on", nil, ptrTo(true), true, nil, on},
		{"recording made before, nothing left to the test", nil, ptrTo(false), false, nil, none},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.proven {
				t.Setenv(provenswitch.Env, "1")
			} else {
				t.Setenv(provenswitch.Env, "0")
			}
			models.SetSessionCalls(tc.rules)
			t.Cleanup(func() { models.SetSessionCalls(nil) })
			var db integrations.MockMemDb = &mockMemDb{mocks: tc.pool}
			if tc.recording != nil {
				db = statedDb{MockMemDb: db, states: *tc.recording}
			}
			report := h.buildHTTPMismatchReport(reqToHost("POST", "/oauth/token", "auth.example.com"), nil, db, tc.pool, nil, nil,
				&matchDiag{phase: models.MatchPhaseSchema, candidates: len(tc.pool)})
			snippet := strings.Contains(report.NextSteps, `sessionCalls: [{host: "auth.example.com", method: "POST", pathPrefix: "/oauth/token"}]`)
			for _, hint := range []string{off, on, declared} {
				if got := strings.Contains(report.NextSteps, hint); got != (hint == tc.want) {
					t.Fatalf("hint %q present %v, want %v:\n%s", hint, got, hint == tc.want, report.NextSteps)
				}
			}
			if want := tc.want == off || tc.want == on; snippet != want {
				t.Fatalf("declaration snippet present %v, want %v:\n%s", snippet, want, report.NextSteps)
			}
			if tc.want == on && !strings.Contains(report.NextSteps, "a recording another test made of it is served to that test only, declared or not") {
				t.Fatalf("the switch's hint does not say another test's recording stays that test's:\n%s", report.NextSteps)
			}
		})
	}
}

// statedDb is a mock db that says whether the set's recording states its HTTP
// mocks' lifetimes, as the proxy's mock manager does.
type statedDb struct {
	integrations.MockMemDb
	states bool
}

func (d statedDb) StatesLifetimes(kind models.Kind) bool { return d.states && kind == models.HTTP }

// AnsweredRedacting forwards the db's (integrations.AnsweredRedactingReader),
// as the proxy's mock manager tells both.
func (d statedDb) AnsweredRedacting(kind models.Kind) ([]*models.Mock, bool) {
	return integrations.AnsweredRedacting(d.MockMemDb, kind)
}

func ptrTo[T any](v T) *T { return &v }
