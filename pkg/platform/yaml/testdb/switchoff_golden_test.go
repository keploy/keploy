package testdb

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// Without the proven switch, a test case's file is written byte for byte as
// the goldens hold it (S12 v16 §4.5): a test case without an Origin writes
// what keploy wrote before test cases had one, and reads back without one.
//
// To regenerate: KEPLOY_GOLDEN_OUT=<dir> go test -run
// TestTestCaseFilesAreUnchangedWithoutTheSwitch ./pkg/platform/yaml/testdb/,
// then copy <dir> to testdata/switchoff_golden. To check a change that must
// leave these files alone, generate them on a checkout from before the
// change, with this file added to it, and run this test after the change.
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

// goldenTestCases are an HTTP and a gRPC test case as keploy record writes them.
func goldenTestCases() []*models.TestCase {
	at := time.Date(2026, 10, 22, 9, 0, 1, 500000000, time.UTC)
	httpTc := &models.TestCase{
		Version: models.GetVersion(), Kind: models.HTTP, Name: "post-url-1", Created: at.Unix(),
		HTTPReq: models.HTTPReq{Method: "POST", ProtoMajor: 1, ProtoMinor: 1, URL: "http://localhost:8080/url",
			Header: map[string]string{"Content-Type": "application/json"}, Body: `{"url":"https://example.com"}`, Timestamp: at},
		HTTPResp: models.HTTPResp{StatusCode: 200, Header: map[string]string{"Content-Type": "application/json"},
			Body: `{"ts":1,"url":"http://localhost:8080/Lhr4BWAi"}`, Timestamp: at.Add(20 * time.Millisecond)},
		Curl:  "curl --request POST --url http://localhost:8080/url",
		Noise: map[string][]string{"body.ts": {}},
	}
	grpcTc := &models.TestCase{
		Version: models.GetVersion(), Kind: models.GRPC_EXPORT, Name: "test-2", Created: at.Unix(),
		GrpcReq: models.GrpcReq{
			Headers:   models.GrpcHeaders{PseudoHeaders: map[string]string{":path": "/orders.Orders/Get", ":method": "POST"}, OrdinaryHeaders: map[string]string{"content-type": "application/grpc"}},
			Body:      models.GrpcLengthPrefixedMessage{MessageLength: 4, DecodedData: "1: 42\n"},
			Timestamp: at.Add(time.Second),
		},
		GrpcResp: models.GrpcResp{
			Headers:   models.GrpcHeaders{PseudoHeaders: map[string]string{":status": "200"}, OrdinaryHeaders: map[string]string{"grpc-status": "0"}},
			Body:      models.GrpcLengthPrefixedMessage{MessageLength: 6, DecodedData: "1: \"ok\"\n"},
			Timestamp: at.Add(time.Second + 5*time.Millisecond),
		},
	}
	return []*models.TestCase{httpTc, grpcTc}
}

func TestTestCaseFilesAreUnchangedWithoutTheSwitch(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	for _, format := range []yaml.Format{yaml.FormatYAML, yaml.FormatJSON} {
		ext := format.FileExtension()
		t.Run(ext, func(t *testing.T) {
			ts := NewWithFormat(zap.NewNop(), t.TempDir(), format)
			for _, tc := range goldenTestCases() {
				f, err := ts.TestCaseFile(context.Background(), tc, "test-set-0")
				if err != nil {
					t.Fatalf("%s: %v", tc.Name, err)
				}
				checkSwitchOffGolden(t, tc.Name+"."+ext, f.Data)
			}
		})
	}
}
