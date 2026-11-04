package http

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// answeredStore is a store whose pool is its mockMemDb's, that tells the
// recordings of the set that answered and redacted something
// (integrations.AnsweredRedactingReader) and whether it kept them all.
type answeredStore struct {
	*mockMemDb
	answered []*models.Mock
	complete bool
}

func (s answeredStore) AnsweredRedacting(kind models.Kind) ([]*models.Mock, bool) {
	var out []*models.Mock
	for _, m := range s.answered {
		if m.Kind == kind {
			out = append(out, m)
		}
	}
	return out, s.complete
}

// The secret the call sends in its path, beside liveSecret in its body.
const pathSecret = "pq7Seg4Ret"

// answeredRedacting is a recording that answered a call like the row's
// earlier and is in no read now: the obfuscator redacted its second path
// segment and its assertion.
func answeredRedacting() *models.Mock {
	m := recorded("answered", "/x/REDACTED_abc", map[string]string{"Content-Type": "application/json"}, `{"assertion":"REDACTED_abc","x":"alpha1"}`)
	m.Noise = []string{`^REDACTED_abc$`}
	return m
}

// answeredMiss is the report of the call POST /x/pathSecret, liveSecret in
// its body, at the pool, matched with the switch off (mode "off") or on,
// the report built at store.
func answeredMiss(t *testing.T, mode string, store answeredStore) *models.MockMismatchReport {
	t.Helper()
	t.Setenv(provenswitch.Env, map[string]string{"on": "1"}[mode])
	path, body := "/x/"+pathSecret, `{"assertion":"`+liveSecret+`","x":"alpha1"}`
	in := sent(path, map[string]string{"Content-Type": "application/json"}, body)
	request, _ := http.NewRequest("POST", "http://api.example"+path, strings.NewReader(body))
	request.Header = in.header.Clone()
	var diag *matchDiag
	if mode == "on" {
		got, d := strictMatch(t, in, nil, nil, nil, store.mocks...)
		if got != nil {
			t.Fatalf("served the call with %q", got.Name)
		}
		diag = d
	} else {
		ok, _, d, err := newHTTP().match(context.Background(), in, store, nil, nil, nil, true, false, true, true, true)
		if err != nil || ok {
			t.Fatalf("served the call (%v)", err)
		}
		diag = d
	}
	r := newHTTP().buildHTTPMismatchReport(request, []byte(body), store, nil, nil, nil, diag)
	if r == nil {
		t.Fatal("no report")
	}
	return r
}

// A miss is redacted by the places of the set's recordings that answered a
// call before and redacted something (integrations.AnsweredRedacting), which
// a read of the pool no longer holds: a recording of another call left, or
// none (the summary names the path redacted then). When the store did not
// keep them all, the call's path, query values and body are redacted whole.
// Switch off and on.
func TestAMissIsRedactedByTheRecordingsThatAnsweredBefore(t *testing.T) {
	other := func() []*models.Mock {
		return []*models.Mock{recorded("other", "/other/thing", map[string]string{"Content-Type": "application/json"}, `{"x":"alpha1"}`)}
	}
	for _, mode := range []string{"off", "on"} {
		for _, tc := range []struct {
			name     string
			pool     []*models.Mock
			answered []*models.Mock
			complete bool
			// shown is what of the call the report still shows.
			shown string
		}{
			{"another call's recording left", other(), []*models.Mock{answeredRedacting()}, true, "alpha1"},
			{"none left", nil, []*models.Mock{answeredRedacting()}, true, "POST /x/"},
			{"another call's recording left, not every answered kept", other(), nil, false, ""},
			{"none left, not every answered kept", nil, nil, false, ""},
		} {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				// The pool has room past its end, which the redaction
				// must not write into: a store may share it.
				pool := append(make([]*models.Mock, 0, len(tc.pool)+4), tc.pool...)
				r := answeredMiss(t, mode, answeredStore{mockMemDb: &mockMemDb{mocks: pool}, answered: tc.answered, complete: tc.complete})
				for i, m := range pool[len(pool):cap(pool)] {
					if m != nil {
						t.Errorf("the redaction wrote %q past the pool's end (%d)", m.Name, i)
					}
				}
				carriesNo(t, r, liveSecret)
				carriesNo(t, r, pathSecret)
				all := r.ActualSummary + "\n" + r.ReceivedReq + "\n" + r.Diff
				if tc.shown != "" && !strings.Contains(all, tc.shown) {
					t.Errorf("the report lost %q, which no recording redacted:\n%s", tc.shown, all)
				}
				if !tc.complete && strings.Contains(all, "alpha1") {
					t.Errorf("not every answered recording kept, the report still shows the body:\n%s", all)
				}
			})
		}
	}
}

// The debug line of the schema pass for a recording at another path names
// the call's path with the segments the set's answered recordings redacted
// redacted too, and redacted whole when the store did not keep them all.
func TestTheSchemaPassLogsThePathRedactedByTheRecordingsThatAnsweredBefore(t *testing.T) {
	for _, complete := range []bool{true, false} {
		name := map[bool]string{true: "every answered kept", false: "not every answered kept"}[complete]
		t.Run(name, func(t *testing.T) {
			t.Setenv(provenswitch.Env, "")
			core, logs := observer.New(zap.DebugLevel)
			h := newHTTP()
			h.Logger = zap.New(core)
			body := `{"assertion":"` + liveSecret + `","x":"alpha1"}`
			in := sent("/x/"+pathSecret, map[string]string{"Content-Type": "application/json"}, body)
			// A recording of the call's shape at another path, which
			// redacted nothing.
			pool := []*models.Mock{recorded("other", "/y/z", map[string]string{"Content-Type": "application/json"}, `{"x":"alpha1"}`)}
			var answered []*models.Mock
			if complete {
				answered = []*models.Mock{answeredRedacting()}
			}
			store := answeredStore{mockMemDb: &mockMemDb{mocks: pool}, answered: answered, complete: complete}
			if ok, _, _, err := h.match(context.Background(), in, store, nil, nil, nil, true, false, true, true, true); err != nil || ok {
				t.Fatalf("served the call (%v)", err)
			}
			lines := logs.FilterMessageSnippet("The url path of mock and request aren't the same").All()
			if len(lines) == 0 {
				t.Fatalf("no schema pass line: %v", logs.All())
			}
			for _, e := range lines {
				got, _ := e.ContextMap()["input path"].(string)
				if strings.Contains(got, pathSecret) {
					t.Errorf("the line names the path %q", got)
				}
				if complete && !strings.HasPrefix(got, "/x/") {
					t.Errorf("the line names the path %q; want /x/ and the redacted segment", got)
				}
			}
		})
	}
}

var _ integrations.AnsweredRedactingReader = answeredStore{}
