package tls

import (
	"errors"
	"fmt"
	"os"
	"time"

	"go.keploy.io/server/v3/pkg/kubeconfig"
	"go.uber.org/zap"
)

// chownCopy gives the copy at path to the owner of the file ref, when keploy
// (under sudo) made it its own; a test stubs it.
var chownCopy = chownToOwnerOf

// pointKubeconfig points KUBECONFIG, for the app keploy starts, at copies in
// run's directory of the kubeconfig files kubectl would read, made to trust
// keploy's CA (kubeconfig.Rewrite), so kubectl and helm accept keploy's
// certificate for their API servers. The copies hold what the files hold
// (client keys and tokens among it), so each is readable by its file's owner
// alone, and they go with the run's directory when the run ends, or at the
// next run's start when it died. As the run ends, what the test wrote to the
// copies is written back to the user's files (writeKubeconfigsBack). With no
// kubeconfig, or one keploy cannot read, KUBECONFIG is left as it is, and
// kubectl will refuse keploy's certificate: that is logged.
func pointKubeconfig(logger *zap.Logger, run *runTrustDir, trust kubeconfig.Trust) {
	files := kubeconfig.Files()
	if len(files) == 0 {
		return
	}
	copies := make([]string, 0, len(files))
	origs := make([][]byte, 0, len(files))
	wrote := make([][]byte, 0, len(files))
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
		p, err := run.writePrivate(fmt.Sprintf("kubeconfig-%d", i), data)
		if err == nil {
			// keploy under sudo: the copy is the file owner's, as the file is.
			err = chownCopy(f, p)
		}
		if err != nil {
			logger.Warn("keploy could not write a copy of the kubeconfig that trusts its CA; kubectl and helm will refuse keploy's certificate for their API servers",
				zap.String("kubeconfig", f), zap.Error(err))
			return
		}
		copies = append(copies, p)
		origs = append(origs, orig)
		wrote = append(wrote, data)
	}
	if err := kubeconfig.Point(copies); err != nil {
		logger.Warn("keploy could not point KUBECONFIG at the copy of the kubeconfig that trusts its CA", zap.Error(err))
		return
	}
	logger.Info("pointing kubectl and helm at a copy of the kubeconfig that trusts keploy's CA too; what the test changes in it is written back to the kubeconfig when the run ends",
		zap.Strings("kubeconfig", files), zap.Strings("copy", copies))
	run.atRemove(func() { writeKubeconfigsBack(logger, files, copies, origs, wrote, trust.CA) })
}

// writeKubeconfigsBack writes, as the run ends, what the test changed in each
// copy of the kubeconfig files keploy pointed it at to the file it copies
// (kubeconfig.WriteBack), as kubectl would have written them without keploy:
// kubectl wrote each entry to the copy of the file it would have written, and
// copies and files go in the same order. Each file written is first kept in a
// backup beside it, and one line says what changed and where the backup is.
// When the file changed during the run in an entry the test changed too,
// nothing is written: the warning names the entries and the file beside it
// that holds the test's changes. A cluster the test added or changed whose CA
// leaves keploy's out is named too: kubectl and helm refused keploy's
// certificate for it (the proxy counts each such call as lost,
// tls_handshake_failed), and this says which cluster those calls were for.
// What the files hold is never logged, only names.
func writeKubeconfigsBack(logger *zap.Logger, files, copies []string, origs, wrote [][]byte, ca []byte) {
	for i, cp := range copies {
		now, err := os.ReadFile(cp)
		if errors.Is(err, os.ErrNotExist) {
			logger.Warn("the test removed its kubeconfig during the run; keploy had pointed it at a copy, and leaves the kubeconfig itself as it is",
				zap.String("kubeconfig", files[i]), zap.String("next_step", "remove the kubeconfig after the run, if the test meant to"))
			continue
		}
		if err != nil {
			logger.Warn("keploy could not read its copy of the kubeconfig as the run ended, so it cannot write what the test changed in it to the kubeconfig",
				zap.String("kubeconfig", files[i]), zap.Error(err))
			continue
		}
		w, err := kubeconfig.WriteBack(files[i], cp, origs[i], wrote[i], now, ca, kubeconfig.WriteOptions{Now: time.Now(), Chown: chownCopy})
		fields := append([]zap.Field{zap.String("kubeconfig", files[i])}, changeFields(w.Changes)...)
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
				zap.String("kubeconfig", files[i]), zap.Strings("cluster", w.Untrusted),
				zap.String("next_step", "create the cluster before keploy's run starts: keploy's copy of the kubeconfig then trusts keploy's CA for it"))
		}
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
