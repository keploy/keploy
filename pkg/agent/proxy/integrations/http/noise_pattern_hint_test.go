package http

import (
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// keploy.yml's request-body noise with patterns excuses only a value one of
// them matches, so a call whose value there is outside them misses on that
// field. The hint names the field and its pattern and says to widen it or
// record again, never to declare the field, which keploy.yml already does
// (main said "add requestbody: {id: []}" where both values were outside, and
// "Request structure changed" where only the live one was). A drifted field
// keploy.yml does not declare keeps the requestbody remedy.
func TestHTTPMissHintNamesTheDeclaredPatternAValueIsOutside(t *testing.T) {
	const declared = "body.id is noise in keploy.yml only for a value matching ^o-[0-9]+$, and the "
	const widen = ". If the change is expected, widen the pattern under test.globalNoise.global.requestbody in keploy.yml, or re-record with 'keploy record'."
	for _, c := range []struct {
		name, recorded, live string
		want                 []string
	}{
		{"the live value outside, the recorded one inside", `{"id":"o-1","note":"a"}`, `{"id":"x-9","note":"a"}`,
			[]string{declared + "live value is outside it" + widen}},
		{"both values outside", `{"id":"r-1","note":"a"}`, `{"id":"x-9","note":"a"}`,
			[]string{declared + "recorded and live values are outside it" + widen}},
		{"the recorded value outside, the live one inside", `{"id":"r-1","note":"a"}`, `{"id":"o-9","note":"a"}`,
			[]string{declared + "recorded value is outside it" + widen}},
		{"beside a drift keploy.yml does not declare", `{"id":"r-1","note":"a"}`, `{"id":"x-9","note":"b"}`,
			[]string{declared + "recorded and live values are outside it" + widen, "Only values drifted (body.note)", "requestbody: {note: []}"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			mock := learnRecorded("mock-1", 0, "POST", "/v1/find", c.recorded, nil, `{}`)
			req := reqToHost("POST", "/v1/find", "api.test")
			// The recorded headers' keys, so the body is the only difference.
			req.Header.Set("Host", "api.test")
			req.Header.Set("Content-Type", "application/json")
			r := newHTTP().buildHTTPMismatchReport(req, []byte(c.live), &mockMemDb{mocks: []*models.Mock{mock}}, []*models.Mock{mock}, nil, map[string][]string{"id": {"^o-[0-9]+$"}}, nil)
			for _, w := range c.want {
				if !strings.Contains(r.NextSteps, w) {
					t.Errorf("the hint\n%s\ndoes not say\n%s", r.NextSteps, w)
				}
			}
			if strings.Contains(r.NextSteps, "{id:") || strings.Contains(r.NextSteps, "Request structure changed") {
				t.Errorf("the hint tells the user to declare body.id, or calls a value outside its pattern a structural change:\n%s", r.NextSteps)
			}
		})
	}
}

// The hint names a keploy.yml entry with patterns only where the comparison
// that refused the call applies it. Under the proven switch the strict
// comparison reads an entry as whole fields (matcher.NoiseLookup), so "id"
// never names "paid": widening its pattern would change nothing, and the
// hint names body.paid as a field keploy.yml does not declare. With the
// switch off the request matcher reads "id" as a substring of "paid", and
// the hint names the entry's pattern.
func TestHTTPMissHintNamesOnlyAnEntryTheJudgingComparisonApplies(t *testing.T) {
	const named = "body.paid is noise in keploy.yml only for a value matching ^o-[0-9]+$"
	for _, c := range []struct {
		proven string
		want   string
		never  string
	}{
		{"1", "requestbody: {paid: []}", named},
		{"0", named + ", and the recorded and live values are outside it", "{paid:"},
	} {
		t.Run("KEPLOY_PROVEN="+c.proven, func(t *testing.T) {
			t.Setenv(provenswitch.Env, c.proven)
			mock := learnRecorded("mock-1", 0, "POST", "/v1/find", `{"paid":"r-1"}`, nil, `{}`)
			req := reqToHost("POST", "/v1/find", "api.test")
			req.Header.Set("Host", "api.test")
			req.Header.Set("Content-Type", "application/json")
			r := newHTTP().buildHTTPMismatchReport(req, []byte(`{"paid":"x-9"}`), &mockMemDb{mocks: []*models.Mock{mock}}, []*models.Mock{mock}, nil, map[string][]string{"id": {"^o-[0-9]+$"}}, nil)
			if !strings.Contains(r.NextSteps, c.want) {
				t.Errorf("the hint\n%s\ndoes not say\n%s", r.NextSteps, c.want)
			}
			if strings.Contains(r.NextSteps, c.never) {
				t.Errorf("the hint says %q:\n%s", c.never, r.NextSteps)
			}
		})
	}
}
