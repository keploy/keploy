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

// detectGitRepo reads the slug of the repository a CI run builds from the
// first provider variable that names one, whether it holds a slug or a git
// URL, before it would ask git; the rows are enterprise's CIRepo rows.
func TestDetectGitRepoFromCIVariables(t *testing.T) {
	for _, tc := range []struct {
		env  map[string]string
		want string
	}{
		{env: map[string]string{"GITHUB_REPOSITORY": "keploy/keploy"}, want: "keploy/keploy"},
		{env: map[string]string{"CI_REPO": "keploy/enterprise"}, want: "keploy/enterprise"},
		{env: map[string]string{"CI_PROJECT_PATH": "acme/platform/shop"}, want: "acme/platform/shop"},
		{env: map[string]string{"BUILDKITE_REPO": "git@github.com:acme/shop.git"}, want: "acme/shop"},
		{env: map[string]string{"BUILDKITE_REPO": "https://github.com/acme/shop.git"}, want: "acme/shop"},
		{env: map[string]string{"BUILDKITE_REPO": "ssh://git@github.com:22/acme/shop"}, want: "acme/shop"},
		{env: map[string]string{"BUILDKITE_REPO": "github.com:acme/shop.git"}, want: "acme/shop"},
		{env: map[string]string{"BUILD_REPOSITORY_NAME": "acme/shop"}, want: "acme/shop"},
		// A git URL without its scheme: exactly host/owner/repo, and only
		// from a variable that holds a URL.
		{env: map[string]string{"BUILDKITE_REPO": "github.com/acme/shop"}, want: "acme/shop"},
		{env: map[string]string{"BUILDKITE_REPO": "gitlab.example.com/acme/sub/shop"}, want: "gitlab.example.com/acme/sub/shop"},
		// A slug's dotted first group is a GitLab group, not a host.
		{env: map[string]string{"CI_PROJECT_PATH": "acme.io/backend/api"}, want: "acme.io/backend/api"},
		{env: map[string]string{"CI_PROJECT_PATH": "x.y/keploy/z"}, want: "x.y/keploy/z"},
		{env: map[string]string{"GITHUB_REPOSITORY": "github.com/acme/shop"}, want: "github.com/acme/shop"},
		{env: map[string]string{"GITHUB_REPOSITORY": "acme/web.app"}, want: "acme/web.app"},
		// The URL variables: CircleCI, Azure, Jenkins, CodeBuild.
		{env: map[string]string{"CIRCLE_REPOSITORY_URL": "git@github.com:keploy/keploy.git"}, want: "keploy/keploy"},
		{env: map[string]string{"BUILD_REPOSITORY_URI": "https://github.com/acme/shop"}, want: "acme/shop"},
		{env: map[string]string{"GIT_URL": "https://github.com/keploy/enterprise.git"}, want: "keploy/enterprise"},
		{env: map[string]string{"CODEBUILD_SOURCE_REPO_URL": "https://github.com/acme/shop.git"}, want: "acme/shop"},
		// A variable that names no repository is read past, to the URL
		// beside it: Azure Repos' bare name.
		{env: map[string]string{"BUILD_REPOSITORY_NAME": "shop", "BUILD_REPOSITORY_URI": "https://acme@dev.azure.com/acme/shop/_git/shop"}, want: "acme/shop/_git/shop"},
		{env: map[string]string{"GITHUB_REPOSITORY": "shop", "GIT_URL": "git@github.com:keploy/k8s-env.git"}, want: "keploy/k8s-env"},
		{env: map[string]string{"CI_PROJECT_PATH": "acme.inc/shop"}, want: "acme.inc/shop"},
		{env: map[string]string{"CI_PROJECT_PATH": "acme/shop/"}, want: "acme/shop"},
		{env: map[string]string{"APPVEYOR_REPO_NAME": "acme/shop"}, want: "acme/shop"},
		{env: map[string]string{"bamboo_planRepository_repositoryUrl": "git@bitbucket.org:acme/shop.git"}, want: "acme/shop"},
		// No owner: a URL to a host's root, a value with a space, an @.
		{env: map[string]string{"BUILDKITE_REPO": "https://github.com/shop", "CI_REPO": "acme/shop"}, want: "acme/shop"},
		{env: map[string]string{"HARNESS_REPO": "acme/my shop", "APPVEYOR_REPO_NAME": "acme/shop"}, want: "acme/shop"},
		{env: map[string]string{"DRONE_REPO": "user@acme/shop", "APPVEYOR_REPO_NAME": "acme/shop"}, want: "acme/shop"},
		// A colon after the first slash is not an scp-like remote's.
		{env: map[string]string{"CI_PROJECT_PATH": "acme/x:y/shop", "APPVEYOR_REPO_NAME": "acme/shop"}, want: "acme/shop"},
		// The first variable that names one wins, in enterprise's order.
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

// A checkout's remote names a repository only as a URL or an scp-like
// remote; git reads any other value as a local path. The CI variables are
// read the same, but a variable that holds a slug never takes a dotted first
// group for a host.
func TestRemoteRepo(t *testing.T) {
	for raw, want := range map[string]string{
		"https://github.com/acme/shop.git\n": "acme/shop",
		"git@github.com:acme/shop.git":       "acme/shop",
		"ssh://git@host:2222/acme/shop":      "acme/shop",
		"/srv/git/acme/shop.git":             "",
		"../acme/shop":                       "",
		"github.com/acme/shop":               "",
		"github.com:acme/shop":               "acme/shop",
		"C:/src/acme/shop":                   "",
		"dir/git@host:acme/shop":             "",
		"":                                   "",
	} {
		if got := remoteRepo(raw); got != want {
			t.Errorf("remoteRepo(%q) = %q, want %q", raw, got, want)
		}
	}
	for _, tc := range []struct {
		raw, want string
		url       bool
	}{
		{"acme.io/backend/api", "acme.io/backend/api", false},
		{"acme.io/backend/api", "backend/api", true},
		{"x.y/keploy/z", "x.y/keploy/z", false},
		{"x.y/keploy/z/extra", "x.y/keploy/z/extra", true},
		{"acme/sub/shop", "acme/sub/shop", true},
		{"acme/shop", "acme/shop", true},
	} {
		if got := normalizeRepo(tc.raw, tc.url); got != tc.want {
			t.Errorf("normalizeRepo(%q, %v) = %q, want %q", tc.raw, tc.url, got, tc.want)
		}
	}
	// Each URL variable may hold a URL without its scheme. Named here, not
	// read from ciRepoURLVars: a name dropped from it must fail a row.
	for _, name := range []string{
		"CIRCLE_REPOSITORY_URL", "BUILDKITE_REPO", "BUILD_REPOSITORY_URI",
		"bamboo_planRepository_repositoryUrl", "GIT_URL", "CODEBUILD_SOURCE_REPO_URL",
	} {
		for _, k := range ciRepoVars {
			t.Setenv(k, "")
		}
		t.Setenv(name, "github.com/acme/shop")
		if got := detectGitRepo(); got != "acme/shop" {
			t.Errorf("%s=github.com/acme/shop: detectGitRepo() = %q, want acme/shop", name, got)
		}
	}
}
