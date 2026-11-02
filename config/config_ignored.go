package config

import "slices"

// KeepConfigIgnores keeps IgnoredTests as keploy.yml gave it
// (ConfigIgnoredTests). It runs where keploy.yml is loaded: what the run's
// IgnoredTests gains after that, from whatever skips a test while the run
// happens, carries its own reason in SkipReasons.
func (t *Test) KeepConfigIgnores() {
	t.ConfigIgnoredTests = make(map[string][]string, len(t.IgnoredTests))
	for set, tests := range t.IgnoredTests {
		t.ConfigIgnoredTests[set] = slices.Clone(tests)
	}
}

// ConfigIgnores reports whether keploy.yml's test.ignoredTests names the
// test, or ignores its whole set (an empty list). A test the run skips with
// no reason in SkipReasons reports models.IgnoredByConfigReason for it.
func (t *Test) ConfigIgnores(testSetID, testCaseID string) bool {
	tests, listed := t.ConfigIgnoredTests[testSetID]
	return listed && (len(tests) == 0 || slices.Contains(tests, testCaseID))
}
