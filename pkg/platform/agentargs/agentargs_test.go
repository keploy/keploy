package agentargs

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"go.keploy.io/server/v3/config"
)

// The agent resolves the root settings again from its own config, so each
// one travels with its value, false included: a false that stayed home could
// not override a keploy.yml that says true.
func TestRoot(t *testing.T) {
	for _, c := range []struct {
		name string
		set  func(*config.Config)
		want []string
	}{
		{
			name: "debug and disable-tele on, disable-ansi and disable-mapping off",
			set: func(conf *config.Config) {
				conf.Debug = true
				conf.DisableTele = true
				conf.DisableANSI = false
				conf.DisableMapping = false
			},
			want: []string{"--debug=true", "--disable-tele=true", "--disable-ansi=false", "--disable-mapping=false"},
		},
		{
			name: "every setting at keploy's default",
			set:  func(*config.Config) {},
			want: []string{"--debug=false", "--disable-tele=false", "--disable-ansi=false", "--disable-mapping=false"},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			conf := config.New()
			c.set(conf)
			assert.ElementsMatch(t, c.want, Root(conf))
		})
	}
}
