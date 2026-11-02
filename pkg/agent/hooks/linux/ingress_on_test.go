//go:build linux

package linux

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// keploy mock's record attaches the ingress hooks whenever it records
// requests, with or without a pass-through port, so a caller from outside
// the run (a provider's webhook callback) is recorded; its replay attaches
// them only for a pass-through port; a record attaches them always.
func TestIngressHooksAttachInEveryRecordThatKeepsRequests(t *testing.T) {
	for _, c := range []struct {
		name     string
		mode     models.Mode
		mock     bool
		requests bool
		ports    int
		want     bool
	}{
		{"mock record, requests on, no pass-through port", models.MODE_RECORD, true, true, 0, true},
		{"mock record, requests on, a pass-through port", models.MODE_RECORD, true, true, 1, true},
		{"mock record, requests off", models.MODE_RECORD, true, false, 1, false},
		{"mock replay, requests on, no pass-through port", models.MODE_TEST, true, true, 0, false},
		{"mock replay, requests on, a pass-through port", models.MODE_TEST, true, true, 1, true},
		{"mock replay, requests off, a pass-through port", models.MODE_TEST, true, false, 1, false},
		{"record", models.MODE_RECORD, false, false, 0, true},
		{"test", models.MODE_TEST, false, true, 1, false},
	} {
		if got := ingressOn(c.mode, c.mock, c.requests, c.ports); got != c.want {
			t.Errorf("%s: ingress hooks attach = %v, want %v", c.name, got, c.want)
		}
	}
}
