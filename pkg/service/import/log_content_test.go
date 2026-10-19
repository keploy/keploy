package postmanimport

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils/logcontent/logtest"
)

const canary = "kp-canary-postman-2a7c"

// The answer to a request an import sends is not logged, unless
// KEPLOY_LOG_CONTENT is set.
func TestSendRequest_TheAnswersBodyIsNotLogged(t *testing.T) {
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"note":"` + canary + `"}`))
	}))
	t.Cleanup(app.Close)
	logtest.Each(t, func(t *testing.T, on bool) {
		logger, logs := logtest.Observe()
		pi := NewPostmanImporter(context.Background(), logger)
		if _, err := pi.sendRequest(models.HTTPReq{Method: "GET", URL: app.URL + "/notes"}, ""); err != nil {
			t.Fatal(err)
		}
		logtest.Check(t, logs, canary, on, "Response Body")
	})
}

// A collection's variable whose value is not a string is reported by its
// key, not its value, unless KEPLOY_LOG_CONTENT is set.
func TestExtractGlobalVariables_AValueThatIsNotAStringIsReportedByItsKey(t *testing.T) {
	logtest.Each(t, func(t *testing.T, on bool) {
		logger, logs := logtest.Observe()
		pi := NewPostmanImporter(context.Background(), logger)
		pi.extractGlobalVariables([]map[string]interface{}{{"key": "token", "value": []string{canary}}})
		logtest.Check(t, logs, canary, on, "Global variable value is not a string")
	})
}
