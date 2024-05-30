package settings

import (
	"embed"
	"log"
	"os"
	"path/filepath"
	"sync"

	"github.com/spf13/viper"
)

// SingletonSettings manages the singleton instance of the configuration settings
type SingletonSettings struct {
	viper *viper.Viper
}

var instance *SingletonSettings
var once sync.Once

//go:embed settings/*.toml
var settingsFiles embed.FS

// NewSingletonSettings initializes the singleton settings instance
func NewSingletonSettings() *SingletonSettings {
	once.Do(func() {

		settingsFiles := []string{
			"test_generation_prompt.toml",
			"language_extensions.toml",
			"analyze_suite_test_headers_indentation.toml",
			"analyze_suite_test_insert_line.toml",
		}

		v := viper.New()
		v.SetConfigType("toml")
		for _, file := range settingsFiles {
			fileContent, err := settingsFiles.ReadFile("settings/" + file)
			if err != nil {
				log.Fatalf("Failed to read embedded settings file %s: %v", file, err)
			}
			if _, err := os.Stat(configPath); os.IsNotExist(err) {
				log.Fatalf("Settings file not found: %s", configPath)
			}
			v.SetConfigFile(configPath)
			if err := v.MergeConfig(); err != nil {
				log.Fatalf("Error loading config file %s: %v", configPath, err)
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

// getBaseDir determines the base directory for bundled app or normal environment
func getBaseDir() (string, error) {
	if baseDir, exists := os.LookupEnv("_MEIPASS"); exists {
		return baseDir, nil
	}
	return filepath.Abs(filepath.Dir(os.Args[0]))
}
