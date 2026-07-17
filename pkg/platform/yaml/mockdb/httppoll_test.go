package mockdb

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.uber.org/zap"
)

// An async mock keeps kind Http (poll-ness is not a distinct kind) and carries
// its bookkeeping in a top-level async block that must round-trip through
// mocks.yaml byte-equivalently — including a held long-poll's poll/pollDurationMs.
func TestAsyncBlockRoundTrips(t *testing.T) {
	want := &models.AsyncMeta{
		Lane:           "config-watch",
		Seq:            1,
		AnchorAfter:    "get-rules-1",
		AnchorPos:      3,
		Poll:           true,
		PollDurationMs: 300099,
	}
	m := &models.Mock{
		Version: models.GetVersion(),
		Kind:    models.HTTP, // poll mocks stay Http
		Name:    "mock-poll-1",
		Spec: models.MockSpec{
			Metadata: map[string]string{"name": "Http", "operation": "GET"},
			Async:    want,
			HTTPReq:  &models.HTTPReq{Method: "GET", URL: "http://svc/poll?watch=true"},
			HTTPResp: &models.HTTPResp{StatusCode: 200, Body: `{"delivery":"data-2"}`},
		},
	}
	doc, err := EncodeMock(m, zap.NewNop())
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if doc.Kind != models.HTTP {
		t.Fatalf("encoded kind = %q want Http", doc.Kind)
	}
	if doc.Async == nil || *doc.Async != *want {
		t.Fatalf("doc.Async = %+v want %+v", doc.Async, want)
	}
	out, err := DecodeMocks([]*yaml.NetworkTrafficDoc{doc}, zap.NewNop())
	if err != nil || len(out) != 1 {
		t.Fatalf("decode: err=%v n=%d", err, len(out))
	}
	got := out[0]
	if got.Kind != models.HTTP {
		t.Fatalf("decoded kind = %q want Http", got.Kind)
	}
	if got.Spec.Async == nil || *got.Spec.Async != *want {
		t.Fatalf("round-trip async block: got %+v want %+v", got.Spec.Async, want)
	}
	if got.Spec.HTTPReq == nil || got.Spec.HTTPReq.URL != "http://svc/poll?watch=true" {
		t.Fatalf("round-trip lost http payload: %+v", got)
	}
	// The async keys must NOT leak into the parser metadata.
	if _, bad := got.Spec.Metadata["async"]; bad {
		t.Fatalf("async data leaked into Spec.Metadata: %v", got.Spec.Metadata)
	}
}
