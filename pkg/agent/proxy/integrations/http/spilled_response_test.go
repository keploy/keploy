// Package http_test is the EXTERNAL test package for the HTTP integration. It
// exists so a test can import pkg/agent/proxy (for the real DiskMocks store)
// without the import cycle an internal `package http` test would create.
package http_test

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy"
	kphttp "go.keploy.io/server/v3/pkg/agent/proxy/integrations/http"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// spillMinBytes mirrors responseSpillMinBytes (pkg/agent/proxy/diskmocks.go).
// It is unexported there, so the value is restated here — and every case below
// asserts proxy.EligibleForResponseSpill agrees, so a retune of the real
// constant fails this test loudly instead of silently testing nothing.
const spillMinBytes = 8 * 1024

// bodyOfSize returns a deterministic, non-uniform body of exactly n bytes. A
// repeated single character would let a truncation or an off-by-one slip past
// a hash comparison far too easily.
func bodyOfSize(n int) string {
	var b strings.Builder
	b.Grow(n + 16)
	for i := 0; b.Len() < n; i++ {
		b.WriteString(strconv.Itoa(i))
		b.WriteByte('-')
	}
	return b.String()[:n]
}

func sha(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

const hdrSep = "\r\n\r\n"

// splitWire returns the status line, the SORTED header lines, and the body.
// Sorting matters: buildMockResponseBytes emits headers by ranging over a Go
// map, so their order is randomised per call. That is pre-existing behaviour
// and out of scope here — but comparing two serialisations byte-for-byte
// without normalising it would make this test flaky rather than meaningful.
func splitWire(t *testing.T, wire string) (status string, headers []string, body string) {
	t.Helper()
	idx := strings.Index(wire, hdrSep)
	if idx < 0 {
		t.Fatalf("no header/body separator in serialized response")
	}
	head, body := wire[:idx], wire[idx+len(hdrSep):]
	lines := strings.Split(head, "\r\n")
	sorted := append([]string(nil), lines[1:]...)
	sort.Strings(sorted)
	return lines[0], sorted, body
}

// wireDigest is an order-insensitive fingerprint of a serialized response.
func wireDigest(t *testing.T, wire string) string {
	t.Helper()
	status, headers, body := splitWire(t, wire)
	return sha(status + "\n" + strings.Join(headers, "\n") + "\n" + sha(body))
}

// perTestHTTPMock builds a mock shaped exactly the way DiskMocks requires to
// consider it for the disk tier: HTTP kind, per-test lifetime, and a valid
// req<=res timestamp pair (see EligibleForDisk).
func perTestHTTPMock(name, body string) *models.Mock {
	req := time.Unix(1700000000, 0).UTC()
	m := &models.Mock{
		Name: name,
		Kind: models.HTTP,
		Spec: models.MockSpec{
			ReqTimestampMock: req,
			ResTimestampMock: req.Add(time.Millisecond),
			HTTPReq:          &models.HTTPReq{ProtoMajor: 1, ProtoMinor: 1, Method: "GET", URL: "http://svc.internal/big"},
			HTTPResp: &models.HTTPResp{
				StatusCode: 200,
				Body:       body,
				// A deliberately stale Content-Length: the serializer must
				// recompute it from the HYDRATED body, not from the elided one.
				Header: map[string]string{"Content-Type": "application/json", "Content-Length": "999"},
			},
		},
	}
	m.TestModeInfo.Lifetime = models.LifetimePerTest
	return m
}

// TestBuildMockResponseBytesServesSpilledResponse is the B27 regression.
//
// A recorded HTTP response of at least responseSpillMinBytes is written to the
// agent's per-test disk store as a SEPARATE blob, and the mock is re-encoded
// with Spec.HTTPResp set to nil plus a lazy responseHydrator (DiskMocks.Add /
// readAt). buildMockResponseBytes used to check `Spec.HTTPResp == nil` BEFORE
// calling HydrateResponse, so every spilled mock was rejected with
// "has no response to serialize" and the client got a hard mock-miss instead
// of its recorded response.
//
// The three sizes bracket the threshold so the boundary itself is pinned.
func TestBuildMockResponseBytesServesSpilledResponse(t *testing.T) {
	cases := []struct {
		name      string
		size      int
		wantSpill bool
	}{
		{"just_under_threshold", spillMinBytes - 1, false},
		{"exactly_at_threshold", spillMinBytes, true},
		{"just_over_threshold", spillMinBytes + 1, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := bodyOfSize(tc.size)
			if len(body) != tc.size {
				t.Fatalf("body builder produced %d bytes, want %d", len(body), tc.size)
			}
			m := perTestHTTPMock("mock-big", body)

			if got := proxy.EligibleForResponseSpill(m); got != tc.wantSpill {
				t.Fatalf("EligibleForResponseSpill(body=%d B) = %v, want %v -- the "+
					"responseSpillMinBytes threshold moved; retune spillMinBytes here",
					tc.size, got, tc.wantSpill)
			}

			// Expected wire bytes: the SAME mock serialized while fully
			// resident, i.e. what replay produced before HTTP was routed into
			// the disk tier. Spilling must be byte-transparent against this.
			h := kphttp.NewForTest()
			want, err := h.BuildMockResponseBytesForTest(perTestHTTPMock("mock-big", body))
			if err != nil {
				t.Fatalf("resident baseline failed to serialize: %v", err)
			}

			store, err := proxy.NewDiskMocks(zap.NewNop())
			if err != nil {
				t.Fatalf("NewDiskMocks: %v", err)
			}
			defer func() { _ = store.Close() }()

			if err := store.Add(m); err != nil {
				t.Fatalf("DiskMocks.Add: %v", err)
			}
			loaded, err := store.LoadByNames([]string{"mock-big"})
			if err != nil {
				t.Fatalf("LoadByNames: %v", err)
			}
			if len(loaded) != 1 {
				t.Fatalf("LoadByNames returned %d mocks, want 1", len(loaded))
			}
			got := loaded[0]

			// Confirm the load really did (or did not) elide the response, so
			// a passing test can never mean "the spill path was never taken".
			if got.HasSpilledResponse() != tc.wantSpill {
				t.Fatalf("loaded mock HasSpilledResponse() = %v, want %v", got.HasSpilledResponse(), tc.wantSpill)
			}
			if tc.wantSpill && got.Spec.HTTPResp != nil {
				t.Fatalf("a spilled mock must arrive with Spec.HTTPResp elided, got non-nil")
			}
			if !tc.wantSpill && got.Spec.HTTPResp == nil {
				t.Fatalf("an unspilled mock must arrive with its response inline, got nil")
			}

			// THE REGRESSION: on the broken ordering this returns
			// `http: mock "mock-big" has no response to serialize`.
			out, err := h.BuildMockResponseBytesForTest(got)
			if err != nil {
				t.Fatalf("serialize spilled mock: %v", err)
			}

			if got, wantD := wireDigest(t, string(out)), wireDigest(t, string(want)); got != wantD {
				t.Fatalf("served response differs from the resident baseline:\n"+
					" got %d B digest=%s\nwant %d B digest=%s",
					len(out), got, len(want), wantD)
			}

			// Byte-identity of the body itself, and the recomputed
			// Content-Length, spelled out so a failure names the cause.
			_, gotHeaders, gotBody := splitWire(t, string(out))
			if sha(gotBody) != sha(body) {
				t.Fatalf("body not byte-identical: got %d B sha=%s, want %d B sha=%s",
					len(gotBody), sha(gotBody), len(body), sha(body))
			}
			wantCL := fmt.Sprintf("Content-Length: %d", tc.size)
			if !slices.Contains(gotHeaders, wantCL) {
				t.Fatalf("Content-Length not recomputed from the hydrated body: want %q in %v", wantCL, gotHeaders)
			}
		})
	}
}

