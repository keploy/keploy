package replay

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// What keploy test sends a test with: under the proven switch, the app's
// answer is bounded only by an apiTimeout of the user's own (row W1); without
// one, a late answer is the app's answer. Without the switch, or with the
// user's bound, the bound holds, as before.
func TestKeployTestBoundsTheAppsAnswerOnlyByTheUsersAPITimeout(t *testing.T) {
	for _, tc := range []struct {
		name       string
		proven     bool
		set        bool
		wantAnswer bool
	}{
		{"the switch, no bound of the user's", true, false, true},
		{"the switch, the user's bound", true, true, false},
		{"no switch", false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.proven {
				t.Setenv(provenswitch.Env, "1")
			} else {
				t.Setenv(provenswitch.Env, "")
			}
			entered, release := make(chan struct{}, 1), make(chan struct{})
			app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				entered <- struct{}{}
				<-release
				_, _ = w.Write([]byte(`{}`))
			}))
			t.Cleanup(app.Close)
			cfg := &config.Config{}
			cfg.Test.BasePath = app.URL
			cfg.Test.APITimeout = 1
			cfg.Test.APITimeoutSet = tc.set
			test := bpTestCase("orders", 1)
			rewritten, err := ReplaceBaseURL(app.URL, test.HTTPReq.URL)
			if err != nil {
				t.Fatal(err)
			}
			test.HTTPReq.URL = rewritten
			done := make(chan error, 1)
			go func() {
				_, err := NewHooks(zap.NewNop(), cfg, &bpNoAgent{}).SimulateRequest(context.Background(), test, "test-set-0")
				done <- err
			}()
			<-entered
			if tc.wantAnswer {
				// The app answers past the 1 s apiTimeout (induced load).
				time.Sleep(1500 * time.Millisecond)
				close(release)
				if err := <-done; err != nil {
					t.Fatalf("the test gave up on the app's late answer: %v", err)
				}
				return
			}
			err = <-done
			close(release)
			if err == nil {
				t.Fatal("the test waited past the apiTimeout that bounds it")
			}
		})
	}
}
