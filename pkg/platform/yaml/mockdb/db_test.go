// Tests for MockYaml.InsertMock's shutdown-flush contract.
//
// Background: the pre-fix code did `if ctx.Err() == nil { writer.Flush() }`
// AFTER encoder.Encode. yaml.v3's encoder streams into the bufio.Writer
// as it goes — full pages auto-flush to the file, but the tail of the
// last mock typically sits in the bufio buffer until the explicit
// Flush() at the bottom of InsertMock. If ctx got cancelled between
// encoder.Encode finishing and the gated Flush, the tail was silently
// dropped: file.Close() does NOT drain a wrapping bufio.Writer, so the
// mocks.yaml on disk ended truncated mid-mock. That truncation tripped
// wire-encode validation at replay time and was the root cause of the
// recorder-shutdown-flush bug.
//
// The fix has two pieces:
//   1. An early-exit gate at the top of InsertMock (before opening the
//      file): `if ctx.Err() != nil { return ctx.Err() }`. Cancelled
//      ctx leaves nothing on disk — clean state.
//   2. A deferred bufio.Writer Flush() so EVERY return path (including
//      a ctx cancel mid-encode) drains the buffer before file.Close.
//      The trailing explicit Flush() at the end of the function still
//      surfaces flush errors as the return value; the defer is the
//      belt-and-braces drain for the partial-write race.
//
// These tests assert the disk file is NEVER left in a truncated state.
// A corrupt half-written mock (i.e. bytes present but missing the
// trailing newline / required terminators) is the only outcome the fix
// rules out.

package mockdb

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	yaml "go.keploy.io/server/v3/pkg/platform/yaml"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	yamlLib "gopkg.in/yaml.v3"
)

// bigHTTPMock builds a *models.Mock whose YAML encoding comfortably
// exceeds bufio.Writer's 4 KiB default buffer (we pad to 8 KiB+) so
// the encoder is guaranteed to leave tail bytes in the bufio buffer
// at return time. Without the deferred Flush, those tail bytes would
// stay in the buffer if the explicit Flush ever got skipped.
func bigHTTPMock(payloadSize int) *models.Mock {
	body := strings.Repeat("a", payloadSize)
	return &models.Mock{
		Version: "api.keploy.io/v1beta1",
		Kind:    models.HTTP,
		Spec: models.MockSpec{
			Metadata: map[string]string{"src": "flushtest"},
			HTTPReq: &models.HTTPReq{
				Method: "POST", URL: "http://x/y", ProtoMajor: 1, ProtoMinor: 1,
				Header: map[string]string{"Content-Type": "application/json"},
				Body:   `{"a":1}`,
			},
			HTTPResp: &models.HTTPResp{
				StatusCode: 200, StatusMessage: "OK",
				Header: map[string]string{"X-Big": "yes"},
				Body:   body,
			},
		},
	}
}

