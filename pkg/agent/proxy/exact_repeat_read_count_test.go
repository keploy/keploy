package proxy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	httpint "go.keploy.io/server/v3/pkg/agent/proxy/integrations/http"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// A call past the one a repeated read was answered once more with misses as
// one call more than its test recorded (models.MissExtraCall), and the miss
// the proxy files says that the read was answered once more
// (UnmatchedCall.AnsweredAgain): a call of the test's that neither its
// recordings nor its misses count, which its extra-call line counts. A read
// the replay names whose recordings answer two ways is answered no call
// more, and its misses say none.
func TestAMissPastARepeatedReadsOnceMoreAnswerSaysIt(t *testing.T) {
	for _, tc := range []struct {
		name            string
		answers         [2]string
		sends, answered int
		again           int
	}{
		{name: "one call past it", answers: [2]string{onboarding, onboarding}, sends: 4, answered: 3, again: 1},
		{name: "two calls past it", answers: [2]string{onboarding, onboarding}, sends: 5, answered: 3, again: 1},
		{name: "named, but answered two ways", answers: [2]string{onboarding, formScript}, sends: 3, answered: 2, again: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(provenswitch.Env, "1")
			p := startedDrainProxy(t, 100)
			at := time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)
			read := &models.RepeatReadMark{Name: "status", At: 1}
			var mapping []*models.Mock
			for i, answer := range tc.answers {
				mk := readMock(fmt.Sprintf("mock-%d", i), at.Add(time.Duration(i)*time.Second), "GET", "/status", "", answer)
				mk.TestModeInfo.RepeatRead = read
				mapping = append(mapping, mk)
			}
			mm := NewMockManager(nil, nil, zap.NewNop())
			t.Cleanup(mm.Close)
			mm.SetMocksWithWindow(nil, mapping, models.BaseTime, time.Now().Add(time.Hour))
			p.setMockManager(mm)
			t.Cleanup(func() { p.setMockManager(nil) })
			mm.ServeExactPool("k", nil)
			mm.ServeExactTest("test-a", corsOwned(mapping...))
			p.ContinueTestErrorCapture()
			h := httpint.New(zap.NewNop())
			for i := 0; i < tc.sends; i++ {
				code, body, err := sendWithin(h, p, mm, readCall("GET", "/status", ""))
				if errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("call %d: %v", i+1, err)
				}
				if answered := err == nil && code == http.StatusOK; answered != (i < tc.answered) {
					t.Fatalf("call %d got %d %s %v; want it answered: %v", i+1, code, body, err, i < tc.answered)
				}
			}
			misses, err := p.GetMockErrors(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(misses) != tc.sends-tc.answered {
				t.Fatalf("misses %+v; want %d", misses, tc.sends-tc.answered)
			}
			for _, m := range misses {
				if m.Kind != models.MissExtraCall || m.Recorded != 2 || m.AnsweredAgain != tc.again {
					t.Fatalf("the miss is %s, recorded %d, answered once more %d; want %s, recorded 2, answered once more %d", m.Kind, m.Recorded, m.AnsweredAgain, models.MissExtraCall, tc.again)
				}
			}
		})
	}
}
