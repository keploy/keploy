//go:build linux

package replay

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// The console says nothing that contradicts a verdict of the proven switch: a
// test it fails on a miss or on an unconsumed test mock prints no "Testrun
// passed" line, though its answer matched, for a plain HTTP test, one under
// --schema-match, a gRPC test and a streamed test; nor does a test that
// --assert-dependencies fails on an unconsumed test mock with the switch off (a
// streamed test asserts its dependencies only under the switch), nor one the
// switch fails on a capture loss. The rows where nothing stands against the
// test are the controls: the line is printed, and captured, where it is due.
// Under the switch --schema-match is ignored
// (TestSchemaMatchIsIgnoredUnderTheProvenSwitch), so there its rows are plain
// HTTP tests; with the switch off they take the schema comparison. A passing
// plain HTTP test printed none at all: compareHTTPRespForReplay matched it
// quietly first (#3987) and returned the pass before the comparison that prints
// the line ran.
func TestRunTestSet_AProvenFailurePrintsNoPassLine(t *testing.T) {
	const body = `{"orders":[{"id":"ord_LIT","status":"paid"}]}`
	miss := func() ([]models.UnmatchedCall, error) {
		return []models.UnmatchedCall{{Protocol: "HTTP", ActualSummary: "GET /quotes"}}, nil
	}
	for _, path := range []string{"HTTP", "HTTP --schema-match", "gRPC", "streamed"} {
		for _, c := range []struct {
			name                 string
			on, miss, unconsumed bool
			passLine             bool
			assertDeps           bool
			captureLoss          bool
		}{
			{"nothing against it", true, false, false, true, false, false},
			{"a miss, the switch off", false, true, false, true, false, false},
			{"a miss", true, true, false, false, false, false},
			{"an unconsumed test mock", true, false, true, false, false, false},
			// --assert-dependencies fails it as the switch does.
			{"an unconsumed test mock, --assert-dependencies, the switch off", false, false, true, false, true, false},
			// The switch fails a test keploy's agent lost a call of.
			{"a capture loss", true, false, false, false, false, true},
		} {
			if c.assertDeps && path == "streamed" {
				// A streamed test asserts its dependencies only under the
				// switch: nothing fails it here.
				continue
			}
			t.Run(path+"/"+c.name, func(t *testing.T) {
				setSwitch(t, c.on)
				app := newTmplApp(t, func(a *tmplApp, mux *http.ServeMux) {
					mux.HandleFunc("/orders", func(w http.ResponseWriter, r *http.Request) {
						a.record(r)
						if path != "streamed" {
							writeJSON(w, http.StatusOK, body)
							return
						}
						w.Header().Set("Content-Type", "application/x-ndjson")
						w.WriteHeader(http.StatusOK)
						_, _ = io.WriteString(w, body+"\n")
					})
				})
				var tc *models.TestCase
				var tune func(*config.Config)
				switch path {
				case "HTTP":
					tc = tmplCase(1, app.addr, "GET", "/orders", nil, "", recorded(200, nil, body, nil))
				case "HTTP --schema-match":
					tc = tmplCase(1, app.addr, "GET", "/orders", nil, "", recorded(200, nil, body, nil))
					tune = func(cfg *config.Config) { cfg.Test.SchemaMatch = true }
				case "gRPC":
					tc = &models.TestCase{Version: models.GetVersion(), Kind: models.GRPC_EXPORT, Name: "test-1", GrpcReq: provenGRPCReq(app.addr), GrpcResp: provenGRPCResp()}
				case "streamed":
					tc = tmplCase(1, app.addr, "GET", "/orders", nil, "", models.HTTPResp{
						StatusCode: 200, StatusMessage: "OK",
						Header:     map[string]string{"Content-Type": "application/x-ndjson", "Date": "Mon, 05 Oct 2026 10:00:00 GMT"},
						Body:       body,
						StreamBody: []models.HTTPStreamChunk{{Data: []models.HTTPStreamDataField{{Key: "raw", Value: body}}}},
					})
				}
				if c.assertDeps || c.captureLoss {
					pathTune := tune
					tune = func(cfg *config.Config) {
						if pathTune != nil {
							pathTune(cfg)
						}
						cfg.Test.AssertDependencies = c.assertDeps
						// Capture losses are counted only for a run that mocks.
						cfg.Test.Mocking = c.captureLoss
					}
				}
				h := newTmplHarness(t, &tmplConf{}, []*models.TestCase{tc}, tune)
				if path == "gRPC" {
					h.r.hookImpl = &grpcStubHooks{TestHooks: h.r.hookImpl, answer: func(models.GrpcReq) *models.GrpcResp {
						r := provenGRPCResp()
						return &r
					}}
				}
				if c.miss {
					h.r.instrumentation.(*prInstr).mockErrorsFor = missesByTest(miss)
				}
				if c.unconsumed {
					h.r.mappingDB = &mappedDB{tests: map[string][]models.MockEntry{"test-1": {{Name: "mock-1", Kind: string(models.HTTP)}}}}
					h.r.hookImpl = &consumingHooks{TestHooks: h.r.hookImpl}
				}
				if c.captureLoss {
					// The mark, then the test's read: a loss while it ran.
					h.r.instrumentation = &prLossInstr{prInstr: h.r.instrumentation.(*prInstr), answers: []models.CaptureLosses{nil, {{Kind: models.CaptureLossParserStall, Count: 1, What: "to 10.0.0.9:5432"}}}}
					h.r.markPIDMaps(context.Background())
				}
				console := captureConsole(t, func() { h.run(t) })
				if printed := strings.Contains(console, "Testrun passed for testcase"); printed != c.passLine {
					t.Errorf("the console printed the pass line: %v, want %v\n%s", printed, c.passLine, console)
				}
				want := models.TestStatusPassed
				if !c.passLine {
					want = models.TestStatusFailed
				}
				// The verdict the set's report holds, after every pass over
				// the set's results (failTestsWithCaptureLoss among them).
				if h.report.report == nil {
					t.Fatal("the set wrote no report")
				}
				got := models.TestStatus("")
				for _, r := range h.report.report.Tests {
					if r.TestCaseID == "test-1" {
						got = r.Status
					}
				}
				if got != want {
					t.Errorf("test-1 is %s in the set's report, want %s: the switch's verdict", got, want)
				}
			})
		}
	}
}

