package user

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"go.keploy.io/server/v3/pkg/platform/telemetry"
	"go.uber.org/zap"
)

// withMachineID makes the OS machine id raw (or err) for one test.
func withMachineID(t *testing.T, raw string, err error) {
	t.Helper()
	old := machineID
	machineID = func() (string, error) { return raw, err }
	t.Cleanup(func() { machineID = old })
}

func withHostname(t *testing.T, host string, err error) {
	t.Helper()
	old := hostnameFn
	hostnameFn = func() (string, error) { return host, err }
	t.Cleanup(func() { hostnameFn = old })
}

func withProcFiles(t *testing.T, files map[string]string) {
	t.Helper()
	old := readFileFn
	readFileFn = func(name string) ([]byte, error) {
		if v, ok := files[name]; ok {
			return []byte(v), nil
		}
		return nil, os.ErrNotExist
	}
	t.Cleanup(func() { readFileFn = old })
}

func withRandRead(t *testing.T, fn func([]byte) (int, error)) {
	t.Helper()
	old := randReadFn
	randReadFn = fn
	t.Cleanup(func() { randReadFn = old })
}

func withCommandLine(t *testing.T, args []string) {
	t.Helper()
	old := commandLine
	commandLine = func() []string { return args }
	t.Cleanup(func() { commandLine = old })
}

func resetProcessFallback(t *testing.T) {
	t.Helper()
	processFallbackMu.Lock()
	old := processFallbackID
	processFallbackID = ""
	processFallbackMu.Unlock()
	t.Cleanup(func() {
		processFallbackMu.Lock()
		processFallbackID = old
		processFallbackMu.Unlock()
	})
}

var allCIEnvVars = []string{
	"CI",
	"ACT",
	"GITEA_ACTIONS",
	"FORGEJO_ACTIONS",
	"CODESPACES",
	"GITHUB_ACTIONS",
	"GITHUB_SERVER_URL",
	"GITHUB_REPOSITORY",
	"RUNNER_NAME",
	"GITLAB_CI",
	"CI_SERVER_URL",
	"CI_PROJECT_PATH",
	"CI_RUNNER_ID",
	"JENKINS_URL",
	"JENKINS_HOME",
	"JOB_NAME",
	"BRANCH_NAME",
	"CIRCLECI",
	"CIRCLE_PROJECT_USERNAME",
	"CIRCLE_PROJECT_REPONAME",
	"TRAVIS",
	"TRAVIS_REPO_SLUG",
	"BUILDKITE",
	"BUILDKITE_ORGANIZATION_SLUG",
	"BUILDKITE_PIPELINE_SLUG",
	"BUILDKITE_AGENT_NAME",
	"TF_BUILD",
	"SYSTEM_COLLECTIONURI",
	"SYSTEM_TEAMPROJECTID",
	"BUILD_REPOSITORY_NAME",
	"BITBUCKET_PIPELINE_UUID",
	"BITBUCKET_BUILD_NUMBER",
	"BITBUCKET_REPO_FULL_NAME",
	"CODEBUILD_BUILD_ID",
	"CODEBUILD_BUILD_ARN",
	"CODEBUILD_INITIATOR",
	"DRONE",
	"DRONE_SYSTEM_HOST",
	"DRONE_REPO",
	"WOODPECKER",
	"CI_SYSTEM_HOST",
	"CI_REPO",
	"HARNESS_BUILD_ID",
	"HARNESS_ACCOUNT_ID",
	"SEMAPHORE",
	"SEMAPHORE_ORGANIZATION_URL",
	"SEMAPHORE_GIT_REPO_SLUG",
	"APPVEYOR",
	"APPVEYOR_ACCOUNT_NAME",
	"APPVEYOR_REPO_NAME",
	"KEPLOY_INSTALLATION_ID",
}

func clearFallbackCIEnv(t *testing.T) {
	t.Helper()
	for _, k := range allCIEnvVars {
		t.Setenv(k, "")
	}
	withCommandLine(t, []string{"keploy", "record"})
}

func clearHomeEnv(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", "")
	t.Setenv("HOMEDRIVE", "")
	t.Setenv("HOMEPATH", "")
	t.Setenv("USERPROFILE", "")
}

func setHomeEnv(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("HOME", dir)
	t.Setenv("HOMEDRIVE", "")
	t.Setenv("HOMEPATH", "")
	t.Setenv("USERPROFILE", dir)
}

func assertHex64(t *testing.T, id string) {
	t.Helper()
	if len(id) != 64 {
		t.Fatalf("installation id %q has length %d, want 64 hex chars", id, len(id))
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			t.Fatalf("installation id %q contains non-lowercase-hex char %q", id, c)
		}
	}
}

// The installation id is the keyed hash of the OS machine id, never the id:
// the expected value is HMAC-SHA256("keploy") keyed by the id, computed
// apart (Python's hmac), which is what machineid.ProtectedID("keploy") gives.
func TestGetInstallationIDSendsAKeyedHashOfTheMachineID(t *testing.T) {
	const raw = "3f2c9a1b7d4e4c0f9a6b2d8e1c5f7a90"
	const want = "31b6af4fa06650c85cf7e197dd0398d0e1dc51a027362a36d87dec51c5aa0ee6"
	const override = "keploy-operator-id-0123456789"
	for _, tc := range []struct {
		name       string
		inDocker   string
		raw        string
		err        error
		envID      string
		keployID   string
		want       string
		wantNoFile bool
	}{
		{name: "a machine id is sent hashed and writes nothing to keploy home", raw: raw, want: want, wantNoFile: true},
		{name: "a machine id with whitespace is trimmed before hashing", raw: "  " + raw + "\n", want: want, wantNoFile: true},
		{name: "a container keploy started sends the host's id as given", inDocker: "true", raw: "container-id", envID: want, want: want, wantNoFile: true},
		{name: "host ignores generic INSTALLATION_ID (e.g. GitHub App id)", inDocker: "", raw: raw, envID: "12345678901234567890", want: want, wantNoFile: true},
		{name: "KEPLOY_INDOCKER other than true ignores INSTALLATION_ID and reads machine", inDocker: "1", raw: raw, envID: want, want: want, wantNoFile: true},
		{name: "namespaced KEPLOY_INSTALLATION_ID is trimmed and honored anywhere", inDocker: "", raw: raw, keployID: "  " + override + " \n", want: override, wantNoFile: true},
		{name: "whitespace-only KEPLOY_INSTALLATION_ID is ignored", inDocker: "", raw: raw, keployID: " \t\n ", want: want, wantNoFile: true},
		{name: "unsafe KEPLOY_INSTALLATION_ID with quote is rejected", inDocker: "", raw: raw, keployID: "x'; id; echo '1234567890", want: want, wantNoFile: true},
		{name: "unsafe KEPLOY_INSTALLATION_ID with space is rejected", inDocker: "", raw: raw, keployID: "has space 1234567890", want: want, wantNoFile: true},
		{name: "unsafe KEPLOY_INSTALLATION_ID with dollar is rejected", inDocker: "", raw: raw, keployID: "has$var1234567890123", want: want, wantNoFile: true},
		{name: "unsafe KEPLOY_INSTALLATION_ID with cmd operator is rejected", inDocker: "", raw: raw, keployID: "cmd&whoami1234567890", want: want, wantNoFile: true},
		{name: "too-short KEPLOY_INSTALLATION_ID is rejected", inDocker: "", raw: raw, keployID: "short-id", want: want, wantNoFile: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			setHomeEnv(t, home)
			clearFallbackCIEnv(t)
			t.Setenv("KEPLOY_INDOCKER", tc.inDocker)
			t.Setenv("INSTALLATION_ID", tc.envID)
			t.Setenv("KEPLOY_INSTALLATION_ID", tc.keployID)
			withMachineID(t, tc.raw, tc.err)
			got, err := New(zap.NewNop(), nil).GetInstallationID(context.Background())
			if err != nil {
				t.Fatalf("GetInstallationID: %v", err)
			}
			if got != tc.want {
				t.Fatalf("GetInstallationID = %q, want %q", got, tc.want)
			}
			if tc.wantNoFile {
				if _, err := os.Stat(filepath.Join(home, ".keploy")); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("expected ~/.keploy not to be created, stat err = %v", err)
				}
			}
		})
	}
}

