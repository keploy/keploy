// Coverage for the per-mock Format override (task #225 / DaemonSet
// Phase 0 per-session mock format) and its mixed-format guard. Tests:
//
//  1. TestPerMockFormat_RouteSelection — table-driven InsertMock
//     routing: empty Format falls back to the testset-level format
//     (yaml for the fixture), explicit "yaml" matches the default,
//     "gob" routes a single mock to mocks.gob, and an unknown value
//     ("xml") falls through to the testset default rather than
//     erroring, which is what unblocks multi-session DS flows.
//
//  2. TestPerMockFormat_WireRoundTrip — encode/decode preservation
//     of the Format field through the EncodeMock -> yaml.Marshal ->
//     yaml.Unmarshal -> DecodeMocks path, including byte-level
//     omitempty checks on the `format:` line. Pure function test,
//     no filesystem.
//
//  3. TestPerMockFormat_DeepCopyPreserves — regression guard that
//     models.Mock.DeepCopy carries the Format field through, so the
//     async gob writer's deep-copy-before-enqueue step cannot drop
//     the override mid-flight.
//
//  4. TestInsertMock_RejectsMixedFormat — both directions (yaml then
//     gob, gob then yaml) of the single-testset-one-format contract.
//     Read/prune paths prefer mocks.gob by file presence and never
//     merge a sibling mocks.yaml, so InsertMock must reject the
//     second-format write; the assertion covers both the error
//     message and that no sibling file was created.
//
//  5. TestInsertMock_RaceFreeMixedFormatGuard — race-window coverage
//     for the in-process guard: a gob InsertMock enqueues to the
//     async writer without waiting for gobReopenLocked to create
//     mocks.gob on disk, and an immediately-following yaml InsertMock
//     in the same goroutine must still be rejected by the in-memory
//     per-testSetID format map rather than racing the async file
//     creation.
package mockdb

