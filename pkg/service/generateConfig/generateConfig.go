package generateConfig

import (
	"os"
	"os/exec"
	"sync"

	"go.keploy.io/server/utils"
	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
)

var Emoji = "\U0001F430" + " Keploy:"

type generatorConfig struct {
	logger *zap.Logger
	mutex  sync.Mutex
}

func NewGeneratorConfig(logger *zap.Logger) GeneratorConfig {
	return &generatorConfig{
		logger: logger,
		mutex:  sync.Mutex{},
	}
}

var config = `
record:
  path: ""
  # mandatory
  command: ""
  proxyport: 0
  containerName: ""
  networkName: ""
  delay: 5
  buildDelay: 30s
  tests: 
    filters:
      - path: ""
        urlMethods: []
        headers: {}
        host: ""
  stubs:
    filters:
      - path: ""
        host: ""
        port: 0
test:
  path: ""
  # mandatory
  command: ""
  proxyport: 0
  containerName: ""
  networkName: ""
  # example: "test-set-1": ["test-1", "test-2", "test-3"]
  selectedTests: 
  # to use globalNoise, please follow the guide at the end of this file.
  globalNoise:
    global:
      body: {}
      header: {}
  delay: 5
  buildDelay: 30s
  apiTimeout: 5
  ignoreOrdering: false
  tests: 
    filters:
      - path: ""
        urlMethods: []
        headers: {}
        host: ""
  stubs:
    filters:
      - path: ""
        host: ""
        port: 0
  withCoverage: false
  coverageReportPath: ""
  # Example on using tests
  # tests: 
  #   filters:
  #    - path: "/user/app"
  #      urlMethods: ["GET"]
  #      headers: {
  #        "^asdf*": "^test"
  #      }
  #      host: "dc.services.visualstudio.com"
  # Example on using stubs
  # stubs: 
  #   filters:
  #    - path: "/user/app"
  #      port: 8080
  #    - port: 8081
  #    - host: "dc.services.visualstudio.com"
  #    - port: 8081
  #      host: "dc.services.visualstudio.com"
  #      path: "/user/app"
  #
  # Example on using globalNoise
  # globalNoise: 
  #    global:
  #      body: {
  #         # to ignore some values for a field, 
  #         # pass regex patterns to the corresponding array value
  #         "url": ["https?://\S+", "http://\S+"],
  #      }
  #      header: {
  #         # to ignore the entire field, pass an empty array
  #         "Date": [],
  #       }
  #     # to ignore fields or the corresponding values for a specific test-set,
  #     # pass the test-set-name as a key to the "test-sets" object and
  #     # populate the corresponding "body" and "header" objects 
  #     test-sets:
  #       test-set-1:
  #         body: {
  #           # ignore all the values for the "url" field
  #           "url": []
  #         }
  #         header: { 
  #           # we can also pass the exact value to ignore for a field
  #           "User-Agent": ["PostmanRuntime/7.34.0"]
  #         }
`

func (g *generatorConfig) GenerateConfig(filePath string) {
	var node yaml.Node
	data := []byte(config)
	if err := yaml.Unmarshal(data, &node); err != nil {
		g.logger.Fatal("Unmarshalling failed %s", zap.Error(err))
	}
	results, err := yaml.Marshal(node.Content[0])
	if err != nil {
		g.logger.Fatal("Failed to marshal the config", zap.Error(err))
	}

	finalOutput := append(results, []byte(utils.ConfigGuide)...)

	err = os.WriteFile(filePath, finalOutput, os.ModePerm)
	if err != nil {
		g.logger.Fatal("Failed to write config file", zap.Error(err))
	}

	cmd := exec.Command("sudo", "chmod", "-R", "777", filePath)
	err = cmd.Run()
	if err != nil {
		g.logger.Error("failed to set the permission of config file", zap.Error(err))
		return
	}

	g.logger.Info("Config file generated successfully")
}