// TestInsertMock_FlushOnCtxCancel asserts the bufio Flush contract.
//
// Three sub-tests:
//
//   - happy_path_flushes_full_mock: a normal InsertMock with a payload
//     >> 4 KiB. The file MUST contain the tail of the body. Pre-fix
//     this passed too (the explicit trailing Flush ran), so this is a
//     baseline: it pins the success-path contract so subsequent tests
//     can distinguish "happy path broken" from "flush-on-cancel broken".
//
//   - precancelled_ctx_writes_nothing: the early-exit gate at the top
//     of InsertMock returns ctx.Err() before touching the file. The
//     disk MUST stay untouched.
//
//   - cancel_after_first_insert_preserves_first: a real-shutdown
//     scenario — first InsertMock with a fresh ctx succeeds, then we
//     cancel the parent ctx and attempt a second InsertMock. The
//     second call should bail at the early gate (returning ctx.Err)
//     and the first call's bytes MUST already be on disk intact
//     (proves the first call's deferred Flush drained its buffer
//     and did not leave the second InsertMock's cancellation to
//     corrupt anything).
func TestInsertMock_FlushOnCtxCancel(t *testing.T) {
	const payloadSize = 8 * 1024 // 8 KiB, > bufio default 4 KiB

	t.Run("happy_path_flushes_full_mock", func(t *testing.T) {
		dir := t.TempDir()
		ys := New(zap.NewNop(), dir, "mocks")
		mock := bigHTTPMock(payloadSize)

		if err := ys.InsertMock(context.Background(), mock, "set-0"); err != nil {
			t.Fatalf("InsertMock: %v", err)
		}
		ys.Close()

		// The yaml file is written at <mockPath>/<testSetID>/<mockName>.yaml.
		yamlPath := filepath.Join(dir, "set-0", "mocks.yaml")
		got, err := os.ReadFile(yamlPath)
		if err != nil {
			t.Fatalf("read %s: %v", yamlPath, err)
		}
		// The file must contain the END of the response body — the
		// tail bytes that lived in the bufio buffer until Flush().
		// If the deferred Flush regressed and the explicit Flush got
		// skipped (e.g. via a refactor that re-introduced the ctx
		// gate), this assertion fails.
		tailMarker := strings.Repeat("a", 64) // last 64 of the 8 KiB body
		if !strings.Contains(string(got), tailMarker) {
			t.Fatalf("yaml file does not contain payload tail (regression: bufio buffer not flushed); file size = %d", len(got))
		}
		// Sanity: file must be at least the size of the body (plus
		// yaml envelope). 4 KiB threshold guards against the file
		// containing just the version comment.
		if len(got) < payloadSize {
			t.Fatalf("yaml file too small: got %d bytes, expected >= %d (mock body)", len(got), payloadSize)
		}
	})

	t.Run("precancelled_ctx_writes_nothing", func(t *testing.T) {
		dir := t.TempDir()
		ys := New(zap.NewNop(), dir, "mocks")
		mock := bigHTTPMock(payloadSize)

		ctx, cancel := context.WithCancel(context.Background())
		cancel() // cancel BEFORE InsertMock

		err := ys.InsertMock(ctx, mock, "set-0")
		if err == nil {
			t.Fatalf("InsertMock with pre-cancelled ctx returned nil error; expected ctx.Err propagation")
		}

		// File should NOT exist OR be empty. The early-exit gate
		// bails before yaml.CreateYamlFile runs, so the test-set
		// directory itself shouldn't have been created.
		yamlPath := filepath.Join(dir, "set-0", "mocks.yaml")
		info, statErr := os.Stat(yamlPath)
		if statErr == nil {
			// If the file did get created somehow (a future code path
			// that creates the dir before the gate would land here),
			// it must not contain mock data — only the file header.
			b, rerr := os.ReadFile(yamlPath)
			if rerr != nil {
				t.Fatalf("file exists but unreadable: %v", rerr)
			}
			if len(b) > 256 {
				t.Fatalf("pre-cancelled InsertMock left a non-trivial file on disk (size=%d). Early-exit gate bypassed.", info.Size())
			}
		}
		// statErr != nil (file missing) is the expected clean shape.
	})

	t.Run("cancel_after_first_insert_preserves_first", func(t *testing.T) {
		dir := t.TempDir()
		ys := New(zap.NewNop(), dir, "mocks")
		first := bigHTTPMock(payloadSize)

		// First call — fresh ctx, succeeds, full mock on disk.
		if err := ys.InsertMock(context.Background(), first, "set-0"); err != nil {
			t.Fatalf("first InsertMock: %v", err)
		}
		ys.Close()

		yamlPath := filepath.Join(dir, "set-0", "mocks.yaml")
		firstBytes, err := os.ReadFile(yamlPath)
		if err != nil {
			t.Fatalf("read after first insert: %v", err)
		}
		if !strings.Contains(string(firstBytes), strings.Repeat("a", 64)) {
			t.Fatalf("first mock body tail not on disk after first InsertMock; file size = %d", len(firstBytes))
		}

		// Now cancel the parent ctx and attempt a second InsertMock.
		// The early-exit gate must fire; the first call's bytes must
		// remain intact (no corruption from the second cancelled
		// call's filesystem operations).
		cancelledCtx, cancel := context.WithCancel(context.Background())
		cancel()
		second := bigHTTPMock(payloadSize)
		if err := ys.InsertMock(cancelledCtx, second, "set-0"); err == nil {
			t.Fatalf("second (cancelled) InsertMock returned nil error; expected ctx.Err")
		}

		// Re-read the file. It must still contain the first mock's
		// tail; the cancelled second call must not have appended
		// partial bytes (and must not have truncated the file).
		afterBytes, err := os.ReadFile(yamlPath)
		if err != nil {
			t.Fatalf("read after second cancelled insert: %v", err)
		}
		if len(afterBytes) < len(firstBytes) {
			t.Fatalf("file shrunk after cancelled second InsertMock: before=%d, after=%d (the cancel must not truncate)", len(firstBytes), len(afterBytes))
		}
		if !strings.Contains(string(afterBytes), strings.Repeat("a", 64)) {
			t.Fatalf("first mock's body tail disappeared after cancelled second InsertMock (file size = %d)", len(afterBytes))
		}
	})
}

