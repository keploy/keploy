package telemetry

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestDetectCI(t *testing.T) {
	tests := []struct {
		name         string
		env          map[string]string
		wantCI       bool
		wantProvider string
	}{
		{name: "GitHub Actions", env: map[string]string{"GITHUB_ACTIONS": "true"}, wantCI: true, wantProvider: "github-actions"},
		{name: "GitLab CI", env: map[string]string{"GITLAB_CI": "true"}, wantCI: true, wantProvider: "gitlab-ci"},
		{
			name:         "a named provider wins over the generic CI flag",
			env:          map[string]string{"CI": "true", "JENKINS_URL": "http://jenkins.local"},
			wantCI:       true,
			wantProvider: "jenkins",
		},
		{name: "only the generic CI flag", env: map[string]string{"CI": "true"}, wantCI: true, wantProvider: "unknown"},
		{name: "no CI variables", env: nil, wantCI: false, wantProvider: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearCIEnv(t)
			for k, v := range tt.env {
				t.Setenv(k, v)
			}

			isCI, provider := detectCI()

			assert.Equal(t, tt.wantCI, isCI)
			assert.Equal(t, tt.wantProvider, provider)
		})
	}
}

func TestNormalizeRepo(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "SCP-like SSH remote", raw: "git@github.com:keploy/keploy.git", want: "keploy/keploy"},
		{name: "HTTPS remote", raw: "https://github.com/keploy/keploy", want: "keploy/keploy"},
		{name: "HTTPS remote with .git", raw: "https://github.com/keploy/keploy.git", want: "keploy/keploy"},
		{name: "SSH URL remote", raw: "ssh://git@github.com/keploy/keploy.git", want: "keploy/keploy"},
		{name: "owner/repo slug", raw: "keploy/keploy", want: "keploy/keploy"},
		{name: "host/owner/repo", raw: "github.com/keploy/keploy", want: "keploy/keploy"},
		{name: "empty", raw: "", want: ""},
		{name: "not a repository", raw: "not a repo", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, normalizeRepo(tt.raw))
		})
	}
}

// clearCIEnv blanks every variable detectCI reads, so the test sees a non-CI
// machine even when it runs on a CI runner.
func clearCIEnv(t *testing.T) {
	t.Helper()
	for _, p := range ciProviders {
		t.Setenv(p.envVar, "")
	}
	t.Setenv("CI", "")
}
