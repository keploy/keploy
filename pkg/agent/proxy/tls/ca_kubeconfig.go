package tls

import (
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
	}
	if err := kubeconfig.Point(copies); err != nil {
		logger.Warn("keploy could not point KUBECONFIG at the copy of the kubeconfig that trusts its CA", zap.Error(err))
		return
	}
	logger.Info("pointing kubectl and helm at a copy of the kubeconfig that trusts keploy's CA too (the kubeconfig itself is left untouched)",
		zap.Strings("kubeconfig", files), zap.Strings("copy", copies))
}
