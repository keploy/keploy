package secrets

import (
	"os"

	"gopkg.in/yaml.v3"
)

// ConfigFileRules represents the secret protection rules that can be defined
// in the existing keploy.yaml config file under record.secretProtection.
//
// Example keploy.yaml:
//
//	record:
//	  secretProtection:
//	    customHeaders:
//	      - "X-Internal-Token"
//	    customBodyKeys:
//	      - "signing_secret"
//	    customURLParams:
//	      - "auth_code"
//	    customPatterns:
//	      - name: "Internal Token"
//	        pattern: "itk_[A-Za-z0-9]{32}"
//	    allowlist:
//	      - "header.X-Request-ID"
type ConfigFileRules struct {
	CustomHeaders   []string      `json:"customHeaders,omitempty" yaml:"customHeaders,omitempty"`
	CustomBodyKeys  []string      `json:"customBodyKeys,omitempty" yaml:"customBodyKeys,omitempty"`
	CustomURLParams []string      `json:"customURLParams,omitempty" yaml:"customURLParams,omitempty"`
	CustomPatterns  []CustomRegex `json:"customPatterns,omitempty" yaml:"customPatterns,omitempty"`
	Allowlist       []string      `json:"allowlist,omitempty" yaml:"allowlist,omitempty"`
}

// CustomRegex is a user-defined regex pattern for value-based secret detection.
type CustomRegex struct {
	Name    string `json:"name" yaml:"name"`
	Pattern string `json:"pattern" yaml:"pattern"`
}

// ParseConfigContent parses inline YAML content for secret protection rules
// (sent from the UI in K8s environments where the keploy.yaml isn't accessible).
func ParseConfigContent(content string) (*ConfigFileRules, error) {
	if content == "" {
		return nil, nil
	}
	var cfg ConfigFileRules
	if err := yaml.Unmarshal([]byte(content), &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// keployConfig is a minimal representation of keploy.yaml for extracting
// the secretProtection section. We only unmarshal what we need.
type keployConfig struct {
	Record struct {
		SecretProtection *ConfigFileRules `yaml:"secretProtection,omitempty"`
	} `yaml:"record"`
}

// LoadFromKeployConfig loads secret protection rules from the existing keploy.yaml
// config file. Returns nil if the file doesn't exist or has no secretProtection section.
func LoadFromKeployConfig(path string) (*ConfigFileRules, error) {
	if path == "" {
		path = os.Getenv("KEPLOY_CONFIG_PATH")
	}
	if path == "" {
		// Standard keploy config locations.
		candidates := []string{"keploy.yaml", "keploy.yml", ".keploy/keploy.yaml", ".keploy/keploy.yml"}
		for _, c := range candidates {
			if _, err := os.Stat(c); err == nil {
				path = c
				break
			}
		}
	}
	if path == "" {
		return nil, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var cfg keployConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return cfg.Record.SecretProtection, nil
}
