package tls

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"go.keploy.io/server/v3/pkg/kubeconfig"
	"go.uber.org/zap"
)

// chownCopy gives the copy at path to the owner of the file ref, when keploy
// (under sudo) made it its own; a test stubs it.
var chownCopy = chownToOwnerOf

// recordSuffix names, beside each copy, the record of what writing the
// test's changes in it back takes (kubeconfigRecord).
const recordSuffix = ".writeback"

// kubeconfigRecord is what writing back what the test changed in one copy
// takes (kubeconfig.WriteBack), kept as JSON beside the copy until the run has
// written it back, so that the next run writes it back for a run that ended
// without its cleanup (writeBackEndedRun). It holds what the copy holds, the
// user's file with its client keys and tokens, so it is readable by keploy's
// user alone and goes with the run's directory, as the copy does.
type kubeconfigRecord struct {
	File    string    `json:"file"`    // the user's kubeconfig
	Copy    string    `json:"copy"`    // the copy's name in the run's directory
	Orig    []byte    `json:"orig"`    // the user's file as the run read it
	Wrote   []byte    `json:"wrote"`   // the copy as keploy wrote it
	CA      []byte    `json:"ca"`      // keploy's CA (PEM)
	PID     int       `json:"pid"`     // the run's process
	Started time.Time `json:"started"` // when the run started
}

// keepRecord writes rec beside its copy in run's directory: whole or not at
// all, as a file renamed into place.
func keepRecord(run *runTrustDir, rec kubeconfigRecord) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	tmp, err := run.writePrivate("."+rec.Copy+recordSuffix, data)
	if err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(run.dir, rec.Copy+recordSuffix))
}

// pointKubeconfig points KUBECONFIG, for the app keploy starts, at copies in
// run's directory of the kubeconfig files kubectl would read, made to trust
// keploy's CA (kubeconfig.Rewrite), so kubectl and helm accept keploy's
// certificate for their API servers. The copies hold what the files hold
// (client keys and tokens among it), so each is readable by its file's owner
// alone, and they go with the run's directory when the run ends, or at the
// next run's start when it died. As the run ends, what the test wrote to the
// copies is written back to the user's files (writeKubeconfigsBack); for a
// run that ends without its cleanup, the next run does it, from the record
// kept beside each copy (keepRecord). With no kubeconfig, or one keploy cannot
// read, KUBECONFIG is left as it is, and kubectl will refuse keploy's
// certificate: that is logged.
func pointKubeconfig(logger *zap.Logger, run *runTrustDir, trust kubeconfig.Trust) {
	files := kubeconfig.Files()
	if len(files) == 0 {
		return
	}
	copies := make([]string, 0, len(files))
	recs := make([]kubeconfigRecord, 0, len(files))
	for i, f := range files {
		orig, err := os.ReadFile(f)
		var data []byte
		if err == nil {
			var notes []string
			data, notes, err = kubeconfig.Rewrite(f, orig, trust)
			for _, n := range notes {
				logger.Warn(n)
			}
		}
		if err != nil {
			logger.Warn("keploy could not make a copy of the kubeconfig that trusts its CA; kubectl and helm will refuse keploy's certificate for their API servers",
				zap.String("kubeconfig", f), zap.Error(err),
				zap.String("next_step", "make the kubeconfig a file keploy can read and kubectl can parse"))
			return
		}
		rec := kubeconfigRecord{File: f, Copy: fmt.Sprintf("kubeconfig-%d", i), Orig: orig, Wrote: data, CA: trust.CA, PID: os.Getpid(), Started: run.started}
		p, err := run.writePrivate(rec.Copy, data)
		if err == nil {
			// keploy under sudo: the copy is the file owner's, as the file is.
			err = chownCopy(f, p)
		}
		if err == nil {
			// Before the test can change the copy.
			err = keepRecord(run, rec)
		}
		if err != nil {
			logger.Warn("keploy could not write a copy of the kubeconfig that trusts its CA; kubectl and helm will refuse keploy's certificate for their API servers",
				zap.String("kubeconfig", f), zap.Error(err))
			return
		}
		copies = append(copies, p)
		recs = append(recs, rec)
	}
	if err := kubeconfig.Point(copies); err != nil {
		logger.Warn("keploy could not point KUBECONFIG at the copy of the kubeconfig that trusts its CA", zap.Error(err))
		return
	}
	logger.Info("pointing kubectl and helm at a copy of the kubeconfig that trusts keploy's CA too; what the test changes in it is written back to the kubeconfig when the run ends",
		zap.Strings("kubeconfig", files), zap.Strings("copy", copies))
	run.atRemove(func() { writeKubeconfigsBack(logger, run.dir, recs) })
}

// writeKubeconfigsBack writes back, for each of recs, what the test changed
// in its copy in dir (writeKubeconfigBack), then removes its record: a run
// that dies after leaves the next run nothing to write again.
func writeKubeconfigsBack(logger *zap.Logger, dir string, recs []kubeconfigRecord) {
	for _, rec := range recs {
		writeKubeconfigBack(logger, dir, rec)
		if err := os.Remove(filepath.Join(dir, rec.Copy+recordSuffix)); err != nil {
			logger.Debug("could not remove the record of a kubeconfig copy written back", zap.String("dir", dir), zap.Error(err))
		}
	}
}

