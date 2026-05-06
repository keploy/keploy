// Package provider provides functionality for the keploy provider.
package provider

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"

	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/fatih/color"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/agent/memoryguard"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/service/tools"
	"go.keploy.io/server/v3/utils"
	"go.keploy.io/server/v3/utils/log"
	"go.uber.org/zap"
)

func LogExample(example string) string {
	return fmt.Sprintf("Example usage: %s", example)
}

var CustomHelpTemplate = `
{{if .Example}}Examples:
{{.Example}}
{{end}}
{{if .HasAvailableSubCommands}}Guided Commands:{{range .Commands}}{{if .IsAvailableCommand}}
  {{rpad .Name .NamePadding }} {{.Short}}{{end}}{{end}}
{{end}}
{{if .HasAvailableFlags}}Flags:
{{.LocalFlags.FlagUsages | trimTrailingWhitespaces}}
{{end}}
Use "{{.CommandPath}} [command] --help" for more information about a command.
`

var WithoutexampleOneClickInstall = `
Note: If installed keploy without One Click Install, use "keploy example --customSetup true"
`
var Examples = `
Golang Application
	Record:
	keploy record -c "/path/to/user/app/binary"

	Test:
	keploy test -c "/path/to/user/app/binary" --delay 10

Node Application
	Record:
	keploy record -c “npm start --prefix /path/to/node/app"

	Test:
	keploy test -c “npm start --prefix /path/to/node/app" --delay 10

Java
	Record:
	keploy record -c "java -jar /path/to/java-project/target/jar"

	Test:
	keploy test -c "java -jar /path/to/java-project/target/jar" --delay 10

Docker
	Alias:
	alias keploy='sudo docker run --name keploy-ebpf -p 16789:16789 --privileged --pid=host -it -v $(pwd):$(pwd) -w $(pwd) -v /sys/fs/cgroup:/sys/fs/cgroup
	-v /sys/kernel/debug:/sys/kernel/debug -v /sys/fs/bpf:/sys/fs/bpf -v /var/run/docker.sock:/var/run/docker.sock --rm ghcr.io/keploy/keploy'

	Record:
	keploy record -c "docker run -p 8080:8080 --name <containerName> --network <networkName> <applicationImage>" --buildDelay 60

	Test:
	keploy test -c "docker run -p 8080:8080 --name <containerName> --network <networkName> <applicationImage>" --delay 10 --buildDelay 60

Note: On Linux, Keploy may prompt for sudo password when elevated privileges are required for eBPF operations.
`

var ExampleOneClickInstall = `
Golang Application
	Record:
	keploy record -c "/path/to/user/app/binary"

	Test:
	keploy test -c "/path/to/user/app/binary" --delay 10

Node Application
	Record:
	keploy record -c “npm start --prefix /path/to/node/app"

	Test:
	keploy test -c “npm start --prefix /path/to/node/app" --delay 10

Java
	Record:
	keploy record -c "java -jar /path/to/java-project/target/jar"

	Test:
	keploy test -c "java -jar /path/to/java-project/target/jar" --delay 10

Docker
	Record:
	keploy record -c "docker run -p 8080:8080 --name <containerName> --network <networkName> <applicationImage>" --buildDelay 60

	Test:
	keploy test -c "docker run -p 8080:8080 --name <containerName> --network <networkName> <applicationImage>" --delay 1 --buildDelay 60
`

var RootCustomHelpTemplate = `{{.Short}}

Usage:{{if .Runnable}}
  {{.UseLine}}{{end}}{{if .HasAvailableSubCommands}}
  {{.CommandPath}} [command]{{end}}{{if gt (len .Aliases) 0}}

Aliases:
  {{.NameAndAliases}}{{end}}{{if .HasExample}}

Available Commands:{{range .Commands}}{{if .IsAvailableCommand}}
  {{rpad .Name .NamePadding }} {{.Short}}{{end}}{{end}}{{end}}{{if .HasAvailableFlags}}

Flags:
{{.LocalFlags.FlagUsages | trimTrailingWhitespaces}}{{end}}{{if .HasAvailableLocalFlags}}

Guided Commands:{{range .Commands}}{{if and (not .IsAvailableCommand) (not .Hidden)}}
  {{rpad .Name .NamePadding }} {{.Short}}{{end}}{{end}}

Examples:
{{.Example}}

Use "{{.CommandPath}} [command] --help" for more information about a command.{{end}}
`

var RootExamples = `
  Record:
	keploy record -c "docker run -p 8080:8080 --name <containerName> --network keploy-network <applicationImage>" --container-name "<containerName>" --buildDelay 60

  Test:
	keploy test --c "docker run -p 8080:8080 --name <containerName> --network keploy-network <applicationImage>" --delay 10 --buildDelay 60

  Config:
	keploy config --generate -p "/path/to/localdir"
`

var VersionTemplate = `{{with .Version}}{{printf "Keploy %s" .}}{{end}}{{"\n"}}`
var IsConfigFileFound = true

type CmdConfigurator struct {
	logger *zap.Logger
	cfg    *config.Config
}

func NewCmdConfigurator(logger *zap.Logger, config *config.Config) *CmdConfigurator {
	return &CmdConfigurator{
		logger: logger,
		cfg:    config,
	}
}

