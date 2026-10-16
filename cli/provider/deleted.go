package provider

import (
	"fmt"
	"sync"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// DeletedSetting is a setting the proven model deletes (design v14 U18). A
// row is deleted by the change that lands what replaces it; until the flip,
// under the switch, its flags are unknown flags, which the command refuses,
// and its keploy.yml keys are ignored, with one notice for all of them.
// Ignoring one never makes keploy looser: each deleted setting weakened
// matching, skipped tests, or changed nothing keploy reads.
type DeletedSetting struct {
	// Row is the U18 row, as the refusal and the notice name it.
	Row string
	// Flags are its flags, on whichever command has them.
	Flags []string
	// Keys are its keploy.yml keys, dotted ("test.fallBackOnMiss").
	Keys []string
	// Instead is what to do instead.
	Instead string
	// ignore clears what the keys set in cfg, so nothing that still
	// reads them sees them.
	ignore func(cfg *config.Config)
}

var (
	deletedMu sync.RWMutex
	// deleted are the rows nothing reads (U18 "Read by nothing in a path
	// here", §1 rule 4); keploy's enterprise build adds its own
	// (RegisterDeletedSettings). The other rows are their own changes'.
	deleted = []DeletedSetting{{
		Row:     "read by nothing",
		Flags:   []string{"fallBack-on-miss", "in-ci"},
		Keys:    []string{"test.fallBackOnMiss", "inCi"},
		Instead: "nothing reads it: remove it",
		ignore: func(cfg *config.Config) {
			cfg.Test.FallBackOnMiss = false
			cfg.InCi = false
		},
	}}
)

// RegisterDeletedSettings adds rows to the deleted settings every command
// checks (checkDeletedSettings).
func RegisterDeletedSettings(rows ...DeletedSetting) {
	deletedMu.Lock()
	defer deletedMu.Unlock()
	deleted = append(deleted, rows...)
}

func deletedSettings() []DeletedSetting {
	deletedMu.RLock()
	defer deletedMu.RUnlock()
	return append([]DeletedSetting(nil), deleted...)
}

// checkDeletedSettings refuses, under the proven switch, a deleted flag the
// user gave, as the unknown flag it is at the flip, and ignores the deleted
// keys keploy.yml sets, saying once which and what each row is. Nothing
// without the switch.
func (c *CmdConfigurator) checkDeletedSettings(cmd *cobra.Command) error {
	if !provenswitch.On() {
		return nil
	}
	rows := deletedSettings()
	for _, row := range rows {
		for _, name := range row.Flags {
			if f := cmd.Flags().Lookup(name); f != nil && f.Changed {
				return fmt.Errorf("unknown flag: --%s: it is deleted under %s (U18: %s); %s", name, provenswitch.Env, row.Row, row.Instead)
			}
		}
	}
	var ignored []string
	for _, row := range rows {
		var set bool
		for _, key := range row.Keys {
			if viper.InConfig(key) {
				ignored = append(ignored, fmt.Sprintf("%s (U18: %s; %s)", key, row.Row, row.Instead))
				set = true
			}
		}
		if set && row.ignore != nil {
			row.ignore(c.cfg)
		}
	}
	if len(ignored) > 0 {
		c.logger.Warn(fmt.Sprintf("keploy.yml settings keploy deletes are ignored under %s (U18)", provenswitch.Env),
			zap.Strings("ignored", ignored),
			zap.String("next_step", "remove them from keploy.yml"))
	}
	return nil
}