func TestGetInstallationIDNativeAgentFlag(t *testing.T) {
	const cliID = "1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef"
	flag := "--" + telemetry.AgentInstallationIDFlag

	t.Run("agent flag = form returns CLI id without writing to root home", func(t *testing.T) {
		home := t.TempDir()
		setHomeEnv(t, home)
		clearFallbackCIEnv(t)
		t.Setenv("KEPLOY_INDOCKER", "")
		t.Setenv("INSTALLATION_ID", "")
		withMachineID(t, "", errors.New("no machine id"))
		withCommandLine(t, []string{"keploy", "agent", "--port", "16789", flag + "=" + cliID})

		got, err := New(zap.NewNop(), nil).GetInstallationID(context.Background())
		if err != nil {
			t.Fatalf("GetInstallationID: %v", err)
		}
		if got != cliID {
			t.Fatalf("GetInstallationID = %q, want %q", got, cliID)
		}
		if _, err := os.Stat(filepath.Join(home, ".keploy")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("agent with %s must not create ~/.keploy, stat err = %v", flag, err)
		}
	})

	t.Run("agent flag space-separated form is honored", func(t *testing.T) {
		home := t.TempDir()
		setHomeEnv(t, home)
		clearFallbackCIEnv(t)
		withMachineID(t, "", errors.New("no machine id"))
		withCommandLine(t, []string{"keploy", "agent", flag, cliID, "--", flag + "=ignored1234567890"})

		got, err := New(zap.NewNop(), nil).GetInstallationID(context.Background())
		if err != nil {
			t.Fatalf("GetInstallationID: %v", err)
		}
		if got != cliID {
			t.Fatalf("GetInstallationID = %q, want %q", got, cliID)
		}
	})

	t.Run("non-agent subcommand ignores agent installation-id flag", func(t *testing.T) {
		const raw = "3f2c9a1b7d4e4c0f9a6b2d8e1c5f7a90"
		want := installationID(raw)
		setHomeEnv(t, t.TempDir())
		clearFallbackCIEnv(t)
		t.Setenv("KEPLOY_INDOCKER", "")
		withMachineID(t, raw, nil)
		withCommandLine(t, []string{"keploy", "record", flag + "=" + cliID})

		got, err := New(zap.NewNop(), nil).GetInstallationID(context.Background())
		if err != nil {
			t.Fatalf("GetInstallationID: %v", err)
		}
		if got != want {
			t.Fatalf("non-agent command honored agent flag: got %q, want %q", got, want)
		}
	})

	t.Run("unsafe agent installation-id flag is rejected", func(t *testing.T) {
		setHomeEnv(t, t.TempDir())
		clearFallbackCIEnv(t)
		t.Setenv("KEPLOY_INDOCKER", "true")
		t.Setenv("INSTALLATION_ID", "")
		withCommandLine(t, []string{"keploy", "agent", flag + "=x'; id; '1234567890"})

		got, err := New(zap.NewNop(), nil).GetInstallationID(context.Background())
		if err != nil {
			t.Fatalf("GetInstallationID: %v", err)
		}
		assertHex64(t, got)
	})
}

func TestGetInstallationIDRejectsUnsafeDockerEnvID(t *testing.T) {
	unsafeValues := []string{
		"x'; id; echo '1234567890",
		"has space 1234567890",
		"has$var1234567890123",
		"cmd&whoami1234567890",
		"line1\nline2-1234567890",
		"short",
	}
	for _, bad := range unsafeValues {
		t.Run(bad, func(t *testing.T) {
			setHomeEnv(t, t.TempDir())
			clearFallbackCIEnv(t)
			t.Setenv("KEPLOY_INDOCKER", "true")
			t.Setenv("INSTALLATION_ID", bad)

			got, err := New(zap.NewNop(), nil).GetInstallationID(context.Background())
			if err != nil {
				t.Fatalf("GetInstallationID: %v", err)
			}
			assertHex64(t, got)
			if got == strings.TrimSpace(bad) {
				t.Fatalf("unsafe INSTALLATION_ID %q was accepted", bad)
			}
		})
	}
}

// Two machines get two ids, and one machine the same id on every run.
func TestInstallationIDNamesEachMachineApart(t *testing.T) {
	a, b := installationID("machine-a"), installationID("machine-b")
	if a == b {
		t.Fatalf("two machine ids gave one installation id %q", a)
	}
	if again := installationID("machine-a"); again != a {
		t.Fatalf("one machine id gave %q, then %q", a, again)
	}
	assertHex64(t, a)
}

