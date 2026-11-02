package replay

import (
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// A test keploy.yml's test.ignoredTests names, or whose set it ignores
// whole, is the user's own choice, and its IGNORED row and the run's share
// line say so ("ignored by keploy.yml"), where they read "no reason given".
// A reason the run's caller gives the test wins; a test the run's list skips
// that keploy.yml does not name still has none; and the run's summary does
// not list keploy.yml's tests as skipped by keploy.
func TestAKeployYmlIgnoreSaysKeployYmlIsWhy(t *testing.T) {
	const byYml = models.IgnoredByConfigReason
	for _, c := range []struct {
		name     string
		yml      map[string][]string // keploy.yml's list (ConfigIgnoredTests)
		run      map[string][]string // the run's list (IgnoredTests)
		reasons  map[string]string   // the run's caller's, for test-set-0
		status   models.TestSetStatus
		rows     map[string]string // IGNORED row -> its reason
		line     string
		listsOne bool // the summary lists a test as skipped by keploy
	}{
		{"keploy.yml names one test", map[string][]string{"test-set-0": {"test-2"}}, nil, nil,
			models.TestSetStatusPassed, map[string]string{"test-2": byYml},
			"2 of 3 compared; 1 set aside: 1 ignored by keploy.yml", false},
		{"keploy.yml names all but one test", map[string][]string{"test-set-0": {"test-1", "test-3"}}, nil, nil,
			models.TestSetStatusPassed, map[string]string{"test-1": byYml, "test-3": byYml},
			"1 of 3 compared; 2 set aside: 2 ignored by keploy.yml", false},
		{"keploy.yml ignores the set whole", map[string][]string{"test-set-0": {}}, nil, nil,
			models.TestSetStatusIgnored, map[string]string{"test-1": byYml, "test-2": byYml, "test-3": byYml},
			"0 of 3 compared; 3 set aside: 3 ignored by keploy.yml", false},
		{"the caller's reason for a test keploy.yml names wins", map[string][]string{"test-set-0": {"test-2", "test-3"}}, nil,
			map[string]string{"test-2": "manual: why two"},
			models.TestSetStatusPassed, map[string]string{"test-2": "manual: why two", "test-3": byYml},
			"1 of 3 compared; 2 set aside: 1 ignored by keploy.yml, 1 for 1 other reason", true},
		{"the run's list names a test keploy.yml does not", map[string][]string{"test-set-0": {"test-2"}},
			map[string][]string{"test-set-0": {"test-2", "test-3"}}, nil,
			models.TestSetStatusPassed, map[string]string{"test-2": byYml, "test-3": ""},
			"1 of 3 compared; 2 set aside: 1 ignored by keploy.yml, 1 for 1 other reason", false},
		{"keploy.yml ignores another set whole", map[string][]string{"test-set-1": {}},
			map[string][]string{"test-set-0": {}}, nil,
			models.TestSetStatusIgnored, map[string]string{},
			"0 of 3 compared; 3 set aside: 3 " + models.NoSkipReason, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv(provenswitch.Env, "")
			h := newPartialRunHarness(t, 3, 0)
			test := &h.replayer.config.Test
			test.IgnoredTests = c.yml
			test.KeepConfigIgnores()
			if c.run != nil {
				test.IgnoredTests = c.run
			}
			if c.reasons != nil {
				test.SkipReasons = skipReasonsFor("test-set-0", c.reasons)
			}

			if status := h.run(t); status != c.status {
				t.Fatalf("status = %s, want %s", status, c.status)
			}
			got := map[string]string{}
			for _, row := range h.report.report.Tests {
				if row.Status == models.TestStatusIgnored {
					got[row.TestCaseID] = row.SkipReason
				}
			}
			if len(got) != len(c.rows) {
				t.Fatalf("IGNORED rows %v, want %v", got, c.rows)
			}
			for id, want := range c.rows {
				if reason, ok := got[id]; !ok || reason != want {
					t.Fatalf("IGNORED rows %v, want %v", got, c.rows)
				}
			}
			out := captureStdout(t, func() { h.replayer.printSummary(t.Context(), true) })
			lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
			if last := lines[len(lines)-1]; last != c.line {
				t.Fatalf("the summary's last line is %q, want %q:\n%s", last, c.line, out)
			}
			if listed := strings.Contains(out, "Tests skipped by keploy for a reason: 1\n"); listed != c.listsOne ||
				strings.Contains(out, "test-set-0/test-3: ") {
				t.Fatalf("the summary lists a keploy.yml test as skipped by keploy, or not the caller's:\n%s", out)
			}
		})
	}
}