// TestInsertMock_ClassifiesPayloadFaultsVsEnvironment pins the contract the
// recorder now depends on to survive a bad mock.
//
// A failed InsertMock used to tear the whole recording session down. That is
// right for disk-full or storage-gone — every subsequent mock fails too — and
// badly wrong for one mock whose own payload cannot be encoded, which is how a
// single gzip response body ended a 46-hour production recording. The recorder
// now skips ErrMockEncode and stays fatal on everything else, so the split has
// to be exactly right HERE or the fix either does nothing or swallows real
// storage failures.
func TestInsertMock_ClassifiesPayloadFaultsVsEnvironment(t *testing.T) {
	t.Run("payload fault is tagged skippable", func(t *testing.T) {
		dir := t.TempDir()
		ys := NewWithFormat(zap.NewNop(), dir, "mocks", yaml.FormatJSON)
		// A kind the JSON encoder has no arm for: the mock itself is the
		// problem, and no later mock is affected by it.
		mock := &models.Mock{
			Version: models.GetVersion(),
			Name:    "bad-1",
			Kind:    models.Kind("ThisKindDoesNotExist"),
			Spec:    models.MockSpec{},
		}
		err := ys.InsertMock(context.Background(), mock, "set-0")
		if err == nil {
			t.Fatal("expected an encode failure for an unsupported kind")
		}
		if !errors.Is(err, models.ErrMockEncode) {
			t.Fatalf("payload-fault error is NOT tagged ErrMockEncode, so the recorder will treat "+
				"one bad mock as fatal and destroy the session: %v", err)
		}
	})

	t.Run("environmental failure stays fatal", func(t *testing.T) {
		dir := t.TempDir()
		// Make the test-set directory unwritable: an I/O failure, not a payload
		// problem. Every later mock would fail too, so this MUST stay fatal.
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Skipf("cannot chmod temp dir: %v", err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
		if os.Geteuid() == 0 {
			t.Skip("running as root; a read-only dir does not produce an I/O error")
		}
		ys := New(zap.NewNop(), dir, "mocks")
		mock := &models.Mock{
			Version: models.GetVersion(),
			Name:    "ok-1",
			Kind:    models.HTTP,
			Spec: models.MockSpec{
				Metadata: map[string]string{"type": "config"},
				HTTPReq:  &models.HTTPReq{Method: "GET", URL: "http://example.com/", Timestamp: time.Unix(1, 0)},
				HTTPResp: &models.HTTPResp{StatusCode: 200, Timestamp: time.Unix(2, 0)},
			},
		}
		err := ys.InsertMock(context.Background(), mock, "set-0")
		if err == nil {
			t.Fatal("writing into a read-only directory unexpectedly succeeded; this test must " +
				"actually exercise an I/O failure or it proves nothing")
		}
		// Name the failure so the test cannot quietly start passing on some other
		// error: this is the mkdir of the test-set directory.
		if !errors.Is(err, os.ErrPermission) {
			t.Fatalf("expected a permission error from the read-only dir, got: %v", err)
		}
		if errors.Is(err, models.ErrMockEncode) {
			t.Fatalf("an I/O failure was tagged ErrMockEncode, so the recorder would keep recording "+
				"through a broken disk and silently lose every mock: %v", err)
		}
	})
}

// httpMock returns a minimal, valid HTTP mock — the shape every one of these
// tests uses as its "good" mock.
func httpMock(name string) *models.Mock {
	return &models.Mock{
		Version: models.GetVersion(),
		Name:    name,
		Kind:    models.HTTP,
		Spec: models.MockSpec{
			Metadata: map[string]string{"type": "config"},
			HTTPReq:  &models.HTTPReq{Method: "GET", URL: "http://example.com/", Timestamp: time.Unix(1, 0)},
			HTTPResp: &models.HTTPResp{StatusCode: 200, Timestamp: time.Unix(2, 0)},
		},
	}
}

// TestInsertMock_YAMLPayloadFaultIsSkippable covers the classification on the
// DEFAULT storage format. The 46-hour production recording that died was
// writing YAML, not JSON, so a sibling test that only exercises the JSON arm
// leaves the path that actually broke unpinned.
func TestInsertMock_YAMLPayloadFaultIsSkippable(t *testing.T) {
	clearRegistry(t)
	dir := t.TempDir()
	ys := New(zap.NewNop(), dir, "mocks")
	t.Cleanup(func() { _ = ys.Close() })

	// No mapper registered for this kind, so EncodeMock falls to its default
	// arm: keploy's own encoders cannot represent this mock, and no later mock
	// is affected by that.
	err := ys.InsertMock(context.Background(), &models.Mock{
		Version: models.GetVersion(),
		Name:    "bad-1",
		Kind:    models.Kind("ThisKindDoesNotExist"),
		Spec:    models.MockSpec{},
	}, "set-0")
	if err == nil {
		t.Fatal("expected an encode failure for an unsupported kind on the yaml path")
	}
	if !errors.Is(err, models.ErrMockEncode) {
		t.Fatalf("a yaml payload fault is NOT tagged ErrMockEncode, so the recorder treats one "+
			"bad mock as fatal and tears down the session — the exact 46-hour failure: %v", err)
	}
}

// TestInsertMock_MapperFailureStaysFatal is the other half of the
// classification, and the one with the worse failure mode if it regresses.
//
// A registered MockYAMLMapper is out-of-tree code that may touch the filesystem
// or the network, so its failure can be environmental (ENOSPC, EACCES) rather
// than a property of this one mock. Tagging it skippable would drop EVERY mock
// of that kind and finish green with an empty test set — silent total data loss
// instead of a loud stop.
func TestInsertMock_MapperFailureStaysFatal(t *testing.T) {
	const kind = models.Kind("EnterpriseKind")

	t.Run("yaml", func(t *testing.T) {
		clearRegistry(t)
		RegisterMockYAMLMapper(kind, MockYAMLMapper{
			Encode: func(*models.Mock, *yaml.NetworkTrafficDoc) error {
				return errors.New("mapper offload to assets dir failed: no space left on device")
			},
			Decode: func(*yaml.NetworkTrafficDoc, *models.Mock) error { return nil },
		})
		dir := t.TempDir()
		ys := New(zap.NewNop(), dir, "mocks")
		t.Cleanup(func() { _ = ys.Close() })

		err := ys.InsertMock(context.Background(), &models.Mock{
			Version: models.GetVersion(), Name: "m-1", Kind: kind,
		}, "set-0")
		if err == nil {
			t.Fatal("a failing mapper must surface an error")
		}
		if errors.Is(err, models.ErrMockEncode) {
			t.Fatalf("a mapper failure was tagged skippable; on a full disk the recorder would "+
				"drop every mock of this kind and finish with an empty test set: %v", err)
		}
	})

	// The JSON encoder does not consult the mapper registry at all — a
	// mapper-owned kind reaches it as handled=false, indistinguishable from
	// "keploy cannot encode this" unless InsertMock checks the registry.
	t.Run("json_unhandled_kind_with_mapper", func(t *testing.T) {
		clearRegistry(t)
		RegisterMockYAMLMapper(kind, MockYAMLMapper{
			Encode: func(*models.Mock, *yaml.NetworkTrafficDoc) error { return nil },
			Decode: func(*yaml.NetworkTrafficDoc, *models.Mock) error { return nil },
		})
		dir := t.TempDir()
		ys := NewWithFormat(zap.NewNop(), dir, "mocks", yaml.FormatJSON)
		t.Cleanup(func() { _ = ys.Close() })

		err := ys.InsertMock(context.Background(), &models.Mock{
			Version: models.GetVersion(), Name: "m-1", Kind: kind,
		}, "set-0")
		if err == nil {
			t.Fatal("the json encoder cannot encode a mapper-owned kind; that must be an error")
		}
		if errors.Is(err, models.ErrMockEncode) {
			t.Fatalf("a mapper-owned kind was tagged skippable on the json path, so recording a "+
				"Redis/Kafka/Pulsar app with storageFormat json would silently drop every one of "+
				"those mocks and DELETE every test that touched them: %v", err)
		}
	})
}

// TestInsertMock_SkippedMockLeavesNoStrayDocument guards the file the recorder
// leaves behind once a mock can be skipped. The document separator used to be
// written before the encode was attempted, and the deferred flush commits it —
// so every dropped mock injected an empty YAML document into mocks.yaml.
func TestInsertMock_SkippedMockLeavesNoStrayDocument(t *testing.T) {
	clearRegistry(t)
	dir := t.TempDir()
	ys := New(zap.NewNop(), dir, "mocks")

	ctx := context.Background()
	if err := ys.InsertMock(ctx, httpMock("good-1"), "set-0"); err != nil {
		t.Fatalf("first mock: %v", err)
	}
	if err := ys.InsertMock(ctx, &models.Mock{
		Version: models.GetVersion(), Name: "bad-1", Kind: models.Kind("ThisKindDoesNotExist"),
	}, "set-0"); !errors.Is(err, models.ErrMockEncode) {
		t.Fatalf("want a skippable encode error for the middle mock, got %v", err)
	}
	if err := ys.InsertMock(ctx, httpMock("good-2"), "set-0"); err != nil {
		t.Fatalf("third mock: %v", err)
	}
	_ = ys.Close()

	got, err := os.ReadFile(filepath.Join(dir, "set-0", "mocks.yaml"))
	if err != nil {
		t.Fatalf("read mocks.yaml: %v", err)
	}
	// Two mocks were written, so exactly one separator belongs in the file.
	if n := strings.Count(string(got), "---\n"); n != 1 {
		t.Errorf("mocks.yaml has %d document separators for 2 written mocks; the dropped mock "+
			"left a stray empty document behind:\n%s", n, got)
	}
	// And the file must still parse as exactly two documents.
	dec := yamlLib.NewDecoder(strings.NewReader(string(got)))
	docs := 0
	for {
		var doc yamlLib.Node
		if err := dec.Decode(&doc); err != nil {
			break
		}
		docs++
	}
	if docs != 2 {
		t.Errorf("mocks.yaml decodes to %d documents, want 2 (good-1, good-2)", docs)
	}
}

// TestInsertMock_VersionHeaderSurvivesASkippedFirstMock covers the one-shot
// nature of the version header. CreateFileF reports isFileEmpty=true only for
// the call that CREATED the file, so if the first mock of a test set is
// unencodable and the header write sits after the encode, the file is created
// empty, the header is skipped, and every later InsertMock sees a file that
// already exists — the whole test set loses its provenance comment permanently.
func TestInsertMock_VersionHeaderSurvivesASkippedFirstMock(t *testing.T) {
	clearRegistry(t)
	dir := t.TempDir()
	ys := New(zap.NewNop(), dir, "mocks")

	ctx := context.Background()
	// The FIRST mock of the set is the unencodable one.
	if err := ys.InsertMock(ctx, &models.Mock{
		Version: models.GetVersion(), Name: "bad-1", Kind: models.Kind("ThisKindDoesNotExist"),
	}, "set-0"); !errors.Is(err, models.ErrMockEncode) {
		t.Fatalf("want a skippable encode error for the first mock, got %v", err)
	}
	if err := ys.InsertMock(ctx, httpMock("good-1"), "set-0"); err != nil {
		t.Fatalf("second mock: %v", err)
	}
	_ = ys.Close()

	got, err := os.ReadFile(filepath.Join(dir, "set-0", "mocks.yaml"))
	if err != nil {
		t.Fatalf("read mocks.yaml: %v", err)
	}
	want := utils.GetVersionAsComment()
	if want == "" {
		t.Skip("no version comment configured in this build")
	}
	if !strings.HasPrefix(string(got), want) {
		t.Errorf("mocks.yaml lost its version header because the first mock was skipped.\nwant prefix %q\ngot %q",
			want, string(got[:min(len(got), 80)]))
	}
	// And it must still be a single valid document.
	dec := yamlLib.NewDecoder(strings.NewReader(string(got)))
	docs := 0
	for {
		var doc yamlLib.Node
		if err := dec.Decode(&doc); err != nil {
			break
		}
		docs++
	}
	if docs != 1 {
		t.Errorf("mocks.yaml decodes to %d documents, want 1 (good-1)", docs)
	}
}

// TestInsertMock_JSONBinaryBodyIsRefusedNotCorrupted covers the storageFormat
// the headline fix does NOT cover.
//
// encoding/json has no lossless representation for a Go string holding invalid
// UTF-8: it substitutes U+FFFD for every bad byte and returns no error. So a
// gzip response body — the exact payload that killed the 46-hour recording —
// used to be written to mocks.json silently corrupted, the recording reported
// success, and the damage surfaced only at replay as an unexplained mismatch.
// Silent corruption is worse than the crash it replaced; refuse the one mock
// loudly instead, tagged skippable so the session still survives.
func TestInsertMock_JSONBinaryBodyIsRefusedNotCorrupted(t *testing.T) {
	clearRegistry(t)
	gzipBody := string([]byte{0x1f, 0x8b, 0x08, 0x00, 0xff, 0xfe})

	dir := t.TempDir()
	ys := NewWithFormat(zap.NewNop(), dir, "mocks", yaml.FormatJSON)
	t.Cleanup(func() { _ = ys.Close() })

	mock := httpMock("bin-1")
	mock.Spec.HTTPResp.Body = gzipBody

	err := ys.InsertMock(context.Background(), mock, "set-0")
	if err == nil {
		got, rerr := os.ReadFile(filepath.Join(dir, "set-0", "mocks.json"))
		// encoding/json escapes the replacement rune as the SIX-character ASCII
		// sequence `\ufffd`, never as the raw rune — matching on "�" here
		// would silently never fire and this diagnostic would be dead.
		if rerr == nil && strings.Contains(string(got), `\ufffd`) {
			t.Fatal("a binary response body was written to mocks.json with every invalid byte " +
				"replaced by U+FFFD, and InsertMock reported SUCCESS — silent corruption that only " +
				"surfaces at replay as an unexplained mismatch")
		}
		t.Fatal("a binary body must not be silently accepted on the json path")
	}
	if !errors.Is(err, models.ErrMockEncode) {
		t.Fatalf("the refusal must be tagged skippable, or one binary body kills the whole session "+
			"exactly like the original defect: %v", err)
	}
	// A UTF-8 body on the same path must still work — this must not become a
	// blanket refusal of the json format.
	if err := ys.InsertMock(context.Background(), httpMock("ok-1"), "set-0"); err != nil {
		t.Fatalf("a normal UTF-8 body was refused on the json path: %v", err)
	}
}

// TestGetFilteredMocks_HeaderOnlyFileIsZeroMocksNotAnError closes the loop
// between the two halves of this change. Now that a recording SURVIVES
// unencodable mocks, a test set whose every mock was dropped ends up with a
// mocks.yaml holding only the version comment — a state that previously could
// not occur, because such a recording died instead.
//
// Reading that file used to hard-fail with "failed to get mocks, empty file",
// which makes every test in the set unrunnable and explains nothing. Report
// zero mocks instead so the run proceeds and the per-test results point at the
// real problem. A truncated or malformed file is unaffected: the decode returns
// its own error well before this.
func TestGetFilteredMocks_HeaderOnlyFileIsZeroMocksNotAnError(t *testing.T) {
	clearRegistry(t)
	dir := t.TempDir()
	ys := New(zap.NewNop(), dir, "mocks")

	// Every mock in the set is unencodable, so nothing but the header is written.
	for _, n := range []string{"bad-1", "bad-2"} {
		if err := ys.InsertMock(context.Background(), &models.Mock{
			Version: models.GetVersion(), Name: n, Kind: models.Kind("ThisKindDoesNotExist"),
		}, "set-0"); !errors.Is(err, models.ErrMockEncode) {
			t.Fatalf("%s: want a skippable encode error, got %v", n, err)
		}
	}
	_ = ys.Close()

	got, err := os.ReadFile(filepath.Join(dir, "set-0", "mocks.yaml"))
	if err != nil {
		t.Fatalf("read mocks.yaml: %v", err)
	}
	if strings.Contains(string(got), "kind:") {
		t.Fatalf("expected a document-less file, got:\n%s", got)
	}

	mocks, err := ys.GetFilteredMocks(context.Background(), "set-0", time.Time{}, time.Time{}, nil, nil)
	if err != nil {
		t.Fatalf("a document-less mock file failed the whole test set instead of reporting zero "+
			"mocks; every test in the set becomes unrunnable with no explanation: %v", err)
	}
	if len(mocks) != 0 {
		t.Errorf("want zero mocks from a document-less file, got %d", len(mocks))
	}
}

// TestEncodeMockJSON_HTTP2BinaryBodyIsRefused is the sibling of the HTTP case.
// EncodeMockJSON projects HTTP2Req.Body and HTTP2Resp.Body as plain strings
// too, so a binary body on an Http2 mock hit the identical silent U+FFFD
// corruption — one `case` away from the guard, and easy to miss because no OSS
// path records an Http2 mock today (enterprise egress recorders do).
func TestEncodeMockJSON_HTTP2BinaryBodyIsRefused(t *testing.T) {
	gzipBody := string([]byte{0x1f, 0x8b, 0x08, 0x00, 0xff, 0xfe})

	for _, tc := range []struct {
		name string
		spec models.MockSpec
	}{
		{"request", models.MockSpec{
			HTTP2Req:  &models.HTTP2Req{Body: gzipBody},
			HTTP2Resp: &models.HTTP2Resp{},
		}},
		{"response", models.MockSpec{
			HTTP2Req:  &models.HTTP2Req{},
			HTTP2Resp: &models.HTTP2Resp{Body: gzipBody},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc, _, err := EncodeMockJSON(&models.Mock{
				Version: models.GetVersion(), Name: "h2-1", Kind: models.HTTP2, Spec: tc.spec,
			}, zap.NewNop())
			if err == nil {
				t.Fatalf("an http2 binary %s body was accepted; json.Marshal writes it with every "+
					"invalid byte replaced by U+FFFD and reports success:\n%s", tc.name, doc.Spec)
			}
			if !errors.Is(err, models.ErrMockEncode) {
				t.Fatalf("the refusal must be tagged skippable so the session survives: %v", err)
			}
		})
	}

	// A UTF-8 http2 body must still encode.
	if _, _, err := EncodeMockJSON(&models.Mock{
		Version: models.GetVersion(), Name: "h2-ok", Kind: models.HTTP2,
		Spec: models.MockSpec{HTTP2Req: &models.HTTP2Req{Body: "{}"}, HTTP2Resp: &models.HTTP2Resp{Body: "{}"}},
	}, zap.NewNop()); err != nil {
		t.Fatalf("a normal UTF-8 http2 body was refused: %v", err)
	}
}

// handedMocks is a mock of each way InsertMock writes a YAML document: the
// production-shape lane's mocks and the other kinds it writes in one pass, a
// MySQL mock holding a string it double-quotes (yaml.EncodeQuoted), and a
// Mongo mock, which goes through EncodeMock's yaml.Node.
func handedMocks(t *testing.T) []*models.Mock {
	t.Helper()
	mocks := inPlaceCorpus(t)
	mocks = append(mocks, trapMocks(t, "\tleading tab\nx", "hand")["mysql-query"])
	mocks = append(mocks, &models.Mock{
		Version: models.GetVersion(),
		Kind:    models.Mongo,
		Spec: models.MockSpec{
			Metadata: map[string]string{"operation": "OP_MSG"},
			MongoRequests: []models.MongoRequest{{
				Header:  &models.MongoHeader{Length: 50, RequestID: 1, Opcode: 2013},
				Message: &models.MongoOpMessage{Sections: []string{`{ SectionSingle msg: {"find":"c"} }`}},
			}},
			MongoResponses: []models.MongoResponse{{
				Header:  &models.MongoHeader{Length: 60, RequestID: 2, ResponseTo: 1, Opcode: 2013},
				Message: &models.MongoOpMessage{Sections: []string{`{ SectionSingle msg: {"ok":1} }`}},
			}},
			ReqTimestampMock: time.Date(2026, 9, 30, 8, 42, 54, 0, time.UTC),
			ResTimestampMock: time.Date(2026, 9, 30, 8, 42, 55, 0, time.UTC),
		},
	})
	return mocks
}

// docCollector collects the documents InsertMock hands back on its context.
type docCollector struct {
	mocks   []*models.Mock
	docs    [][]byte
	formats []string
}

func (c *docCollector) ctx() context.Context {
	return models.WithMockDocReceiver(context.Background(), func(m *models.Mock, doc []byte, format string) {
		c.mocks = append(c.mocks, m)
		c.docs = append(c.docs, bytes.Clone(doc))
		c.formats = append(c.formats, format)
	})
}

// insertHanded inserts m and checks what InsertMock handed back for it: one
// document, in the file's format, that is exactly what the insert appended to
// the file after framing (the version comment heading a new YAML file, the
// "---" line before a later YAML document; nothing for JSON). It returns the
// document.
func insertHanded(t *testing.T, ys *MockYaml, c *docCollector, m *models.Mock, format yaml.Format, framing string) []byte {
	t.Helper()
	path := filepath.Join(ys.MockPath, "test-set-0", "mocks."+format.FileExtension())
	before, _ := os.ReadFile(path)
	handed := len(c.docs)
	cp := *m
	if err := ys.InsertMock(c.ctx(), &cp, "test-set-0"); err != nil {
		t.Fatalf("InsertMock(%s %s): %v", m.Kind, m.Name, err)
	}
	if got := len(c.docs) - handed; got != 1 {
		t.Fatalf("InsertMock(%s %s) handed back %d documents, want the 1 it wrote", m.Kind, m.Name, got)
	}
	doc := c.docs[len(c.docs)-1]
	if c.mocks[len(c.mocks)-1] != &cp {
		t.Fatalf("InsertMock(%s) handed its document back for another mock", m.Kind)
	}
	if c.formats[len(c.formats)-1] != string(format) {
		t.Fatalf("InsertMock(%s) handed its document back as %q, wrote %q", m.Kind, c.formats[len(c.formats)-1], format)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(after, before) {
		t.Fatalf("InsertMock(%s) rewrote the file instead of appending", m.Kind)
	}
	if want := append([]byte(framing), doc...); !bytes.Equal(after[len(before):], want) {
		t.Fatalf("InsertMock(%s %s) appended:\n%s\nit handed back, after %q:\n%s", m.Kind, m.Name, after[len(before):], framing, doc)
	}
	return doc
}

// InsertMock hands the receiver on its context (models.WithMockDocReceiver)
// the document it wrote for each mock, byte for byte, however it wrote it: in
// one pass, double-quoted, or through EncodeMock. Laid end to end, with the
// file's version comment and the "---" between YAML documents, the documents
// handed back ARE the file. This is what the recorder gives the AfterMockInsert
// hooks, so a hook that keeps a copy of the file need not encode the mock again.
func TestInsertMock_HandsTheDocumentItWrote(t *testing.T) {
	for _, format := range []yaml.Format{yaml.FormatYAML, yaml.FormatJSON} {
		t.Run(string(format), func(t *testing.T) {
			dir := t.TempDir()
			ys := NewWithFormat(zap.NewNop(), dir, "mocks", format)
			c := &docCollector{}
			mocks := handedMocks(t)
			for i, m := range mocks {
				framing := ""
				if format == yaml.FormatYAML {
					framing = "---\n"
					if i == 0 {
						framing = utils.GetVersionAsComment()
					}
				}
				doc := insertHanded(t, ys, c, m, format, framing)
				if format == yaml.FormatYAML {
					dec := yamlLib.NewDecoder(bytes.NewReader(doc))
					var n yamlLib.Node
					if bytes.HasPrefix(doc, []byte("---")) || dec.Decode(&n) != nil || !errors.Is(dec.Decode(&n), io.EOF) {
						t.Fatalf("document %d handed back is not one YAML document:\n%s", i, doc)
					}
				} else if !json.Valid(doc) || bytes.Count(doc, []byte("\n")) != 1 || doc[len(doc)-1] != '\n' {
					t.Fatalf("document %d handed back is not one JSON line:\n%s", i, doc)
				}
			}
			file, err := os.ReadFile(filepath.Join(dir, "test-set-0", "mocks."+format.FileExtension()))
			if err != nil {
				t.Fatal(err)
			}
			want := bytes.Join(c.docs, nil)
			if format == yaml.FormatYAML {
				want = append([]byte(utils.GetVersionAsComment()), bytes.Join(c.docs, []byte("---\n"))...)
			}
			if !bytes.Equal(want, file) {
				t.Fatalf("the documents handed back are not what is in the file (%d mocks)\nhanded:\n%s\nfile:\n%s", len(mocks), want, file)
			}
		})
	}
}

// The document handed back is the mock's alone, whatever framing the file
// gives it: after a first mock that was dropped once the file's version
// comment was written, and in a file an earlier recording left. And it is
// handed back as the format written: YAML, for a MockYaml whose Format was
// left empty.
func TestInsertMock_HandsTheDocumentWhateverFramesIt(t *testing.T) {
	clearRegistry(t)
	mocks := handedMocks(t)
	dir := t.TempDir()
	ys := New(zap.NewNop(), dir, "mocks")
	c := &docCollector{}
	if err := ys.InsertMock(c.ctx(), &models.Mock{
		Version: models.GetVersion(), Name: "bad-1", Kind: models.Kind("ThisKindDoesNotExist"),
	}, "test-set-0"); !errors.Is(err, models.ErrMockEncode) {
		t.Fatalf("want a skippable encode error for the first mock, got %v", err)
	}
	if len(c.docs) != 0 {
		t.Fatalf("a dropped mock handed back %d documents", len(c.docs))
	}
	// The file holds the version comment only, so the first document written
	// follows a "---".
	insertHanded(t, ys, c, mocks[0], yaml.FormatYAML, "---\n")
	insertHanded(t, ys, c, mocks[1], yaml.FormatYAML, "---\n")

	again := New(zap.NewNop(), dir, "mocks")
	insertHanded(t, again, c, mocks[2], yaml.FormatYAML, "---\n")

	unset := NewWithFormat(zap.NewNop(), t.TempDir(), "mocks", "")
	insertHanded(t, unset, c, mocks[3], yaml.FormatYAML, utils.GetVersionAsComment())
}

// A mock InsertMock does not write hands nothing back: not one it cannot
// encode, not one a cancelled context stops, and not a gob mock, which is
// written in the background after InsertMock returns.
func TestInsertMock_HandsNothingForAMockItDidNotWrite(t *testing.T) {
	var good *models.Mock
	for _, m := range inPlaceCorpus(t) {
		if m.Kind == models.MySQL {
			good = m
			break
		}
	}
	handed := 0
	ctx := models.WithMockDocReceiver(context.Background(), func(*models.Mock, []byte, string) { handed++ })

	ys := New(zap.NewNop(), t.TempDir(), "mocks")
	bad := *good
	bad.Spec.MySQLResponses = []mysql.Response{{PacketBundle: mysql.PacketBundle{Message: failingYAML{}}}}
	if err := ys.InsertMock(ctx, &bad, "test-set-0"); !errors.Is(err, models.ErrMockEncode) {
		t.Fatalf("InsertMock of an unencodable mock = %v, want ErrMockEncode", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	c := *good
	if err := ys.InsertMock(cancelled, &c, "test-set-0"); !errors.Is(err, context.Canceled) {
		t.Fatalf("InsertMock on a cancelled context = %v, want context.Canceled", err)
	}
	if handed != 0 {
		t.Fatalf("InsertMock handed back %d documents for mocks it did not write", handed)
	}
	// The same mock, written, is handed back.
	w := *good
	if err := ys.InsertMock(ctx, &w, "test-set-0"); err != nil || handed != 1 {
		t.Fatalf("InsertMock of a mock it wrote = %v, handed back %d documents, want 1", err, handed)
	}

	t.Setenv("KEPLOY_MOCK_FORMAT", "gob")
	gob := New(zap.NewNop(), t.TempDir(), "mocks")
	g := *good
	if err := gob.InsertMock(ctx, &g, "test-set-0"); err != nil {
		t.Fatal(err)
	}
	if err := gob.Close(); err != nil {
		t.Fatal(err)
	}
	if handed != 1 {
		t.Fatalf("InsertMock handed back %d documents for a gob mock", handed-1)
	}
}

// InsertMock hands the document back with the mocks file unlocked: the file's
// lock is a stripe other files share, so a receiver run under it would hold up
// their writers, and one that reads the file back would deadlock.
func TestInsertMock_HandsTheDocumentWithTheFileUnlocked(t *testing.T) {
	mocks := handedMocks(t)
	dir := t.TempDir()
	ys := New(zap.NewNop(), dir, "mocks")
	var readBack int
	ctx := models.WithMockDocReceiver(context.Background(), func(*models.Mock, []byte, string) {
		got, err := ys.GetUnFilteredMocks(context.Background(), "test-set-0", time.Time{}, time.Time{}, nil, nil)
		if err != nil {
			t.Errorf("reading the file back from the receiver: %v", err)
		}
		readBack = len(got)
	})
	done := make(chan error, 1)
	go func() {
		c := *mocks[0]
		done <- ys.InsertMock(ctx, &c, "test-set-0")
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("InsertMock ran its receiver with the mocks file locked: reading the file back deadlocked")
	}
	if readBack != 1 {
		t.Fatalf("the receiver read back %d mocks, want the 1 just written", readBack)
	}
}
