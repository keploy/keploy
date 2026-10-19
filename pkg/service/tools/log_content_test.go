package tools

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils/logcontent/logtest"
)

// canary is a value the app's response gives and a later request sends.
const canary = "kp-canary-templatize-0b93"

// A value templatize captures nothing for is logged by its place, not the
// value, unless KEPLOY_LOG_CONTENT is set: one an array gives under
// test.ignoreOrdering, one that holds a delimiter of its URL part, and a
// read-back's inside an array nothing would tie a use to.
func TestProcessTestCasesV2_AValueLeftAsRecordedIsLoggedWithoutIt(t *testing.T) {
	ordering := config.Config{Test: config.Test{IgnoreOrdering: true}}
	for want, tc := range map[string]struct {
		cfg   config.Config
		cases func() []*models.TestCase
	}{
		"test-set-0/test-1 body.items[0].id gives a value a later request sends, inside an array: test.ignoreOrdering is on, so which element gives it cannot be decided, and templatize captures nothing there": {ordering, func() []*models.TestCase {
			return []*models.TestCase{tcase("test-1", "POST", "http://h/users", nil, `{}`, 201, nil, `{"items":[{"id":"`+canary+`"}]}`),
				tcase("test-2", "GET", "http://h/me", map[string]string{"X-Id": canary}, "", 200, nil, `{"ok":true}`)}
		}},
		"test-set-0/test-1 Location.path.1 gives a value a later request sends, holding a delimiter of its URL part, where it reads as two parts: the replay could not tell it from them, and templatize captures nothing there": {config.Config{}, func() []*models.TestCase {
			return []*models.TestCase{tcase("test-1", "POST", "http://h/files", nil, `{}`, 201, map[string]string{"Location": "/files/" + canary + "%2Fb"}, ``),
				tcase("test-2", "GET", "http://h/me", map[string]string{"X-File": canary + "/b"}, "", 200, nil, `{"ok":true}`)}
		}},
		"test-set-0/test-2 body.items[0].id holds a read-back's value inside an array, and it stays as recorded: test.ignoreOrdering is on and the noise entry body.id (globalNoise) covers it, so nothing would tie a use there to its element and keploy test would refuse it": {config.Config{Test: config.Test{IgnoreOrdering: true,
			GlobalNoise: config.Globalnoise{Global: config.GlobalNoise{"body": {"id": {}}}}}}, func() []*models.TestCase {
			return []*models.TestCase{tcase("test-1", "POST", "http://h/users", nil, `{}`, 201, nil, `{"id":"`+canary+`"}`),
				tcase("test-2", "GET", "http://h/users", map[string]string{"X-Ids": canary}, "", 200, nil, `{"items":[{"id":"`+canary+`","name":"alice"}]}`)}
		}},
	} {
		t.Run(want, func(t *testing.T) {
			logtest.Each(t, func(t *testing.T, on bool) {
				logger, logs := logtest.Observe()
				cfg, cases := tc.cfg, tc.cases()
				db := &placementTestDB{written: map[string]models.TestCase{}, files: caseFiles(t, cases)}
				tools := &Tools{logger: logger, testDB: db, testSetConf: &placementConfStore{stored: models.TestSet{}}, config: &cfg}
				require.NoError(t, tools.ProcessTestCasesV2(context.Background(), cases, "test-set-0"))
				logtest.Check(t, logs, canary, on, want)
			})
		})
	}
}