// TestBuildMockResponseBytesSpilledIsIdempotent covers the fact that the fix
// makes HydrateResponse run on EVERY serialize, including repeat serves of one
// mock. serveOnePassThroughMock deliberately does not consume its mock, so the
// second call must produce the same bytes rather than an empty or errored body.
func TestBuildMockResponseBytesSpilledIsIdempotent(t *testing.T) {
	body := bodyOfSize(spillMinBytes + 1)
	store, err := proxy.NewDiskMocks(zap.NewNop())
	if err != nil {
		t.Fatalf("NewDiskMocks: %v", err)
	}
	defer func() { _ = store.Close() }()
	if err := store.Add(perTestHTTPMock("mock-big", body)); err != nil {
		t.Fatalf("DiskMocks.Add: %v", err)
	}
	loaded, err := store.LoadByNames([]string{"mock-big"})
	if err != nil || len(loaded) != 1 {
		t.Fatalf("LoadByNames: %v (n=%d)", err, len(loaded))
	}

	h := kphttp.NewForTest()
	first, err := h.BuildMockResponseBytesForTest(loaded[0])
	if err != nil {
		t.Fatalf("first serialize: %v", err)
	}
	if loaded[0].HasSpilledResponse() {
		t.Fatalf("hydration should have cleared the hydrator after the first serve")
	}
	second, err := h.BuildMockResponseBytesForTest(loaded[0])
	if err != nil {
		t.Fatalf("second serialize: %v", err)
	}
	if a, b := wireDigest(t, string(first)), wireDigest(t, string(second)); a != b {
		t.Fatalf("repeat serve of the same mock changed the response: %s vs %s", a, b)
	}
	if _, _, body2 := splitWire(t, string(second)); sha(body2) != sha(body) {
		t.Fatalf("repeat serve did not return the recorded body")
	}
}