func (c *CmdConfigurator) AddFlags(cmd *cobra.Command) error {
	//sets the displayment of flag-related errors
	cmd.SilenceErrors = true
	cmd.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		PrintLogo(os.Stdout, true)
		color.Red(fmt.Sprintf("❌ error: %v", err))
		fmt.Println()
		return err
	})

	//add flags
	var err error
	cmd.Flags().SetNormalizeFunc(aliasNormalizeFunc)
	cmd.Flags().String("configPath", ".", "Path to the local directory where keploy configuration file is stored")

	switch cmd.Name() {
	case "generate", "download":
		cmd.Flags().StringSliceP("services", "s", c.cfg.Contract.Services, "Specify the services for which to generate/download contracts")
		cmd.Flags().StringSliceP("tests", "t", c.cfg.Contract.Tests, "Specify the tests for which to generate/download contracts")
		cmd.Flags().StringP("path", "p", ".", "Specify the path to generate/download contracts")
		if cmd.Name() == "download" { // for downloading contracts
			cmd.Flags().String("driven", c.cfg.Contract.Driven, "Specify the path to download contracts")
		}

	case "update", "export", "import":
		return nil
	case "postman":
		cmd.Flags().StringP("path", "p", "", "Specify the path to the postman collection")
		cmd.Flags().String("base-path", c.cfg.Test.BasePath, "basePath to hit the server while importing keploy tests from postman collection with no response in the collection")
	case "normalize":
		cmd.Flags().StringP("path", "p", ".", "Path to local directory where generated testcases/mocks/reports are stored")
		cmd.Flags().String("test-run", "", "Test Run to be normalized")
		cmd.Flags().String("tests", "", "Test Sets to be normalized")
		cmd.Flags().Bool("allow-high-risk", false, "Allow normalization of high-risk test failures")
	case "config":
		cmd.Flags().StringP("path", "p", ".", "Path to local directory where generated config is stored")
		cmd.Flags().Bool("generate", false, "Generate a new keploy configuration file")
	case "templatize":
		cmd.Flags().StringP("path", "p", ".", "Path to local directory where generated testcases/mocks are stored")
		cmd.Flags().StringSliceP("testsets", "t", c.cfg.Templatize.TestSets, "Testsets to run e.g. --testsets \"test-set-1, test-set-2\"")
	case "record", "test":
		if cmd.Parent() != nil && cmd.Parent().Name() == "contract" {
			cmd.Flags().StringSliceP("services", "s", c.cfg.Contract.Services, "Specify the services for which to generate contracts")
			cmd.Flags().StringP("path", "p", ".", "Specify the path to generate contracts")
			cmd.Flags().Bool("download", true, "Specify whether to download contracts or not")
			cmd.Flags().Bool("generate", true, "Specify whether to generate schemas for the current service or not")
			cmd.Flags().String("driven", c.cfg.Contract.Driven, "Specify the driven flag to validate contracts")
			return nil
		}

		cmd.Flags().Bool("sync", c.cfg.Record.Synchronous, "Synchronous recording of testcases")
		cmd.Flags().Bool("global-passthrough", false, "Allow all outgoing calls to be mocked if set to true")
		cmd.Flags().StringP("path", "p", ".", "Path to local directory where generated testcases/mocks are stored")
		cmd.Flags().Uint32("proxy-port", c.cfg.ProxyPort, "Port used by the Keploy proxy server to intercept the outgoing dependency calls")
		cmd.Flags().Uint16("incoming-proxy-port", c.cfg.IncomingProxyPort, "Port used by the Keploy proxy server to intercept the incoming dependency calls")
		cmd.Flags().Uint32("server-port", c.cfg.ServerPort, "Port used by the Keploy Agent server to intercept traffic")
		cmd.Flags().Uint32("dns-port", c.cfg.DNSPort, "Port used by the Keploy DNS server to intercept the DNS queries")
		cmd.Flags().StringP("command", "c", c.cfg.Command, "Command to start the user application")
		cmd.Flags().String("cmd-type", c.cfg.CommandType, "Type of command to start the user application (native/docker/docker-compose)")
		cmd.Flags().Uint64P("build-delay", "b", c.cfg.BuildDelay, "User provided time to wait docker container build")
		cmd.Flags().String("container-name", c.cfg.ContainerName, "Name of the application's docker container")
		cmd.Flags().StringP("network-name", "n", c.cfg.NetworkName, "Name of the application's docker network")
		cmd.Flags().UintSlice("pass-through-ports", config.GetByPassPorts(c.cfg), "Ports to bypass the proxy server and ignore the traffic")
		cmd.Flags().String("app-name", c.cfg.AppName, "Name of the user's application")
		cmd.Flags().MarkDeprecated("app-id", "DEPRICATED : was used for unique name for the user's application")
		cmd.Flags().Bool("generate-github-actions", c.cfg.GenerateGithubActions, "Generate Github Actions workflow file")
		cmd.Flags().String("keploy-container", c.cfg.KeployContainer, "Keploy server container name")
		cmd.Flags().Bool("in-ci", c.cfg.InCi, "is CI Running or not")

		// add rest of the uncommon flags for record and test commands
		c.AddUncommonFlags(cmd)

	case "report":
		cmd.Flags().StringSliceP("test-sets", "t", utils.Keys(c.cfg.Test.SelectedTests), "Testsets to report e.g. --testsets \"test-set-1, test-set-2\"")
		cmd.Flags().StringP("path", "p", ".", "Path to local directory where generated testcases/mocks are stored")
		cmd.Flags().StringP("report-path", "r", "", "Absolute path to a report file")
		cmd.Flags().Bool("full", false, "Show full diffs (colorized for JSON) instead of compact table diff")
		cmd.Flags().Bool("summary", false, "Print only the summary of the test run (optionally restrict with --test-sets)")
		cmd.Flags().StringSlice("test-case", nil, "Filter to specific test case IDs (repeat or comma-separated). Alias: --tc")
		cmd.Flags().String("format", "text", "Output format for test report (text or junit)")
	case "diff":
		cmd.Flags().String("run1", "", "First test run ID to compare")
		cmd.Flags().String("run2", "", "Second test run ID to compare")
		cmd.Flags().StringSliceP("test-sets", "t", utils.Keys(c.cfg.Test.SelectedTests), "Test-sets to compare e.g. --test-sets \"test-set-1, test-set-2\"")
		cmd.Flags().StringP("path", "p", ".", "Path to local directory where generated testcases/mocks are stored")
	case "sanitize":
		cmd.Flags().StringSliceP("test-sets", "t", utils.Keys(c.cfg.Test.SelectedTests), "Testsets to sanitize e.g. -t \"test-set-1, test-set-2\"")
		cmd.Flags().StringP("path", "p", ".", "Path to local directory where generated testcases/mocks are stored")

	case "keploy":
		cmd.PersistentFlags().Bool("debug", c.cfg.Debug, "Run in debug mode")
		cmd.PersistentFlags().Bool("disable-tele", c.cfg.DisableTele, "Run in telemetry mode")
		cmd.PersistentFlags().Bool("disable-ansi", c.cfg.DisableANSI, "Disable ANSI color in logs")
		cmd.PersistentFlags().Bool("json", c.cfg.JSONOutput, "Print output in JSON format")
		err = cmd.PersistentFlags().MarkHidden("disable-tele")
		if err != nil {
			errMsg := "failed to mark telemetry as hidden flag"
			utils.LogError(c.logger, err, errMsg)
			return errors.New(errMsg)
		}
		cmd.PersistentFlags().Bool("enable-testing", c.cfg.EnableTesting, "Enable testing keploy with keploy")
		err = cmd.PersistentFlags().MarkHidden("enable-testing")
		if err != nil {
			errMsg := "failed to mark enableTesting as hidden flag"
			utils.LogError(c.logger, err, errMsg)
			return errors.New(errMsg)
		}
	case "agent":
		cmd.Flags().Bool("is-docker", c.cfg.Agent.IsDocker, "Flag to check if the application is running in docker")
		cmd.Flags().Uint32("port", c.cfg.Agent.AgentPort, "Port used by the Keploy agent to communicate with Keploy's clients")
		cmd.Flags().Uint32("client-pid", 0, "must be provided (pgid of the keploy client)")
		cmd.Flags().Uint32("proxy-port", c.cfg.Agent.ProxyPort, "Port used by the Keploy proxy server to intercept the outgoing dependency calls")
		cmd.Flags().Uint16("incoming-proxy-port", c.cfg.Agent.IncomingProxyPort, "Port used by the Keploy proxy server to intercept the incoming dependency calls")
		cmd.Flags().Uint32("dns-port", c.cfg.Agent.DnsPort, "Port used by the Keploy DNS server to intercept the DNS queries")
		cmd.Flags().Bool("enable-testing", c.cfg.Agent.EnableTesting, "Enable testing keploy with keploy")
		cmd.Flags().String("mode", string(c.cfg.Agent.Mode), "Mode of operation for Keploy (record or test)")
		cmd.Flags().Bool("sync", c.cfg.Agent.Synchronous, "Synchronous recording of testcases")
		// Mirrors the root-level --disable-mapping switch so that the
		// docker-compose / k8s sidecar agent (which reads its config
		// from CLI args, not from the host's keploy.yml — the config
		// directory isn't bind-mounted into the agent container) can
		// honour the operator's choice and gate ResolveRange's
		// TestMockMapping emission accordingly.
		cmd.Flags().Bool("disable-mapping", c.cfg.DisableMapping, "Disable test-mock mapping production during synchronous record")
		cmd.Flags().Int("enable-sampling", c.cfg.Agent.EnableSampling, "Enable sampling of testcases recording")
		cmd.Flags().Lookup("enable-sampling").NoOptDefVal = "5"
		cmd.Flags().Uint64("memory-limit", c.cfg.Agent.MemoryLimit, "Memory limit for the keploy-agent container in MB")
		cmd.Flags().Bool("global-passthrough", c.cfg.Agent.GlobalPassthrough, "Allow all outgoing calls to be mocked if set to true")
		cmd.Flags().Uint64P("build-delay", "b", c.cfg.Agent.BuildDelay, "User provided time to wait docker container build")
		cmd.Flags().UintSlice("pass-through-ports", c.cfg.Agent.PassThroughPorts, "Ports to bypass the proxy server and ignore the traffic")
		// --ca-java-home is the manual override for the app-aware Java
		// truststore install. When the auto-detector in
		// pkg/agent/proxy/tls/java_detect.go cannot resolve the app's JDK
		// (exotic launcher, containerised re-exec), operators can point
		// at the correct JDK with --ca-java-home=/path/to/jdk. Empty
		// string = auto-detect from /proc/<client-pid>/{environ,exe}.
		cmd.Flags().String("ca-java-home", c.cfg.Agent.CAJavaHome, "Override JAVA_HOME for Keploy CA truststore install (auto-detected from /proc/<client-pid> by default)")
		// Mirror the orchestrator-side --max-memory-per-conn / --queue-size
		// flags on the agent command so containerised agents
		// (docker-compose, k8s sidecar) honour values forwarded via argv
		// — the host's keploy.yml is not bind-mounted into the agent
		// container, so argv is the only propagation channel here.
		// Hidden: only relevant when the operator already knows they
		// need to bump these (saw drops with reason per_conn_cap /
		// channel_full). See pkg/agent/proxy/relay/config.go.
		cmd.Flags().Uint64("max-memory-per-conn", c.cfg.Record.RecordBuffer.MaxMemoryPerConnection, "Bytes; per-connection recording buffer cap (default 64MiB).")
		cmd.Flags().Int("queue-size", c.cfg.Record.RecordBuffer.QueueSize, "Number of chunk slots in the recording queue (default 1024).")
		_ = cmd.Flags().MarkHidden("max-memory-per-conn")
		_ = cmd.Flags().MarkHidden("queue-size")

	default:
		return errors.New("unknown command name")
	}

	return nil
}

