package postmanimport

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
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

// A collection's request URL can carry its api keys and tokens in its query,
// and url.Parse's and client.Do's errors quote it whole. The lines that say a
// request could not be sent name it without its query, unless
// KEPLOY_LOG_CONTENT is set; the error returned names it whole.
func TestSendRequest_AFailedRequestIsLoggedWithoutItsQuery(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	refusing := "http://" + l.Addr().String()
	_ = l.Close() // the port now refuses
	for name, c := range map[string]struct {
		url, basePath, line string
	}{
		"one the app refuses":                  {refusing + "/orders?api_key=" + canary, "", "failed to send HTTP request"},
		"one whose URL does not parse":         {"http://api.test/orders?api_key=" + canary + "\x7f", "", "failed to create HTTP request"},
		"one whose URL the base path rewrites": {"http://api.test/orders?api_key=" + canary + "\x7f", refusing, "failed to replace hostname"},
	} {
		t.Run(name, func(t *testing.T) {
			logtest.Each(t, func(t *testing.T, on bool) {
				logger, logs := logtest.Observe()
				pi := NewPostmanImporter(context.Background(), logger)
				_, err := pi.sendRequest(models.HTTPReq{Method: "GET", URL: c.url}, c.basePath)
				if err == nil || !strings.Contains(err.Error(), canary) {
					t.Fatalf("sendRequest = %v; want its error, naming the URL whole", err)
				}
				logtest.Check(t, logs, canary, on, c.line)
			})
		})
	}
}
