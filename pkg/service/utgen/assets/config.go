// Package settings provides prompt settings for the test generation
package settings

import (
	"bytes"
	"embed"
	"log"
	"sync"

	"github.com/spf13/viper"
)

// SingletonSettings manages the singleton instance of the configuration settings
type SingletonSettings struct {
	viper *viper.Viper
}

var instance *SingletonSettings
var once sync.Once

//go:embed *.toml
var settings embed.FS

// NewSingletonSettings initializes the singleton settings instance
func NewSingletonSettings() *SingletonSettings {
	once.Do(func() {

		settingsFiles := []string{
			"test_generation.toml",
			"language.toml",
			"indentation.toml",
			"insert_line.toml",
			"refactor_prompt.toml",
		}

		v := viper.New()
		v.SetConfigType("toml")
		for _, file := range settingsFiles {
			fileContent, err := settings.ReadFile(file)
			if err != nil {
				log.Fatalf("Failed to read settings file %s: %v", file, err)
			}
			v.SetConfigFile(file)
			if err := v.MergeConfig(bytes.NewBuffer(fileContent)); err != nil {
				log.Fatalf("Error loading config file : %v", err)
			}
		}

		instance = &SingletonSettings{
			viper: v,
		}
	})
	return instance
}

// GetSettings returns the singleton settings instance
func GetSettings() *viper.Viper {
	return NewSingletonSettings().viper
}
