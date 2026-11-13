package telemetry

import "testing"

// detectCI names the provider from its own variable, and Woodpecker, which
// sets none, from CI=woodpecker; any other CI value is an unknown provider.
func TestDetectCI(t *testing.T) {
	for _, tc := range []struct {
		env      map[string]string
		ci       bool
		provider string
	}{
		{env: nil, ci: false, provider: ""},
		{env: map[string]string{"GITHUB_ACTIONS": "true", "CI": "true"}, ci: true, provider: "github-actions"},
		{env: map[string]string{"CI": "true"}, ci: true, provider: "unknown"},
		{env: map[string]string{"CI": "woodpecker", "CI_REPO": "keploy/keploy"}, ci: true, provider: "woodpecker"},
		{env: map[string]string{"CI": " Woodpecker "}, ci: true, provider: "woodpecker"},
		{env: map[string]string{"CI": "woodpeckers"}, ci: true, provider: "unknown"},
	} {
		t.Setenv("CI", "")
		for _, p := range ciProviders {
			t.Setenv(p.envVar, "")
		}
		for k, v := range tc.env {
			t.Setenv(k, v)
		}
		if ci, provider := detectCI(); ci != tc.ci || provider != tc.provider {
			t.Errorf("%v: detectCI() = %v, %q; want %v, %q", tc.env, ci, provider, tc.ci, tc.provider)
		}
	}
}
