// Package main is a small harness that exercises pkg/agent/proxy/tls's
// setupSharedVolume code path end-to-end: it writes the merged CA bundle to
// /tmp/keploy-tls/ca.crt (the same path the k8s-proxy admission webhook
// injects into application containers) and then exits. The e2e test then
// copies that path into a shared volume and has a second container curl a
// real TLS endpoint with REQUESTS_CA_BUNDLE=/shared/ca.crt — proving that
// the merge fix gives apps simultaneous trust of both system anchors and
// the Keploy MITM CA.
package main

import (
	"context"
	"log"
	"os"
	"path/filepath"

	tlsmod "go.keploy.io/server/v3/pkg/agent/proxy/tls"
	"go.uber.org/zap"
)

func main() {
	exportPath := "/tmp/keploy-tls"
	if v := os.Getenv("EXPORT_PATH"); v != "" {
		exportPath = v
	}
	if err := os.MkdirAll(exportPath, 0755); err != nil {
		log.Fatalf("mkdir %s: %v", exportPath, err)
	}

	logger, err := zap.NewDevelopment()
	if err != nil {
		log.Fatalf("logger: %v", err)
	}
	defer func() { _ = logger.Sync() }()

	// isDocker=true triggers setupSharedVolume (the code path we are testing).
	// SetupCA writes to the hard-coded /tmp/keploy-tls when isDocker=true;
	// for test isolation we symlink /tmp/keploy-tls -> EXPORT_PATH before
	// invoking it, but in the containerised e2e runner EXPORT_PATH IS
	// /tmp/keploy-tls so it's a no-op.
	if exportPath != "/tmp/keploy-tls" {
		// Point the conventional path at our chosen export dir so the
		// production call writes into a tempdir the test owns.
		//
		// Use RemoveAll (not Remove): if /tmp/keploy-tls already exists as a
		// non-empty directory from a prior run, Remove returns ENOTEMPTY and
		// the subsequent Symlink would then error with "file exists" and mask
		// the real cleanup failure. RemoveAll tolerates both files and
		// populated directories; a legitimate error here (permission denied,
		// read-only FS, ...) must be surfaced, not ignored.
		if err := os.RemoveAll("/tmp/keploy-tls"); err != nil {
			log.Fatalf("cleanup /tmp/keploy-tls before creating symlink: %v", err)
		}
		if err := os.Symlink(exportPath, "/tmp/keploy-tls"); err != nil {
			log.Fatalf("symlink: %v", err)
		}
	}

	if err := tlsmod.SetupCA(context.Background(), logger, true); err != nil {
		log.Fatalf("SetupCA: %v", err)
	}

	caPath := filepath.Join(exportPath, "ca.crt")
	info, err := os.Stat(caPath)
	if err != nil {
		log.Fatalf("stat %s: %v", caPath, err)
	}
	logger.Info("ca.crt written", zap.String("path", caPath), zap.Int64("bytes", info.Size()))
}