func (c *CmdConfigurator) AddUncommonFlags(cmd *cobra.Command) {
	switch cmd.Name() {
	case "record":
		cmd.Flags().Duration("record-timer", 0, "User provided time to record its application (e.g., \"5s\" for 5 seconds, \"1m\" for 1 minute)")
		cmd.Flags().String("base-path", c.cfg.Record.BasePath, "Base URL to hit the server while recording the testcases")
		cmd.Flags().Int("enable-sampling", c.cfg.Record.EnableSampling, "Enable sampling of testcases")
		cmd.Flags().Lookup("enable-sampling").NoOptDefVal = "5"
		cmd.Flags().Uint64("memory-limit", c.cfg.Record.MemoryLimit, "Memory limit for the keploy-agent container in MB")
		cmd.Flags().String("metadata", c.cfg.Record.Metadata, "Metadata to be stored in config.yaml as key-value pairs (e.g., \"key1=value1,key2=value2\")")
		cmd.Flags().String("tls-private-key-path", c.cfg.Record.TLSPrivateKeyPath, "Path to the private key for TLS connection")
		cmd.Flags().Bool("capture-packets", c.cfg.Record.CapturePackets, "Capture raw network packets on the proxy ports and write a pcap file into each test-set directory")
		cmd.Flags().Bool("opportunistic-tls-intercept", c.cfg.Record.OpportunisticTLSIntercept, "Sniff and hijack TLS connections in passthrough mode. Bytes flow verbatim between app and upstream until a TLS ClientHello is seen; the proxy then MITM-terminates both halves so the captured pcap is decryptable. Independent of --global-passthrough.")
		// Advanced record-buffer tuning. Hidden from --help: only relevant
		// when the operator already knows they need to bump these (saw
		// per_conn_cap / channel_full drops in agent logs). Env vars
		// KEPLOY_RECORD_MAX_MEMORY_PER_CONN and KEPLOY_RECORD_QUEUE_SIZE
		// override these flags.
		cmd.Flags().Uint64("max-memory-per-conn", c.cfg.Record.RecordBuffer.MaxMemoryPerConnection, "Bytes; per-connection recording buffer cap (default 64MiB). Bump if you see per_conn_cap drops.")
		cmd.Flags().Int("queue-size", c.cfg.Record.RecordBuffer.QueueSize, "Number of chunk slots in the recording queue (default 1024). Bump if you see channel_full drops.")
		_ = cmd.Flags().MarkHidden("max-memory-per-conn")
		_ = cmd.Flags().MarkHidden("queue-size")
	case "test":
		cmd.Flags().StringSliceP("test-sets", "t", utils.Keys(c.cfg.Test.SelectedTests), "Testsets to run e.g. --testsets \"test-set-1, test-set-2\"")
		cmd.Flags().String("host", c.cfg.Test.Host, "Custom host to replace the actual host in the testcases")
		cmd.Flags().Uint32("port", c.cfg.Test.Port, "Custom http port to replace the actual port in the testcases")
		cmd.Flags().Uint32("grpc-port", c.cfg.Test.GRPCPort, "Custom grpc port to replace the actual port in the testcases")
		cmd.Flags().Uint32("sse-port", c.cfg.Test.SSEPort, "Custom SSE port to replace the actual port in the SSE testcases")
		cmd.Flags().Uint64P("delay", "d", 5, "User provided time to run its application")
		cmd.Flags().String("health-url", c.cfg.Test.HealthURL, "HTTP(S) URL polled before the first test is fired; first 2xx response proceeds immediately. Empty (default) preserves the fixed --delay behavior.")
		cmd.Flags().Duration("health-poll-timeout", c.cfg.Test.HealthPollTimeout, "Ceiling for --health-url polling (e.g. 60s, 2m). If no 2xx is seen within this window, replay logs an info message and falls back to --delay.")
		cmd.Flags().String("proto-file", c.cfg.Test.ProtoFile, "Path of main proto file")
		cmd.Flags().String("proto-dir", c.cfg.Test.ProtoDir, "Path of the directory where all protos of a service are located")
		cmd.Flags().StringArray("proto-include", c.cfg.Test.ProtoInclude, "Path of directories to be included while parsing import statements in proto files")
		cmd.Flags().Uint64("api-timeout", c.cfg.Test.APITimeout, "User provided timeout for calling its application")
		// Default mirrors keploy.yml's `disableMapping` (zero-value false → mapping
		// enabled). Hardcoding the default to true silently disabled mapping-based
		// mock filtering in test mode even when mappings.yaml was correctly
		// produced during record, forcing replay onto the brittle timestamp-window
		// path that loses tightly-spaced per-test mocks (listmonk-postgres
		// pipeline 604, 3/38 tests with ~117 µs boundary races on the
		// session-lookup query). determineMockingStrategy already falls back to
		// timestamp-based filtering when no mappings.yaml exists, so flipping
		// the default does not break recordings without mapping data.
		cmd.Flags().Bool("disable-mapping", c.cfg.DisableMapping, "Disable mapping of testcases during test mode")
		cmd.Flags().Bool("retry-passing-test", c.cfg.RetryPassing, "Enable retry passing test mode")
		cmd.Flags().Bool("disableAutoHeaderNoise", c.cfg.Test.DisableAutoHeaderNoise, "Disable automatic noise for flaky headers (e.g. AWS SigV4: Authorization, X-Amz-Date, X-Amz-Security-Token) during mock matching")
		cmd.Flags().String("mongo-password", c.cfg.Test.MongoPassword, "Authentication password for mocking MongoDB conn")
		cmd.Flags().String("coverage-report-path", c.cfg.Test.CoverageReportPath, "Write a go coverage profile to the file in the given directory.")
		cmd.Flags().VarP(&c.cfg.Test.Language, "language", "l", "Application programming language")
		cmd.Flags().Bool("ignore-ordering", c.cfg.Test.IgnoreOrdering, "Ignore ordering of array in response")
		cmd.Flags().Bool("skip-coverage", c.cfg.Test.SkipCoverage, "skip code coverage computation while running the test cases")
		cmd.Flags().Bool("remove-unused-mocks", c.cfg.Test.RemoveUnusedMocks, "Clear the unused mocks for the passed test-sets")
		cmd.Flags().Bool("fallBack-on-miss", c.cfg.Test.FallBackOnMiss, "[DEPRECATED] This flag is ignored. Replay is now always deterministic.")
		_ = cmd.Flags().MarkDeprecated("fallBack-on-miss", "replay is now always deterministic; this flag is ignored")
		cmd.Flags().String("jacoco-agent-path", c.cfg.Test.JacocoAgentPath, "Only applicable for test coverage for Java projects. You can override the jacoco agent jar by proving its path")
		cmd.Flags().String("base-path", c.cfg.Test.BasePath, "Custom api basePath/origin to replace the actual basePath/origin in the testcases; App flag is ignored and app will not be started & instrumented when this is set since the application running on a different machine")
		cmd.Flags().Bool("update-template", c.cfg.Test.UpdateTemplate, "Update the template with the result of the testcases.")
		cmd.Flags().Bool("mocking", true, "enable/disable mocking for the testcases")
		cmd.Flags().Bool("disable-line-coverage", c.cfg.Test.DisableLineCoverage, "Disable line coverage generation.")
		cmd.Flags().Bool("must-pass", c.cfg.Test.MustPass, "enforces that the tests must pass, if it doesn't, remove failing testcases")
		cmd.Flags().Uint32Var(&c.cfg.Test.MaxFailAttempts, "max-fail-attempts", 5, "maximum number of testset failure that can be allowed during must-pass mode")
		cmd.Flags().Uint32Var(&c.cfg.Test.MaxFlakyChecks, "flaky-check-retry", 1, "maximum number of retries to check for flakiness")
		cmd.Flags().Bool("compare-all", false, "Compare all response body types including non-JSON (default: false, only JSON bodies are compared)")
		cmd.Flags().Bool("schema-match", false, "Compare only the schema of the response body")
		cmd.Flags().Bool("update-test-mapping", c.cfg.Test.UpdateTestMapping, "Update the mapping of testcases")
	}
}

func aliasNormalizeFunc(_ *pflag.FlagSet, name string) pflag.NormalizedName {
	var flagNameMapping = map[string]string{
		"testsets":               "test-sets",
		"fullBody":               "full",
		"reportPath":             "report-path",
		"tc":                     "test-case",
		"delay":                  "delay",
		"apiTimeout":             "api-timeout",
		"mongoPassword":          "mongo-password",
		"tlsPrivateKeyPath":      "tls-private-key-path",
		"coverageReportPath":     "coverage-report-path",
		"language":               "language",
		"ignoreOrdering":         "ignore-ordering",
		"coverage":               "coverage",
		"removeUnusedMocks":      "remove-unused-mocks",
		"goCoverage":             "go-coverage",
		"fallBackOnMiss":         "fallBack-on-miss",
		"basePath":               "base-path",
		"updateTemplate":         "update-template",
		"mocking":                "mocking",
		"configPath":             "config-path",
		"path":                   "path",
		"port":                   "port",
		"grpcPort":               "grpc-port",
		"ssePort":                "sse-port",
		"proxyPort":              "proxy-port",
		"incomingProxyPort":      "incoming-proxy-port",
		"dnsPort":                "dns-port",
		"command":                "command",
		"cmdType":                "cmd-type",
		"buildDelay":             "build-delay",
		"containerName":          "container-name",
		"networkName":            "network-name",
		"passThroughPorts":       "pass-through-ports",
		"memoryLimit":            "memory-limit",
		"maxMemoryPerConnection": "max-memory-per-conn",
		"queueSize":              "queue-size",
		"appId":                  "app-id",
		"appName":                "app-name",
		"generateGithubActions":  "generate-github-actions",
		"disableTele":            "disable-tele",
		"disableANSI":            "disable-ansi",
		"jsonOutput":             "json",
		"selectedTests":          "selected-tests",
		"testReport":             "test-report",
		"enableTesting":          "enable-testing",
		"inDocker":               "in-docker",
		"keployContainer":        "keploy-container",
		"keployNetwork":          "keploy-network",
		"recordTimer":            "record-timer",
		"healthUrl":              "health-url",
		"healthPollTimeout":      "health-poll-timeout",
		"urlMethods":             "url-methods",
		"inCi":                   "in-ci",
		"protoFile":              "proto-file",
		"protoDir":               "proto-dir",
		"protoInclude":           "proto-include",
		"allowHighRisk":          "allow-high-risk",
		"disableMapping":         "disable-mapping",
		"compareAll":             "compare-all",
		"schemaMatch":            "schema-match",
		"updateTestMapping":      "update-test-mapping",
		"capturePackets":         "capture-packets",
		"opportunisticTlsIntercept": "opportunistic-tls-intercept",
	}

	if newName, ok := flagNameMapping[name]; ok {
		name = newName
	}
	return pflag.NormalizedName(name)
}

