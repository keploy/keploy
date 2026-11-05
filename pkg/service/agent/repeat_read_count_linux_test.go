//go:build linux

package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
)

// A test's extra-call line counts every call the test made of the call: those
// its recordings answered, the one call more its repeated read was answered
// with (exact_repeat_read.go), and the calls past that, which missed. A test
// that recorded a read twice and made it four times is said to have called
// it four times, not three. A read the replay names whose recordings answer
// two ways is no repeated read: it is answered no call more, and its line is
// the plain one.
func TestAnExtraCallLineCountsTheCallARepeatedReadWasAnsweredOnceMore(t *testing.T) {
	for _, tc := range []struct {
		name     string
		answers  [2]string
		calls    int
		answered []string
		want     string
	}{
		{
			name: "one call past it", answers: [2]string{"same", "same"}, calls: 4,
			answered: []string{"200 same", "200 same", "200 same"},
			want:     "GET /n to 127.0.0.1:18093 was called 4 times: 2 recorded, 1 answered once more as a repeated read, 1 extra",
		},
		{
			name: "two calls past it", answers: [2]string{"same", "same"}, calls: 5,
			answered: []string{"200 same", "200 same", "200 same"},
			want:     "GET /n to 127.0.0.1:18093 was called 5 times: 2 recorded, 1 answered once more as a repeated read, 2 extra",
		},
		{
			name: "named, but answered two ways", answers: [2]string{"one", "two"}, calls: 3,
			answered: []string{"200 one", "200 two"},
			want:     "GET /n to 127.0.0.1:18093 was called 3 times, recorded 2.",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, p := exactAgent(t,
				[]*models.Mock{pingMock("mock-1", "/n", tc.answers[0], t0), pingMock("mock-2", "/n", tc.answers[1], t0.Add(time.Second))},
				map[string][]string{"o::TestList": {"mock-1", "mock-2"}}, map[string][]string{})
			require.NoError(t, a.SetScopeRepeatReads(context.Background(), map[string][]models.RepeatRead{"o::TestList": {{Name: "list", Mocks: []string{"mock-1", "mock-2"}}}}))
			paths := make([]string, tc.calls)
			for i := range paths {
				paths[i] = "/n"
			}
			got, e := runTest(t, a, p, "TestList", paths...)
			require.Equal(t, tc.answered, got[:len(tc.answered)])
			for i, g := range got[len(tc.answered):] {
				require.True(t, strings.HasPrefix(g, "502"), "call %d has no recorded answer: %s", len(tc.answered)+i+1, g)
			}
			require.Equal(t, models.ScopeActionFail, e.Action)
			require.Contains(t, e.Reason, tc.want)
		})
	}
}