func TestGetInstallationIDCIStableAcrossEphemeralJobs(t *testing.T) {
	t.Run("same repo across fresh writable homes and ephemeral runners gets identical id", func(t *testing.T) {
		clearFallbackCIEnv(t)
		t.Setenv("KEPLOY_INDOCKER", "true")
		t.Setenv("INSTALLATION_ID", "")
		t.Setenv("GITHUB_ACTIONS", "true")
		t.Setenv("GITHUB_SERVER_URL", "https://github.com")
		t.Setenv("GITHUB_REPOSITORY", "acme/payments")

		// Job 1: fresh writable $HOME, unique runner/container/boot_id.
		home1 := t.TempDir()
		setHomeEnv(t, home1)
		t.Setenv("RUNNER_NAME", "GitHub Actions 101")
		withHostname(t, "runner-vm-101", nil)
		withProcFiles(t, map[string]string{
			procBootIDPath: "11111111-1111-1111-1111-111111111111\n",
			procCgroupPath: "12:pids:/docker/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n",
		})
		job1, err := New(zap.NewNop(), nil).GetInstallationID(context.Background())
		if err != nil {
			t.Fatalf("job1: %v", err)
		}
		assertHex64(t, job1)
		// CI derivation must not mint a random file in ephemeral $HOME.
		if _, err := os.Stat(filepath.Join(home1, ".keploy")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("CI derivation unexpectedly created ~/.keploy: %v", err)
		}

		// Job 2: brand-new writable $HOME, different runner/container/boot_id, same repo.
		home2 := t.TempDir()
		setHomeEnv(t, home2)
		t.Setenv("RUNNER_NAME", "GitHub Actions 202")
		withHostname(t, "runner-vm-202", nil)
		withProcFiles(t, map[string]string{
			procBootIDPath: "22222222-2222-2222-2222-222222222222\n",
			procCgroupPath: "12:pids:/docker/bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\n",
		})
		job2, err := New(zap.NewNop(), nil).GetInstallationID(context.Background())
		if err != nil {
			t.Fatalf("job2: %v", err)
		}
		if job2 != job1 {
			t.Fatalf("ephemeral CI jobs for same repo diverged: %q != %q", job2, job1)
		}

		// Same repo on a different GitHub Enterprise Server produces a distinct ID.
		t.Setenv("GITHUB_SERVER_URL", "https://ghe.internal.example")
		gheID, err := New(zap.NewNop(), nil).GetInstallationID(context.Background())
		if err != nil {
			t.Fatalf("gheID: %v", err)
		}
		assertHex64(t, gheID)
		if gheID == job1 {
			t.Fatalf("github.com and GitHub Enterprise Server collided on %q", gheID)
		}

		// Different repository produces a distinct deterministic ID.
		t.Setenv("GITHUB_SERVER_URL", "https://github.com")
		t.Setenv("GITHUB_REPOSITORY", "acme/billing")
		otherRepo, err := New(zap.NewNop(), nil).GetInstallationID(context.Background())
		if err != nil {
			t.Fatalf("otherRepo: %v", err)
		}
		assertHex64(t, otherRepo)
		if otherRepo == job1 {
			t.Fatalf("different CI repos collided on %q", otherRepo)
		}

		// Same slug on GitLab CI produces a distinct ID from GitHub Actions.
		t.Setenv("GITHUB_ACTIONS", "")
		t.Setenv("GITHUB_REPOSITORY", "")
		t.Setenv("GITLAB_CI", "true")
		t.Setenv("CI_SERVER_URL", "https://gitlab.com")
		t.Setenv("CI_PROJECT_PATH", "acme/payments")
		gitlabID, err := New(zap.NewNop(), nil).GetInstallationID(context.Background())
		if err != nil {
			t.Fatalf("gitlabID: %v", err)
		}
		assertHex64(t, gitlabID)
		if gitlabID == job1 {
			t.Fatalf("different CI providers with same slug collided on %q", gitlabID)
		}
	})

	t.Run("Codespaces without GITHUB_ACTIONS persists distinct per-user IDs", func(t *testing.T) {
		clearFallbackCIEnv(t)
		t.Setenv("KEPLOY_INDOCKER", "")
		t.Setenv("INSTALLATION_ID", "")
		t.Setenv("CODESPACES", "true")
		t.Setenv("GITHUB_REPOSITORY", "keploy/samples-go")
		withMachineID(t, "", errors.New("no machine id"))

		home1 := t.TempDir()
		setHomeEnv(t, home1)
		dev1, err := New(zap.NewNop(), nil).GetInstallationID(context.Background())
		if err != nil {
			t.Fatalf("dev1: %v", err)
		}
		assertHex64(t, dev1)

		home2 := t.TempDir()
		setHomeEnv(t, home2)
		dev2, err := New(zap.NewNop(), nil).GetInstallationID(context.Background())
		if err != nil {
			t.Fatalf("dev2: %v", err)
		}
		assertHex64(t, dev2)
		if dev1 == dev2 {
			t.Fatalf("two Codespaces users collapsed onto one shared ID %q", dev1)
		}
	})

	t.Run("non-CI repo vars without provider markers do not trigger CI derivation", func(t *testing.T) {
		for _, k := range []string{"GITHUB_REPOSITORY", "CI_PROJECT_PATH", "CI_REPO", "DRONE_REPO", "JOB_NAME", "BUILD_REPOSITORY_NAME"} {
			t.Run(k, func(t *testing.T) {
				clearFallbackCIEnv(t)
				t.Setenv(k, "acme/app")
				if id, ok := deriveDeterministicCIID(); ok {
					t.Fatalf("ungated %s triggered CI derivation: %q", k, id)
				}
			})
		}
		t.Run("local nektos/act emulator with ACT=true does not trigger CI derivation, while Gitea/Forgejo act_runner does", func(t *testing.T) {
			clearFallbackCIEnv(t)
			t.Setenv("GITHUB_ACTIONS", "true")
			t.Setenv("ACT", "true")
			t.Setenv("GITHUB_REPOSITORY", "acme/app")
			if id, ok := deriveDeterministicCIID(); ok {
				t.Fatalf("ACT=true local emulator triggered CI derivation: %q", id)
			}

			// Gitea/Forgejo act_runner sets ACT=true alongside GITEA_ACTIONS=true.
			t.Setenv("GITEA_ACTIONS", "true")
			t.Setenv("GITHUB_SERVER_URL", "https://gitea.acme.example")
			giteaID, ok := deriveDeterministicCIID()
			if !ok {
				t.Fatal("Gitea Actions (ACT=true + GITEA_ACTIONS=true) should derive a stable CI id")
			}
			assertHex64(t, giteaID)

			t.Setenv("GITEA_ACTIONS", "")
			t.Setenv("FORGEJO_ACTIONS", "true")
			forgejoID, ok := deriveDeterministicCIID()
			if !ok {
				t.Fatal("Forgejo Actions (ACT=true + FORGEJO_ACTIONS=true) should derive a stable CI id")
			}
			assertHex64(t, forgejoID)
		})
	})

	t.Run("Jenkins scopes JOB_NAME by normalized JENKINS_URL and strips multibranch BRANCH_NAME suffix", func(t *testing.T) {
		clearFallbackCIEnv(t)
		t.Setenv("JOB_NAME", "backend/nightly")
		t.Setenv("JENKINS_URL", "https://ci.acme.example/")
		id1, ok := deriveDeterministicCIID()
		if !ok {
			t.Fatal("expected Jenkins CI id")
		}
		assertHex64(t, id1)

		// Trailing slash and embedded credentials normalize to the same server scope.
		t.Setenv("JENKINS_URL", "https://user:secret@CI.ACME.EXAMPLE")
		id1Norm, ok := deriveDeterministicCIID()
		if !ok || id1Norm != id1 {
			t.Fatalf("normalized Jenkins URL diverged: %q != %q (ok=%v)", id1Norm, id1, ok)
		}

		// Different Jenkins server with same JOB_NAME produces a distinct ID.
		t.Setenv("JENKINS_URL", "https://ci.globex.example/")
		id2, ok := deriveDeterministicCIID()
		if !ok {
			t.Fatal("expected second Jenkins CI id")
		}
		if id2 == id1 {
			t.Fatalf("two Jenkins servers with same JOB_NAME collided on %q", id1)
		}

		// Multibranch pipeline: BRANCH_NAME causes trailing branch/PR segment
		// to be stripped so main, PR-7, and encoded feature branches share one ID.
		t.Setenv("JENKINS_URL", "https://ci.acme.example/")
		t.Setenv("JOB_NAME", "acme/payments/main")
		t.Setenv("BRANCH_NAME", "main")
		mbMain, ok := deriveDeterministicCIID()
		if !ok {
			t.Fatal("expected Jenkins multibranch id for main")
		}
		t.Setenv("JOB_NAME", "acme/payments/PR-7")
		t.Setenv("BRANCH_NAME", "PR-7")
		mbPR, ok := deriveDeterministicCIID()
		if !ok || mbPR != mbMain {
			t.Fatalf("Jenkins multibranch PR-7 diverged from main: %q != %q", mbPR, mbMain)
		}
		t.Setenv("JOB_NAME", "acme/payments/feature%2Ffoo")
		t.Setenv("BRANCH_NAME", "feature/foo")
		mbFeat, ok := deriveDeterministicCIID()
		if !ok || mbFeat != mbMain {
			t.Fatalf("Jenkins multibranch encoded branch diverged from main: %q != %q", mbFeat, mbMain)
		}

		// Freestyle job (no BRANCH_NAME) keeps its full JOB_NAME distinct from its parent folder.
		t.Setenv("BRANCH_NAME", "")
		t.Setenv("JOB_NAME", "acme/payments/nightly")
		freestyle, ok := deriveDeterministicCIID()
		if !ok || freestyle == mbMain {
			t.Fatalf("freestyle job unexpectedly collapsed onto multibranch parent: %q vs %q", freestyle, mbMain)
		}

		// Parameterized pipeline/freestyle jobs with user-defined BRANCH_NAME
		// that does not match JOB_NAME's trailing segment keep distinct IDs.
		t.Setenv("BRANCH_NAME", "main")
		t.Setenv("JOB_NAME", "qa/keploy-regression")
		paramA, ok := deriveDeterministicCIID()
		if !ok {
			t.Fatal("expected parameterized Jenkins job A id")
		}
		t.Setenv("JOB_NAME", "qa/keploy-smoke")
		paramB, ok := deriveDeterministicCIID()
		if !ok || paramB == paramA {
			t.Fatalf("parameterized Jenkins jobs in same folder collided: %q vs %q", paramA, paramB)
		}
	})

	t.Run("Drone uses DRONE_SYSTEM_HOST and Harness takes precedence over DRONE=true", func(t *testing.T) {
		clearFallbackCIEnv(t)
		t.Setenv("DRONE", "true")
		t.Setenv("DRONE_REPO", "platform/api")
		t.Setenv("DRONE_SYSTEM_HOST", "drone.acme.example")
		d1, ok := deriveDeterministicCIID()
		if !ok {
			t.Fatal("expected Drone CI id")
		}
		assertHex64(t, d1)

		t.Setenv("DRONE_SYSTEM_HOST", "drone.globex.example")
		d2, ok := deriveDeterministicCIID()
		if !ok || d2 == d1 {
			t.Fatalf("two Drone hosts with same DRONE_REPO collided: %q vs %q", d1, d2)
		}

		// Harness sets DRONE=true and DRONE_REPO alongside HARNESS_BUILD_ID + HARNESS_ACCOUNT_ID.
		clearFallbackCIEnv(t)
		t.Setenv("DRONE", "true")
		t.Setenv("DRONE_REPO", "platform/api")
		t.Setenv("DRONE_SYSTEM_HOST", "drone.acme.example")
		t.Setenv("HARNESS_BUILD_ID", "build-100")
		if id, ok := deriveDeterministicCIID(); ok {
			t.Fatalf("Harness without HARNESS_ACCOUNT_ID must not fall back to Drone or share ID: %q", id)
		}

		t.Setenv("HARNESS_ACCOUNT_ID", "acct-alpha")
		h1, ok := deriveDeterministicCIID()
		if !ok {
			t.Fatal("expected Harness CI id")
		}
		assertHex64(t, h1)
		if h1 == d1 {
			t.Fatalf("Harness run collided with Drone run of same repo: %q", h1)
		}

		t.Setenv("HARNESS_ACCOUNT_ID", "acct-beta")
		h2, ok := deriveDeterministicCIID()
		if !ok || h2 == h1 {
			t.Fatalf("two Harness accounts with same repo collided: %q vs %q", h1, h2)
		}
	})

	t.Run("CodeBuild derives project+region+account from BUILD_ID and ARN, ignoring initiator", func(t *testing.T) {
		clearFallbackCIEnv(t)
		t.Setenv("CODEBUILD_INITIATOR", "CodeBuild-Jenkins-Plugin")
		t.Setenv("CODEBUILD_BUILD_ID", "payments-api:1111-aaaa")
		t.Setenv("CODEBUILD_BUILD_ARN", "arn:aws:codebuild:us-east-1:111111111111:build/payments-api:1111-aaaa")
		id1, ok := deriveDeterministicCIID()
		if !ok {
			t.Fatal("expected CodeBuild CI id")
		}
		assertHex64(t, id1)

		// Same project in a new build run gets the exact same ID.
		t.Setenv("CODEBUILD_BUILD_ID", "payments-api:2222-bbbb")
		t.Setenv("CODEBUILD_BUILD_ARN", "arn:aws:codebuild:us-east-1:111111111111:build/payments-api:2222-bbbb")
		id1Again, ok := deriveDeterministicCIID()
		if !ok || id1Again != id1 {
			t.Fatalf("same CodeBuild project across builds diverged: %q != %q", id1Again, id1)
		}

		// Different project with the same initiator gets a distinct ID.
		t.Setenv("CODEBUILD_BUILD_ID", "billing-api:3333-cccc")
		t.Setenv("CODEBUILD_BUILD_ARN", "arn:aws:codebuild:us-east-1:111111111111:build/billing-api:3333-cccc")
		idOtherProj, ok := deriveDeterministicCIID()
		if !ok || idOtherProj == id1 {
			t.Fatalf("different CodeBuild projects collided: %q vs %q (ok=%v)", idOtherProj, id1, ok)
		}

		// Same project name in a different AWS account gets a distinct ID.
		t.Setenv("CODEBUILD_BUILD_ID", "payments-api:4444-dddd")
		t.Setenv("CODEBUILD_BUILD_ARN", "arn:aws:codebuild:us-east-1:222222222222:build/payments-api:4444-dddd")
		idOtherAcct, ok := deriveDeterministicCIID()
		if !ok || idOtherAcct == id1 {
			t.Fatalf("same CodeBuild project name in different AWS accounts collided: %q vs %q", idOtherAcct, id1)
		}
	})

	t.Run("Buildkite requires org slug and Azure requires collection/project scope for bare repo names", func(t *testing.T) {
		clearFallbackCIEnv(t)
		t.Setenv("BUILDKITE", "true")
		t.Setenv("BUILDKITE_PIPELINE_SLUG", "backend")
		if id, ok := deriveDeterministicCIID(); ok {
			t.Fatalf("Buildkite without org slug should not derive shared CI id: %q", id)
		}
		t.Setenv("BUILDKITE_ORGANIZATION_SLUG", "acme")
		bk1, ok := deriveDeterministicCIID()
		if !ok {
			t.Fatal("expected Buildkite id with org slug")
		}
		t.Setenv("BUILDKITE_ORGANIZATION_SLUG", "globex")
		bk2, ok := deriveDeterministicCIID()
		if !ok || bk2 == bk1 {
			t.Fatalf("Buildkite pipelines across orgs collided: %q vs %q", bk1, bk2)
		}

		clearFallbackCIEnv(t)
		t.Setenv("TF_BUILD", "True")
		t.Setenv("BUILD_REPOSITORY_NAME", "api")
		if id, ok := deriveDeterministicCIID(); ok {
			t.Fatalf("bare Azure repo name without org/project scope should not derive shared CI id: %q", id)
		}
		t.Setenv("SYSTEM_COLLECTIONURI", "https://dev.azure.com/acme/")
		t.Setenv("SYSTEM_TEAMPROJECTID", "proj-1")
		az1, ok := deriveDeterministicCIID()
		if !ok {
			t.Fatal("expected Azure Pipelines id with org+project scope")
		}
		t.Setenv("SYSTEM_COLLECTIONURI", "https://dev.azure.com/globex/")
		az2, ok := deriveDeterministicCIID()
		if !ok || az2 == az1 {
			t.Fatalf("Azure Pipelines across orgs collided: %q vs %q", az1, az2)
		}
	})

	t.Run("preexisting persisted id in cached keploy home takes precedence over CI derivation", func(t *testing.T) {
		home := t.TempDir()
		setHomeEnv(t, home)
		clearFallbackCIEnv(t)
		t.Setenv("KEPLOY_INDOCKER", "true")
		t.Setenv("INSTALLATION_ID", "")
		t.Setenv("GITHUB_ACTIONS", "true")
		t.Setenv("GITHUB_REPOSITORY", "acme/payments")

		const cached = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
		keployDir := filepath.Join(home, ".keploy")
		if err := os.MkdirAll(keployDir, 0o700); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(filepath.Join(keployDir, installationIDFileName), []byte(cached+"\n"), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		got, err := New(zap.NewNop(), nil).GetInstallationID(context.Background())
		if err != nil {
			t.Fatalf("GetInstallationID: %v", err)
		}
		if got != cached {
			t.Fatalf("GetInstallationID = %q, want cached %q", got, cached)
		}
	})
}

func TestGetInstallationIDPersistsInKeployHome(t *testing.T) {
	cases := []struct {
		name     string
		inDocker string
		raw      string
		err      error
	}{
		{name: "docker skips machine-id and persists", inDocker: "true", raw: "baked-image-machine-id"},
		{name: "missing machine-id error persists", raw: "", err: errors.New("no machine id")},
		{name: "empty machine-id persists", raw: ""},
		{name: "uninitialized machine-id persists", raw: "uninitialized\n"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			setHomeEnv(t, home)
			clearFallbackCIEnv(t)
			t.Setenv("KEPLOY_INDOCKER", tc.inDocker)
			t.Setenv("INSTALLATION_ID", "")
			withMachineID(t, tc.raw, tc.err)

			// Exercise nil-logger safety on at least one path as well.
			db := New(nil, nil)
			first, err := db.GetInstallationID(context.Background())
			if err != nil {
				t.Fatalf("first GetInstallationID: %v", err)
			}
			assertHex64(t, first)
			if tc.raw != "" && first == installationID(strings.TrimSpace(tc.raw)) {
				t.Fatalf("docker/uninitialized path must not hash raw machine-id %q", tc.raw)
			}

			// File must exist at ~/.keploy/installation-id with 0600 perms and no orphan temp files.
			idPath := filepath.Join(home, ".keploy", installationIDFileName)
			info, err := os.Stat(idPath)
			if err != nil {
				t.Fatalf("expected persisted file %s: %v", idPath, err)
			}
			if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
				t.Fatalf("persisted file mode = %o, want 0600", info.Mode().Perm())
			}
			// Must NOT touch enterprise's installation-id.yaml marker.
			if _, err := os.Stat(filepath.Join(home, ".keploy", "installation-id.yaml")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("unexpected installation-id.yaml created: %v", err)
			}
			entries, err := os.ReadDir(filepath.Join(home, ".keploy"))
			if err != nil {
				t.Fatalf("ReadDir: %v", err)
			}
			if len(entries) != 1 || entries[0].Name() != installationIDFileName {
				t.Fatalf("unexpected files in ~/.keploy: %v", entries)
			}

			// Second call must return the exact same persisted ID.
			second, err := New(zap.NewNop(), nil).GetInstallationID(context.Background())
			if err != nil {
				t.Fatalf("second GetInstallationID: %v", err)
			}
			if second != first {
				t.Fatalf("persisted id changed between calls: %q != %q", second, first)
			}
		})
	}
}

