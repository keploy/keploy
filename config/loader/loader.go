// Package loader reads keploy's config file into a viper instance the way the
// CLI does -- which file, then viper -- but without letting a config file in an
// untrusted checkout decide how much time or memory keploy spends reading it.
//
// keploy runs in whatever repository it is pointed at, and the first thing
// every command does is read keploy.yml (PreProcessFlags). A cloned repo is
// untrusted input: keploy.yml can be a symlink to /dev/zero, which ran every
// command out of memory, or a FIFO, which blocked it for good. Read and Merge
// keep viper's own read of the file -- case-insensitivity, merge keys,
// duplicate-key, parse and type errors in viper's words -- and add only a
// bound: the file must be a regular file of at most MaxConfigBytes.
//
// PreProcessFlags reads keploy.yml and the override through Find, Read and
// Merge. They are exported so that a reader outside the CLI -- `keploy status`,
// which the VS Code extension runs on every sidebar render -- reads keploy.yml
// with the same code, and does not drift from the CLI.
package loader

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/viper"
	"go.keploy.io/server/v3/pkg/platform/safeyaml"
)

// MaxConfigBytes bounds keploy.yml, and the override merged into it. The
// default keploy.yml, every option in it, is about 7 KB.
const MaxConfigBytes = 1 << 20

// configNames are the files that are keploy's configuration, in the order the
// CLI reads them. CreateConfigFile writes keploy.yml.
var configNames = []string{"keploy.yaml", "keploy.yml"}

// Find returns the path of keploy's config file in dir, or "" when there is
// none. A directory, or a file that cannot be stat'd, is not one -- as viper's
// lookup treated them. A FIFO or device IS returned (the CLI would try it too);
// Read then refuses it rather than blocking on it, and does not fall through
// to the next name.
func Find(dir string) string {
	for _, name := range configNames {
		p := filepath.Join(dir, name)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return ""
}

// Read reads the config file into v as viper.ReadInConfig does -- v must have
// its config type set to yaml -- once the file is within keploy's limits.
// The error is viper's, or why keploy did not read the file, naming it: it is
// not a regular file (safeyaml.ErrNotRegular), it changed as it was read
// (safeyaml.ErrSizeChanged), or it is past one of keploy's limits
// (safeyaml.IsRefused).
func Read(v *viper.Viper, file string) error {
	raw, err := readConfigFile(file)
	if err != nil {
		return err
	}
	return v.ReadConfig(bytes.NewReader(raw))
}

// Merge merges the config file over the config v holds, as viper.MergeInConfig
// does, once the file is within keploy's limits. Its error is Read's.
func Merge(v *viper.Viper, file string) error {
	raw, err := readConfigFile(file)
	if err != nil {
		return err
	}
	return v.MergeConfig(bytes.NewReader(raw))
}

// readConfigFile reads a config file for viper: a regular file of at most
// MaxConfigBytes.
func readConfigFile(p string) ([]byte, error) {
	raw, err := safeyaml.ReadFile(p, MaxConfigBytes)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Base(p), safeyaml.StripPath(err))
	}
	return raw, nil
}
