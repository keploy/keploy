// Table-driven coverage for the per-mock Format override (task #225 /
// DaemonSet Phase 0 per-session mock format). Three cases:
//
//  1. Empty Format field falls back to the testset-level format (yaml
//     here, exercised with a fresh MockYaml that was not configured
//     for gob). The mock lands in mocks.yaml and carries no Format
//     on disk.
//
//  2. A per-mock Format="gob" override routes one mock to mocks.gob
//     even though the testset-level format is yaml — this is the case
//     that unblocks multi-session DS (one session writing gob while
//     another writes yaml into a sibling test-set directory).
//
//  3. Round-trip preserves the field: encode a mock with Format set
//     through the platform/yaml EncodeMock -> DecodeMocks path and
//     assert the field survives the wire-format hop. (Pure function
//     test, no filesystem.)
package mockdb

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.uber.org/zap"
	yamlLib "gopkg.in/yaml.v3"
)

// newHTTPMock builds a minimal valid HTTP mock so the encode path has a
// non-empty spec to serialise. The specific payload is unimportant for
// these tests — the only field under inspection is Format.
func newHTTPMock(format string) *models.Mock {
	return &models.Mock{
		Version: "api.keploy.io/v1beta1",
		Kind:    models.HTTP,
		Format:  format,
		Spec: models.MockSpec{
			Metadata: map[string]string{"src": "per-mock-format-test"},
			HTTPReq: &models.HTTPReq{
				Method: "GET", URL: "http://example/x",
				ProtoMajor: 1, ProtoMinor: 1,
				Header: map[string]string{},
			},
			HTTPResp: &models.HTTPResp{
				StatusCode: 200,
				Header:     map[string]string{},
			},
		},
	}
}

// TestPerMockFormat_RouteSelection covers the InsertMock routing logic:
// the configured-default path vs a per-mock override. The process-wide
// toggle (KEPLOY_MOCK_FORMAT / configuredMockFormat) is left unset so
// "testset-level format" defaults to yaml for the fixture.
func TestPerMockFormat_RouteSelection(t *testing.T) {
	// Guard against pollution from other tests in this package that
	// may have set configuredMockFormat; restore on exit.
	prev := configuredMockFormat
	t.Cleanup(func() { SetConfiguredMockFormat(prev) })
	SetConfiguredMockFormat("")
	// Also clear any env var inherited from the shell so we really
	// start with "testset default = yaml".
	t.Setenv("KEPLOY_MOCK_FORMAT", "")

	type want struct {
		yamlExists bool
		gobExists  bool
	}
	cases := []struct {
		name   string
		format string
		want   want
	}{
		{
			name:   "empty format falls back to testset default (yaml)",
			format: "",
			want:   want{yamlExists: true, gobExists: false},
		},
		{
			name:   "explicit yaml override matches testset default (yaml)",
			format: "yaml",
			want:   want{yamlExists: true, gobExists: false},
		},
		{
			name:   "gob override persists to mocks.gob",
			format: "gob",
			want:   want{yamlExists: false, gobExists: true},
		},
		{
			name:   "unknown value falls through to testset default (yaml)",
			format: "xml",
			want:   want{yamlExists: true, gobExists: false},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			ys := New(zap.NewNop(), dir, "mocks")
			mock := newHTTPMock(tc.format)
			if err := ys.InsertMock(context.Background(), mock, "set-0"); err != nil {
				t.Fatalf("InsertMock: %v", err)
			}
			// Close is a no-op for the yaml path, but it drains the
			// async gob writer when the gob branch fired. Call it
			// unconditionally so the assertion below sees the fully
			// flushed filesystem state.
			if err := ys.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}

			yamlPath := filepath.Join(dir, "set-0", "mocks.yaml")
			gobPath := filepath.Join(dir, "set-0", "mocks.gob")
			yamlInfo, yamlErr := os.Stat(yamlPath)
			gobInfo, gobErr := os.Stat(gobPath)
			haveYaml := yamlErr == nil && yamlInfo.Size() > 0
			haveGob := gobErr == nil && gobInfo.Size() > 0

			if haveYaml != tc.want.yamlExists {
				t.Fatalf("yaml file presence mismatch: got %v want %v (err=%v)",
					haveYaml, tc.want.yamlExists, yamlErr)
			}
			if haveGob != tc.want.gobExists {
				t.Fatalf("gob file presence mismatch: got %v want %v (err=%v)",
					haveGob, tc.want.gobExists, gobErr)
			}
		})
	}
}

// TestPerMockFormat_WireRoundTrip asserts that the Format field round-
// trips through the wire format unchanged. This is the contract that
// makes the read-side of the DS multi-session scenario work: mocks
// written with Format=gob must come back with Format=gob so the
// writer side can preserve the override on any subsequent re-write
// (e.g. UpdateMocks' prune-and-rewrite flow).
func TestPerMockFormat_WireRoundTrip(t *testing.T) {
	cases := []struct {
		name   string
		format string
	}{
		{name: "empty", format: ""},
		{name: "yaml", format: "yaml"},
		{name: "gob", format: "gob"},
	}

	logger := zap.NewNop()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := newHTTPMock(tc.format)
			doc, err := EncodeMock(in, logger)
			if err != nil {
				t.Fatalf("EncodeMock: %v", err)
			}
			if doc.Format != tc.format {
				t.Fatalf("EncodeMock did not carry Format: got %q want %q",
					doc.Format, tc.format)
			}
			// Marshal + unmarshal through yaml.v3 so we also cover
			// the omitempty serialisation behaviour (empty string
			// must round-trip as empty, not as the literal "format: \"\"").
			raw, err := yamlLib.Marshal(doc)
			if err != nil {
				t.Fatalf("yaml.Marshal doc: %v", err)
			}
			var back yaml.NetworkTrafficDoc
			if err := yamlLib.Unmarshal(raw, &back); err != nil {
				t.Fatalf("yaml.Unmarshal doc: %v", err)
			}
			if back.Format != tc.format {
				t.Fatalf("wire round-trip Format mismatch: got %q want %q\nraw:\n%s",
					back.Format, tc.format, string(raw))
			}
			// DecodeMocks rehydrates onto models.Mock.Format.
			mocks, err := DecodeMocks([]*yaml.NetworkTrafficDoc{&back}, logger)
			if err != nil {
				t.Fatalf("DecodeMocks: %v", err)
			}
			if len(mocks) != 1 {
				t.Fatalf("DecodeMocks: want 1 mock got %d", len(mocks))
			}
			if mocks[0].Format != tc.format {
				t.Fatalf("DecodeMocks Format mismatch: got %q want %q",
					mocks[0].Format, tc.format)
			}
		})
	}
}

// TestPerMockFormat_DeepCopyPreserves is a regression guard for the
// async gob writer path: insertMockGob deep-copies the mock before
// enqueueing, and if DeepCopy dropped Format the writer would persist
// mocks with an empty Format even though the caller set one. Two DS
// sessions with different formats sharing a single mockdb instance
// would then silently blend.
func TestPerMockFormat_DeepCopyPreserves(t *testing.T) {
	in := newHTTPMock("gob")
	out := in.DeepCopy()
	if out.Format != "gob" {
		t.Fatalf("DeepCopy dropped Format: got %q", out.Format)
	}
}
