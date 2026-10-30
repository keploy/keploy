package mock

import (
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/logleak"
	"go.keploy.io/server/v3/pkg/models"
)

// A miss keploy mock warns of is logged by the call's shape
// (models.LoggedCall) and its next step with the advised sessionCalls path
// prefix in that shape (SanitizedNextStep): no stretch of the path's
// account or the query's token appears in any encoding. The receipt keeps
// the call. The line logged the call's summary and its next step whole.
func TestLogMiss_LogsTheCallAndItsNextStepByTheirShape(t *testing.T) {
	const token, account = "sk_live_Q7wZr9Kf", "acct_9Lq2Zx7WbN4"
	logger, out := logleak.Logger(nil)
	logMiss(logger, models.UnmatchedCall{
		Protocol:      "HTTP",
		ActualSummary: "GET /tenants/" + account + "/config?token=" + token,
		Destination:   "api.example.com",
		NextSteps:     `declare it in keploy.yaml (sessionCalls: [{host: "api.example.com", method: "GET", pathPrefix: "/tenants/` + account + `/config"}])`,
	})
	got := out.String()
	if !strings.Contains(got, "GET /tenants/{}/config?token={}") || !strings.Contains(got, `/tenants/{}/config`) || !strings.Contains(got, "written in the call's shape") {
		t.Errorf("the line does not name the call and its advice by their shapes: %s", got)
	}
	for what, v := range map[string]string{"the token": token, "the account": account} {
		if found := logleak.Find(t, got, []byte(v)); len(found) > 0 {
			t.Errorf("the log holds %s: %v", what, found)
		}
	}
}
