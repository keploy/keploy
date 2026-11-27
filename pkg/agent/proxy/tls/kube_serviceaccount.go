package tls

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"go.uber.org/zap"
)

// Kubernetes API trust for a recorded or replayed app (keploy/enterprise#3564).
//
// An in-cluster Kubernetes client (client-go's rest.InClusterConfig, the Python,
// Java and Node clients alike) trusts ONLY the ServiceAccount CA at
// /var/run/secrets/kubernetes.io/serviceaccount/ca.crt. It ignores
// SSL_CERT_FILE and the system store, so the keploy CA the shared volume carries
// never reaches it, and every call it makes to the API server through keploy's
// MITM fails with "x509: certificate signed by unknown authority": an app that
// reads a ConfigMap at startup crash-loops for the whole recording, and one that
// reads it in a route has the failure recorded as its response.
//
// The pod builder (k8s-proxy) fixes that with this directory. It moves the app
// container's own service-account mount to EnvKubeServiceAccountSource, mounts
// an emptyDir at the service-account path in its place, and mounts the same
// emptyDir into this agent at EnvKubeServiceAccountDir. Here, before the agent
// reports ready (so before the app starts), the directory is filled with what
// the app had there, with one change: ca.crt is the cluster CA AND keploy's CA.
// Every other entry (token, namespace, anything a custom projection adds) is a
// symlink to the original, so the kubelet's token refresh keeps reaching the
// app. The symlinks are absolute: the source is mounted at the same path in the
// app container and in this one.
//
// A replay pod is written before ServiceAccount admission gives it a token, so
// its builder cannot move the app's mount: it mounts the emptyDir at the app's
// service-account path, admission then mounts the token only in this agent, and
// EnvKubeServiceAccountCopy has the files copied from there instead of linked
// (a link would name the app's own path, the emptyDir itself). The copies are
// refreshed every kubeSARefreshInterval, as the kubelet refreshes the token. A
// pod admission gave no token leaves the directory empty, as the app's would be.
const (
	// EnvKubeServiceAccountSource is where the app's own service-account files
	// are mounted, at the same path in the app container and in the agent.
	EnvKubeServiceAccountSource = "KEPLOY_KUBE_SA_SOURCE"
	// EnvKubeServiceAccountDir is the agent's mount of the directory the app
	// sees at its service-account path.
	EnvKubeServiceAccountDir = "KEPLOY_KUBE_SA_DIR"
	// EnvKubeServiceAccountNamespace, when set, is written as the directory's
	// namespace file instead of a link to the source's. A replay pod placed in
	// another namespace sets it to the app's own, so an app that reads its
	// namespace from that file asks for the objects it asked for when recorded.
	EnvKubeServiceAccountNamespace = "KEPLOY_KUBE_SA_NAMESPACE"
	// EnvKubeServiceAccountCopy, when "true", copies the source's files
	// instead of linking them and keeps the copies current, as the file
	// comment says for a replay pod.
	EnvKubeServiceAccountCopy = "KEPLOY_KUBE_SA_COPY"
)

const (
	kubeSACAFile        = "ca.crt"
	kubeSANamespaceFile = "namespace"
)

// kubeSARefreshInterval is how often copied files are compared with their
// source. The kubelet replaces a token well before it expires, and client-go
// re-reads it about once a minute.
var kubeSARefreshInterval = 30 * time.Second

// kubeSAExport is the export the environment asks for.
type kubeSAExport struct {
	src, dir, namespace string
	copy                bool
	keployCertPEM       []byte
}

// exportKubeServiceAccount fills the EnvKubeServiceAccountDir directory, as the
// file comment says, and in copy mode keeps it current until ctx ends. Without
// that variable it does nothing. An error means the app would start without its
// service-account files, so the caller fails CA setup rather than reporting
// ready.
func exportKubeServiceAccount(ctx context.Context, logger *zap.Logger, keployCertPEM []byte) error {
	dir := os.Getenv(EnvKubeServiceAccountDir)
	if dir == "" {
		return nil
	}
	e := kubeSAExport{
		src:           os.Getenv(EnvKubeServiceAccountSource),
		dir:           dir,
		namespace:     os.Getenv(EnvKubeServiceAccountNamespace),
		copy:          os.Getenv(EnvKubeServiceAccountCopy) == "true",
		keployCertPEM: keployCertPEM,
	}
	if e.src == "" || !filepath.IsAbs(e.src) {
		return fmt.Errorf("%s is set but %s is not an absolute path (%q): the app's service-account files cannot be exported", EnvKubeServiceAccountDir, EnvKubeServiceAccountSource, e.src)
	}
	if _, err := os.Stat(e.src); e.copy && errors.Is(err, fs.ErrNotExist) {
		logger.Info("no service-account token is mounted in this pod; the app's service-account directory stays empty, as it would be without keploy",
			zap.String("source", e.src), zap.String("dir", dir))
		return nil
	}

	names, merged, err := e.fill(logger)
	if err != nil {
		return err
	}
	logger.Info("exported the app's service-account directory with keploy's CA added to its ca.crt",
		zap.String("source", e.src),
		zap.String("dir", dir),
		zap.Strings("entries", names),
		zap.Bool("ca_merged", merged),
		zap.Bool("copied", e.copy),
		zap.String("namespace_override", e.namespace))
	if e.copy {
		go e.refresh(ctx, logger, kubeSARefreshInterval)
	}
	return nil
}

