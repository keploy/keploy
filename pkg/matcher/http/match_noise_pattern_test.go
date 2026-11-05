package http

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// A test whose header keploy.yml's noise lets vary only within a pattern
// fails when the live value is outside it, and says why: the header, the
// pattern and the side, never the value.
func TestMatchSaysWhyANoisePatternDoesNotExcuseAHeader(t *testing.T) {
	core, logs := observer.New(zap.WarnLevel)
	tc := &models.TestCase{
		Name: "t1",
		HTTPResp: models.HTTPResp{
			StatusCode: 200,
			Header:     map[string]string{"X-Req": "r-1"},
			Body:       `{"k":"v"}`,
		},
	}
	live := &models.HTTPResp{
		StatusCode: 200,
		Header:     map[string]string{"X-Req": "secret"},
		Body:       `{"k":"v"}`,
	}
	noise := map[string]map[string][]string{"header": {"X-Req": {"^r-"}}}
	if pass, _ := Match(tc, live, noise, false, false, zap.New(core), false); pass {
		t.Fatal("a live header outside its noise pattern passed")
	}
	got := logs.FilterMessage("a keploy.yml noise pattern does not excuse a field").AllUntimed()
	if len(got) != 1 {
		t.Fatalf("want one warning naming the pattern, got %d", len(got))
	}
	f := got[0].ContextMap()
	want := "keploy.yml's noise lets header.X-Req vary only within \"^r-\"; the live value (a string of 6 bytes) is outside it, so it is compared"
	if f["testCase"] != "t1" || f["reason"] != want {
		t.Fatalf("warning fields %v, want reason %q", f, want)
	}
}
