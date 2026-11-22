package provider

import (
	"fmt"
	"os"
	"reflect"
	"strings"
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
	// Ignore clears what the keys set in cfg, so nothing that still reads
	// them sees them, whether keploy.yml or the environment set them. A
	// row without one is read by nothing.
	Ignore func(cfg *config.Config)
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
		Ignore: func(cfg *config.Config) {
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
// keys keploy.yml or the environment (KEPLOY_<KEY>, viper's AutomaticEnv)
// sets, saying once which and what each row is. A key at its zero value sets
// nothing: an older keploy wrote its whole default config into keploy.yml,
// these keys among it, so that one is not said. Nothing without the switch.
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
		for _, key := range row.Keys {
			env := "KEPLOY_" + strings.ToUpper(key)
			_, inEnv := os.LookupEnv(env)
			if !viper.InConfig(key) && !inEnv || zeroSetting(viper.Get(key)) {
				continue
			}
			from := "keploy.yml"
			if inEnv {
				from = "the environment's " + env
			}
			ignored = append(ignored, fmt.Sprintf("%s, from %s (U18: %s; %s)", key, from, row.Row, row.Instead))
		}
		if row.Ignore != nil {
			row.Ignore(c.cfg)
		}
	}
	if len(ignored) > 0 {
		c.logger.Warn(fmt.Sprintf("settings keploy deletes are ignored under %s (U18)", provenswitch.Env),
			zap.Strings("ignored", ignored),
			zap.String("next_step", "remove them from keploy.yml, or unset them in the environment"))
	}
	return nil
}

// zeroSetting reports whether v, a setting's value, is its zero value: unset,
// false, 0, empty, or such a value as the environment spells it.
func zeroSetting(v interface{}) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		switch strings.ToLower(strings.TrimSpace(x)) {
		case "", "false", "0":
			return true
		}
		return false
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Slice, reflect.Map, reflect.Array:
		return rv.Len() == 0
	}
	return rv.IsZero()
}