// writeBackEndedRun writes back what the test of the run whose directory is
// dir changed in its kubeconfig copies, from the records the run kept beside
// them (kubeconfigRecord), as the run's end would have: the run ended without
// its cleanup (killed, crashed). Each line says which run, by its pid and
// start. It reports false when a record cannot be read: the directory then
// stays, with the copy that holds the test's changes, and the warning names
// it; the records it could read are written back.
func writeBackEndedRun(logger *zap.Logger, dir string) bool {
	// Glob fails only on a malformed pattern.
	names, _ := filepath.Glob(filepath.Join(dir, "*"+recordSuffix))
	read := true
	for _, n := range names {
		var rec kubeconfigRecord
		data, err := os.ReadFile(n)
		if err == nil {
			err = json.Unmarshal(data, &rec)
		}
		if err != nil {
			logger.Warn("keploy could not read what a run that ended without its cleanup (killed or crashed) kept for writing the test's changes to its kubeconfig back, so it leaves that run's directory, whose kubeconfig copy holds them",
				zap.String("dir", dir), zap.String("record", filepath.Base(n)), zap.Error(err),
				zap.String("next_step", "merge what the test changed, in dir's kubeconfig-N copies, into your kubeconfig by hand if you need it, then remove dir"))
			read = false
			continue
		}
		ended := fmt.Sprintf("pid %d, started %s; it ended without its cleanup (killed or crashed)", rec.PID, rec.Started.Format(time.RFC3339))
		writeKubeconfigsBack(logger.With(zap.String("ended_run", ended)), dir, []kubeconfigRecord{rec})
	}
	return read
}

// writeKubeconfigBack writes, as the run ends, what the test changed in rec's
// copy of a kubeconfig file keploy pointed it at, in dir, to the file it
// copies (kubeconfig.WriteBack), as kubectl would have written it without
// keploy: kubectl wrote each entry to the copy of the file it would have
// written, and copies and files go in the same order. Each file written is
// first kept in a backup beside it, and one line says what changed and where
// the backup is. When the file changed during the run in an entry the test
// changed too, nothing is written: the warning names the entries and the file
// beside it that holds the test's changes. A cluster the test added or
// changed whose CA leaves keploy's out is named too: kubectl and helm refused
// keploy's certificate for it (the proxy counts each such call as lost,
// tls_handshake_failed), and this says which cluster those calls were for.
// What the files hold is never logged, only names.
func writeKubeconfigBack(logger *zap.Logger, dir string, rec kubeconfigRecord) {
	cp := filepath.Join(dir, rec.Copy)
	now, err := os.ReadFile(cp)
	if errors.Is(err, os.ErrNotExist) {
		logger.Warn("the test removed its kubeconfig during the run; keploy had pointed it at a copy, and leaves the kubeconfig itself as it is",
			zap.String("kubeconfig", rec.File), zap.String("next_step", "remove the kubeconfig after the run, if the test meant to"))
		return
	}
	if err != nil {
		logger.Warn("keploy could not read its copy of the kubeconfig as the run ended, so it cannot write what the test changed in it to the kubeconfig",
			zap.String("kubeconfig", rec.File), zap.Error(err))
		return
	}
	w, err := kubeconfig.WriteBack(rec.File, cp, rec.Orig, rec.Wrote, now, rec.CA, kubeconfig.WriteOptions{Now: time.Now(), Chown: chownCopy})
	fields := append([]zap.Field{zap.String("kubeconfig", rec.File)}, changeFields(w.Changes)...)
	switch {
	case err != nil:
		if w.Kept != "" {
			fields = append(fields, zap.String("test_changes", w.Kept))
		}
		logger.Warn("keploy could not write what the test changed in its kubeconfig during the run to the kubeconfig, and left it as it is",
			append(fields, zap.Error(err), zap.String("next_step", "make the change again after the run ends; test_changes, when named, is a kubeconfig holding what the test added and changed"))...)
	case len(w.Conflict) > 0:
		if w.Kept != "" {
			fields = append(fields, zap.String("test_changes", w.Kept))
		}
		logger.Warn("the kubeconfig was changed during the run, outside the test, in entries the test changed too; keploy left it as it is",
			append(fields, zap.Strings("conflict", w.Conflict),
				zap.String("next_step", "merge what the test changed, kept in test_changes, into the kubeconfig by hand (KUBECONFIG=<test_changes>:<kubeconfig> kubectl config view --flatten shows them merged)"))...)
	case w.Backup != "":
		logger.Info("keploy wrote to the kubeconfig what the test changed in it during the run",
			append(fields, zap.String("written", w.File), zap.String("backup", w.Backup))...)
	}
	if len(w.Untrusted) > 0 {
		logger.Warn("kubectl and helm do not trust keploy's certificate for a cluster the test added or changed in its kubeconfig during the run: any call they made to it through keploy failed its TLS handshake and was neither recorded nor mocked (capture incomplete: tls_handshake_failed)",
			zap.String("kubeconfig", rec.File), zap.Strings("cluster", w.Untrusted),
			zap.String("next_step", "create the cluster before keploy's run starts: keploy's copy of the kubeconfig then trusts keploy's CA for it"))
	}
}

// changeFields names what the test changed (kubeconfig.Changes), for a log
// line.
func changeFields(ch kubeconfig.Changes) []zap.Field {
	var fields []zap.Field
	for _, f := range []struct {
		key   string
		names []string
	}{{"added", ch.Added}, {"changed", ch.Changed}, {"removed", ch.Removed}} {
		if len(f.names) > 0 {
			fields = append(fields, zap.Strings(f.key, f.names))
		}
	}
	if ch.CurrentContext != "" {
		fields = append(fields, zap.String("current_context", ch.CurrentContext))
	}
	if ch.Preferences {
		fields = append(fields, zap.Bool("preferences", true))
	}
	return fields
}
