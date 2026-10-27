//go:build linux

package replay

import (
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"golang.org/x/sys/unix"
)

// captureConsole runs fn with the process's standard output (fd 1) going to a
// pipe, and returns what was written there: what a user's console shows,
// whichever printer wrote it.
func captureConsole(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved, err := unix.Dup(1)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Dup2(int(w.Fd()), 1); err != nil {
		t.Fatal(err)
	}
	out := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		out <- string(b)
	}()
	defer func() {
		_ = unix.Dup2(saved, 1)
		_ = unix.Close(saved)
	}()
	fn()
	_ = unix.Dup2(saved, 1)
	_ = w.Close()
	return ansi.ReplaceAllString(<-out, "")
}

// ansi is a terminal color sequence, which the console printers write around
// a test case's id.
var ansi = regexp.MustCompile("\x1b\\[[0-9;]*m")

// A gRPC test case that fails on a template variable never prints the
// comparison's pass line: the comparison, under noise over the message,
// matches, and the console says nothing that contradicts the verdict.
func TestRunTestSet_AGRPCCaseFailedOnAVariablePrintsNoPassLine(t *testing.T) {
	const live = "usr_LIVE001"
	for _, c := range []struct {
		name, answerID string
		passLine       bool
	}{
		{"the answer holds the id", live, true},
		{"the answer holds another id under noise", "usr_OTHER01", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			app := newTmplApp(t, func(a *tmplApp, mux *http.ServeMux) {
				mux.HandleFunc("/users", func(w http.ResponseWriter, r *http.Request) {
					a.record(r)
					writeJSON(w, http.StatusCreated, `{"id":"`+live+`"}`)
				})
			})
			conf := &tmplConf{stored: models.TestSet{Template: map[string]interface{}{"id": "usr_REC0001"}}}
			resp := func(id string) models.GrpcResp {
				r := models.GrpcResp{
					Headers:  models.GrpcHeaders{PseudoHeaders: map[string]string{":status": "200"}, OrdinaryHeaders: map[string]string{"content-type": "application/grpc"}},
					Trailers: models.GrpcHeaders{OrdinaryHeaders: map[string]string{"grpc-status": "0"}},
				}
				r.SetMessages([]models.GrpcLengthPrefixedMessage{{DecodedData: `1: "` + id + `"`}})
				return r
			}
			req := models.GrpcReq{Headers: models.GrpcHeaders{
				PseudoHeaders:   map[string]string{":authority": app.addr, ":path": "/users.Users/Get", ":method": "POST", ":scheme": "http"},
				OrdinaryHeaders: map[string]string{"content-type": "application/grpc"},
			}}
			req.SetMessages([]models.GrpcLengthPrefixedMessage{{DecodedData: `1: "{{string .id}}"`}})
			cases := []*models.TestCase{
				tmplCase(1, app.addr, "POST", "/users", map[string]string{"Content-Type": "application/json"}, `{}`,
					recorded(201, nil, `{"id":"{{capture string .id}}"}`, conf.stored.Template)),
				{Version: models.GetVersion(), Kind: models.GRPC_EXPORT, Name: "test-2", GrpcReq: req, GrpcResp: resp("{{string .id}}")},
			}
			h := newTmplHarness(t, conf, cases, func(cfg *config.Config) {
				cfg.Test.GlobalNoise.Global = map[string]map[string][]string{"body": {"decoded_data": {}}}
			})
			h.r.hookImpl = &grpcStubHooks{TestHooks: h.r.hookImpl, answer: func(models.GrpcReq) *models.GrpcResp {
				r := resp(c.answerID)
				return &r
			}}
			console := captureConsole(t, func() { h.run(t) })
			printed := strings.Contains(console, `Testrun passed for testcase with id: "test-2"`)
			if printed != c.passLine {
				t.Errorf("the console printed the pass line for test-2: %v, want %v\n%s", printed, c.passLine, console)
			}
			want := models.TestStatusPassed
			if !c.passLine {
				want = models.TestStatusFailed
			}
			assertStatus(t, h, "test-2", want, "the gRPC answer's id against the captured one")
		})
	}
}

