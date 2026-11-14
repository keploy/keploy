package telemetry

import (
	"context"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// ciProviders maps environment variables to CI provider names.
// More specific checks come first.
var ciProviders = []struct {
	envVar   string
	provider string
}{
	{"GITHUB_ACTIONS", "github-actions"},
	{"GITLAB_CI", "gitlab-ci"},
	{"JENKINS_URL", "jenkins"},
	{"CIRCLECI", "circleci"},
	{"TRAVIS", "travis-ci"},
	{"BUILDKITE", "buildkite"},
	{"TF_BUILD", "azure-pipelines"},
	{"BITBUCKET_PIPELINE_UUID", "bitbucket"},
	{"CODEBUILD_BUILD_ID", "aws-codebuild"},
	{"TEAMCITY_VERSION", "teamcity"},
	{"DRONE", "drone"},
	{"HARNESS_BUILD_ID", "harness"},
	{"WOODPECKER", "woodpecker"},
	{"SEMAPHORE", "semaphore"},
	{"APPVEYOR", "appveyor"},
	// Bamboo and GoCD set no CI variable: each is known by its own.
	{"bamboo_planKey", "bamboo"},
	{"GO_PIPELINE_LABEL", "gocd"},
}

// ciRepoVars are the variables in which a CI provider names the repository
// its run builds, in the order enterprise's CLI reads them (its
// pkg/analytics/ci.go), so that an event's git_repo reads the same from either.
var ciRepoVars = []string{
	"GITHUB_REPOSITORY",                   // github-actions: owner/repo
	"CI_PROJECT_PATH",                     // gitlab-ci: group/subgroup/project
	"TRAVIS_REPO_SLUG",                    // travis-ci: owner/repo
	"CIRCLE_REPOSITORY_URL",               // circleci: the git URL
	"BUILDKITE_REPO",                      // buildkite: the git URL
	"BITBUCKET_REPO_FULL_NAME",            // bitbucket: owner/repo
	"BUILD_REPOSITORY_NAME",               // azure-pipelines: owner/repo of a GitHub repo, a bare name in Azure Repos
	"BUILD_REPOSITORY_URI",                // azure-pipelines: the git URL
	"DRONE_REPO",                          // drone: owner/repo
	"CI_REPO",                             // woodpecker: owner/repo
	"SEMAPHORE_GIT_REPO_SLUG",             // semaphore: owner/repo
	"HARNESS_REPO",                        // harness
	"APPVEYOR_REPO_NAME",                  // appveyor: owner/repo
	"bamboo_planRepository_repositoryUrl", // bamboo: the git URL
	"GIT_URL",                             // jenkins (its git plugin): the git URL
	"CODEBUILD_SOURCE_REPO_URL",           // aws-codebuild: the git URL
	// gocd names no repository in its variables.
}

// ciRepoURLVars are the ciRepoVars that hold a git URL. Only theirs may be a
// URL without its scheme, host/owner/repo; every other holds a slug, whose
// dotted first group is a GitLab group (acme.io/backend/api), not a host.
var ciRepoURLVars = map[string]bool{
	"CIRCLE_REPOSITORY_URL": true, "BUILDKITE_REPO": true, "BUILD_REPOSITORY_URI": true,
	"bamboo_planRepository_repositoryUrl": true, "GIT_URL": true, "CODEBUILD_SOURCE_REPO_URL": true,
}

// detectCI returns whether the process is running inside a CI environment
// and the name of the CI provider (empty string if unknown or not CI).
func detectCI() (bool, string) {
	for _, p := range ciProviders {
		if os.Getenv(p.envVar) != "" {
			return true, p.provider
		}
	}
	if ci := os.Getenv("CI"); ci != "" {
		// Woodpecker sets no WOODPECKER variable: every step has
		// CI=woodpecker, its system name, beside CI_REPO.
		if strings.EqualFold(strings.TrimSpace(ci), "woodpecker") {
			return true, "woodpecker"
		}
		return true, "unknown"
	}
	return false, ""
}

// gitRepoOnce caches the result of the (potentially slow) git command.
var gitRepoOnce struct {
	sync.Once
	repo string
}

// detectGitRepo returns the slug of the repository being worked on (e.g.
// "keploy/keploy"; a GitLab project keeps its groups). In CI it is the first
// of ciRepoVars whose value names one: a variable that names none (Azure
// Repos' bare BUILD_REPOSITORY_NAME) is read past, to the URL beside it.
// Otherwise it is read from the checkout's remote.origin.url. Returns "" if
// nothing names one. It never panics or blocks for more than 2 seconds.
func detectGitRepo() string {
	// 1. CI variables first (fast, always available in CI)
	for _, name := range ciRepoVars {
		if s := normalizeRepo(os.Getenv(name), ciRepoURLVars[name]); s != "" {
			return s
		}
	}

	// 2. Fall back to local .git (cached; runs git only once per process)
	gitRepoOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "git", "config", "--get", "remote.origin.url")
		cmd.Stderr = io.Discard
		out, err := cmd.Output()
		if err == nil {
			gitRepoOnce.repo = remoteRepo(string(out))
		}
	})
	return gitRepoOnce.repo
}

// remoteRepo is the slug a checkout's remote.origin.url names. Only a URL
// with a scheme or an scp-like [user@]host:owner/repo (no slash before the
// colon, and a user or a dotted host, so that a drive letter's C:/src is
// not one) names one: git reads any other value as a local path
// (/srv/git/shop, ../shop), which names none.
func remoteRepo(raw string) string {
	s := strings.TrimSpace(raw)
	if host, _, _ := strings.Cut(s, ":"); !strings.Contains(s, "://") &&
		(strings.Contains(host, "/") || !strings.ContainsAny(host, "@.")) {
		return ""
	}
	return normalizeRepo(s, true)
}

// normalizeRepo reads owner/repo from a slug ("owner/repo", a GitLab path
// "group/sub/project"), a URL ("https://host/owner/repo.git",
// "ssh://git@host/owner/repo"), an scp-like remote ("git@host:owner/repo.git",
// or "host:owner/repo" with no user) and, when url says the value is a git
// URL, one without its scheme ("github.com/owner/repo": exactly a dotted host,
// an owner and a repo). A slug keeps a dotted first group: a GitLab group may
// hold a dot (acme.io/backend/api), and x.y/keploy/z is not keploy's. A value
// with no owner, or with a space, an @ or a colon left in it, is "".
func normalizeRepo(raw string, url bool) string {
	s := strings.TrimSuffix(strings.TrimSpace(raw), ".git")
	if _, rest, ok := strings.Cut(s, "://"); ok {
		_, s, _ = strings.Cut(rest, "/") // the path after the host
	} else if host, rest, ok := strings.Cut(s, ":"); ok && !strings.Contains(host, "/") {
		s = rest // [user@]host:owner/repo
	} else if host, rest, ok := strings.Cut(s, "/"); ok && url && strings.Contains(host, ".") && strings.Count(rest, "/") == 1 {
		s = rest // host/owner/repo
	}
	s = strings.Trim(s, "/")
	if !strings.Contains(s, "/") || strings.ContainsAny(s, " \t@:") {
		return ""
	}
	return s
}
