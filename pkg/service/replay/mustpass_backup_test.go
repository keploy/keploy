package replay

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
)

// --must-pass deletes no test case of a set it could not back up: a backup that
// failed leaves the user nothing to restore, so the failing test cases are kept
// and the set stays failing. The run used to log the failure and delete them
// anyway, which on a set whose copy ran out of open files left an emptied set
// with no backup at all.
func TestMustPassDeletesNothingWhenTheBackupFails(t *testing.T) {
	for _, on := range []bool{false, true} {
		t.Run(fmt.Sprintf("switch %v", on), func(t *testing.T) {
			setSwitch(t, on)
			h, logs := composeRetried(t, 3)
			h.replayer.config.Test.MustPass = true
			h.replayer.config.Test.MaxFailAttempts = 5
			db := &deleteRecDB{prTestDB: h.replayer.testDB.(*prTestDB)}
			h.replayer.testDB = db
			h.replayer.hookImpl = prHooks{wrongBody: map[string]bool{"test-2": true}}

			// A regular file where the backup directory belongs: creating
			// .backup/<timestamp> under it fails, which is the backup failing
			// before it holds a single test case.
			set := filepath.Join(h.replayer.config.Path, "test-set-0")
			if err := os.MkdirAll(set, 0755); err != nil {
				t.Fatalf("create the set's directory: %v", err)
			}
			if err := os.WriteFile(filepath.Join(set, "test-1.yaml"), []byte("name: test-1\n"), 0644); err != nil {
				t.Fatalf("write the set's test case: %v", err)
			}
			if err := os.WriteFile(filepath.Join(set, ".backup"), []byte("not a directory"), 0644); err != nil {
				t.Fatalf("put a file where the backup directory belongs: %v", err)
			}

			startRetried(t, h)

			db.mu.Lock()
			deleted := fmt.Sprint(db.deleted)
			db.mu.Unlock()
			if deleted != "[]" {
				t.Errorf("--must-pass deleted %s from a test set it could not back up, want none", deleted)
			}
			kept := logs.FilterMessageSnippet("--must-pass deletes no test case of a test set it could not back up").All()
			if len(kept) != 1 {
				t.Errorf("%d lines say --must-pass kept the test cases of a set it could not back up, want 1", len(kept))
			} else if kept[0].ContextMap()["next_step"] == nil {
				t.Errorf("the line says nothing of what to do next: %v", kept[0].ContextMap())
			}
			if ran := setsRan(logs); len(ran) != 1 {
				t.Errorf("the run ran %v, want test-set-0 once: a set that cannot be backed up is not run again", ran)
			}
			h.report.mu.Lock()
			rep := h.report.report
			h.report.mu.Unlock()
			if rep == nil || rep.Status != string(models.TestSetStatusFailed) {
				t.Errorf("the set's report is %+v, want FAILED: its failing test cases are kept and it is not rerun", rep)
			}
			if utils.ErrCode != 1 {
				t.Errorf("exit code %d, want 1: the set still fails", utils.ErrCode)
			}
		})
	}
}
