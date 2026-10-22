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
// row is deleted by the change that lands what replaces it: under the switch
// (keploy enterprise's default on Linux, KEPLOY_PROVEN=1 otherwise), its flags
// are unknown flags, which the command refuses, and its keploy.yml keys are
// ignored, with one notice for all of them.
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
	// here", §1 rule 4), and the rows exact serving (S12) replaced, which
	// the flip deletes (P220); keploy's enterprise build adds its own
	// (RegisterDeletedSettings). The other rows are their own changes'.
	//
	// The S12 rows were ignored under the switch, each said once per run,
	// while the switch was a staging gate. keploy enterprise runs the proven
	// model by default on Linux now, so under the switch they are deleted as
	// U18 says: unknown flags and ignored keys. The code that reads them stays
	// for every run with the switch off (keploy's open-source build unless
	// KEPLOY_PROVEN=1, keploy enterprise on Windows and macOS, P221, and
	// KEPLOY_PROVEN=0), where they work as before.
	deleted = []DeletedSetting{{
		Row:     "read by nothing",
		Flags:   []string{"fallBack-on-miss", "in-ci"},
		Keys:    []string{"test.fallBackOnMiss", "inCi"},
		Instead: "nothing reads it: remove it",
		Ignore: func(cfg *config.Config) {
			cfg.Test.FallBackOnMiss = false
			cfg.InCi = false
		},
	}, {
		Row:     "mappings always; only record writes them",
		Flags:   []string{"disable-mapping", "update-test-mapping"},
		Keys:    []string{"disableMapping", "test.updateTestMapping"},
		Instead: "each test case is served exactly the mocks its recorded mapping lists, and only keploy record writes mappings.yaml: record a test set whose mapping is stale again",
		Ignore: func(cfg *config.Config) {
			cfg.DisableMapping = false
			cfg.Test.UpdateTestMapping = false
		},
	}, {
		Row:     "nothing is pruned",
		Flags:   []string{"remove-unused-mocks"},
		Keys:    []string{"test.removeUnusedMocks", "test.preserveFailedMocks"},
		Instead: "a passing test leaves none of its own mocks unconsumed, and the pool's mocks are other runs' too, so keploy test deletes no mock: remove it",
		Ignore: func(cfg *config.Config) {
			cfg.Test.RemoveUnusedMocks = false
			cfg.Test.PreserveFailedMocks = false
		},
	}, {
		Row:     "exact consumption",
		Keys:    []string{"test.disableStatefulMocks"},
		Instead: "a test's own mocks answer once each, in recorded order, and the pool keeps its cursors: remove it",
		Ignore: func(cfg *config.Config) {
			cfg.Test.DisableStatefulMocks = false
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

// DeletedKeys are the keploy.yml settings, dotted, that the proven model
// deletes (U18) and keploy ignores under it: every deleted row's keys
// (RegisterDeletedSettings' included), the settings that made calls live
// (passthroughKeys) and test.healthPollTimeout (dropHealthPollTimeout).
// `keploy config defaults` leaves them out under the proven model.
func DeletedKeys() []string {
	var keys []string
	for _, row := range deletedSettings() {
		keys = append(keys, row.Keys...)
	}
	keys = append(keys, passthroughKeys...)
	return append(keys, healthPollTimeoutKey)
}

func deletedSettings() []DeletedSetting {
	deletedMu.RLock()
	defer deletedMu.RUnlock()
	return append([]DeletedSetting(nil), deleted...)
}

// checkDeletedSettings refuses, under the proven switch, a deleted flag the
// user gave, as the unknown flag it is there, and ignores the deleted
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
				return fmt.Errorf("unknown flag: --%s: the proven model deletes it (U18: %s); %s", name, row.Row, row.Instead)
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
		c.logger.Warn("settings the proven model deletes are ignored (U18)",
			zap.Strings("ignored", ignored),
			zap.String("next_step", "remove them from keploy.yml, or unset them in the environment"))
	}
	return nil
}

// hideDeletedFlags hides, under the proven switch, every flag of cmd that the
// switch refuses from its --help: the flags of the deleted settings, which
// checkDeletedSettings refuses as unknown; for keploy record and keploy test,
// the passthrough flags mock.real replaced (refusePassthroughFlags) and
// test's --health-poll-timeout (refuseHealthPollTimeout); for keploy mock
// record and replay, --from-container, a docker run (mock.ProvenRefusal), and
// for keploy mock replay --strict and --run-only, which mock.ProvenRefusal
// refuses too. --help must not offer a flag the command then refuses
// (addMockFlags says which values of --cmd-type and --on-miss the proven
// model takes). keploy mock record and replay keep their own
// --pass-through-ports, the app's ingress port. Without the switch --help is
// as it was. AddFlags calls it once cmd has all its flags.
func hideDeletedFlags(cmd *cobra.Command) {
	if !provenswitch.On() {
		return
	}
	hide := func(names ...string) {
		for _, name := range names {
			if cmd.Flags().Lookup(name) != nil {
				_ = cmd.Flags().MarkHidden(name)
			}
		}
	}
	for _, row := range deletedSettings() {
		hide(row.Flags...)
	}
	if cmd.Parent() != nil && cmd.Parent().Name() == "mock" {
		hide("from-container")
		if cmd.Name() == "replay" {
			hide("strict", "run-only")
		}
		return
	}
	switch cmd.Name() {
	case "test":
		hide("health-poll-timeout")
		fallthrough
	case "record":
		hide(passthroughFlags...)
	}
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
