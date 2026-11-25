package proxy

import (
	"net/http"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// In test-2, a call the learn step would pair with test-2's own recording
// that is exactly an authored mock of test-1 (keploy mock derive copies its
// source's request byte for byte, with its source's variables, so a mock
// derived into another test is exactly its source test's call) is not that
// mock's call made where it was not recorded: an authored mock was written,
// never recorded, so it is no evidence of where the application made a
// call. The call is paired with mock-own2, its item learned onto it, and
// answered, as with no authored mock beside it. Before, the learn step read
// it as test-1's late call, learned nothing, and the source test missed in
// record's full round every time a derived mock sat in a later test.
func TestACallExactlyAnotherTestsAuthoredMockIsPairedWithItsOwnRecording(t *testing.T) {
	for _, routed := range []bool{false, true} {
		for _, vars := range []bool{false, true} {
			name := map[bool]string{false: "today's rules", true: "proven routing"}[routed] + map[bool]string{false: "/no variables", true: "/its source's variable"}[vars]
			t.Run(name, func(t *testing.T) {
				p, mm := elsewhereOrders(t, routed, func(pool, own1, own2 *models.Mock) []*models.Mock {
					a1 := r3Mock("mock-a1", own1.Spec.ReqTimestampMock.Add(-50*time.Millisecond), "POST", "/orders", `{"item":"kiwi"}`, `{"order":9}`)
					a1.Spec.Authored = &models.Authored{How: models.AuthoredDerived, Reason: "card declined", Source: "test-2/mock-own2"}
					if vars {
						a1.Spec.Variables = models.MockVariables{"item": models.MockVariable{Recorded: "kiwi", Bind: "body.item"}}
					}
					return []*models.Mock{pool, own1, own2, a1}
				}, nil)
				mm.ServeExactTest("test-2", []*models.Mock{roundMock(mm, "mock-own2")})
				p.ContinueTestErrorCapture()
				code, got, learned, misses := elsewhereSent(t, p, mm, `{"item":"kiwi"}`)
				if _, ok := learned["mock-own2"]; code != http.StatusOK || got != `{"order":2}` || !ok || len(misses) != 0 {
					t.Fatalf("in test-2 a call that is exactly test-1's authored mock-a1 was answered %d %s, learned %v, misses %+v; want it paired with mock-own2, its item learned there", code, got, learned, misses)
				}
				if _, ok := learned["mock-a1"]; ok {
					t.Fatalf("learned %v onto the authored mock-a1", learned["mock-a1"])
				}
			})
		}
	}
}