// A streamed test case that fails on a template variable never prints the
// comparison's pass line either: the stream comparison, under noise over the
// frame field, matches, and the console agrees with the verdict. Under
// --schema-match too (P1): the schema comparison matches, and no pass line
// contradicts the failure.
func TestRunTestSet_AStreamedCaseFailedOnAVariablePrintsNoPassLine(t *testing.T) {
	const live = "usr_LIVE001"
	for _, c := range []struct {
		name, frameID string
		schemaMatch   bool
		passLine      bool
	}{
		{"the frames hold the id", live, false, true},
		{"the frames hold another id under noise", "usr_OTHER01", false, false},
		{"--schema-match, the frames hold the id", live, true, true},
		{"--schema-match, the frames hold another id", "usr_OTHER01", true, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			app := newTmplApp(t, func(a *tmplApp, mux *http.ServeMux) {
				mux.HandleFunc("/events/", func(w http.ResponseWriter, r *http.Request) {
					a.record(r)
					w.Header().Set("Content-Type", "application/x-ndjson")
					w.WriteHeader(http.StatusOK)
					_, _ = io.WriteString(w, `{"user":"`+c.frameID+`"}`+"\n")
				})
			})
			conf := &tmplConf{stored: models.TestSet{Template: map[string]interface{}{"id": live}}}
			cases := []*models.TestCase{tmplCase(1, app.addr, "GET", "/events/{{string .id}}", nil, "", models.HTTPResp{
				StatusCode: 200, StatusMessage: "OK",
				Header:     map[string]string{"Content-Type": "application/x-ndjson", "Date": "Mon, 05 Oct 2026 10:00:00 GMT"},
				Body:       `{"user":"{{string .id}}"}`,
				StreamBody: []models.HTTPStreamChunk{{Data: []models.HTTPStreamDataField{{Key: "raw", Value: `{"user":"{{string .id}}"}`}}}},
			})}
			h := newTmplHarness(t, conf, cases, func(cfg *config.Config) {
				cfg.Test.GlobalNoise.Global = map[string]map[string][]string{"body": {"user": {}}}
				cfg.Test.SchemaMatch = c.schemaMatch
			})
			console := captureConsole(t, func() { h.run(t) })
			printed := strings.Contains(console, `Testrun passed for testcase with id: "test-1"`)
			if printed != c.passLine {
				t.Errorf("the console printed the pass line for test-1: %v, want %v\n%s", printed, c.passLine, console)
			}
			want := models.TestStatusPassed
			if !c.passLine {
				want = models.TestStatusFailed
			}
			assertStatus(t, h, "test-1", want, "the frames' id against the run's")
		})
	}
}

// UNDER --schema-match A TEMPLATE VARIABLE IS STILL ASSERTED (SM1): a
// read-back answering another id has the recorded schema, yet the variable's
// place does not hold the captured id, so the test case fails, graded
// TEMPLATE_VARIABLE and HIGH, and the console prints no pass line for it.
func TestRunTestSet_SchemaMatchStillFailsATemplateVariable(t *testing.T) {
	const live = "usr_LIVE001"
	for _, c := range []struct {
		name, answerID string
		want           models.TestStatus
	}{
		{"the read-back answers the captured id", live, models.TestStatusPassed},
		{"the read-back answers another id", "usr_OTHER01", models.TestStatusFailed},
	} {
		t.Run(c.name, func(t *testing.T) {
			app := newTmplApp(t, func(a *tmplApp, mux *http.ServeMux) {
				mux.HandleFunc("/users", func(w http.ResponseWriter, r *http.Request) {
					a.record(r)
					writeJSON(w, http.StatusCreated, `{"id":"`+live+`"}`)
				})
				mux.HandleFunc("/users/", func(w http.ResponseWriter, r *http.Request) {
					a.record(r)
					writeJSON(w, http.StatusOK, `{"id":"`+c.answerID+`"}`)
				})
			})
			conf := &tmplConf{stored: models.TestSet{Template: map[string]interface{}{"id": "usr_REC0001"}}}
			cases := []*models.TestCase{
				tmplCase(1, app.addr, "POST", "/users", map[string]string{"Content-Type": "application/json"}, `{"name":"alice"}`,
					recorded(201, nil, `{"id":"{{capture string .id}}"}`, conf.stored.Template)),
				tmplCase(2, app.addr, "GET", "/users/{{string .id}}", nil, "", recorded(200, nil, `{"id":"{{string .id}}"}`, conf.stored.Template)),
			}
			h := newTmplHarness(t, conf, cases, func(cfg *config.Config) { cfg.Test.SchemaMatch = true })
			console := captureConsole(t, func() { h.run(t) })
			r := h.result(t, "test-2")
			if r.Status != c.want {
				t.Fatalf("test-2: %s, want %s — %s; template failures %+v", r.Status, c.want, why(r), r.TemplateFailures)
			}
			if c.want == models.TestStatusFailed && (!hasCategory(r, models.TemplateVariable) || r.FailureInfo.Risk != models.High || len(r.TemplateFailures) != 1) {
				t.Errorf("test-2 is graded %s %v with %+v; want HIGH, TEMPLATE_VARIABLE and its place", r.FailureInfo.Risk, r.FailureInfo.Category, r.TemplateFailures)
			}
			printed := strings.Contains(console, `Testrun passed for testcase with id: "test-2"`)
			if printed != (c.want == models.TestStatusPassed) {
				t.Errorf("the console printed test-2's pass line: %v, want %v\n%s", printed, c.want == models.TestStatusPassed, console)
			}
		})
	}
}
