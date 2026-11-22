package provider

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/spf13/viper"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/config/loader"
)

// WantsMachineOutput reports whether a keploy run with this command line
// (os.Args) writes a document to stdout, as ValidateFlags will decide it
// (isMachineReadableOutput): --json, or `keploy report` with --format json or
// junit, on the command line or, without the flag, as report.format in the
// config file. It is read before cobra parses anything, so that main can
// build a logger that keeps no log file (log.NewWithoutFile) and a run killed
// before its flags are checked leaves none behind: the `logs:` line that
// would name one is PrintLogo's, which a document has no room for.
//
// It reads the command line as cobra will, as far as that decides it: it stops
// at "--"; the last --json or --format wins; --format counts after the
// command `report`, the first word that is no flag and no root flag's value
// (--config-path, --storage-format); the config file is keploy.yml or
// keploy.yaml in --config-path (the working directory by default), merged
// with the override file <dir>.keploy.yml in the working directory, as
// ValidateFlags reads them. A spelling it does not read (an abbreviation, a
// flag ahead of the command whose value it does not know to skip) costs
// nothing but a log file that ValidateFlags then closes and removes.
func WantsMachineOutput(args []string) bool {
	jsonOutput := false
	command := ""
	format, formatSet := "", false
	configPath := "."
	for i := 1; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			break
		}
		name, value, hasValue := strings.Cut(a, "=")
		takeValue := func() (string, bool) {
			if hasValue {
				return value, true
			}
			if i+1 < len(args) {
				i++
				return args[i], true
			}
			return "", false
		}
		switch name {
		case "--json":
			jsonOutput = true
			if hasValue {
				v, err := strconv.ParseBool(value)
				jsonOutput = err == nil && v
			}
			continue
		case "--config-path", "--configPath":
			if v, ok := takeValue(); ok {
				configPath = v
			}
			continue
		case "--storage-format":
			_, _ = takeValue()
			continue
		case "--format":
			if command == reportCmdName {
				format, formatSet = takeValue()
				continue
			}
		}
		if command == "" && !strings.HasPrefix(a, "-") {
			command = a
		}
	}
	if jsonOutput {
		return true
	}
	if command != reportCmdName {
		return false
	}
	if !formatSet {
		format = configReportFormat(configPath)
	}
	return config.IsMachineReportFormat(strings.ToLower(strings.TrimSpace(format)))
}

// configReportFormat is report.format in the config file in configPath, and
// in the override file beside it, as ValidateFlags reads them (loader.Read,
// loader.Merge: bounded, a FIFO or a device refused, never waited on); ""
// where there is none, or the file cannot be read, which ValidateFlags then
// reports.
func configReportFormat(configPath string) string {
	dir, err := filepath.Abs(configPath)
	if err != nil {
		return ""
	}
	file := loader.Find(dir)
	if file == "" {
		return ""
	}
	v := viper.New()
	v.SetConfigType("yml")
	if err := loader.Read(v, file); err != nil {
		return ""
	}
	if cwd, err := os.Getwd(); err == nil {
		override := filepath.Join(cwd, filepath.Base(cwd)+".keploy.yml")
		if _, err := os.Stat(override); err == nil {
			_ = loader.Merge(v, override)
		}
	}
	return v.GetString("report.format")
}
