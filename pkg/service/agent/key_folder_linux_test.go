//go:build linux

package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
)

// recordOne claims the run for this test process with req, then begins and
// ends TestA under it, and returns TestA's window: the folder a record
// writes as its mappings.yaml dir. A begin with dir after the claim is
// refused: protocol v1's begins and ends never carry one, whatever the claim
// sent.
func recordOne(t *testing.T, project string, req models.InstanceReq) models.ScopeWindow {
	t.Helper()
	a, _ := wireAgent(t, models.MODE_RECORD)
	a.config.ConfigPath = project
	ctx := context.Background()
	_, serr := a.ClaimInstance(ctx, req, os.Getpid())
	require.Nil(t, serr)
	_, serr = a.ProvenBegin(ctx, models.ScopeReq{Name: "TestA", CanSkip: true, Dir: filepath.Join(project, req.Key)})
	require.NotNil(t, serr, "a begin with dir under a claim that sent %q", req.Dir)
	require.Equal(t, models.ScopeErrUpgradeIntegration, serr.Code)
	require.Contains(t, serr.Message, "sends dir")
	_, serr = a.ProvenBegin(ctx, begin("TestA"))
	require.Nil(t, serr)
	_, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: "TestA", Outcome: models.ScopeOutcomePassed})
	require.Nil(t, serr)
	return windowOf(t, a, models.TestID("", "", req.Key, "TestA"))
}

// A claim that names no folder, as an integration written by hand sends it
// (keploy's scope protocol v1 asks only for the key: POST /agent/instance
// with the folder relative to the project, "." for the project's own), runs
// its tests in the folder its key names under the project, the folder the
// run read keploy.yml from: each test's window carries it, the folder
// keploytest's claim sends for the same key (its package's working folder,
// resolved: keploytest's packageKey), here with the project named through a
// link. "." is the project's own folder and "tests" the one in it. Before,
// the window carried no folder, the record wrote none into mappings.yaml, and
// keploy mock record refused to commit the recording of every framework
// without one of keploy's integrations ("did not say which folder it runs
// in"). A begin that sends dir is still refused (upgrade_integration).
// Mutation: drop the default in ClaimInstance, and the rows carry no folder;
// join the key to the agent's working folder, leave the key out, or leave the
// linked project unresolved, and a row names another folder than
// keploytest's.
func TestAClaimWithNoFolderRunsItsTestsInTheFolderItsKeyNames(t *testing.T) {
	project := resolvedTempDir(t)
	require.NoError(t, os.MkdirAll(filepath.Join(project, "tests"), 0o755))
	link := filepath.Join(t.TempDir(), "project")
	symlink(t, project, link)
	for _, key := range []string{".", "tests"} {
		// keploytest's folder for key: cmd/go runs the test binary in its
		// package's folder, and keploytest claims with that folder resolved.
		t.Chdir(filepath.Join(link, key))
		wd, err := os.Getwd()
		require.NoError(t, err)
		goSDK, err := filepath.EvalSymlinks(wd)
		require.NoError(t, err)
		t.Chdir(resolvedTempDir(t))
		w := recordOne(t, link, models.InstanceReq{Key: key})
		require.Equal(t, goSDK, w.Dir, "key %q: the claim's tests run in the folder keploytest claims with", key)
	}
}

// A folder the claim sends is kept, whatever its key: @keploy/jest claims
// with its project's displayName as key and the project's folder as dir.
// Mutation: fill the folder from the key whatever the claim sent, and the
// row fails.
func TestAClaimsOwnFolderIsKept(t *testing.T) {
	project := resolvedTempDir(t)
	w := recordOne(t, project, models.InstanceReq{Key: "api", Dir: project})
	require.Equal(t, project, w.Dir)
}

// A test worker's claim with no folder runs its tests in its own key's
// folder, as a single test process's does: each worker's window carries its
// own. Mutation: default the folder only on a single test process's claim
// (after ClaimInstance hands a worker's to claimWorker), and the rows carry
// none.
func TestAWorkersClaimWithNoFolderRunsItsTestsInItsKeysFolder(t *testing.T) {
	project := resolvedTempDir(t)
	a, _ := workerAgent(t, models.MODE_RECORD, 4)
	a.config.ConfigPath = project
	w1, w2 := child(t), child(t)
	for _, c := range []struct {
		pid int
		key string
	}{{w1.Process.Pid, "a"}, {w2.Process.Pid, "b"}} {
		_, serr := a.ClaimInstance(context.Background(), models.InstanceReq{Key: c.key, Workers: 2}, c.pid)
		require.Nil(t, serr)
	}
	_, serr := a.ProvenBegin(as(w1.Process.Pid), begin("TestOne"))
	require.Nil(t, serr)
	_, serr = a.ProvenEnd(as(w1.Process.Pid), models.ScopeReq{Name: "TestOne", Outcome: models.ScopeOutcomePassed})
	require.Nil(t, serr)
	_, serr = a.ProvenBegin(as(w2.Process.Pid), begin("TestTwo"))
	require.Nil(t, serr)
	_, serr = a.ProvenEnd(as(w2.Process.Pid), models.ScopeReq{Name: "TestTwo", Outcome: models.ScopeOutcomePassed})
	require.Nil(t, serr)
	require.Equal(t, filepath.Join(project, "a"), windowOf(t, a, "a::TestOne").Dir)
	require.Equal(t, filepath.Join(project, "b"), windowOf(t, a, "b::TestTwo").Dir)
}
