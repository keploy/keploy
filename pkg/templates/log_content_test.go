package templates

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils/logcontent/logtest"
)

// canary is a template value, and a secret's: the application's.
const canary = "kp-canary-templates-47d1"

// A placeholder whose value is not a number of its type is logged by the
// placeholder, not its value, unless KEPLOY_LOG_CONTENT is set: a secret's in
// a header, a template value as a JSON body's whole value. Each value is said
// once, with the switch on or off.
func TestRequest_AValueThatIsNoNumberIsLoggedWithoutIt(t *testing.T) {
	const msg = "a placeholder's value is not a number of its type; it is left as written"
	for name, req := range map[string]models.HTTPReq{
		"a secret in a header": {Header: map[string]string{"X-Pin": "{{int .secret.pin}}"}},
		"a JSON body's value": {Header: map[string]string{"Content-Type": "application/json"},
			Body: `{"pin":{{int .pin}}}`},
	} {
		t.Run(name, func(t *testing.T) {
			logtest.Each(t, func(t *testing.T, on bool) {
				logger, logs := logtest.Observe()
				r := NewRun(logger, map[string]interface{}{"pin": canary}, map[string]interface{}{"pin": canary})
				r.Request(req)
				r.Request(req)
				logtest.Check(t, logs, canary, on, msg)
				if n := logs.FilterMessage(msg).Len(); n != 1 {
					t.Fatalf("the value was said %d times, want once", n)
				}
				// A credential's value (a pin) is said by its length alone:
				// a fingerprint of a short one gives it away.
				if v, ok := logs.FilterMessage(msg).All()[0].ContextMap()["value"].(map[string]interface{}); !on && (!ok || v["fp"] != nil || v["len"] == nil) {
					t.Fatalf("a pin's value is logged as %v, want its length alone", logs.FilterMessage(msg).All()[0].ContextMap()["value"])
				}
				other := NewRun(logger, map[string]interface{}{"pin": canary + "-2"}, map[string]interface{}{"pin": canary + "-2"})
				other.warned = r.warned
				other.Request(req)
				if n := logs.FilterMessage(msg).Len(); n != 2 {
					t.Fatalf("another value was said with the first %d times, want twice in all", n)
				}
			})
		})
	}
}
