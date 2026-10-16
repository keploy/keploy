//go:build linux

package replay

import (
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
// passed" line, though its answer matched, wherever the comparison prints one
// (an HTTP test, compared loudly once it has failed on its own; a gRPC test; a
// streamed test). The rows where nothing stands against the test are the
// controls: the line is printed, and captured, where it is due.
//
// The HTTP rows set --schema-match, whose comparison prints the line. Under
// the switch the setting is ignored (every body is compared value by value),
// so there they are plain HTTP tests, and a plain HTTP test that nothing
// stands against prints no line, the switch on or off: the value comparison
// matches quietly first (compareHTTPRespForReplay). That cell is not this
// test's to pin either way (a line for a test that passed contradicts
// nothing); the switch-off row is the HTTP control, where --schema-match is
// honoured and the schema comparison prints the line.
func TestRunTestSet_AProvenFailurePrintsNoPassLine(t *testing.T) {
	const body = `{"orders":[{"id":"ord_LIT","status":"paid"}]}`
	miss := func() ([]models.UnmatchedCall, error) {
		return []models.UnmatchedCall{{Protocol: "HTTP", ActualSummary: "GET /quotes"}}, nil
	}
	for _, path := range []string{"HTTP --schema-match", "gRPC", "streamed"} {
		for _, c := range []struct {
			name                 string
			on, miss, unconsumed bool
			passed               bool
		}{
			{"nothing against it", true, false, false, true},
			{"a miss, the switch off", false, true, false, true},
			{"a miss", true, true, false, false},
			{"an unconsumed test mock", true, false, true, false},
		} {
			t.Run(path+"/"+c.name, func(t *testing.T) {
				// plainHTTP: an HTTP row under the switch, compared as plain
				// HTTP since --schema-match is ignored there.
				plainHTTP := path == "HTTP --schema-match" && c.on
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
				case "HTTP --schema-match":
					// The plain comparison prints no pass line in the main
					// loop for a test nothing stands against
					// (compareHTTPRespForReplay matches quietly first); the
					// schema comparison, honoured with the switch off, does.
					// A test failed on its own is compared loudly
					// (compareHTTPResp), which would print it.
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
				console := captureConsole(t, func() { h.run(t) })
				switch printed := strings.Contains(console, "Testrun passed for testcase"); {
				case !c.passed && printed:
					t.Errorf("the console printed the pass line for a test the switch failed\n%s", console)
				case c.passed && !plainHTTP && !printed:
					t.Errorf("the console printed no pass line where the comparison prints it\n%s", console)
				}
				want := models.TestStatusPassed
				if !c.passed {
					want = models.TestStatusFailed
				}
				assertStatus(t, h, "test-1", want, "the switch's verdict")
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