// fill writes the directory once. It returns the entries written and whether
// ca.crt carries keploy's CA.
func (e kubeSAExport) fill(logger *zap.Logger) ([]string, bool, error) {
	entries, err := os.ReadDir(e.src)
	if err != nil {
		return nil, false, fmt.Errorf("read the app's service-account files at %s: %w", e.src, err)
	}
	names := make([]string, 0, len(entries))
	for _, ent := range entries {
		// "..data" and the timestamped "..2026_..." directory are the kubelet's
		// atomic-writer internals; the files the app reads link through them.
		if strings.HasPrefix(ent.Name(), "..") {
			continue
		}
		names = append(names, ent.Name())
	}
	if len(names) == 0 {
		return nil, false, fmt.Errorf("the app's service-account directory %s is empty", e.src)
	}
	if e.namespace != "" && !slices.Contains(names, kubeSANamespaceFile) {
		names = append(names, kubeSANamespaceFile)
	}

	merged := false
	for _, name := range names {
		from, dst := filepath.Join(e.src, name), filepath.Join(e.dir, name)
		switch {
		case name == kubeSACAFile:
			clusterCA, rerr := os.ReadFile(from)
			if rerr == nil && len(clusterCA) > 0 {
				if werr := writeFileIfChanged(dst, mergeCABundle(clusterCA, e.keployCertPEM)); werr != nil {
					return nil, false, fmt.Errorf("write %s: %w", dst, werr)
				}
				merged = true
				continue
			}
			// The app gets the cluster's file as it is: its calls through keploy
			// fail as before, and nothing else changes.
			logger.Warn("could not read the cluster CA from the app's service-account files; the app's Kubernetes client will not trust keploy's certificate",
				zap.String("path", from), zap.Error(rerr))
		case name == kubeSANamespaceFile && e.namespace != "":
			if werr := writeFileIfChanged(dst, []byte(e.namespace)); werr != nil {
				return nil, false, fmt.Errorf("write %s: %w", dst, werr)
			}
			continue
		}
		if !e.copy {
			if lerr := replaceSymlink(from, dst); lerr != nil {
				return nil, false, fmt.Errorf("link %s to %s: %w", dst, from, lerr)
			}
			continue
		}
		data, rerr := os.ReadFile(from)
		if rerr != nil {
			return nil, false, fmt.Errorf("copy %s: %w", from, rerr)
		}
		if werr := writeFileIfChanged(dst, data); werr != nil {
			return nil, false, fmt.Errorf("write %s: %w", dst, werr)
		}
	}
	return names, merged, nil
}

// refresh re-fills a copied directory every interval until ctx ends. A
// failure is logged once, until a fill succeeds again; the app keeps the last
// copies meanwhile.
func (e kubeSAExport) refresh(ctx context.Context, logger *zap.Logger, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	failing := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		_, _, err := e.fill(logger)
		if err != nil && !failing {
			logger.Warn("could not refresh the app's copied service-account files; it keeps the last copies",
				zap.String("source", e.src), zap.String("dir", e.dir), zap.Error(err))
		}
		failing = err != nil
	}
}

// writeFileIfChanged replaces path with data unless it already holds it, so a
// refresh that finds nothing new writes nothing.
func writeFileIfChanged(path string, data []byte) error {
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, data) {
		return nil
	}
	return replaceFile(path, data, 0o644)
}

// replaceFile writes data to path through a temporary file in the same
// directory and a rename, so a reader never sees it half-written: an agent that
// restarts rewrites it while the app runs.
func replaceFile(path string, data []byte, mode os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, werr := f.Write(data)
	cerr := f.Close()
	if err = errors.Join(werr, cerr); err == nil {
		err = os.Chmod(tmp, mode)
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		_ = os.Remove(tmp)
	}
	return err
}

// replaceSymlink points path at target, replacing whatever is at path in one
// rename.
func replaceSymlink(target, path string) error {
	tmp := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".tmp-link")
	_ = os.Remove(tmp)
	if err := os.Symlink(target, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
