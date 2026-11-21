package tools

import (
	"fmt"
	"io"
	"os"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/config/loader"
)

// AddAppName writes the app's name into the keploy.yml at path, which names
// no app: config.WithAppName's one line, every other byte of the file kept.
// The file is rewritten in place, so it keeps its owner and mode under sudo.
// It writes nothing, and says why, when the file is a link out of its folder
// (ResolveConfigTarget), not a regular file, past loader.MaxConfigBytes, or
// one WithAppName refuses.
func AddAppName(path, name string) error {
	target, err := ResolveConfigTarget(path)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(target, os.O_RDWR|oNoFollow, 0)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	// Stat of the open file fails only on an I/O error.
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", target)
	}
	doc, err := io.ReadAll(io.LimitReader(f, loader.MaxConfigBytes+1))
	if err != nil {
		return err
	}
	if len(doc) > loader.MaxConfigBytes {
		return fmt.Errorf("%s is over %d bytes", target, loader.MaxConfigBytes)
	}
	out, err := config.WithAppName(doc, name)
	if err != nil {
		return err
	}
	if _, err := f.WriteAt(out, 0); err != nil {
		return err
	}
	// A line put in place of `appName: null` can be shorter than it.
	if err := f.Truncate(int64(len(out))); err != nil {
		return err
	}
	return f.Close()
}
