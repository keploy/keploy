package matcher

import (
	"testing"

	"go.keploy.io/server/v3/utils/logcontent/logtest"
)

// A body that is not JSON is reported without it, unless KEPLOY_LOG_CONTENT
// is set.
func TestUnmarshallJSON_ABodyThatIsNotJSONIsLoggedWithoutIt(t *testing.T) {
	const canary = "kp-canary-matcher-json-6c2d"
	logtest.Each(t, func(t *testing.T, on bool) {
		logger, logs := logtest.Observe()
		if _, err := UnmarshallJSON(`{"note":"`+canary, logger); err == nil {
			t.Fatal("a truncated body was read as JSON")
		}
		logtest.Check(t, logs, canary, on, "cannot convert json string into json object")
	})
}
