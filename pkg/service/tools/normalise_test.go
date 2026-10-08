package tools

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
)

// An answer that becomes the expected response names the RECORDED ids, not the
// ones the app made in the run it was taken from. An id the app mints is
// another on every run, and a replay follows it only from its recorded value;
// written as that run made it, the test case would match no later run, and the
// id could not be followed again.
//
// The pairs are the ones the test result was reported with (run_ids): what was
// swapped into its expected response, and what the app's answer named.
func TestNormalizePutsTheRecordedIDsBack(t *testing.T) {
	const x, y = "0f8fad5b-d9cb-469f-a165-70867728950e", "7c9e6679-7425-40de-944b-e07fc1f90ae7"
	const x2, y2 = "11111111-1111-4111-8111-111111111111", "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	recordedAt := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	golden := func(name, body string) *models.TestCase {
		return &models.TestCase{Kind: models.HTTP, Name: name, HTTPResp: models.HTTPResp{StatusCode: 200, Body: body, Timestamp: recordedAt}}
	}
	failed := func(name string, res models.HTTPResp, runIDs map[string]string) models.TestResult {
		return models.TestResult{Kind: models.HTTP, TestCaseID: name, Status: models.TestStatusFailed, Res: res, RunIDs: runIDs}
	}
	cases := []*models.TestCase{
		golden("read", `{"id":"`+x+`","status":"pending"}`),
		golden("list", `{"orders":["`+x+`","`+x2+`"],"count":2}`),
		golden("plain", `{"id":"`+x+`","status":"pending"}`),
		golden("passed", `{"id":"`+x+`"}`),
	}
	results := []models.TestResult{
		// The answer names this run's id in the body and in a header, and
		// once inside a longer token, which is another word.
		failed("read", models.HTTPResp{StatusCode: 200, Header: map[string]string{"Location": "/orders/" + y, "Content-Type": "application/json"},
			Body: `{"id":"` + y + `","status":"shipped","ref":"order-` + y + `"}`}, map[string]string{x: y}),
		failed("list", models.HTTPResp{StatusCode: 200, Body: `{"orders":["` + y + `","` + y2 + `"],"count":3}`}, map[string]string{x: y, x2: y2}),
		// A result with no pairs — a run that followed nothing, or a report
		// written before ids were followed — is taken as it is.
		failed("plain", models.HTTPResp{StatusCode: 200, Body: `{"id":"` + y + `","status":"shipped"}`}, nil),
		{Kind: models.HTTP, TestCaseID: "passed", Status: models.TestStatusPassed, Res: models.HTTPResp{Body: `{"id":"` + y + `"}`}, RunIDs: map[string]string{x: y}},
	}

	written := map[string]models.HTTPResp{}
	db := NewMockTestDB(t)
	db.On("GetTestCases", mock.Anything, "set").Return(cases, nil)
	db.On("UpdateTestCase", mock.Anything, mock.Anything, "set", true).
		Run(func(args mock.Arguments) {
			tc := args.Get(1).(*models.TestCase)
			written[tc.Name] = tc.HTTPResp
		}).Return(nil)
	tools := &Tools{logger: zap.NewNop(), testDB: db, config: &config.Config{}}

	require.NoError(t, tools.NormalizeTestCases(context.Background(), "run", "set", nil, results))

	require.Len(t, written, 3, "a test case that passed is left alone")
	require.JSONEq(t, `{"id":"`+x+`","status":"shipped","ref":"order-`+y+`"}`, written["read"].Body)
	require.Equal(t, map[string]string{"Location": "/orders/" + x, "Content-Type": "application/json"}, written["read"].Header)
	require.Equal(t, recordedAt, written["read"].Timestamp, "the recorded timestamp is kept, as before")
	require.JSONEq(t, `{"orders":["`+x+`","`+x2+`"],"count":3}`, written["list"].Body)
	require.JSONEq(t, `{"id":"`+y+`","status":"shipped"}`, written["plain"].Body)

	require.Equal(t, "/orders/"+y, results[0].Res.Header["Location"], "the report's own answer is not changed")
}
