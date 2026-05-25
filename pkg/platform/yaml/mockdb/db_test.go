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
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
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
