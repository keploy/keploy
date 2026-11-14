package replay

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"go.keploy.io/server/v3/internal/logleak"
	"go.keploy.io/server/v3/pkg"
)

// The console's line for a test with no answer names a stalled request by
// its shape, keeps a cancelled test quiet as before (utils.LogError reads
// the error's chain), and withholds what another error quotes.
func TestSayNoAnswerLogsTheRequestsShapeAndKeepsACancelQuiet(t *testing.T) {
	const s = logleak.Canary
	for _, tc := range []struct {
		name string
		err  error
		want string // a line's message, or "" for none
	}{
		{"a cancelled test", fmt.Errorf("failed to send: %w", context.Canceled), ""},
		{"a stalled request", &pkg.AnswerStallError{Target: "GET /v1/reset/" + s + "?token=" + s}, "not judged: the app never answered; waited 0s of keploy's runnable time for GET /{}/{}/{}"},
		{"a client's error", fmt.Errorf("Post %q: %w", "http://app/v1/reset/"+s, errors.New("EOF")), "failed to simulate request"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logger, logs := logleak.Observe()
			sayNoAnswer(logger, "test-1", tc.err, "failed to simulate request")
			if tc.want == "" {
				if logs.Len() != 0 {
					t.Fatalf("logged %d lines for a cancelled test, want none", logs.Len())
				}
				return
			}
			if logs.FilterMessage(tc.want).Len() != 1 {
				t.Fatalf("did not log %q (%d lines logged)", tc.want, logs.Len())
			}
			logleak.Check(t, logs, s, "the no-answer line")
		})
	}
}