func (c *CmdConfigurator) Validate(ctx context.Context, cmd *cobra.Command) error {
	err := isCompatible(c.logger)
	if err != nil {
		return err
	}
	defaultCfg := *c.cfg
	err = c.PreProcessFlags(cmd)
	if err != nil {
		c.logger.Error("failed to preprocess flags", zap.Error(err))
		return err
	}
	err = c.ValidateFlags(ctx, cmd)
	if err != nil {
		if err == c.noCommandError() {
			utils.LogError(c.logger, nil, "missing required -c flag or appCmd in config file")
			if c.cfg.InDocker {
				c.logger.Info(`Example usage: keploy test -c "docker run -p 8080:8080 --network myNetworkName myApplicationImageName" --delay 6`)
			} else {
				c.logger.Info(LogExample(RootExamples))
			}
		}
		c.logger.Error("failed to validate flags", zap.Error(err))
		return err
	}

	appName, err := utils.GetLastDirectory()
	if err != nil {
		return fmt.Errorf("failed to get the last directory for appName: %v", err)
	}

	if c.cfg.AppName == "" {
		c.logger.Debug("Using the last directory name as appName : " + appName)
		c.cfg.AppName = appName
	} else if c.cfg.AppName != appName {
		c.logger.Info("AppName in config (" + c.cfg.AppName + ") does not match current directory name (" + appName + ")")
	}

	// The "create config file if missing" behavior is meaningful
	// only for user-facing commands (record/test/etc.) that are
	// expected to persist keploy.yml for reuse across invocations.
	// The `agent` subcommand is a worker process spawned by the
	// parent keploy: it still reads an existing keploy.yml via
	// viper.ReadInConfig() in PreProcessFlags to pick up the same
	// settings the parent resolved, but it has no use for writing
	// a fresh one if the file is missing — the parent has already
	// handed it the effective config via CLI flags + env. Running
	// CreateConfigFile from the agent therefore produces a file
	// no one reads; worse, on containerised runs the host's
	// absolute --config-path doesn't resolve inside the agent's
	// filesystem and the call fails with a misleading ENOENT
	// "failed to write config file" ERROR (kafka-ecommerce CI
	// run 2541/10). Skip the create-on-missing branch for the
	// agent subcommand specifically.
	if !IsConfigFileFound && cmd.Name() != "agent" {
		err := c.CreateConfigFile(ctx, defaultCfg)
		if err != nil {
			c.logger.Error("failed to create config file", zap.Error(err))
			return err
		}
	}
	return nil
}

func (c *CmdConfigurator) PreProcessFlags(cmd *cobra.Command) error {
	// 1) Bind flags (highest precedence in Viper)
	// used to bind common flags for commands like record, test. For eg: PATH, PORT, COMMAND etc.
	if err := viper.BindPFlags(cmd.Flags()); err != nil {
		errMsg := "failed to bind flags to config"
		utils.LogError(c.logger, err, errMsg)
		return errors.New(errMsg)
	}

	// 2) Env: KEPLOY_*
	viper.AutomaticEnv()
	viper.SetEnvPrefix("KEPLOY")

	// 3) Nested flag binding (your existing util)
	if err := utils.BindFlagsToViper(c.logger, cmd, ""); err != nil {
		errMsg := "failed to bind cmd specific flags to viper"
		utils.LogError(c.logger, err, errMsg)
		return errors.New(errMsg)
	}

	// 4) Use provided configPath and convert to absolute path
	configPath, err := cmd.Flags().GetString("config-path")
	if err != nil {
		utils.LogError(c.logger, nil, "failed to read the config path")
		return err
	}

	// Convert to absolute path to ensure viper can find the config file correctly
	absConfigPath, err := utils.GetAbsPath(configPath)
	if err != nil {
		errMsg := fmt.Sprintf("failed to convert config path to absolute path: %v", err)
		utils.LogError(c.logger, err, errMsg)
		return errors.New(errMsg)
	}
	configPath = absConfigPath

	c.logger.Debug("config path is ", zap.String("configPath", configPath))

	// 5) Read base keploy.yml exactly like before
	viper.SetConfigName("keploy")
	viper.SetConfigType("yml")
	viper.AddConfigPath(configPath)

	if err := viper.ReadInConfig(); err != nil {
		var notFound viper.ConfigFileNotFoundError
		if !errors.As(err, &notFound) {
			errMsg := "failed to read config file"
			utils.LogError(c.logger, err, errMsg)
			return errors.New(errMsg)
		}
		IsConfigFileFound = false
		c.logger.Debug("config file not found; proceeding with flags only")
	} else {
		// 6) Base exists → try merging <last-dir>.keploy.yml (override) from the application folder (current working directory)
		lastDir, err := utils.GetLastDirectory()
		if err != nil {
			errMsg := "failed to get last directory name for override config file"
			utils.LogError(c.logger, err, errMsg)
			return errors.New(errMsg)
		}

		// Get current working directory (application folder) for override file
		appDir, err := os.Getwd()
		if err != nil {
			errMsg := "failed to get current working directory for override config file"
			utils.LogError(c.logger, err, errMsg)
			return errors.New(errMsg)
		}

		// overridePath is <appDir>/<lastDir>.keploy.yml (in application folder, not configPath)
		overridePath := filepath.Join(appDir, fmt.Sprintf("%s.keploy.yml", lastDir))

		if _, statErr := os.Stat(overridePath); statErr == nil {
			viper.SetConfigFile(overridePath)
			if err := viper.MergeInConfig(); err != nil {
				errMsg := fmt.Sprintf("failed to merge override config file: %s", overridePath)
				utils.LogError(c.logger, err, errMsg)
				return errors.New(errMsg)
			}
			c.logger.Info("merged override config file", zap.String("file", overridePath))
		} else if !errors.Is(statErr, os.ErrNotExist) {
			errMsg := fmt.Sprintf("failed to stat override config file: %s", overridePath)
			utils.LogError(c.logger, statErr, errMsg)
			return errors.New(errMsg)
		}
		IsConfigFileFound = true
	}

	// 7) Unmarshal
	if err := viper.Unmarshal(c.cfg); err != nil {
		errMsg := "failed to unmarshal the config"
		utils.LogError(c.logger, err, errMsg)
		return errors.New(errMsg)
	}

	// 8) Persist the path used
	c.cfg.ConfigPath = configPath
	return nil
}

