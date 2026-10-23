package models

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/provenswitch"
)

// Without the proven switch, a mock's and a test case's JSON (a test case is
// uploaded as JSON) and the recorded test windows a replay sends its agent are
// byte for byte what the goldens hold: a mock or test case
// without an Origin, and a window without a Clock, encode as they did before
// they had one. (Gob, which the agent streams mocks in, names every field of
// a type in its stream whatever the values, and was never byte-stable: map
// order varies. What it must keep is what it reads back.)
//
// To regenerate: KEPLOY_GOLDEN_OUT=<dir> go test -run
// TestWireJSONIsUnchangedWithoutTheSwitch ./pkg/models/, then copy <dir> to
// testdata/switchoff_golden. To check a change that must leave these bytes
// alone, generate them on a checkout from before the change, with this file
// added to it, and run this test after the change.
const goldenSwitchOffDir = "testdata/switchoff_golden"

func checkSwitchOffGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	if out := os.Getenv("KEPLOY_GOLDEN_OUT"); out != "" {
		if err := os.MkdirAll(out, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(out, name), got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(filepath.Join(goldenSwitchOffDir, name))
	if err != nil {
		t.Fatalf("read golden %s: %v", name, err)
	}
	if bytes.Equal(got, want) {
		return
	}
	gl, wl := strings.Split(string(got), "\n"), strings.Split(string(want), "\n")
	for i := 0; i < len(gl) || i < len(wl); i++ {
		var g, w string
		if i < len(gl) {
			g = gl[i]
		}
		if i < len(wl) {
			w = wl[i]
		}
		if g != w {
			t.Fatalf("%s differs from the golden without the proven switch, first at line %d:\n got: %q\nwant: %q", name, i+1, g, w)
		}
	}
}

func TestWireJSONIsUnchangedWithoutTheSwitch(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	at := time.Date(2026, 10, 22, 9, 0, 1, 500000000, time.UTC)
	mock := &Mock{Version: V1Beta1, Kind: HTTP, Name: "mock-6", ConnectionID: "7",
		Spec: MockSpec{
			Metadata:         map[string]string{"type": "mocks", "connID": "7"},
			HTTPReq:          &HTTPReq{Method: "POST", URL: "http://events:9000/e", Body: `{"event":"x"}`, Timestamp: at},
			HTTPResp:         &HTTPResp{StatusCode: 201, Body: `{"ok":true}`, Timestamp: at.Add(time.Millisecond)},
			ReqTimestampMock: at, ResTimestampMock: at.Add(time.Millisecond),
		}}
	tc := &TestCase{Version: V1Beta1, Kind: HTTP, Name: "post-url-1", Created: at.Unix(),
		HTTPReq:  HTTPReq{Method: "POST", URL: "http://localhost:8080/url", Body: `{"url":"x"}`, Timestamp: at},
		HTTPResp: HTTPResp{StatusCode: 200, Body: `{"ok":true}`, Timestamp: at.Add(20 * time.Millisecond)}}
	windows := []TestWindow{RecordedWindow(tc), {TestCase: "post-url-0", Start: at.Add(-time.Second), End: at.Add(-time.Second + time.Millisecond)}}
	for name, v := range map[string]any{"mock.json": mock, "testcase.json": tc, "windows.json": windows} {
		got, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		checkSwitchOffGolden(t, name, got)
	}
}
