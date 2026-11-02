package mock

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"

	"go.keploy.io/server/v3/pkg/models"
)

// recordedDeliveries is, by test ID, each test's deliver: entries
// (models.MappedTestCase.Deliver) with their cases' recorded requests,
// answers, app ports and noise, and the replay's own judging settings
// (test.globalNoise.global, test.ignoreOrdering, test.compareAll: a callback's
// answer is judged as a replayed case's is), read from the set's test cases
// and keploy.yml: what the replay's table carries to
// the agent (models.ScopeTableReq.Deliver). Nil when no test has any. An
// entry keploy cannot send as recorded is an error, never left out: its test
// would wait for a callback that never comes and fail on its handler's
// calls, naming neither. That is a set whose cases this keploy does not
// read, cases that cannot be read, a case with no recorded request, and a
// case that is not an HTTP request.
func (m *mockService) recordedDeliveries(ctx context.Context, name string, mp *models.Mapping) (map[string][]models.DeliveryCase, error) {
	if mp == nil {
		return nil, nil
	}
	var tests []string
	byTest := make(map[string][]models.Delivery)
	for _, tc := range mp.TestCases {
		if len(tc.Deliver) > 0 {
			tests = append(tests, tc.ID)
			byTest[tc.ID] = tc.Deliver
		}
	}
	if len(tests) == 0 {
		return nil, nil
	}
	sort.Strings(tests)
	cannot := func(format string, a ...any) error {
		return fmt.Errorf("keploy cannot deliver the recorded callbacks of mock set %q: %s", name, fmt.Sprintf(format, a...))
	}
	if m.testDB == nil {
		return nil, cannot("test %q lists deliver: entries, but this keploy reads no recorded cases (mock.recordRequests is false in keploy.yml); set it to true and record the set again", tests[0])
	}
	tcs, err := m.testDB.GetTestCases(ctx, name)
	if err != nil {
		return nil, cannot("its recorded cases cannot be read: %v", err)
	}
	byName := make(map[string]*models.TestCase, len(tcs))
	for _, tc := range tcs {
		if tc != nil {
			byName[tc.Name] = tc
		}
	}
	dir := filepath.Join(filepath.Base(m.config.Path), name, "tests")
	out := make(map[string][]models.DeliveryCase, len(tests))
	for _, test := range tests {
		for _, d := range byTest[test] {
			tc, ok := byName[d.Case]
			if !ok {
				return nil, cannot("case %q of test %q's deliver: has no recorded request in %s; restore its file there, or record the set again", d.Case, test, dir)
			}
			if tc.Kind != models.HTTP {
				return nil, cannot("case %q of test %q's deliver: is a %s request; keploy delivers recorded HTTP requests only", d.Case, test, tc.Kind)
			}
			out[test] = append(out[test], models.DeliveryCase{
				Delivery: d, Req: tc.HTTPReq, Resp: tc.HTTPResp, AppPort: tc.AppPort, Noise: tc.Noise,
				GlobalNoise:    m.config.Test.GlobalNoise.Global,
				IgnoreOrdering: m.config.Test.IgnoreOrdering,
				CompareAll:     m.config.Test.CompareAll,
			})
		}
	}
	return out, nil
}
