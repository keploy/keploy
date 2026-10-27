package http

import (
	"net/http"
	"net/url"
	"testing"

	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// A learn-step miss's report names the recording it refused as the closest
// and says what differs from it, with the learn step's reason.
func TestALearnMissReportNamesTheRefusedRecordingAndWhatDiffers(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	refused := onHost("charge-rec", "/charge/book?amount=5", answeredHost)
	db := answeredDb{mockMemDb: &mockMemDb{mocks: []*models.Mock{refused}}}
	lmiss := &learnMiss{res: learn.Result{Outcome: learn.Miss, Reason: models.MissRequestChanged, Why: "a value at url.query.amount flowed"}, closest: refused, pool: []*models.Mock{refused}}
	request := &http.Request{Method: "GET", URL: &url.URL{Path: "/charge/book", RawQuery: "amount=6"}, Host: answeredHost, Header: http.Header{"Accept": {"application/json"}}}
	r := newHTTP().buildHTTPMismatchReport(request, nil, db, nil, nil, nil, lmiss.diag())
	lmiss.explain(r)
	if r.ClosestMock != "charge-rec" || r.Diff == "" || r.Reason != models.MissRequestChanged {
		t.Fatalf("closest %q, diff %q, reason %q", r.ClosestMock, r.Diff, r.Reason)
	}
}