func (c *CmdConfigurator) ValidateFlags(ctx context.Context, cmd *cobra.Command) error {
	// The --json flag isn't registered on every subcommand (record / agent
	// don't define it in enterprise builds), so Lookup + fallback avoids
	// an early-return that would skip every subsequent validation step
	// below (including the agent-mode wiring that the agent subprocess
	// needs — without it, the agent starts up with mode="").
	jsonOutput := false
	if cmd.Flags().Lookup("json") != nil {
		if v, err := cmd.Flags().GetBool("json"); err == nil {
			jsonOutput = v
		} else {
			utils.LogError(c.logger, err, "failed to get the json flag")
		}
	}
	c.cfg.JSONOutput = jsonOutput

	// In JSON mode, redirect logs to stderr so they don't contaminate JSON on stdout
	if c.cfg.JSONOutput {
		logger, err := log.RedirectToStderr()
		if err == nil {
			*c.logger = *logger
		}
	}

	disableAnsi, _ := (cmd.Flags().GetBool("disable-ansi"))
	// Skip printing logo for agent command to avoid duplicate logos in native mode
	if cmd.Name() != "agent" && !c.cfg.JSONOutput {
		PrintLogo(os.Stdout, disableAnsi)
	}
	if c.cfg.Debug {
		logger, err := log.ChangeLogLevel(zap.DebugLevel)
		*c.logger = *logger
		if err != nil {
			errMsg := "failed to change log level"
			utils.LogError(c.logger, err, errMsg)
			return errors.New(errMsg)
		}
	}

	if c.cfg.Record.BasePath != "" {
		port, err := pkg.ExtractPort(c.cfg.Record.BasePath)
		if err != nil {
			errMsg := "failed to extract port from base URL"
			utils.LogError(c.logger, err, errMsg)
			return errors.New(errMsg)
		}
		c.cfg.Port = port
		c.cfg.E2E = true
	}

	// Add mode to logger for agent command to differentiate agent logs from client logs
	if cmd.Name() == "agent" {
		logger, err := log.AddMode(cmd.Name())
		*c.logger = *logger
		if err != nil {
			errMsg := "failed to add mode to logger"
			utils.LogError(c.logger, err, errMsg)
			return errors.New(errMsg)
		}
	}

	if c.cfg.EnableTesting {
		// Add mode to logger to debug the keploy during testing
		if cmd.Name() != "agent" { // Skip if already added for agent
			logger, err := log.AddMode(cmd.Name())
			*c.logger = *logger
			if err != nil {
				errMsg := "failed to add mode to logger"
				utils.LogError(c.logger, err, errMsg)
				return errors.New(errMsg)
			}
		}
		c.cfg.DisableTele = true
	}

	if c.cfg.DisableANSI {
		logger, err := log.ChangeColorEncoding()
		models.IsAnsiDisabled = true
		*c.logger = *logger
		if err != nil {
			errMsg := "failed to change color encoding"
			utils.LogError(c.logger, err, errMsg)
			return errors.New(errMsg)
		}
		c.logger.Info("Color encoding is disabled")
	}

	if cmd.Name() == "test" {
		schemaMatch, _ := cmd.Flags().GetBool("schema-match")
		if schemaMatch {
			// since schemaMatch is not being set in the config from the flag, we are setting it here
			c.cfg.Test.SchemaMatch = schemaMatch
		}
	}

	c.logger.Debug("config has been initialised", zap.Any("for cmd", cmd.Name()), zap.Any("config", c.cfg))

	switch cmd.Name() {

	case "report":
		path, err := cmd.Flags().GetString("path")
		if err != nil {
			errMsg := "failed to get the path"
			utils.LogError(c.logger, err, errMsg)
			return errors.New(errMsg)
		}
		c.cfg.Path = utils.ToAbsPath(c.logger, path)

		testSets, err := cmd.Flags().GetStringSlice("test-sets")
		if err != nil {
			errMsg := "failed to get the test-sets"
			utils.LogError(c.logger, err, errMsg)
			return errors.New(errMsg)
		}
		config.SetSelectedTestSets(c.cfg, testSets)

		reportPath, err := cmd.Flags().GetString("report-path")
		if err != nil {
			errMsg := "failed to get the report path"
			utils.LogError(c.logger, err, errMsg)
			return errors.New(errMsg)
		}

		// validate the report path if provided
		if reportPath != "" {

			//convert to absolute path
			reportPath, err = utils.GetAbsPath(reportPath)
			if err != nil {
				errMsg := "failed to get the absolute report path"
				utils.LogError(c.logger, err, errMsg)
				return errors.New(errMsg)
			}

			fi, statErr := os.Stat(reportPath)
			if statErr != nil {
				errMsg := fmt.Sprintf("failed to stat report-path %q", reportPath)
				utils.LogError(c.logger, statErr, errMsg)
				return errors.New(errMsg)
			}
			if fi.IsDir() {
				errMsg := fmt.Sprintf("report-path must point to a file, not a directory: %q", reportPath)
				utils.LogError(c.logger, nil, errMsg)
				return errors.New(errMsg)
			}
		}

		c.cfg.Report.ReportPath = reportPath

		// whether to print entire body for comparison
		fb, err := cmd.Flags().GetBool("full")
		if err != nil {
			errMsg := "failed to get the full flag"
			utils.LogError(c.logger, err, errMsg)
			return errors.New(errMsg)
		}

		c.cfg.Report.ShowFullBody = fb

		summary, err := cmd.Flags().GetBool("summary")
		if err != nil {
			utils.LogError(c.logger, err, "failed to get the summary flag")
			return errors.New("failed to get the summary flag")
		}
		c.cfg.Report.Summary = summary

		tcIDs, err := cmd.Flags().GetStringSlice("test-case")
		if err != nil {
			utils.LogError(c.logger, err, "failed to get the test-case flag")
			return errors.New("failed to get the test-case flag")
		}
		// Allow comma-separated or repeated flags
		cleaned := make([]string, 0, len(tcIDs))
		for _, x := range tcIDs {
			for _, y := range strings.Split(x, ",") {
				y = strings.TrimSpace(y)
				if y != "" {
					cleaned = append(cleaned, y)
				}
			}
		}
		c.cfg.Report.TestCaseIDs = cleaned

		format, err := cmd.Flags().GetString("format")
		if err != nil {
			utils.LogError(c.logger, err, "failed to get the format flag")
			return errors.New("failed to get the format flag")
		}
		format = strings.ToLower(strings.TrimSpace(format))
		if format == "" {
			format = "text"
		}
		switch format {
		case "text", "junit":
			// valid
		default:
			return fmt.Errorf("invalid --format value %q: allowed values are 'text' and 'junit'", format)
		}
		c.cfg.Report.Format = format
	case "diff":
		path, err := cmd.Flags().GetString("path")
		if err != nil {
			errMsg := "failed to get the path"
			utils.LogError(c.logger, err, errMsg)
			return errors.New(errMsg)
		}
		c.cfg.Path = utils.ToAbsPath(c.logger, path)

		testSets, err := cmd.Flags().GetStringSlice("test-sets")
		if err != nil {
			errMsg := "failed to get the test-sets"
			utils.LogError(c.logger, err, errMsg)
			return errors.New(errMsg)
		}
		config.SetSelectedTestSets(c.cfg, testSets)

	case "sanitize":
		path, err := cmd.Flags().GetString("path")
		if err != nil {
			errMsg := "failed to get the path"
			utils.LogError(c.logger, err, errMsg)
			return errors.New(errMsg)
		}
		c.cfg.Path = utils.ToAbsPath(c.logger, path)

		testSets, err := cmd.Flags().GetStringSlice("test-sets")
		if err != nil {
			errMsg := "failed to get the test-sets"
			utils.LogError(c.logger, err, errMsg)
			return errors.New(errMsg)
		}
		config.SetSelectedTests(c.cfg, testSets)

	case "generate", "download":
		path, err := cmd.Flags().GetString("path")
		if err != nil {
			errMsg := "failed to get the path"
			utils.LogError(c.logger, err, errMsg)
			return errors.New(errMsg)
		}

		c.cfg.Contract.Path = utils.ToAbsPath(c.logger, path)

		services, err := cmd.Flags().GetStringSlice("services")
		if err != nil {
			errMsg := "failed to get the services"
			utils.LogError(c.logger, err, errMsg)
			return errors.New(errMsg)
		}
		config.SetSelectedServices(c.cfg, services)

		selectedTests, err := cmd.Flags().GetStringSlice("tests")
		if err != nil {
			errMsg := "failed to get the tests"
			utils.LogError(c.logger, err, errMsg)
			return errors.New(errMsg)
		}
		config.SetSelectedContractTests(c.cfg, selectedTests)

		if cmd.Name() == "download" {
			c.cfg.Contract.Driven, err = cmd.Flags().GetString("driven")
			if err != nil {
				errMsg := "failed to get the driven flag"
				utils.LogError(c.logger, err, errMsg)
				return errors.New(errMsg)
			}

		}

		c.cfg.Path = utils.ToAbsPath(c.logger, path)

	case "config":
		path, err := cmd.Flags().GetString("path")
		if err != nil {
			errMsg := "failed to get the path"
			utils.LogError(c.logger, err, errMsg)
			return errors.New(errMsg)
		}
		c.cfg.Path, err = utils.GetAbsPath(path)
		if err != nil {
			errMsg := "failed to get the absolute path"
			utils.LogError(c.logger, err, errMsg)
			return errors.New(errMsg)
		}
	case "record", "test":
		if cmd.Parent() != nil && cmd.Parent().Name() == "contract" {
			path, err := cmd.Flags().GetString("path")
			if err != nil {
				errMsg := "failed to get the path"
				utils.LogError(c.logger, err, errMsg)
				return errors.New(errMsg)
			}

			c.cfg.Contract.Path = utils.ToAbsPath(c.logger, path)

			services, err := cmd.Flags().GetStringSlice("services")
			if err != nil {
				errMsg := "failed to get the services"
				utils.LogError(c.logger, err, errMsg)
				return errors.New(errMsg)
			}
			config.SetSelectedServices(c.cfg, services)

			c.cfg.Contract.Download, err = cmd.Flags().GetBool("download")
			if err != nil {
				errMsg := "failed to get the download flag"
				utils.LogError(c.logger, err, errMsg)
				return errors.New(errMsg)
			}
			c.cfg.Contract.Generate, err = cmd.Flags().GetBool("generate")
			if err != nil {
				errMsg := "failed to get the generate flag"
				utils.LogError(c.logger, err, errMsg)
				return errors.New(errMsg)
			}
			c.cfg.Contract.Driven, err = cmd.Flags().GetString("driven")
			if err != nil {
				errMsg := "failed to get the driven flag"
				utils.LogError(c.logger, err, errMsg)
				return errors.New(errMsg)
			}

			c.cfg.Path = utils.ToAbsPath(c.logger, path)
			return nil
		}

		// set the command type
		c.cfg.CommandType = string(utils.FindDockerCmd(c.cfg.Command))
		if (c.cfg.CommandType == string(utils.Native) || c.cfg.CommandType == string(utils.Empty)) && !(runtime.GOOS == "linux" || (runtime.GOOS == "windows" && runtime.GOARCH == "amd64")) {
			return fmt.Errorf("non docker command not supported for OS: %s , Arch: %s", runtime.GOOS, runtime.GOARCH)
		}
		// memory-limit non-Docker gate is applied after flag parsing below

		// empty the command if base path is provided, because no need of command even if provided
		if c.cfg.Test.BasePath != "" {
			c.cfg.CommandType = string(utils.Empty)
			c.cfg.Command = ""
		}

		if c.cfg.GenerateGithubActions && utils.CmdType(c.cfg.CommandType) != utils.Empty {
			defer utils.GenerateGithubActions(c.logger, c.cfg.Command)
		}
		if c.cfg.InDocker {
			c.logger.Info("detected that Keploy is running in a docker container")
			if len(c.cfg.Path) > 0 {
				curDir, err := os.Getwd()
				if err != nil {
					errMsg := "failed to get current working directory"
					utils.LogError(c.logger, err, errMsg)
					return errors.New(errMsg)
				}
				if strings.Contains(c.cfg.Path, "..") {

					c.cfg.Path, err = utils.GetAbsPath(filepath.Clean(c.cfg.Path))
					if err != nil {
						return fmt.Errorf("failed to get the absolute path from relative path: %w", err)
					}

					relativePath, err := filepath.Rel(curDir, c.cfg.Path)
					if err != nil {
						errMsg := "failed to get the relative path from absolute path"
						utils.LogError(c.logger, err, errMsg)
						return errors.New(errMsg)
					}
					if relativePath == ".." || strings.HasPrefix(relativePath, "../") {
						errMsg := "path provided is not a subdirectory of current directory. Keploy only supports recording testcases in the current directory or its subdirectories"
						utils.LogError(c.logger, err, errMsg, zap.String("path:", c.cfg.Path))
						return errors.New(errMsg)
					}
				}
			}
			// check if the buildDelay is less than 30 seconds
			if time.Duration(c.cfg.BuildDelay)*time.Second <= 30*time.Second {
				c.logger.Info(fmt.Sprintf("buildDelay is set to %v, incase your docker container takes more time to build use --buildDelay to set custom delay", c.cfg.BuildDelay))
				c.logger.Info(`Example usage: keploy record -c "docker-compose up --build" --buildDelay 35`)
			}
			if utils.CmdType(c.cfg.Command) == utils.DockerCompose {
				if c.cfg.ContainerName == "" {
					utils.LogError(c.logger, nil, "Couldn't find containerName")
					c.logger.Info(`Example usage: keploy record -c "docker run -p 8080:8080 --network myNetworkName myApplicationImageName" --delay 6`)
					return errors.New("missing required --container-name flag or containerName in config file")
				}
			}
		}

		absPath, err := utils.GetAbsPath(c.cfg.Path)
		if err != nil {
			utils.LogError(c.logger, err, "error while getting absolute path")
			return errors.New("failed to get the absolute path")
		}
		c.cfg.Path = absPath + "/keploy"

		// Check and fix keploy folder permissions for native mode only
		// (handles root-owned files from older sudo-based versions)
		// Docker commands use sudo re-exec, so they run as root and don't need this
		cmdType := utils.FindDockerCmd(c.cfg.Command)
		if !utils.IsDockerCmd(cmdType) {
			// Native mode: fix permissions immediately (this caches sudo credentials)
			if err := utils.EnsureKeployFolderPermissions(cmd.Context(), c.logger, c.cfg.Path); err != nil {
				utils.LogError(c.logger, err, "failed to ensure keploy folder permissions")
				return err
			}
		}

		// handle the app command
		if c.cfg.Command == "" {
			if !alreadyRunning(cmd.Name(), c.cfg.Test.BasePath) {
				return c.noCommandError()
			}
		}

		bypassPorts, err := cmd.Flags().GetUintSlice("passThroughPorts")
		if err != nil {
			errMsg := "failed to read the ports of outgoing calls to be ignored"
			utils.LogError(c.logger, err, errMsg)
			return errors.New(errMsg)
		}
		config.SetByPassPorts(c.cfg, bypassPorts)

		if cmd.Name() == "record" {
			metadata, err := cmd.Flags().GetString("metadata")
			if err != nil {
				errMsg := "failed to get the metadata flag"
				utils.LogError(c.logger, err, errMsg)
				return errors.New(errMsg)
			}
			c.cfg.Record.Metadata = metadata

			if cmd.Flags().Changed("memory-limit") {
				memoryLimit, err := cmd.Flags().GetUint64("memory-limit")
				if err != nil {
					utils.LogError(c.logger, err, "failed to get the memory-limit flag")
					return fmt.Errorf("failed to get memory-limit flag: %w", err)
				}
				c.cfg.Record.MemoryLimit = memoryLimit
			}
			if _, err := memoryguard.LimitBytes(c.cfg.Record.MemoryLimit); err != nil {
				utils.LogError(c.logger, err, "invalid memory limit for keploy agent")
				return err
			}
			if c.cfg.Record.MemoryLimit > 0 {
				cmdType := utils.CmdType(c.cfg.CommandType)
				if cmdType != utils.DockerRun && cmdType != utils.DockerCompose {
					c.logger.Info("memory-limit is only supported for docker run and docker compose recording; ignoring it for this command type",
						zap.String("cmd-type", c.cfg.CommandType))
					c.cfg.Record.MemoryLimit = 0
				}
			}

			enableSampling, err := cmd.Flags().GetInt("enable-sampling")
			if err != nil {
				errMsg := "failed to get the enable-sampling flag"
				utils.LogError(c.logger, err, errMsg)
				return errors.New(errMsg)
			}
			c.cfg.Record.EnableSampling = enableSampling

			// Resolve record-buffer values: flag overrides yaml, env
			// overrides flag. Range validation and clamping live in
			// clampRecordBuffer (pkg/agent/proxy/proxy.go); only the
			// cross-check against MemoryLimit happens here so the user
			// gets fast feedback at the CLI surface.
			if err := c.resolveRecordBufferUint64(cmd, "max-memory-per-conn", "KEPLOY_RECORD_MAX_MEMORY_PER_CONN", &c.cfg.Record.RecordBuffer.MaxMemoryPerConnection); err != nil {
				return err
			}
			if err := c.resolveRecordBufferInt(cmd, "queue-size", "KEPLOY_RECORD_QUEUE_SIZE", &c.cfg.Record.RecordBuffer.QueueSize); err != nil {
				return err
			}

			// Cross-check: a single connection's recording buffer must
			// not exceed the docker container's memory limit. Otherwise
			// even one large response can OOM-kill the agent before any
			// concurrency. Mirrors k8s-proxy's ApplyRecordConfig — both
			// produce the same UX (fail at config time, not at runtime)
			// with a UI-aligned error string. Record.MemoryLimit is in
			// MB and is only meaningful for docker run / docker compose
			// (zeroed earlier for native runs), so we gate on >0 to
			// skip native runs where this comparison is moot.
			if c.cfg.Record.MemoryLimit > 0 && c.cfg.Record.RecordBuffer.MaxMemoryPerConnection > 0 {
				memBytes := c.cfg.Record.MemoryLimit * 1024 * 1024
				if c.cfg.Record.RecordBuffer.MaxMemoryPerConnection > memBytes {
					return fmt.Errorf("Max Memory Per API Call (%d MB) cannot exceed the Keploy Agent Memory Limit (%d MB).",
						bytesToMBCeil(c.cfg.Record.RecordBuffer.MaxMemoryPerConnection),
						c.cfg.Record.MemoryLimit)
				}
			}
		}

		if cmd.Name() == "test" {
			//check if the keploy folder exists
			//check if the keploy folder exists
			if _, err := os.Stat(c.cfg.Path); os.IsNotExist(err) {
				recordCmd := models.HighlightGrayString("keploy record")
				c.logger.Info(fmt.Sprintf("No test-sets found. Please record testcases using %s command", recordCmd))
				cmdType := utils.CmdType(c.cfg.CommandType)
				if cmdType == utils.DockerRun || cmdType == utils.DockerStart || cmdType == utils.DockerCompose {
					c.logger.Info(`Example: keploy record -c "docker run -p 8080:8080 --network myNetworkName myApplicationImageName" --delay 6`)
				} else {
					c.logger.Info(`Example: keploy record -c "./myApp serve" --delay 6`)
				}
				os.Exit(1)
			}

			testSets, err := cmd.Flags().GetStringSlice("testsets")
			if err != nil {
				errMsg := "failed to get the testsets"
				utils.LogError(c.logger, err, errMsg)
				return errors.New(errMsg)
			}
			if len(testSets) != 0 {
				config.SetSelectedTests(c.cfg, testSets)
			}

			// get disable-mapping flag value
			if cmd.Flags().Changed("disable-mapping") || !viper.IsSet("disableMapping") {
				disableMapping, err := cmd.Flags().GetBool("disable-mapping")
				if err != nil {
					errMsg := "failed to get the disable-mapping flag"
					utils.LogError(c.logger, err, errMsg)
					return errors.New(errMsg)
				}
				c.cfg.DisableMapping = disableMapping
			}

			retryPassing, err := cmd.Flags().GetBool("retry-passing-test")
			if err != nil {
				errMsg := "failed to get the retry-passing-test flag"
				utils.LogError(c.logger, err, errMsg)
				return errors.New(errMsg)
			}
			c.cfg.RetryPassing = retryPassing

			c.cfg.Test.DisableAutoHeaderNoise, err = cmd.Flags().GetBool("disableAutoHeaderNoise")
			if err != nil {
				errMsg := "failed to read the --disableAutoHeaderNoise flag; check the flag name with --help and confirm this command supports it"
				utils.LogError(c.logger, err, errMsg)
				return errors.New(errMsg)
			}

			// enforce that the test-sets are provided when --must-pass is set to true
			// to prevent accidental deletion of failed testcases in testsets which was due to application changes
			// and not due to flakiness or our internal issue.
			mustPass, err := cmd.Flags().GetBool("must-pass")
			if err != nil {
				errMsg := "failed to get the must-pass flag"
				utils.LogError(c.logger, err, errMsg)
				return errors.New(errMsg)
			}

			if mustPass {
				c.cfg.Test.SkipCoverage = true
			}

			// in mustpass mode, set the maxFlakyChecks count to 3 explicitly,
			// if it is not set through cmd flag.
			if mustPass && !cmd.Flags().Changed("flaky-check-retry") {
				c.cfg.Test.MaxFlakyChecks = 3
			}

			// if the user passes a value for this field, store it
			if cmd.Flags().Changed("flaky-check-retry") {
				c.cfg.Test.MaxFlakyChecks, err = cmd.Flags().GetUint32("flaky-check-retry")
				if err != nil {
					errMsg := "failed to get the provided flaky-check-retry count"
					utils.LogError(c.logger, err, errMsg)
					return errors.New(errMsg)
				}
			}

			// if the user passes a value for this field, store it
			if cmd.Flags().Changed("max-fail-attempts") {
				c.cfg.Test.MaxFailAttempts, err = cmd.Flags().GetUint32("max-fail-attempts")
				if err != nil {
					errMsg := "failed to get the provided max-fail-attempts count"
					utils.LogError(c.logger, err, errMsg)
					return errors.New(errMsg)
				}
			}

			// don't allow zero maxFlakyChecks and if must pass mode is enabled, then maxFailAttempts can't be zero.
			if c.cfg.Test.MaxFlakyChecks == 0 {
				return fmt.Errorf("value for maxFlakyChecks cannot be zero")
			}
			if mustPass && c.cfg.Test.MaxFailAttempts == 0 {
				return fmt.Errorf("in must pass mode, value for maxFailAttempts cannot be zero")
			}

			if mustPass && !cmd.Flags().Changed("test-sets") {
				return fmt.Errorf("--test-sets flag must be set to use --must-pass=true")
			}

			// skip coverage by default if command is of type docker
			if utils.CmdType(c.cfg.CommandType) != "native" && !cmd.Flags().Changed("skip-coverage") {
				c.cfg.Test.SkipCoverage = true
			}

			if c.cfg.Test.Delay <= 5 {
				c.logger.Info(fmt.Sprintf("Delay is set to %d seconds, incase your app takes more time to start use --delay to set custom delay", c.cfg.Test.Delay))
				if c.cfg.InDocker {
					c.logger.Info(`Example usage: keploy test -c "docker run -p 8080:8080 --network myNetworkName myApplicationImageName" --delay 6`)
				} else {
					c.logger.Info("Example usage: " + cmd.Example)
				}
			}
		}
		globalPassthrough, err := cmd.Flags().GetBool("global-passthrough")
		if err != nil {
			errMsg := "failed to read the global passthrough flag"
			utils.LogError(c.logger, err, errMsg)
			return errors.New(errMsg)
		}
		c.cfg.Record.GlobalPassthrough = globalPassthrough

		opportunisticTLSIntercept, err := cmd.Flags().GetBool("opportunistic-tls-intercept")
		if err != nil {
			errMsg := "failed to read the opportunistic-tls-intercept flag"
			utils.LogError(c.logger, err, errMsg)
			return errors.New(errMsg)
		}
		c.cfg.Record.OpportunisticTLSIntercept = opportunisticTLSIntercept

	case "normalize":
		c.cfg.Path = utils.ToAbsPath(c.logger, c.cfg.Path)
		tests, err := cmd.Flags().GetString("tests")
		if err != nil {
			errMsg := "failed to read tests to be normalized"
			utils.LogError(c.logger, err, errMsg)
			return errors.New(errMsg)
		}
		err = config.SetSelectedTestsNormalize(c.cfg, tests)
		if err != nil {
			errMsg := "failed to normalize the selected tests"
			utils.LogError(c.logger, err, errMsg)
			return errors.New(errMsg)
		}

		c.cfg.Normalize.AllowHighRisk, err = cmd.Flags().GetBool("allow-high-risk")
		if err != nil {
			errMsg := "failed to read allow-high-risk flag"
			utils.LogError(c.logger, err, errMsg)
			return errors.New(errMsg)
		}

	case "templatize":
		c.cfg.Path = utils.ToAbsPath(c.logger, c.cfg.Path)
	case "agent":
		globalPassthrough, err := cmd.Flags().GetBool("global-passthrough")
		if err != nil {
			errMsg := "failed to read the global passthrough flag"
			utils.LogError(c.logger, err, errMsg)
			return errors.New(errMsg)
		}
		c.cfg.Agent.GlobalPassthrough = globalPassthrough

		isdocker, err := cmd.Flags().GetBool("is-docker")
		if err != nil {
			utils.LogError(c.logger, err, "failed to get is-docker flag")
			return nil
		}
		c.cfg.Agent.IsDocker = isdocker

		enableTesting, err := cmd.Flags().GetBool("enable-testing")
		if err != nil {
			utils.LogError(c.logger, err, "failed to get enable-testing flag")
			return nil
		}
		c.cfg.Agent.EnableTesting = enableTesting

		port, err := cmd.Flags().GetUint32("port")
		if err != nil {
			utils.LogError(c.logger, err, "failed to get port flag")
			return nil
		}
		c.cfg.Agent.AgentPort = port

		clientNSPid, err := cmd.Flags().GetUint32("client-pid")
		if err != nil {
			utils.LogError(c.logger, err, "failed to get clientPID flag")
			return nil
		}
		c.cfg.Agent.ClientNSPID = clientNSPid

		caJavaHome, err := cmd.Flags().GetString("ca-java-home")
		if err != nil {
			utils.LogError(c.logger, err, "failed to get ca-java-home flag")
			return nil
		}
		// Trim whitespace so accidental `--ca-java-home=" "` doesn't
		// short-circuit auto-detection with a junk path that will
		// stat-fail in installJavaCAForHome and fall back to PATH
		// keytool anyway (but logged at Debug as a spurious override).
		c.cfg.Agent.CAJavaHome = strings.TrimSpace(caJavaHome)

		mode, err := cmd.Flags().GetString("mode")
		if err != nil {
			utils.LogError(c.logger, err, "failed to get mode flag")
			return nil
		}

		c.cfg.Agent.Mode = models.Mode(mode)

		proxyPort, err := cmd.Flags().GetUint32("proxy-port")
		if err != nil {
			utils.LogError(c.logger, err, "failed to get proxyPort flag")
			return nil
		}
		c.cfg.Agent.ProxyPort = proxyPort

		incomingProxyPort, err := cmd.Flags().GetUint16("incoming-proxy-port")
		if err != nil {
			utils.LogError(c.logger, err, "failed to get incomingProxyPort flag")
			return nil
		}
		c.cfg.Agent.IncomingProxyPort = incomingProxyPort

		dnsPort, err := cmd.Flags().GetUint32("dns-port")
		if err != nil {
			utils.LogError(c.logger, err, "failed to get dns-port flag; verify the --dns-port value and try again")
			return fmt.Errorf("failed to get dns-port flag: %w", err)
		}
		c.cfg.Agent.DnsPort = dnsPort

		synchronous, err := cmd.Flags().GetBool("sync")
		if err != nil {
			errMsg := "failed to get the synchronous flag"
			utils.LogError(c.logger, err, errMsg)
			return errors.New(errMsg)
		}
		c.cfg.Agent.Synchronous = synchronous

		// Honour --disable-mapping passed to the agent (typically by the
		// host CLI when starting the docker-compose / k8s sidecar agent).
		// Without this, the agent process inside the container falls
		// back to the embedded default of disableMapping=true and
		// silently no-ops mappings.yaml production even when the
		// operator's keploy.yml has disableMapping: false.
		if cmd.Flags().Changed("disable-mapping") {
			disableMapping, err := cmd.Flags().GetBool("disable-mapping")
			if err != nil {
				errMsg := "failed to get the disable-mapping flag"
				utils.LogError(c.logger, err, errMsg)
				return errors.New(errMsg)
			}
			c.cfg.DisableMapping = disableMapping
		}

		enableSampling, err := cmd.Flags().GetInt("enable-sampling")
		if err != nil {
			errMsg := "failed to get the enable-sampling flag"
			utils.LogError(c.logger, err, errMsg)
			return errors.New(errMsg)
		}
		c.cfg.Agent.EnableSampling = enableSampling

		buildDelay, err := cmd.Flags().GetUint64("build-delay")
		if err != nil {
			utils.LogError(c.logger, err, "failed to get build-delay flag; verify the --build-delay value and try again")
			return fmt.Errorf("failed to get build-delay flag: %w", err)
		}
		c.cfg.Agent.BuildDelay = buildDelay

		passThroughPorts, err := cmd.Flags().GetUintSlice("pass-through-ports")
		if err != nil {
			utils.LogError(c.logger, err, "failed to get pass-through-ports flag; verify the --pass-through-ports value and try again")
			return fmt.Errorf("failed to get pass-through-ports flag: %w", err)
		}
		c.cfg.Agent.PassThroughPorts = passThroughPorts

		if cmd.Flags().Changed("memory-limit") {
			memoryLimit, err := cmd.Flags().GetUint64("memory-limit")
			if err != nil {
				errMsg := "failed to get memory-limit flag; verify the --memory-limit value and try again"
				utils.LogError(c.logger, err, errMsg)
				return fmt.Errorf("failed to get memory-limit flag: %w", err)
			}
			c.cfg.Agent.MemoryLimit = memoryLimit
		}

		// Resolve record-buffer values forwarded via argv from the
		// orchestrator (host CLI). The agent's own keploy.yml may also
		// set these; argv wins if explicitly passed. Env vars on the
		// agent process win over argv (lets operators tune a running
		// container without rebuilding the orchestrator command).
		if err := c.resolveRecordBufferUint64(cmd, "max-memory-per-conn", "KEPLOY_RECORD_MAX_MEMORY_PER_CONN", &c.cfg.Record.RecordBuffer.MaxMemoryPerConnection); err != nil {
			return err
		}
		if err := c.resolveRecordBufferInt(cmd, "queue-size", "KEPLOY_RECORD_QUEUE_SIZE", &c.cfg.Record.RecordBuffer.QueueSize); err != nil {
			return err
		}

		// Cross-check: max-memory-per-conn must not exceed the agent's
		// memory limit. The agent's --memory-limit (Agent.MemoryLimit in
		// MB) is what memoryguard enforces; if the per-connection buffer
		// alone is larger than the agent's whole budget, a single large
		// response will OOM the recording loop before any concurrency.
		// Skip when MemoryLimit=0 (uncapped); proxy.go's
		// clampRecordBuffer still applies the 2 GiB upper bound there.
		if c.cfg.Agent.MemoryLimit > 0 && c.cfg.Record.RecordBuffer.MaxMemoryPerConnection > 0 {
			memBytes := c.cfg.Agent.MemoryLimit * 1024 * 1024
			if c.cfg.Record.RecordBuffer.MaxMemoryPerConnection > memBytes {
				return fmt.Errorf("Max Memory Per API Call (%d MB) cannot exceed the Keploy Agent Memory Limit (%d MB).",
					bytesToMBCeil(c.cfg.Record.RecordBuffer.MaxMemoryPerConnection),
					c.cfg.Agent.MemoryLimit)
			}
		}

		if _, err := memoryguard.LimitBytes(c.cfg.Agent.MemoryLimit); err != nil {
			utils.LogError(c.logger, err, "invalid memory limit for keploy agent; use a positive MB value within int64 range or set --memory-limit=0 to disable")
			return err
		}
		if c.cfg.Agent.MemoryLimit > 0 && !c.cfg.Agent.IsDocker {
			c.logger.Info("--memory-limit is only effective in Docker mode; ignoring",
				zap.Uint64("memory_limit_mb", c.cfg.Agent.MemoryLimit))
			c.cfg.Agent.MemoryLimit = 0
		}

	}

	return nil
}