// TestBuildMockResponseBytesSpilledAfterStoreClosed pins the teardown ordering.
// finalizeClientMocks closes a superseded generation's DiskMocks file, so an
// in-flight serve can reach a hydrator whose store is gone. That must surface
// as an error (the caller then reports a mock-miss), never as a panic and never
// as a silently empty body written to the client.
func TestBuildMockResponseBytesSpilledAfterStoreClosed(t *testing.T) {
	body := bodyOfSize(spillMinBytes + 1)
	store, err := proxy.NewDiskMocks(zap.NewNop())
	if err != nil {
		t.Fatalf("NewDiskMocks: %v", err)
	}
	if err := store.Add(perTestHTTPMock("mock-big", body)); err != nil {
		t.Fatalf("DiskMocks.Add: %v", err)
	}
	loaded, err := store.LoadByNames([]string{"mock-big"})
	if err != nil || len(loaded) != 1 {
		t.Fatalf("LoadByNames: %v (n=%d)", err, len(loaded))
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	out, err := kphttp.NewForTest().BuildMockResponseBytesForTest(loaded[0])
	if err == nil {
		t.Fatalf("serializing a spilled mock after store close must fail, got %d bytes", len(out))
	}
	if out != nil {
		t.Fatalf("failed serialize must return nil bytes, got %d", len(out))
	}
	if !strings.Contains(err.Error(), "store closed") {
		t.Fatalf("want a store-closed error, got: %v", err)
	}
}

// TestBuildMockResponseBytesStillRejectsGenuinelyMissingResponse guards the
// other half of the reorder: HydrateResponse is a NO-OP when the mock was never
// spilled, so a mock whose response is absent for any other reason must still
// be rejected rather than sliding into a nil dereference.
func TestBuildMockResponseBytesStillRejectsGenuinelyMissingResponse(t *testing.T) {
	h := kphttp.NewForTest()

	t.Run("nil_response_no_hydrator", func(t *testing.T) {
		m := perTestHTTPMock("mock-empty", "x")
		m.Spec.HTTPResp = nil
		if m.HasSpilledResponse() {
			t.Fatalf("precondition: this mock must not carry a hydrator")
		}
		out, err := h.BuildMockResponseBytesForTest(m)
		if err == nil {
			t.Fatalf("want an error for a mock with no response, got %d bytes", len(out))
		}
		if !strings.Contains(err.Error(), "no response to serialize") {
			t.Fatalf("want the no-response error, got: %v", err)
		}
	})

	t.Run("nil_mock", func(t *testing.T) {
		out, err := h.BuildMockResponseBytesForTest(nil)
		if err == nil {
			t.Fatalf("want an error for a nil mock, got %d bytes", len(out))
		}
	})
}