func provenGRPCReq(addr string) models.GrpcReq {
	req := models.GrpcReq{Headers: models.GrpcHeaders{
		PseudoHeaders:   map[string]string{":authority": addr, ":path": "/orders.Orders/Get", ":method": "POST", ":scheme": "http"},
		OrdinaryHeaders: map[string]string{"content-type": "application/grpc"},
	}}
	req.SetMessages([]models.GrpcLengthPrefixedMessage{{DecodedData: `1: "ord_LIT"`}})
	return req
}

func provenGRPCResp() models.GrpcResp {
	r := models.GrpcResp{
		Headers:  models.GrpcHeaders{PseudoHeaders: map[string]string{":status": "200"}, OrdinaryHeaders: map[string]string{"content-type": "application/grpc"}},
		Trailers: models.GrpcHeaders{OrdinaryHeaders: map[string]string{"grpc-status": "0"}},
	}
	r.SetMessages([]models.GrpcLengthPrefixedMessage{{DecodedData: `1: "ord_LIT"`}})
	return r
}

// A test the switch fails on its own (a miss) is compared as written: the
// additive-schema auto-pass, which passes an answer that only gained fields,
// does not run for it, so its rows say the answer differed and nothing says it
// passed. With strict comparison under the switch (O-SF) it runs for no test.
func TestRunTestSet_NoAdditiveAutoPassForATestFailedOnItsOwn(t *testing.T) {
	for _, miss := range []bool{false, true} {
		name := "nothing against it"
		if miss {
			name = "a miss"
		}
		t.Run(name, func(t *testing.T) {
			setSwitch(t, true)
			app := newTmplApp(t, func(a *tmplApp, mux *http.ServeMux) {
				mux.HandleFunc("/orders", func(w http.ResponseWriter, r *http.Request) {
					a.record(r)
					writeJSON(w, http.StatusOK, `{"ok":true,"added":1}`)
				})
			})
			h := newTmplHarness(t, &tmplConf{}, []*models.TestCase{tmplCase(1, app.addr, "GET", "/orders", nil, "", recorded(200, nil, `{"ok":true}`, nil))}, nil)
			if miss {
				h.r.instrumentation.(*prInstr).mockErrorsFor = missesByTest(func() ([]models.UnmatchedCall, error) {
					return []models.UnmatchedCall{{Protocol: "HTTP", ActualSummary: "GET /quotes"}}, nil
				})
			}
			core, logs := observer.New(zap.InfoLevel)
			h.r.logger = zap.New(core)
			h.run(t)
			autoPassed := len(logs.FilterMessageSnippet("Passing testcase by default").All()) > 0
			r := h.result(t, "test-1")
			bodyNormal := len(r.Result.BodyResult) > 0 && r.Result.BodyResult[0].Normal
			// Under the switch O-SF turns the additive auto-pass off for every
			// test (additiveAutoPassOff), so neither row is auto-passed: an
			// answer that only gained a field fails like any other difference.
			if autoPassed || bodyNormal {
				t.Fatalf("auto-passed %v, body row normal %v; want neither under the switch", autoPassed, bodyNormal)
			}
		})
	}
}