func TestGetInstallationIDReusesPreexistingAndReplacesCorruptFile(t *testing.T) {
	t.Run("reuses valid preexisting persisted id", func(t *testing.T) {
		home := t.TempDir()
		setHomeEnv(t, home)
		clearFallbackCIEnv(t)
		t.Setenv("KEPLOY_INDOCKER", "true")
		t.Setenv("INSTALLATION_ID", "")

		keployDir := filepath.Join(home, ".keploy")
		if err := os.MkdirAll(keployDir, 0o700); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		const existing = "550e8400-e29b-41d4-a716-446655440000"
		if err := os.WriteFile(filepath.Join(keployDir, installationIDFileName), []byte("  "+existing+"\n"), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		got, err := New(zap.NewNop(), nil).GetInstallationID(context.Background())
		if err != nil {
			t.Fatalf("GetInstallationID: %v", err)
		}
		if got != existing {
			t.Fatalf("GetInstallationID = %q, want %q", got, existing)
		}
	})

	corruptCases := []struct {
		name    string
		content []byte
	}{
		{name: "empty file", content: nil},
		{name: "whitespace only", content: []byte("  \n\t ")},
		{name: "too short", content: []byte("short")},
		{name: "contains newline inside", content: []byte("abcdef0123456789\nabcdef0123456789")},
		{name: "binary garbage", content: []byte("\x00\xff\xfe0123456789abcdef")},
		{name: "oversized file", content: []byte(strings.Repeat("a", maxPersistedIDBytes+32))},
	}

	for _, tc := range corruptCases {
		t.Run("replaces "+tc.name, func(t *testing.T) {
			home := t.TempDir()
			setHomeEnv(t, home)
			clearFallbackCIEnv(t)
			t.Setenv("KEPLOY_INDOCKER", "true")
			t.Setenv("INSTALLATION_ID", "")

			keployDir := filepath.Join(home, ".keploy")
			if err := os.MkdirAll(keployDir, 0o700); err != nil {
				t.Fatalf("MkdirAll: %v", err)
			}
			idPath := filepath.Join(keployDir, installationIDFileName)
			if err := os.WriteFile(idPath, tc.content, 0o644); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}

			first, err := New(zap.NewNop(), nil).GetInstallationID(context.Background())
			if err != nil {
				t.Fatalf("GetInstallationID: %v", err)
			}
			assertHex64(t, first)

			if runtime.GOOS != "windows" {
				info, err := os.Stat(idPath)
				if err != nil {
					t.Fatalf("Stat: %v", err)
				}
				if info.Mode().Perm() != 0o600 {
					t.Fatalf("replaced file mode = %o, want 0600", info.Mode().Perm())
				}
			}

			second, err := New(zap.NewNop(), nil).GetInstallationID(context.Background())
			if err != nil {
				t.Fatalf("second GetInstallationID: %v", err)
			}
			if second != first {
				t.Fatalf("id not stable after replacing corrupt file: %q != %q", second, first)
			}
		})
	}

	t.Run("corrupt recovery detects winner replaced before remove via SameFile", func(t *testing.T) {
		home := t.TempDir()
		setHomeEnv(t, home)
		clearFallbackCIEnv(t)
		t.Setenv("KEPLOY_INDOCKER", "true")
		t.Setenv("INSTALLATION_ID", "")

		keployDir := filepath.Join(home, ".keploy")
		if err := os.MkdirAll(keployDir, 0o700); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		idPath := filepath.Join(keployDir, installationIDFileName)
		if err := os.WriteFile(idPath, []byte("corrupt"), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		const winnerID = "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
		oldPre := preRemoveCorruptFn
		preRemoveCorruptFn = func() {
			// Simulate process A atomically replacing the corrupt file with a new inode
			// while process B was writing and fsyncing its temp file.
			winnerTmp := filepath.Join(keployDir, ".winner-tmp")
			_ = os.WriteFile(winnerTmp, []byte(winnerID+"\n"), 0o600)
			_ = os.Rename(winnerTmp, idPath)
		}
		t.Cleanup(func() { preRemoveCorruptFn = oldPre })

		got, err := New(zap.NewNop(), nil).GetInstallationID(context.Background())
		if err != nil {
			t.Fatalf("GetInstallationID: %v", err)
		}
		if got != winnerID {
			t.Fatalf("GetInstallationID = %q, want pre-remove winner %q", got, winnerID)
		}
		onDisk, err := os.ReadFile(idPath)
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		if strings.TrimSpace(string(onDisk)) != winnerID {
			t.Fatalf("on-disk winner was clobbered: got %q, want %q", strings.TrimSpace(string(onDisk)), winnerID)
		}
	})

	t.Run("corrupt recovery coordinates via hard link without clobbering concurrent winner", func(t *testing.T) {
		home := t.TempDir()
		setHomeEnv(t, home)
		clearFallbackCIEnv(t)
		t.Setenv("KEPLOY_INDOCKER", "true")
		t.Setenv("INSTALLATION_ID", "")

		keployDir := filepath.Join(home, ".keploy")
		if err := os.MkdirAll(keployDir, 0o700); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		idPath := filepath.Join(keployDir, installationIDFileName)
		if err := os.WriteFile(idPath, []byte("corrupt"), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		const winnerID = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
		oldLink := osLinkFn
		osLinkFn = func(_, newname string) error {
			// Another process beat us to replacing the corrupt file.
			if err := os.WriteFile(newname, []byte(winnerID+"\n"), 0o600); err != nil {
				return err
			}
			return os.ErrExist
		}
		t.Cleanup(func() { osLinkFn = oldLink })

		got, err := New(zap.NewNop(), nil).GetInstallationID(context.Background())
		if err != nil {
			t.Fatalf("GetInstallationID: %v", err)
		}
		if got != winnerID {
			t.Fatalf("GetInstallationID = %q, want concurrent recovery winner %q", got, winnerID)
		}
	})
}

func TestGetInstallationIDRejectsSymlinkAndRandFailureInPersist(t *testing.T) {
	t.Run("symlink at installation-id is not followed or overwritten", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("symlink creation requires elevated privileges on Windows")
		}
		home := t.TempDir()
		setHomeEnv(t, home)
		clearFallbackCIEnv(t)
		t.Setenv("KEPLOY_INDOCKER", "true")
		t.Setenv("INSTALLATION_ID", "")
		withHostname(t, "symlink-guard-host", nil)
		withProcFiles(t, nil)

		keployDir := filepath.Join(home, ".keploy")
		if err := os.MkdirAll(keployDir, 0o700); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		target := filepath.Join(home, "outside-target")
		const targetID = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
		if err := os.WriteFile(target, []byte(targetID+"\n"), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		linkPath := filepath.Join(keployDir, installationIDFileName)
		if err := os.Symlink(target, linkPath); err != nil {
			t.Fatalf("Symlink: %v", err)
		}

		got, err := New(zap.NewNop(), nil).GetInstallationID(context.Background())
		if err != nil {
			t.Fatalf("GetInstallationID: %v", err)
		}
		assertHex64(t, got)
		if got == targetID {
			t.Fatalf("GetInstallationID followed symlink and returned target id %q", got)
		}
		rawTarget, err := os.ReadFile(target)
		if err != nil {
			t.Fatalf("ReadFile target: %v", err)
		}
		if strings.TrimSpace(string(rawTarget)) != targetID {
			t.Fatalf("symlink target was modified: %q", string(rawTarget))
		}
	})

	t.Run("rand failure in persist step falls through without creating file", func(t *testing.T) {
		home := t.TempDir()
		setHomeEnv(t, home)
		clearFallbackCIEnv(t)
		t.Setenv("KEPLOY_INDOCKER", "true")
		t.Setenv("INSTALLATION_ID", "")
		withHostname(t, "rand-fail-host-1", nil)
		withProcFiles(t, nil)
		withRandRead(t, func([]byte) (int, error) { return 0, errors.New("entropy failure") })

		got, err := New(zap.NewNop(), nil).GetInstallationID(context.Background())
		if err != nil {
			t.Fatalf("GetInstallationID: %v", err)
		}
		assertHex64(t, got)
		if _, err := os.Stat(filepath.Join(home, ".keploy", installationIDFileName)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("expected no persisted file on rand failure, stat err = %v", err)
		}
	})
}

func TestGetInstallationIDConcurrentPersistIsRaceFreeAndConsistent(t *testing.T) {
	home := t.TempDir()
	setHomeEnv(t, home)
	clearFallbackCIEnv(t)
	t.Setenv("KEPLOY_INDOCKER", "true")
	t.Setenv("INSTALLATION_ID", "")

	const workers = 16
	results := make([]string, workers)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			id, err := New(zap.NewNop(), nil).GetInstallationID(context.Background())
			if err != nil {
				t.Errorf("worker %d error: %v", idx, err)
				return
			}
			results[idx] = id
		}(i)
	}
	close(start)
	wg.Wait()

	assertHex64(t, results[0])
	for i := 1; i < workers; i++ {
		if results[i] != results[0] {
			t.Fatalf("worker %d got %q, want %q", i, results[i], results[0])
		}
	}
}

