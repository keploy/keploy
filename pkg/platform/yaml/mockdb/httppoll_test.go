package mockdb

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.uber.org/zap"
)

// A HttpPoll mock must encode+decode identically to an HTTP mock (same wire
// payload) so held long-poll mocks round-trip through mocks.yaml.
func TestHttpPollRoundTripsAsHTTP(t *testing.T) {
	m := &models.Mock{
		Version: models.GetVersion(),
		Kind:    models.HttpPoll,
		Name:    "mock-poll-1",
		Spec: models.MockSpec{
			Metadata: map[string]string{models.MetaAsync: "true", models.MetaAsyncPoll: "true"},
			HTTPReq:  &models.HTTPReq{Method: "GET", URL: "http://svc/poll?cursor=1"},
			HTTPResp: &models.HTTPResp{StatusCode: 200, Body: `{"delivery":"data-2"}`},
		},
	}
	doc, err := EncodeMock(m, zap.NewNop())
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if doc.Kind != models.HttpPoll {
		t.Fatalf("encoded kind = %q want HttpPoll", doc.Kind)
	}
	out, err := DecodeMocks([]*yaml.NetworkTrafficDoc{doc}, zap.NewNop())
	if err != nil || len(out) != 1 {
		t.Fatalf("decode: err=%v n=%d", err, len(out))
	}
	if out[0].Kind != models.HttpPoll || out[0].Spec.HTTPReq == nil ||
		out[0].Spec.HTTPReq.URL != "http://svc/poll?cursor=1" {
		t.Fatalf("round-trip lost payload: %+v", out[0])
	}
}
