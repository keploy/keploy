package assets

import (
	"embed"
	"sync"

	"github.com/pelletier/go-toml"
)

var assets embed.FS

var (
	settingsOnce sync.Once
	settings     *toml.Tree
)

func GetSettings() *toml.Tree {
	settingsOnce.Do(func() {
		data, err := assets.ReadFile("ai_chat.toml")
		if err != nil {
			panic("failed to read embedded toml file: " + err.Error())
		}
		settings, err = toml.Load(string(data))
		if err != nil {
			panic("failed to parse embedded toml file: " + err.Error())
		}
	})
	return settings
}
