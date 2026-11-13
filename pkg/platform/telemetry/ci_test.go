package telemetry

import "testing"

// A remote in git's SCP-like form on an IPv6 host brackets it, as git writes
// it (git@[fd00::1]:owner/repo.git, or [git@fd00::1]:owner/repo.git), and the
// path follows the "]:". Taken from the first colon, which is the address's,
// the repository came out as ":1]:owner/repo".
func TestNormalizeRepoTakesAnIPv6Host(t *testing.T) {
	for _, c := range []struct{ raw, want string }{
		{"git@[fd00::1]:owner/repo.git", "owner/repo"},
		{"git@[::1]:owner/repo", "owner/repo"},
		{"[git@fd00::1]:owner/repo.git", "owner/repo"},
		{"[fd00::1]:owner/repo", ""},
		{"git@[fd00::1]:/owner/repo.git", "owner/repo"},
		{"git@[fd00::1]", ""},
		{"git@[fd00::1]/owner/repo", ""},
		{"git@[fd00::1]:repo", ""},
		{"ssh://git@[fd00::1]:2222/owner/repo.git", "owner/repo"},
		{"git@github.com:owner/repo.git", "owner/repo"},
		{"git@10.0.0.1:owner/repo.git", "owner/repo"},
		{"https://github.com/owner/repo.git", "owner/repo"},
		{"owner/repo", "owner/repo"},
	} {
		if got := normalizeRepo(c.raw); got != c.want {
			t.Errorf("normalizeRepo(%q) = %q, want %q", c.raw, got, c.want)
		}
	}
}

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
