package tls

import (
	"errors"
	"fmt"
	"os"

	"go.keploy.io/server/v3/pkg/kubeconfig"
	"go.uber.org/zap"
)

// chownCopy gives the copy at path to the owner of the file ref, when keploy
// (under sudo) made it its own; a test stubs it.
var chownCopy = chownToOwnerOf

// pointKubeconfig points KUBECONFIG, for the app keploy starts, at copies in
// run's directory of the kubeconfig files kubectl would read, made to trust
// keploy's CA (kubeconfig.Rewrite), so kubectl and helm accept keploy's
// certificate for their API servers. The user's files are only read. The
// copies hold what the files hold (client keys and tokens among it), so each
// is readable by its file's owner alone, and they go with the run's directory
// when the run ends, or at the next run's start when it died. With no
// kubeconfig, or one keploy cannot read, KUBECONFIG is left as it is, and
// kubectl will refuse keploy's certificate: that is logged.
func pointKubeconfig(logger *zap.Logger, run *runTrustDir, trust kubeconfig.Trust) {
	files := kubeconfig.Files()
	if len(files) == 0 {
		return
	}
	copies := make([]string, 0, len(files))
	wrote := make([][]byte, 0, len(files))
	for i, f := range files {
		data, err := os.ReadFile(f)
		if err == nil {
			var notes []string
			data, notes, err = kubeconfig.Rewrite(f, data, trust)
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
		wrote = append(wrote, data)
	}
	if err := kubeconfig.Point(copies); err != nil {
		logger.Warn("keploy could not point KUBECONFIG at the copy of the kubeconfig that trusts its CA", zap.Error(err))
		return
	}
	logger.Info("pointing kubectl and helm at a copy of the kubeconfig that trusts keploy's CA too (the kubeconfig itself is left untouched)",
		zap.Strings("kubeconfig", files), zap.Strings("copy", copies))
	run.atRemove(func() { reportKubeconfigWrites(logger, files, copies, wrote, trust.CA) })
}

// reportKubeconfigWrites says, as the run ends, what the test wrote to the
// copies of the kubeconfig files keploy pointed it at (kubeconfig.Diff): the
// copies go with the run and keploy never writes the user's files, so those
// files do not get it, as they would have without keploy. A cluster the test
// added or changed whose CA leaves keploy's out is named too: kubectl and helm
// refused keploy's certificate for it (the proxy counts each such call as
// lost, tls_handshake_failed), and this says which cluster those calls were
// for.
// What the copies hold is never logged, only names.
func reportKubeconfigWrites(logger *zap.Logger, files, copies []string, wrote [][]byte, ca []byte) {
	const lost = "keploy had pointed it at a copy, which goes with the run, so the kubeconfig itself does not have it"
	const nextStep = "make the change before keploy's run starts (keploy copies the kubeconfig as the run starts), or make it again after the run ends"
	for i, cp := range copies {
		now, err := os.ReadFile(cp)
		if errors.Is(err, os.ErrNotExist) {
			logger.Warn("the test removed its kubeconfig during the run; "+lost,
				zap.String("kubeconfig", files[i]), zap.String("next_step", nextStep))
			continue
		}
		if err != nil {
			logger.Warn("keploy could not read its copy of the kubeconfig as the run ended, so it cannot say whether the test changed it",
				zap.String("kubeconfig", files[i]), zap.Error(err))
			continue
		}
		ch, err := kubeconfig.Diff(cp, wrote[i], now, ca)
		if err != nil {
			logger.Warn("the test changed its kubeconfig during the run, in a way keploy cannot read; "+lost,
				zap.String("kubeconfig", files[i]), zap.Error(err), zap.String("next_step", nextStep))
			continue
		}
		if ch.Empty() {
			continue // as keploy wrote it, or written again as it was
		}
		fields := []zap.Field{zap.String("kubeconfig", files[i])}
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
		logger.Warn("the test changed its kubeconfig during the run; "+lost, append(fields, zap.String("next_step", nextStep))...)
		if len(ch.Untrusted) > 0 {
			logger.Warn("kubectl and helm do not trust keploy's certificate for a cluster the test added or changed in its kubeconfig during the run: any call they made to it through keploy failed its TLS handshake and was neither recorded nor mocked (capture incomplete: tls_handshake_failed)",
				zap.String("kubeconfig", files[i]), zap.Strings("cluster", ch.Untrusted),
				zap.String("next_step", "create the cluster before keploy's run starts: keploy's copy of the kubeconfig then trusts keploy's CA for it"))
		}
	}
}