func TestGetInstallationIDCrossProcessLinkRaceAndFallbackRename(t *testing.T) {
	t.Run("returns winner id when another process wins os.Link race", func(t *testing.T) {
		home := t.TempDir()
		setHomeEnv(t, home)
		clearFallbackCIEnv(t)
		t.Setenv("KEPLOY_INDOCKER", "true")
		t.Setenv("INSTALLATION_ID", "")

		const winnerID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		oldLink := osLinkFn
		osLinkFn = func(_, newname string) error {
			// Simulate another process creating the file right before our link.
			if err := os.WriteFile(newname, []byte(winnerID+"\n"), 0o600); err != nil {
				return err
			}
			return os.ErrExist
		}
		t.Cleanup(func() { osLinkFn = oldLink })

		got, err := New(zap.NewNop(), nil).GetInstallationID(context.Background())
		if err != nil {
			t.Fatalf("GetInstallationID: %v", err)
		}
		if got != winnerID {
			t.Fatalf("GetInstallationID = %q, want winner %q", got, winnerID)
		}
		entries, err := os.ReadDir(filepath.Join(home, ".keploy"))
		if err != nil {
			t.Fatalf("ReadDir: %v", err)
		}
		if len(entries) != 1 || entries[0].Name() != installationIDFileName {
			t.Fatalf("orphan temp file left after link race: %v", entries)
		}
	})

	t.Run("falls back to atomic rename when filesystem rejects hard links", func(t *testing.T) {
		home := t.TempDir()
		setHomeEnv(t, home)
		clearFallbackCIEnv(t)
		t.Setenv("KEPLOY_INDOCKER", "true")
		t.Setenv("INSTALLATION_ID", "")

		oldLink := osLinkFn
		osLinkFn = func(_, _ string) error {
			return errors.New("operation not supported")
		}
		t.Cleanup(func() { osLinkFn = oldLink })

		first, err := New(zap.NewNop(), nil).GetInstallationID(context.Background())
		if err != nil {
			t.Fatalf("first GetInstallationID: %v", err)
		}
		assertHex64(t, first)

		idPath := filepath.Join(home, ".keploy", installationIDFileName)
		persisted, err := os.ReadFile(idPath)
		if err != nil {
			t.Fatalf("expected persisted file %s after rename fallback: %v", idPath, err)
		}
		if strings.TrimSpace(string(persisted)) != first {
			t.Fatalf("persisted file = %q, want %q", strings.TrimSpace(string(persisted)), first)
		}

		second, err := New(zap.NewNop(), nil).GetInstallationID(context.Background())
		if err != nil {
			t.Fatalf("second GetInstallationID: %v", err)
		}
		if second != first {
			t.Fatalf("id not persisted after rename fallback: %q != %q", second, first)
		}
	})

	t.Run("read-only keploy dir with preexisting valid id still returns it", func(t *testing.T) {
		if runtime.GOOS == "windows" || os.Geteuid() == 0 {
			t.Skip("chmod read-only dir test requires non-root unix")
		}
		home := t.TempDir()
		setHomeEnv(t, home)
		clearFallbackCIEnv(t)
		t.Setenv("KEPLOY_INDOCKER", "true")
		t.Setenv("INSTALLATION_ID", "")

		keployDir := filepath.Join(home, ".keploy")
		if err := os.MkdirAll(keployDir, 0o700); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		const seeded = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		if err := os.WriteFile(filepath.Join(keployDir, installationIDFileName), []byte(seeded+"\n"), 0o400); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		if err := os.Chmod(keployDir, 0o500); err != nil {
			t.Fatalf("Chmod: %v", err)
		}
		t.Cleanup(func() { _ = os.Chmod(keployDir, 0o700) })

		got, err := New(zap.NewNop(), nil).GetInstallationID(context.Background())
		if err != nil {
			t.Fatalf("GetInstallationID: %v", err)
		}
		if got != seeded {
			t.Fatalf("GetInstallationID = %q, want %q", got, seeded)
		}
	})

	t.Run("read-only keploy dir without id file falls through to deterministic hash", func(t *testing.T) {
		if runtime.GOOS == "windows" || os.Geteuid() == 0 {
			t.Skip("chmod read-only dir test requires non-root unix")
		}
		home := t.TempDir()
		setHomeEnv(t, home)
		clearFallbackCIEnv(t)
		t.Setenv("KEPLOY_INDOCKER", "true")
		t.Setenv("INSTALLATION_ID", "")
		withHostname(t, "ro-dir-host", nil)
		withProcFiles(t, nil)

		keployDir := filepath.Join(home, ".keploy")
		if err := os.MkdirAll(keployDir, 0o500); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		t.Cleanup(func() { _ = os.Chmod(keployDir, 0o700) })

		first, err := New(zap.NewNop(), nil).GetInstallationID(context.Background())
		if err != nil {
			t.Fatalf("GetInstallationID: %v", err)
		}
		assertHex64(t, first)
		second, err := New(zap.NewNop(), nil).GetInstallationID(context.Background())
		if err != nil {
			t.Fatalf("second GetInstallationID: %v", err)
		}
		if second != first {
			t.Fatalf("ro-dir fallback not deterministic: %q != %q", second, first)
		}
	})
}