// resolveRecordBufferUint64 applies flag→env override semantics for one
// record-buffer uint64 field. Used by ValidateFlags' record and agent
// branches which would otherwise duplicate this dance verbatim.
//
// Semantics: a Changed flag wins over the existing config value; an
// env var wins over both. Malformed env values are warned and ignored
// (the config keeps whatever the flag/yaml resolved to) — record-path
// configuration mistakes must never tear down the daemon.
func (c *CmdConfigurator) resolveRecordBufferUint64(cmd *cobra.Command, flagName, envName string, target *uint64) error {
	if cmd.Flags().Changed(flagName) {
		v, err := cmd.Flags().GetUint64(flagName)
		if err != nil {
			utils.LogError(c.logger, err, "failed to get "+flagName+" flag")
			return fmt.Errorf("failed to get %s flag: %w", flagName, err)
		}
		*target = v
	}
	if envVal := os.Getenv(envName); envVal != "" {
		v, err := strconv.ParseUint(envVal, 10, 64)
		if err != nil {
			c.logger.Debug("ignoring malformed env var; expected unsigned integer",
				zap.String("envVar", envName),
				zap.String("value", envVal),
				zap.Error(err))
		} else {
			*target = v
		}
	}
	return nil
}

// resolveRecordBufferInt is the int sibling of resolveRecordBufferUint64.
// Kept as a separate helper rather than a generic because cobra's flag
// accessors are typed and the env-parse uses strconv.Atoi here.
func (c *CmdConfigurator) resolveRecordBufferInt(cmd *cobra.Command, flagName, envName string, target *int) error {
	if cmd.Flags().Changed(flagName) {
		v, err := cmd.Flags().GetInt(flagName)
		if err != nil {
			utils.LogError(c.logger, err, "failed to get "+flagName+" flag")
			return fmt.Errorf("failed to get %s flag: %w", flagName, err)
		}
		*target = v
	}
	if envVal := os.Getenv(envName); envVal != "" {
		v, err := strconv.Atoi(envVal)
		if err != nil {
			c.logger.Debug("ignoring malformed env var; expected integer",
				zap.String("envVar", envName),
				zap.String("value", envVal),
				zap.Error(err))
		} else {
			*target = v
		}
	}
	return nil
}

