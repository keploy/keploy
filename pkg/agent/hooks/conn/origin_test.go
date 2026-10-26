package conn

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// A captured test case's origin is the clock the ingress stamped its window
// start on, carried on the capture context: under the proven switch only, and
// never for a start the ingress took on no named clock.
func TestStampOriginGivesATestCaseItsWindowsClock(t *testing.T) {
	ctx := WithWindowClock(context.Background(), "proxy:run-1")
	if got := WindowClockFromContext(ctx); got != "proxy:run-1" {
		t.Fatalf("the context carries %q", got)
	}
	if WithWindowClock(context.Background(), "") != context.Background() || WindowClockFromContext(nil) != "" { //nolint:staticcheck // a nil context carries nothing
		t.Fatal("an empty clock is carried, or a nil context carries one")
	}

	t.Setenv(provenswitch.Env, "")
	tc := &models.TestCase{Name: "post-url-1"}
	StampOrigin(ctx, tc)
	if tc.Origin != nil {
		t.Fatalf("without the switch the test case got origin %+v", tc.Origin)
	}
	t.Setenv(provenswitch.Env, "1")
	StampOrigin(ctx, tc)
	if tc.Origin == nil || *tc.Origin != (models.Origin{Clock: "proxy:run-1"}) {
		t.Fatalf("the test case's origin is %+v; want the window's clock", tc.Origin)
	}
	untrusted := &models.TestCase{Name: "fallback"}
	StampOrigin(context.Background(), untrusted)
	if untrusted.Origin != nil {
		t.Fatalf("a start on no named clock got origin %+v", untrusted.Origin)
	}
	StampOrigin(ctx, nil) // no test case: nothing
}

// The real capture of an HTTP exchange gives its test case the clock its
// ingress carried (WithWindowClock) as its origin, under the proven switch
// only: what a recorded test case's window clock comes from, so a capture
// that stamped none would leave every window on no clock and route nothing.
func TestCaptureGivesTheTestCaseItsWindowsClock(t *testing.T) {
	for _, on := range []bool{true, false} {
		t.Run(map[bool]string{true: "switch on", false: "switch off"}[on], func(t *testing.T) {
			t.Setenv(provenswitch.Env, map[bool]string{true: "1", false: ""}[on])
			ch := make(chan *models.TestCase, 1)
			req := httptest.NewRequest(http.MethodGet, "http://app.local/url", nil)
			resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("ok"))}
			at := time.Unix(1000, 0)
			ctx := WithWindowClock(context.Background(), "proxy:run-1")
			Capture(ctx, zap.NewNop(), ch, req, resp, at, at.Add(time.Millisecond), models.IncomingOptions{}, false, false, 8080)
			var tc *models.TestCase
			select {
			case tc = <-ch:
			default:
				t.Fatal("the exchange was not captured")
			}
			if !on {
				if tc.Origin != nil {
					t.Fatalf("without the switch the test case got origin %+v", tc.Origin)
				}
				return
			}
			if tc.Origin == nil || tc.Origin.Clock != "proxy:run-1" {
				t.Fatalf("the test case's origin is %+v; want the window's clock proxy:run-1", tc.Origin)
			}
		})
	}
}
