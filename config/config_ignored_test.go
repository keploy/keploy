package config

import (
	"reflect"
	"testing"
)

// keploy.yml's list is kept as it was loaded: a test or set the run's list
// gains later is not keploy.yml's. ConfigIgnores names a test keploy.yml
// lists, and every test of a set it ignores whole, and no other.
func TestKeployYmlsIgnoreListIsKeptAsLoaded(t *testing.T) {
	test := &Test{IgnoredTests: map[string][]string{"named": {"test-1", "test-3"}, "whole": {}}}
	test.KeepConfigIgnores()
	test.IgnoredTests["named"][0] = "test-2"
	test.IgnoredTests["named"] = append(test.IgnoredTests["named"], "test-4")
	test.IgnoredTests["added"] = []string{}
	want := map[string][]string{"named": {"test-1", "test-3"}, "whole": {}}
	if !reflect.DeepEqual(test.ConfigIgnoredTests, want) {
		t.Fatalf("kept %v, want keploy.yml's %v", test.ConfigIgnoredTests, want)
	}
	for _, c := range []struct {
		set, test string
		want      bool
	}{
		{"named", "test-1", true},
		{"named", "test-2", false},
		{"named", "test-4", false},
		{"whole", "test-9", true},
		{"added", "test-1", false},
		{"other", "test-1", false},
	} {
		if got := test.ConfigIgnores(c.set, c.test); got != c.want {
			t.Errorf("ConfigIgnores(%s, %s) = %v, want %v", c.set, c.test, got, c.want)
		}
	}
	var none Test
	none.KeepConfigIgnores()
	if none.ConfigIgnores("any", "test-1") {
		t.Fatal("no keploy.yml list ignores a test")
	}
}