// bytesToMBCeil converts bytes to MiB with ceiling rounding so the
// formatted error never under-reports the configured value (e.g.,
// 1.5 MiB worth of bytes prints as "2 MB", not "1 MB"). Float
// formatting was tempting but a trailing ".00" looks awkward in the
// majority case where users enter whole-MB values; ceil-int strikes
// the right balance.
func bytesToMBCeil(b uint64) uint64 {
	const mib = uint64(1024 * 1024)
	return (b + mib - 1) / mib
}

func (c *CmdConfigurator) CreateConfigFile(ctx context.Context, defaultCfg config.Config) error {
	defaultCfg = c.UpdateConfigData(defaultCfg)
	toolSvc := tools.NewTools(c.logger, nil, nil, nil, nil, nil)
	configData := defaultCfg
	configDataBytes, err := yaml.Marshal(configData)
	if err != nil {
		utils.LogError(c.logger, err, "failed to marshal config data")
		return errors.New("failed to marshal config data")
	}

	// Ensure the config directory exists before creating the file
	if err := os.MkdirAll(c.cfg.ConfigPath, os.ModePerm); err != nil {
		errMsg := fmt.Sprintf("failed to create config directory: %v", err)
		utils.LogError(c.logger, err, errMsg)
		return errors.New(errMsg)
	}

	configFilePath := filepath.Join(c.cfg.ConfigPath, "keploy.yml")
	err = toolSvc.CreateConfig(ctx, configFilePath, string(configDataBytes))
	if err != nil {
		utils.LogError(c.logger, err, "failed to create config file")
		return errors.New("failed to create config file")
	}
	c.logger.Info("Generated config file based on the flags that are used")
	return nil
}

func (c *CmdConfigurator) UpdateConfigData(defaultCfg config.Config) config.Config {
	defaultCfg.Command = c.cfg.Command
	defaultCfg.Test.Delay = c.cfg.Test.Delay
	defaultCfg.AppName = c.cfg.AppName
	defaultCfg.Test.APITimeout = c.cfg.Test.APITimeout
	defaultCfg.ContainerName = c.cfg.ContainerName
	defaultCfg.Test.IgnoreOrdering = c.cfg.Test.IgnoreOrdering
	defaultCfg.Test.Language = c.cfg.Test.Language
	defaultCfg.DisableANSI = c.cfg.DisableANSI
	defaultCfg.Test.SkipCoverage = c.cfg.Test.SkipCoverage
	defaultCfg.Test.Mocking = c.cfg.Test.Mocking
	defaultCfg.Test.DisableLineCoverage = c.cfg.Test.DisableLineCoverage
	return defaultCfg
}
