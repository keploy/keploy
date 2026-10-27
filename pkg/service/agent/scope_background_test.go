package agent

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
)

// unusedBackgroundProxy is a wireProxy that also names the latest test's
// background recordings it did not call (coreAgent.UnusedBackgroundReader).
type unusedBackgroundProxy struct {
	*wireProxy
	test   string
	unused []string
}

func (p unusedBackgroundProxy) ExactTestUnusedBackground() (string, []string, bool) {
	return p.test, p.unused, true
}

// A test's background recordings it did not call are said on its evidence,
// apart from its unmade calls; another test's are not this one's.
func TestUnusedBackgroundRecordingsAreSaidApartFromUnmadeCalls(t *testing.T) {
	for name, tc := range map[string]struct {
		test string
		want []string
	}{
		"this test's":    {test: "orders::TestPlaceOrder", want: []string{"mock-2"}},
		"another test's": {test: "orders::TestOther", want: nil},
	} {
		t.Run(name, func(t *testing.T) {
			a, p := wireAgent(t, models.MODE_TEST)
			audit := httpMock("mock-1", "POST", "http://127.0.0.1:18282/audit")
			tick := httpMock("mock-2", "GET", "http://127.0.0.1:18282/tick")
			storeMocks(a, audit, tick)
			a.scopeTable = map[string][]string{"orders::TestPlaceOrder": {"mock-1", "mock-2"}}
			require.NoError(t, a.serveTestExactly(context.Background(), "orders::TestPlaceOrder"))
			a.Proxy = unusedBackgroundProxy{wireProxy: p, test: tc.test, unused: []string{"mock-2"}}

			ev, err := a.testEvidence(context.Background(), "orders::TestPlaceOrder", true)
			require.NoError(t, err)
			require.Equal(t, tc.want, ev.Background)
		})
	}
}
