package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils/logcontent"
	"go.uber.org/zap"
)

// agentCall is a miss as an agent that did not withhold its content sends
// it: one older than this CLI, or one opted in where this CLI is not.
const agentCall = `[{"protocol":"HTTP","actual_summary":"POST /api/users?token=kp-cli-query","closest_mock":"mock-1",` +
	`"diff":"body.email: recorded \"kp-cli-diff\"","field_diffs":[{"path":"body.email","kind":"value_changed","expected":"kp-cli-rec","actual":"kp-cli-live"}],` +
	`"closest_mock_req":"kp-cli-mock-req","received_req":"kp-cli-live-req"}]`

func cliMisses(t *testing.T) []models.UnmatchedCall {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(agentCall))
	}))
	defer srv.Close()
	a := New(zap.NewNop(), nil, &config.Config{Agent: config.Agent{AgentURI: srv.URL}})
	got, err := a.GetMockErrors(context.Background())
	if err != nil || len(got) != 1 {
		t.Fatalf("got %+v, %v; want the one call", got, err)
	}
	return got
}

// Every report the CLI writes or prints reads its calls from the agent here,
// and by default none of them carries the application's content, whatever
// the agent sent.
func TestTheCLIReadsNoContentFromAnAgentByDefault(t *testing.T) {
	got := cliMisses(t)
	b, _ := json.Marshal(got)
	for _, c := range []string{"kp-cli-query", "kp-cli-diff", "kp-cli-rec", "kp-cli-live", "kp-cli-mock-req", "kp-cli-live-req"} {
		if strings.Contains(string(b), c) {
			t.Errorf("the CLI kept %q from the agent: %s", c, b)
		}
	}
	if m := got[0]; m.ActualSummary != "POST /api/users" || !m.ContentWithheld || len(m.FieldDiffs) != 1 || m.FieldDiffs[0].Path != "body.email" {
		t.Errorf("the CLI's call is %+v", m)
	}
}

// With the switch on the CLI keeps what the agent sent.
func TestTheCLIKeepsAnAgentsContentWithTheSwitchOn(t *testing.T) {
	t.Cleanup(logcontent.Set(true))
	var want []models.UnmatchedCall
	if err := json.Unmarshal([]byte(agentCall), &want); err != nil {
		t.Fatal(err)
	}
	got := cliMisses(t)
	gb, _ := json.Marshal(got)
	wb, _ := json.Marshal(want)
	if string(gb) != string(wb) {
		t.Errorf("with the switch on the CLI read\n%s\nwant\n%s", gb, wb)
	}
}