func TestGetInstallationIDDeterministicFallbackWhenUnwritable(t *testing.T) {
	const cidA = "8c4f1e2a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6"
	const cidB = "111122223333444455556666777788889999aaaabbbbccccddddeeeeffff0000"

	t.Run("missing HOME derives stable container hash across host reboot", func(t *testing.T) {
		clearHomeEnv(t)
		clearFallbackCIEnv(t)
		t.Setenv("KEPLOY_INDOCKER", "true")
		t.Setenv("INSTALLATION_ID", "")
		withHostname(t, "app-container-42", nil)
		withProcFiles(t, map[string]string{
			procBootIDPath: "2a9d6a12-7c90-4a1a-9f88-112233445566\n",
			procCgroupPath: "12:pids:/docker/" + cidA + "\n",
		})

		first, err := New(zap.NewNop(), nil).GetInstallationID(context.Background())
		if err != nil {
			t.Fatalf("first GetInstallationID: %v", err)
		}
		second, err := New(zap.NewNop(), nil).GetInstallationID(context.Background())
		if err != nil {
			t.Fatalf("second GetInstallationID: %v", err)
		}
		assertHex64(t, first)
		if first != second {
			t.Fatalf("fallback id not deterministic: %q != %q", first, second)
		}
		if first == cidA {
			t.Fatalf("fallback id must hash container id, not return raw %q", cidA)
		}

		// Host reboot (boot_id change) must NOT churn a container that already
		// has a stable container_id and non-generic hostname.
		withProcFiles(t, map[string]string{
			procBootIDPath: "99999999-7c90-4a1a-9f88-112233445566\n",
			procCgroupPath: "12:pids:/docker/" + cidA + "\n",
		})
		afterReboot, err := New(zap.NewNop(), nil).GetInstallationID(context.Background())
		if err != nil {
			t.Fatalf("afterReboot GetInstallationID: %v", err)
		}
		if afterReboot != first {
			t.Fatalf("container id changed across host boot_id change: %q != %q", afterReboot, first)
		}

		// Changing container id produces a different deterministic hash.
		withProcFiles(t, map[string]string{
			procBootIDPath: "2a9d6a12-7c90-4a1a-9f88-112233445566\n",
			procCgroupPath: "12:pids:/docker/" + cidB + "\n",
		})
		otherContainer, err := New(zap.NewNop(), nil).GetInstallationID(context.Background())
		if err != nil {
			t.Fatalf("otherContainer GetInstallationID: %v", err)
		}
		assertHex64(t, otherContainer)
		if otherContainer == first {
			t.Fatalf("different container ids produced identical fallback id %q", otherContainer)
		}
	})

	t.Run("boot_id used when hostname is generic localhost and no container id exists", func(t *testing.T) {
		clearHomeEnv(t)
		clearFallbackCIEnv(t)
		t.Setenv("KEPLOY_INDOCKER", "true")
		t.Setenv("INSTALLATION_ID", "")
		withHostname(t, "localhost", nil)
		withProcFiles(t, map[string]string{
			procBootIDPath: "2a9d6a12-7c90-4a1a-9f88-112233445566\n",
		})

		first, err := New(zap.NewNop(), nil).GetInstallationID(context.Background())
		if err != nil {
			t.Fatalf("first: %v", err)
		}
		assertHex64(t, first)

		withProcFiles(t, map[string]string{
			procBootIDPath: "99999999-7c90-4a1a-9f88-112233445566\n",
		})
		otherBoot, err := New(zap.NewNop(), nil).GetInstallationID(context.Background())
		if err != nil {
			t.Fatalf("otherBoot: %v", err)
		}
		assertHex64(t, otherBoot)
		if otherBoot == first {
			t.Fatalf("different boot_ids on bare localhost produced identical hash %q", otherBoot)
		}
	})

	t.Run("unwritable HOME (regular file) falls through to deterministic hash", func(t *testing.T) {
		// Pointing HOME at a regular file fails MkdirAll on every OS (including root in Docker).
		blocker := filepath.Join(t.TempDir(), "not-a-dir")
		if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		setHomeEnv(t, blocker)
		clearFallbackCIEnv(t)
		t.Setenv("KEPLOY_INDOCKER", "true")
		t.Setenv("INSTALLATION_ID", "")
		withHostname(t, "readonly-pod-7", nil)
		withProcFiles(t, nil)

		a, err := New(zap.NewNop(), nil).GetInstallationID(context.Background())
		if err != nil {
			t.Fatalf("GetInstallationID: %v", err)
		}
		b, err := New(zap.NewNop(), nil).GetInstallationID(context.Background())
		if err != nil {
			t.Fatalf("second GetInstallationID: %v", err)
		}
		assertHex64(t, a)
		if a != b {
			t.Fatalf("unwritable HOME fallback not stable: %q != %q", a, b)
		}
	})

	t.Run("installation-id path is a directory falls through to deterministic hash", func(t *testing.T) {
		home := t.TempDir()
		setHomeEnv(t, home)
		clearFallbackCIEnv(t)
		t.Setenv("KEPLOY_INDOCKER", "true")
		t.Setenv("INSTALLATION_ID", "")
		if err := os.MkdirAll(filepath.Join(home, ".keploy", installationIDFileName), 0o700); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		withHostname(t, "dir-collision-host", nil)
		withProcFiles(t, nil)

		got, err := New(zap.NewNop(), nil).GetInstallationID(context.Background())
		if err != nil {
			t.Fatalf("GetInstallationID: %v", err)
		}
		assertHex64(t, got)
	})

	t.Run("cgroup v2 falls through to mountinfo container id and pod UID", func(t *testing.T) {
		clearHomeEnv(t)
		clearFallbackCIEnv(t)
		t.Setenv("KEPLOY_INDOCKER", "true")
		t.Setenv("INSTALLATION_ID", "")
		withHostname(t, "localhost", nil)

		withProcFiles(t, map[string]string{
			procCgroupPath:    "0::/\n",
			procMountInfoPath: "610 597 259:2 /var/lib/docker/containers/" + cidA + "/hostname /etc/hostname rw - ext4 /dev/sda1 rw\n",
		})
		fromContainer, err := New(zap.NewNop(), nil).GetInstallationID(context.Background())
		if err != nil {
			t.Fatalf("GetInstallationID: %v", err)
		}
		assertHex64(t, fromContainer)

		withProcFiles(t, map[string]string{
			procCgroupPath:    "0::/\n",
			procMountInfoPath: "611 597 259:2 /var/lib/kubelet/pods/550e8400-e29b-41d4-a716-446655440000/etc-hosts /etc/hosts rw - ext4 /dev/sda1 rw\n",
		})
		fromPod, err := New(zap.NewNop(), nil).GetInstallationID(context.Background())
		if err != nil {
			t.Fatalf("GetInstallationID pod: %v", err)
		}
		assertHex64(t, fromPod)
		if fromPod == fromContainer {
			t.Fatalf("pod UID and container ID produced identical hash %q", fromPod)
		}
	})
}

