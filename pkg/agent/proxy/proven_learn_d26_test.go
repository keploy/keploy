package proxy

import (
	"net/http"
	"reflect"
	"strconv"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// A recording the learn step pairs twice keeps, on its consumed state, a leaf
// either pairing learned whole as whole, and the masks of a leaf both
// masked (founder decision 26): a mask never narrows what was learned whole.
func TestALearnStepReusedMockKeepsALeafLearnedWholeWhole(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	const mask, other = `^a (?:\d{2}:\d{2})$`, `^a b$`
	for _, tc := range []struct {
		name          string
		first, second []string
		want          []string
	}{
		{"whole, then a mask", []string{}, []string{mask}, []string{}},
		{"a mask, then whole", []string{mask}, []string{}, []string{}},
		{"two masks", []string{mask}, []string{other}, []string{mask, other}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mm := NewMockManager(nil, nil, zap.NewNop())
			t.Cleanup(mm.Close)
			for _, noise := range [][]string{tc.first, tc.second} {
				if err := mm.flagMockAsUsed(models.MockState{Name: "mock-0", ReqBodyNoise: map[string][]string{"header.x-sent": noise}}); err != nil {
					t.Fatal(err)
				}
			}
			got := r3States(mm)["mock-0"].ReqBodyNoise["header.x-sent"]
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("the consumed state carries %q, want %q", got, tc.want)
			}
		})
	}
}

// In record's full round a value that drifted only by F's clock is learned
// with a mask on the consumed state, through the proxy as it runs.
func TestALearnStepMasksAClockOnlyDrift(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	p := startedDrainProxy(t, 100)
	p.learnTracker = learn.NewTracker()
	p.ContinueTestErrorCapture()
	now := time.Now()
	m := r3Mock("mock-0", now, "POST", "/orders", `{"note":"order 1234 placed at 2026-10-14T10:00:41Z","rid":"r-1"}`, `{"ok":true}`)
	m.TestModeInfo.Lifetime = models.LifetimePerTest
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	mm.SetMocksWithWindow([]*models.Mock{m}, nil, now.Add(-time.Hour), now.Add(time.Hour))
	at, _ := time.Parse(time.RFC3339, "2026-10-14T10:00:41Z")
	body := `{"note":"order 1234 placed at ` + at.Add(learn.FullRoundLead).Format(time.RFC3339) + `","rid":"r-2"}`
	code, got := r3Send(t, p, mm, models.OutgoingOptions{Learn: true}, "POST /orders HTTP/1.1\r\nHost: deps.example\r\nContent-Type: application/json\r\nContent-Length: "+strconv.Itoa(len(body))+"\r\n\r\n"+body)
	if code != http.StatusOK || got != `{"ok":true}` {
		t.Fatalf("the app got %d %s", code, got)
	}
	noise := r3States(mm)["mock-0"].ReqBodyNoise
	if rid, ok := noise["body.rid"]; !ok || len(rid) != 0 {
		t.Fatalf("a fresh id is learned whole: %v", noise)
	}
	note := noise["body.note"]
	if len(note) != 1 || !learn.Masked(note, "order 1234 placed at 2027-01-01T00:00:00Z", true) || learn.Masked(note, "order 1 placed at 2027-01-01T00:00:00Z", true) {
		t.Fatalf("the clock-only note is learned as %q, want a mask of its clock read", note)
	}
}
