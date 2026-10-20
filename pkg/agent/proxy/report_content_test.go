package proxy

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils/logcontent"
	"go.uber.org/zap"
)

// literalReport is a miss report a parser built without keploy's report
// builder, with the application's content in every field that can carry it.
func literalReport() *models.MockMismatchReport {
	return &models.MockMismatchReport{
		Protocol:      "MySQL",
		ActualSummary: "COM_QUERY SELECT * FROM users WHERE email = 'kp-agent-literal'",
		Diff:          `body.email: recorded "kp-agent-diff"`,
		FieldDiffs:    []models.MockFieldDiff{{Path: "body.email", Kind: models.DiffKindValueChanged, Expected: "kp-agent-rec", Actual: "kp-agent-live"}},
		ClosestMock:   "mock-3", ClosestMockReq: "kp-agent-mock-req", ReceivedReq: "kp-agent-live-req",
	}
}

func agentMisses(t *testing.T, r *models.MockMismatchReport) []models.UnmatchedCall {
	t.Helper()
	p := &Proxy{logger: zap.NewNop(), errChannel: make(chan error, 4)}
	p.BeginTestErrorCapture()
	p.sendMockNotFoundError(models.NewMockMismatchError(models.ErrNoMockMatched, r))
	got, err := p.GetMockErrors(context.Background())
	if err != nil || len(got) != 1 {
		t.Fatalf("want the one miss, got %+v (%v)", got, err)
	}
	return got
}

// Every miss leaves the agent without the application's content by default,
// however its parser built its report, and still names the call, the field
// and how it differs.
func TestAMissLeavesTheAgentWithoutContentByDefault(t *testing.T) {
	got := agentMisses(t, literalReport())
	b, _ := json.Marshal(got)
	for _, c := range []string{"kp-agent-literal", "kp-agent-diff", "kp-agent-rec", "kp-agent-live", "kp-agent-mock-req", "kp-agent-live-req"} {
		if strings.Contains(string(b), c) {
			t.Errorf("the agent's miss carries %q: %s", c, b)
		}
	}
	m := got[0]
	if m.ActualSummary != "COM_QUERY SELECT * FROM users WHERE email = ?" || m.ClosestMock != "mock-3" || !m.ContentWithheld ||
		len(m.FieldDiffs) != 1 || m.FieldDiffs[0].Path != "body.email" || m.Diff != "body.email: value changed" {
		t.Errorf("the agent's miss is %+v", m)
	}
}

// With the switch on the miss is its report, as it was.
func TestAMissLeavesTheAgentWholeWithTheSwitchOn(t *testing.T) {
	t.Cleanup(logcontent.Set(true))
	r := literalReport()
	m := agentMisses(t, r)[0]
	if m.ActualSummary != r.ActualSummary || m.Diff != r.Diff || !reflect.DeepEqual(m.FieldDiffs, r.FieldDiffs) ||
		m.ClosestMockReq != r.ClosestMockReq || m.ReceivedReq != r.ReceivedReq || m.ContentWithheld {
		t.Errorf("with the switch on the miss is %+v, want its report %+v", m, *r)
	}
}

// A report its builder withheld leaves the agent with its own words: the
// parser's diff and its summary as the builder shaped it, and the values
// written into it since withheld again.
func TestAWithheldMissLeavesTheAgentWithItsOwnWords(t *testing.T) {
	r := &models.MockMismatchReport{
		Protocol: "MySQL", ActualSummary: "COM_QUERY SELECT \"Users\" FROM t", ContentWithheld: true,
		Diff:        "this test's recording holds COM_STMT_EXECUTE mocks whose statement keploy cannot tell (mock-1)",
		FieldDiffs:  []models.MockFieldDiff{{Path: "body.a", Kind: models.DiffKindValueChanged, Expected: "kp-agent-later"}},
		ReceivedReq: "kp-agent-later-req",
	}
	m := agentMisses(t, r)[0]
	if m.Diff != r.Diff || m.ActualSummary != r.ActualSummary || !m.ContentWithheld ||
		m.FieldDiffs[0].Expected != "" || m.ReceivedReq != "" {
		t.Errorf("the agent's miss is %+v, want its report's own words and no value", m)
	}
}