func TestExtractContainerIDEdgeCases(t *testing.T) {
	const cid = "8c4f1e2a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6"

	if got := extractContainerIDFromCgroup("1:name=systemd:/kubepods/burstable/pod1/cri-containerd-" + cid + ".scope\n"); got != cid {
		t.Fatalf("cgroup k8s = %q, want %q", got, cid)
	}
	if got := extractContainerIDFromCgroup("0::/user.slice/user-1000.slice\n"); got != "" {
		t.Fatalf("bare-metal cgroup = %q, want empty", got)
	}
	// A 128-char hex string must not be sliced as a 64-char container ID.
	if got := extractHex64Token("/docker/" + cid + cid); got != "" {
		t.Fatalf("128-hex token falsely matched as %q", got)
	}
	if got := extractContainerIDFromMountinfo("10 1 0:1 / / rw - ext4 /dev/sda1 rw\n"); got != "" {
		t.Fatalf("bare-metal mountinfo = %q, want empty", got)
	}
	// A Docker/Kubernetes host's mountinfo lists OTHER containers' mounts; only
	// /etc/hostname, /etc/hosts, and /etc/resolv.conf belong to this container.
	hostOtherContainer := "900 28 0:55 /var/lib/docker/containers/" + cid + "/mounts/shm /var/lib/docker/containers/" + cid + "/mounts/shm rw,nosuid - tmpfs shm rw\n"
	if got := extractContainerIDFromMountinfo(hostOtherContainer); got != "" {
		t.Fatalf("host mountinfo matched another container's shm mount: %q", got)
	}
	hostOtherPod := "901 28 0:56 /var/lib/kubelet/pods/550e8400-e29b-41d4-a716-446655440000/volumes/empty-dir /var/lib/kubelet/pods/550e8400-e29b-41d4-a716-446655440000/volumes/empty-dir rw - tmpfs tmpfs rw\n"
	if got := extractContainerIDFromMountinfo(hostOtherPod); got != "" {
		t.Fatalf("host mountinfo matched another pod's volume mount: %q", got)
	}
	// /etc/resolv.conf bind mount inside a container IS matched.
	resolvMount := "612 597 259:2 /var/lib/docker/containers/" + cid + "/resolv.conf /etc/resolv.conf rw - ext4 /dev/sda1 rw\n"
	if got := extractContainerIDFromMountinfo(resolvMount); got != cid {
		t.Fatalf("resolv.conf container mount = %q, want %q", got, cid)
	}
}

