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
		// AppVeyor also sets CI=True; Bamboo and GoCD set no CI variable.
		{env: map[string]string{"APPVEYOR": "True", "CI": "True"}, ci: true, provider: "appveyor"},
		{env: map[string]string{"bamboo_planKey": "PROJ-PLAN"}, ci: true, provider: "bamboo"},
		{env: map[string]string{"GO_PIPELINE_LABEL": "42"}, ci: true, provider: "gocd"},
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

// detectGitRepo reads AppVeyor's and Bamboo's repository variables as it
// reads every other CI's, before it would ask git.
func TestDetectGitRepoFromCIVariables(t *testing.T) {
	for _, tc := range []struct {
		env  map[string]string
		want string
	}{
		{env: map[string]string{"APPVEYOR_REPO_NAME": "acme/shop"}, want: "acme/shop"},
		{env: map[string]string{"bamboo_planRepository_repositoryUrl": "git@bitbucket.org:acme/shop.git"}, want: "acme/shop"},
		{env: map[string]string{"GITHUB_REPOSITORY": "keploy/keploy", "APPVEYOR_REPO_NAME": "acme/shop"}, want: "keploy/keploy"},
	} {
		for _, v := range ciRepoVars {
			t.Setenv(v, "")
		}
		for k, v := range tc.env {
			t.Setenv(k, v)
		}
		if got := detectGitRepo(); got != tc.want {
			t.Errorf("%v: detectGitRepo() = %q, want %q", tc.env, got, tc.want)
		}
	}
}