import (
	"context"
	"os"
	"path/filepath"
	"strings"
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
			// Byte-level omitempty guard: for an empty Format the
			// wire bytes must not contain a "format:" line at all.
			// A pure unmarshal round-trip would still yield back.Format
			// == "" even if the marshal side emitted `format: ""`, so
			// assert directly on the serialised bytes to catch a
			// regression where the yaml tag loses its omitempty
			// modifier. For non-empty formats the same bytes MUST
			// carry a `format: <value>` line, which keeps the
			// bidirectional contract explicit.
			hasFormatLine := false
			for _, line := range strings.Split(string(raw), "\n") {
				if strings.HasPrefix(strings.TrimLeft(line, " \t"), "format:") {
					hasFormatLine = true
					break
				}
			}
			if tc.format == "" && hasFormatLine {
				t.Fatalf("empty Format emitted a format: line on the wire; omitempty regression:\n%s", string(raw))
			}
			if tc.format != "" && !hasFormatLine {
				t.Fatalf("non-empty Format %q did not emit a format: line on the wire:\n%s", tc.format, string(raw))
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

// TestInsertMock_RejectsMixedFormat guards the read/prune-side
// contract: GetFilteredMocks / GetUnFilteredMocks / UpdateMocks prefer
// mocks.gob over mocks.yaml by file presence and never merge the two.
// InsertMock therefore must refuse to create a second file in a
// different format once one format's file already exists in the
// test-set directory — otherwise the yaml mocks would be silently
// ignored at replay. Both directions are covered: yaml-then-gob and
// gob-then-yaml.
func TestInsertMock_RejectsMixedFormat(t *testing.T) {
	// Guard against pollution from sibling tests.
	prev := configuredMockFormat
	t.Cleanup(func() { SetConfiguredMockFormat(prev) })
	SetConfiguredMockFormat("")
	t.Setenv("KEPLOY_MOCK_FORMAT", "")

	t.Run("yaml first, then gob is rejected", func(t *testing.T) {
		dir := t.TempDir()
		ys := New(zap.NewNop(), dir, "mocks")
		if err := ys.InsertMock(context.Background(), newHTTPMock("yaml"), "set-0"); err != nil {
			t.Fatalf("first InsertMock (yaml): %v", err)
		}
		err := ys.InsertMock(context.Background(), newHTTPMock("gob"), "set-0")
		if err == nil {
			t.Fatalf("expected InsertMock(gob) to fail after yaml file present, got nil")
		}
		if !strings.Contains(err.Error(), "uniform per-testset format required") {
			t.Fatalf("error message missing uniform-format hint: %v", err)
		}
		// The rejected write must not have produced a sibling file.
		if _, statErr := os.Stat(filepath.Join(dir, "set-0", "mocks.gob")); statErr == nil {
			t.Fatalf("mocks.gob was created despite the mixed-format rejection")
		}
		// Clean up any async writer state (no-op for yaml path).
		if err := ys.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})

	t.Run("gob first, then yaml is rejected", func(t *testing.T) {
		dir := t.TempDir()
		ys := New(zap.NewNop(), dir, "mocks")
		if err := ys.InsertMock(context.Background(), newHTTPMock("gob"), "set-0"); err != nil {
			t.Fatalf("first InsertMock (gob): %v", err)
		}
		// Close() drains the async gob writer so the file is
		// present on disk for this variant. The race-free
		// in-memory guard (sync.Map in MockYaml) would catch the
		// rejection even without this Close — see
		// TestInsertMock_RaceFreeMixedFormatGuard — but draining
		// here exercises the on-disk side of the guard too.
		if err := ys.Close(); err != nil {
			t.Fatalf("Close after gob insert: %v", err)
		}
		err := ys.InsertMock(context.Background(), newHTTPMock("yaml"), "set-0")
		if err == nil {
			t.Fatalf("expected InsertMock(yaml) to fail after gob file present, got nil")
		}
		if !strings.Contains(err.Error(), "uniform per-testset format required") {
			t.Fatalf("error message missing uniform-format hint: %v", err)
		}
		if _, statErr := os.Stat(filepath.Join(dir, "set-0", "mocks.yaml")); statErr == nil {
			t.Fatalf("mocks.yaml was created despite the mixed-format rejection")
		}
	})
}

// TestInsertMock_UnknownFormatHonorsLockedTestset covers the
// three-step lock-aware fallback in InsertMock: an unrecognised
// per-mock Format ("gbo" typo, stale value, anything that is not
// "yaml" or "gob") must inherit the testset's already-locked format
// rather than bounce off the mixed-format guard via the process
// default.
//
// Scenario: process default is gob (simulating a run with
// KEPLOY_MOCK_FORMAT=gob), but the first InsertMock for testSetID
// "t1" explicitly asks for yaml — locking t1 to yaml. A follow-up
// InsertMock with Format="gbo" (typo) would, under the old
// resolveMockFormat-only policy, route to the process default (gob)
// and be rejected by the mixed-format guard, dropping the mock. The
// lock-aware policy routes it to the locked format instead and
// appends cleanly into mocks.yaml, preserving the recording.
func TestInsertMock_UnknownFormatHonorsLockedTestset(t *testing.T) {
	// Guard against pollution from sibling tests.
	prev := configuredMockFormat
	t.Cleanup(func() { SetConfiguredMockFormat(prev) })
	SetConfiguredMockFormat("")
	// Process default = gob for the duration of this test. Env var
	// wins over configuredMockFormat in useGobMockFormat so this is
	// the most reliable knob.
	t.Setenv("KEPLOY_MOCK_FORMAT", "gob")

	dir := t.TempDir()
	ys := New(zap.NewNop(), dir, "mocks")
	t.Cleanup(func() {
		if err := ys.Close(); err != nil {
			t.Errorf("deferred Close: %v", err)
		}
	})

	// Mock 1: explicit yaml into t1. Locks t1=yaml in the sync.Map.
	mock1 := newHTTPMock("yaml")
	if err := ys.InsertMock(context.Background(), mock1, "t1"); err != nil {
		t.Fatalf("first InsertMock (yaml): %v", err)
	}

	// Mock 2: typo'd "gbo". Under the old policy this would resolve
	// to useGobMockFormat()=true (because env is gob) and the
	// LoadOrStore-check would reject it as mixed-format. The new
	// policy sees the unknown value, consults the lock (yaml), and
	// routes the mock into the yaml file.
	mock2 := newHTTPMock("gbo")
	if err := ys.InsertMock(context.Background(), mock2, "t1"); err != nil {
		t.Fatalf("second InsertMock (gbo typo) should have inherited the yaml lock, got error: %v", err)
	}

	// mocks.yaml must exist and contain BOTH mock names. mocks.gob
	// must not have been created.
	yamlPath := filepath.Join(dir, "t1", "mocks.yaml")
	gobPath := filepath.Join(dir, "t1", "mocks.gob")
	if _, err := os.Stat(gobPath); err == nil {
		t.Fatalf("mocks.gob was created despite the testset being yaml-locked")
	} else if !os.IsNotExist(err) {
		t.Fatalf("unexpected error statting %s: %v", gobPath, err)
	}
	raw, err := os.ReadFile(yamlPath)
	if err != nil {
		t.Fatalf("read mocks.yaml: %v", err)
	}
	body := string(raw)
	// Both mocks were auto-assigned sequential names via getNextID
	// starting at 0. Assert both show up in the wire bytes so a
	// regression that silently drops mock2 (or routes it elsewhere)
	// would be caught.
	if !strings.Contains(body, "mock-0") {
		t.Fatalf("mocks.yaml missing mock-0:\n%s", body)
	}
	if !strings.Contains(body, "mock-1") {
		t.Fatalf("mocks.yaml missing mock-1 (typo'd Format likely dropped):\n%s", body)
	}
}

// TestInsertMock_RaceFreeMixedFormatGuard exercises the race window
// that the on-disk os.Stat check alone cannot close: the gob writer
// is asynchronous, so InsertMock(gob) enqueues a job and returns
// before gobReopenLocked has created mocks.gob on disk. A
// tightly-following InsertMock(yaml) for the same testSetID in the
// same goroutine must still be rejected — otherwise it would stat
// mocks.gob, get ENOENT, and create mocks.yaml alongside the
// still-queued gob write, at which point the readers (which prefer
// mocks.gob by file presence) would silently drop the yaml mocks at
// replay.
//
// The test deliberately does NOT call ys.Close() between the two
// InsertMock calls. That would drain the gob writer and materialise
// mocks.gob on disk, which would turn this into a cover of the
// TestInsertMock_RejectsMixedFormat case. What we want here is the
// intra-process, pre-flush window — the bug the in-memory sync.Map
// guard in MockYaml was added for.
func TestInsertMock_RaceFreeMixedFormatGuard(t *testing.T) {
	// Guard against pollution from sibling tests.
	prev := configuredMockFormat
	t.Cleanup(func() { SetConfiguredMockFormat(prev) })
	SetConfiguredMockFormat("")
	t.Setenv("KEPLOY_MOCK_FORMAT", "")

	dir := t.TempDir()
	ys := New(zap.NewNop(), dir, "mocks")
	// Drain the writer at the end of the test so the goroutine and
	// any open file descriptors are released even though the test
	// itself deliberately does not invoke Close mid-flight.
	t.Cleanup(func() {
		if err := ys.Close(); err != nil {
			t.Errorf("deferred Close: %v", err)
		}
	})

	// First insert: gob. The async writer may or may not have run
	// the creation side by the time InsertMock returns — the point
	// is that the subsequent yaml insert must be rejected either
	// way, and crucially in the "writer hasn't flushed yet" slice
	// of that either-way.
	if err := ys.InsertMock(context.Background(), newHTTPMock("gob"), "set-0"); err != nil {
		t.Fatalf("first InsertMock (gob): %v", err)
	}

	// Second insert: yaml, same testSetID, same goroutine, no
	// Close() in between. With only the os.Stat guard in place this
	// call could observe ENOENT on mocks.gob and proceed to create
	// mocks.yaml. The in-memory sync.Map guard catches it
	// deterministically.
	err := ys.InsertMock(context.Background(), newHTTPMock("yaml"), "set-0")
	if err == nil {
		t.Fatalf("expected InsertMock(yaml) to fail after same-testset gob insert, got nil")
	}
	if !strings.Contains(err.Error(), "uniform per-testset format required") {
		t.Fatalf("error message missing uniform-format hint: %v", err)
	}

	// Crucially: mocks.yaml must not have been created by the
	// rejected call. Check before Close so a late writer flush
	// cannot mask a buggy write path.
	yamlPath := filepath.Join(dir, "set-0", "mocks.yaml")
	if _, statErr := os.Stat(yamlPath); statErr == nil {
		t.Fatalf("mocks.yaml was created despite the race-free mixed-format rejection")
	} else if !os.IsNotExist(statErr) {
		t.Fatalf("unexpected error statting %s: %v", yamlPath, statErr)
	}
}