func TestResolveHomeWindowsEdgeCases(t *testing.T) {
	envFrom := func(m map[string]string) func(string) string {
		return func(k string) string { return m[k] }
	}
	if got := resolveHome("windows", envFrom(map[string]string{
		"HOMEDRIVE":   "C:",
		"HOMEPATH":    "",
		"USERPROFILE": `C:\Users\alice`,
	})); got != `C:\Users\alice` {
		t.Fatalf("HOMEDRIVE without HOMEPATH = %q, want USERPROFILE fallback", got)
	}
	if got := resolveHome("windows", envFrom(map[string]string{
		"HOMEDRIVE":   "C:",
		"HOMEPATH":    `\Users\bob`,
		"USERPROFILE": `C:\Users\alice`,
	})); got != `C:\Users\bob` {
		t.Fatalf("HOMEDRIVE+HOMEPATH = %q, want C:\\Users\\bob", got)
	}
	if got := resolveHome("linux", envFrom(map[string]string{"HOME": "/home/alice"})); got != "/home/alice" {
		t.Fatalf("linux HOME = %q, want /home/alice", got)
	}
}

func TestGetInstallationIDLastResortNeverEmptyAndStableInProcess(t *testing.T) {
	t.Run("zero signals uses process-cached random id", func(t *testing.T) {
		resetProcessFallback(t)
		clearHomeEnv(t)
		clearFallbackCIEnv(t)
		t.Setenv("KEPLOY_INDOCKER", "true")
		t.Setenv("INSTALLATION_ID", "")
		withHostname(t, "localhost", nil) // generic hostname alone is not distinguishing
		withProcFiles(t, nil)

		if id, ok := deriveDeterministicFallbackID(); ok {
			t.Fatalf("generic localhost alone must not produce a shared deterministic hash, got %q", id)
		}

		first, err := New(zap.NewNop(), nil).GetInstallationID(context.Background())
		if err != nil {
			t.Fatalf("first GetInstallationID: %v", err)
		}
		assertHex64(t, first)

		second, err := New(zap.NewNop(), nil).GetInstallationID(context.Background())
		if err != nil {
			t.Fatalf("second GetInstallationID: %v", err)
		}
		if second != first {
			t.Fatalf("last-resort fallback not stable within process: %q != %q", second, first)
		}

		// A fresh process with only generic "localhost" must not collide with the first.
		processFallbackMu.Lock()
		processFallbackID = ""
		processFallbackMu.Unlock()
		nextProc, err := New(zap.NewNop(), nil).GetInstallationID(context.Background())
		if err != nil {
			t.Fatalf("nextProc GetInstallationID: %v", err)
		}
		assertHex64(t, nextProc)
		if nextProc == first {
			t.Fatalf("two independent processes with only generic localhost collided on %q", nextProc)
		}
	})

	t.Run("rand failure and hostname error still returns non-empty hex id", func(t *testing.T) {
		resetProcessFallback(t)
		clearHomeEnv(t)
		clearFallbackCIEnv(t)
		t.Setenv("KEPLOY_INDOCKER", "true")
		t.Setenv("INSTALLATION_ID", "")
		withHostname(t, "", errors.New("hostname unavailable"))
		withProcFiles(t, nil)
		withRandRead(t, func([]byte) (int, error) { return 0, errors.New("entropy error") })

		first, err := New(zap.NewNop(), nil).GetInstallationID(context.Background())
		if err != nil {
			t.Fatalf("GetInstallationID: %v", err)
		}
		assertHex64(t, first)

		second, err := New(zap.NewNop(), nil).GetInstallationID(context.Background())
		if err != nil {
			t.Fatalf("second GetInstallationID: %v", err)
		}
		if second != first {
			t.Fatalf("entropy-fallback id not stable within process: %q != %q", second, first)
		}
	})
}
