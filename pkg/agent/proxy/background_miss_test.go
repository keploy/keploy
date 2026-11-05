package proxy

import (
	"context"
	"testing"

	"go.uber.org/zap"
)

// The agent's read of a test's misses keeps a parser's background mark
// (MockMismatchReport.Background): the replay lists the call and counts it
// against no test. An unmarked miss stays unmarked.
func TestTheMissReadKeepsTheBackgroundMark(t *testing.T) {
	p := &Proxy{logger: zap.NewNop(), errChannel: make(chan error, 16)}
	p.BeginTestErrorCapture()
	bg := mockMiss("findAndModify ptw_endpoints")
	bg.MismatchReport.Background = true
	p.errChannel <- bg
	p.errChannel <- mockMiss("find users")
	got, err := p.GetMockErrors(context.Background())
	if err != nil || len(got) != 2 {
		t.Fatalf("read %+v, %v; want both misses", got, err)
	}
	for _, c := range got {
		if want := c.ActualSummary == "findAndModify ptw_endpoints"; c.Background != want || c.CountsAgainstTest() == want {
			t.Errorf("%s: Background %v, counts against its test %v", c.ActualSummary, c.Background, c.CountsAgainstTest())
		}
	}
}
