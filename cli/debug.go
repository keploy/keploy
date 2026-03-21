package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/capture"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

func init() {
	Register("debug", Debug)
}

func Debug(ctx context.Context, logger *zap.Logger, cfg *config.Config, _ ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
	var cmd = &cobra.Command{
		Use:   "debug",
		Short: "debug tools for analyzing and replaying network captures",
		Long: `The debug command provides tools for working with Keploy network capture (.kpcap) files.

These files are automatically generated when running keploy record or keploy test with the --debug flag.
They capture raw network packets flowing through the proxy, enabling exact reproduction of issues.

Subcommands:
  analyze    - Show a detailed analysis of a capture file
  validate   - Verify capture file integrity
  compare    - Compare two captures to find where proxy behavior diverges
  reproduce  - Set up mocks/tests from a debug bundle for local reproduction
  replay     - Replay captured traffic against a proxy
  bundle     - Create a debug bundle (capture + mocks + logs + config)
  extract    - Extract a debug bundle`,
	}

	cmd.AddCommand(debugAnalyzeCmd(logger))
	cmd.AddCommand(debugValidateCmd(logger))
	cmd.AddCommand(debugCompareCmd(logger))
	cmd.AddCommand(debugReproduceCmd(logger, cfg))
	cmd.AddCommand(debugReplayCmd(ctx, logger))
	cmd.AddCommand(debugBundleCmd(logger, cfg))
	cmd.AddCommand(debugExtractCmd(logger))

	return cmd
}

func debugAnalyzeCmd(logger *zap.Logger) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "analyze <capture-file>",
		Short:   "analyze a network capture file and show detailed report",
		Example: `keploy debug analyze keploy/debug/capture_record_20240101_120000.kpcap`,
		Args:    cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			report, err := capture.Analyze(args[0])
			if err != nil {
				utils.LogError(logger, err, "failed to analyze capture file")
				return nil
			}
			fmt.Println(capture.FormatReport(report))
			return nil
		},
	}
	return cmd
}

func debugValidateCmd(logger *zap.Logger) *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:     "validate <capture-file>",
		Short:   "validate a capture file for integrity",
		Example: `keploy debug validate capture.kpcap`,
		Args:    cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			result, err := capture.Validate(args[0])
			if err != nil {
				utils.LogError(logger, err, "failed to validate capture file")
				return nil
			}

			if jsonOutput {
				data, _ := json.MarshalIndent(result, "", "  ")
				fmt.Println(string(data))
			} else {
				fmt.Printf("File:        %s\n", result.Path)
				fmt.Printf("Valid:       %v\n", result.Valid)
				fmt.Printf("Packets:     %d\n", result.PacketCount)
				fmt.Printf("Data:        %d bytes\n", result.ByteCount)
				fmt.Printf("Connections: %d\n", result.ConnectionCount)
				fmt.Printf("Mode:        %s\n", result.Metadata.Mode)
				fmt.Printf("Created:     %s\n", result.Metadata.CreatedAt.Format(time.RFC3339))
				if len(result.Errors) > 0 {
					fmt.Println("\nErrors:")
					for _, e := range result.Errors {
						fmt.Printf("  - %s\n", e)
					}
				}
				if len(result.Warnings) > 0 {
					fmt.Println("\nWarnings:")
					for _, w := range result.Warnings {
						fmt.Printf("  - %s\n", w)
					}
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "output in JSON format")
	return cmd
}

func debugReplayCmd(ctx context.Context, logger *zap.Logger) *cobra.Command {
	var proxyAddr string
	var timeout time.Duration
	var jsonOutput bool

	cmd := &cobra.Command{
		Use:   "replay <capture-file>",
		Short: "replay captured traffic against a running proxy to reproduce issues",
		Long: `Replays the network traffic from a capture file against a running Keploy proxy.

This is used to reproduce customer issues by feeding the exact same network traffic
that was captured in their environment. The proxy must be running in test mode.

The replay engine:
1. Reads the capture file and groups packets by connection
2. For each connection, opens a TCP connection to the proxy
3. Sends the captured client data and compares proxy responses
4. Reports any byte-level differences found`,
		Example: `keploy debug replay --proxy localhost:16789 capture.kpcap`,
		Args:    cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			replayer := capture.NewReplayer(logger, proxyAddr, timeout)
			summary, err := replayer.ReplayFile(ctx, args[0])
			if err != nil {
				utils.LogError(logger, err, "replay failed")
				return nil
			}

			if jsonOutput {
				data, _ := json.MarshalIndent(summary, "", "  ")
				fmt.Println(string(data))
			} else {
				fmt.Printf("\nReplay Summary for: %s\n", summary.CaptureFile)
				fmt.Printf("══════════════════════════════════════════\n")
				fmt.Printf("  Total Connections:    %d\n", summary.TotalConns)
				fmt.Printf("  Replayed:            %d\n", summary.ReplayedConns)
				fmt.Printf("  Matched (exact):     %d\n", summary.MatchedConns)
				fmt.Printf("  Mismatched:          %d\n", summary.FailedConns)
				fmt.Printf("  Skipped (TLS/empty): %d\n", summary.SkippedConns)
				fmt.Printf("  Duration:            %s\n", summary.TotalDuration)
				fmt.Printf("══════════════════════════════════════════\n")

				for _, r := range summary.Results {
					if r.Matched {
						continue
					}
					fmt.Printf("\n  Connection #%d (%s → %s) [%s]:\n",
						r.ConnectionID, r.SrcAddr, r.DstAddr, r.Protocol)
					fmt.Printf("    Sent: %d packets (%d bytes), Recv: %d packets (%d bytes)\n",
						r.PacketsSent, r.BytesSent, r.PacketsRecv, r.BytesRecv)
					for _, mm := range r.ByteMismatches {
						fmt.Printf("    MISMATCH at packet %d (%s): expected %d bytes, got %d bytes (first diff at offset %d)\n",
							mm.PacketIndex, mm.Direction, mm.Expected, mm.Actual, mm.Offset)
					}
					for _, e := range r.Errors {
						fmt.Printf("    ERROR: %s\n", e)
					}
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&proxyAddr, "proxy", "localhost:16789", "proxy address to replay against")
	cmd.Flags().DurationVar(&timeout, "timeout", 30*time.Second, "per-connection timeout")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "output in JSON format")
	return cmd
}

func debugBundleCmd(logger *zap.Logger, cfg *config.Config) *cobra.Command {
	var captureFile, mockDir, testDir, logFile, configFile, output, notes string

	cmd := &cobra.Command{
		Use:   "bundle",
		Short: "create a debug bundle with capture file, mocks, logs, and config",
		Long: `Creates a .tar.gz debug bundle containing everything needed to reproduce an issue:
- Network capture file (.kpcap)
- Mock files
- Test case files
- Debug log file
- Keploy configuration

Share this bundle with the Keploy team for issue reproduction.`,
		Example: `keploy debug bundle --capture keploy/debug/capture_record_20240101_120000.kpcap`,
		RunE: func(_ *cobra.Command, _ []string) error {
			if captureFile == "" {
				// Try to find the most recent capture file
				captureFile = findLatestCapture(cfg.Path)
				if captureFile == "" {
					logger.Error("No capture file specified and none found. Use --capture flag.")
					return nil
				}
				logger.Info("Using most recent capture file", zap.String("file", captureFile))
			}

			// Default directories from config
			if mockDir == "" {
				mockDir = filepath.Join(cfg.Path, "keploy")
			}
			if testDir == "" {
				testDir = filepath.Join(cfg.Path, "keploy")
			}
			if configFile == "" {
				configFile = filepath.Join(cfg.Path, "keploy.yml")
				if _, err := os.Stat(configFile); err != nil {
					configFile = ""
				}
			}

			bundlePath, err := capture.CreateBundle(logger, capture.BundleOptions{
				CaptureFile: captureFile,
				MockDir:     mockDir,
				TestDir:     testDir,
				LogFile:     logFile,
				ConfigFile:  configFile,
				OutputPath:  output,
				AppName:     cfg.AppName,
				Mode:        "debug",
				Notes:       notes,
			})
			if err != nil {
				utils.LogError(logger, err, "failed to create debug bundle")
				return nil
			}

			fmt.Printf("\nDebug bundle created: %s\n", bundlePath)
			fmt.Println("Share this file with the Keploy team to reproduce the issue.")
			return nil
		},
	}
	cmd.Flags().StringVar(&captureFile, "capture", "", "path to capture (.kpcap) file")
	cmd.Flags().StringVar(&mockDir, "mocks", "", "path to mock directory")
	cmd.Flags().StringVar(&testDir, "tests", "", "path to test directory")
	cmd.Flags().StringVar(&logFile, "log", "", "path to debug log file")
	cmd.Flags().StringVar(&configFile, "config", "", "path to keploy config file")
	cmd.Flags().StringVar(&output, "output", "", "output path for the bundle (default: auto-generated)")
	cmd.Flags().StringVar(&notes, "notes", "", "notes about the issue being reported")
	return cmd
}

func debugExtractCmd(logger *zap.Logger) *cobra.Command {
	var targetDir string

	cmd := &cobra.Command{
		Use:     "extract <bundle-file>",
		Short:   "extract a debug bundle to a directory",
		Example: `keploy debug extract keploy-debug-bundle_debug_20240101_120000.tar.gz --dir ./debug-output`,
		Args:    cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if targetDir == "" {
				targetDir = "keploy-debug-extracted"
			}

			manifest, err := capture.ExtractBundle(args[0], targetDir)
			if err != nil {
				utils.LogError(logger, err, "failed to extract debug bundle")
				return nil
			}

			fmt.Printf("Bundle extracted to: %s\n", targetDir)
			fmt.Printf("  Mode:    %s\n", manifest.Mode)
			fmt.Printf("  Created: %s\n", manifest.CreatedAt.Format(time.RFC3339))
			if manifest.AppName != "" {
				fmt.Printf("  App:     %s\n", manifest.AppName)
			}
			if manifest.CaptureFile != "" {
				fmt.Printf("  Capture: %s\n", manifest.CaptureFile)
			}
			if manifest.Notes != "" {
				fmt.Printf("  Notes:   %s\n", manifest.Notes)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&targetDir, "dir", "", "target directory for extraction (default: keploy-debug-extracted)")
	return cmd
}

func debugCompareCmd(logger *zap.Logger) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "compare <customer-capture> <engineer-capture>",
		Short: "compare two capture files to find where proxy behavior diverges",
		Long: `Compares two .kpcap capture files connection by connection.

Typical workflow:
  1. Customer shares their debug bundle (contains a .kpcap from their failing run)
  2. Engineer extracts the bundle and sets up the same mocks/tests
  3. Engineer runs keploy test --debug to produce their own .kpcap
  4. Engineer runs: keploy debug compare customer.kpcap engineer.kpcap
  5. The report shows which connections differ and where the first byte difference is

Connections are matched by destination address and protocol.`,
		Example: `keploy debug compare customer-capture.kpcap my-capture.kpcap`,
		Args:    cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			result, err := capture.Compare(args[0], args[1])
			if err != nil {
				utils.LogError(logger, err, "failed to compare captures")
				return nil
			}
			fmt.Println(capture.FormatCompareResult(result))
			return nil
		},
	}
	return cmd
}

func debugReproduceCmd(logger *zap.Logger, cfg *config.Config) *cobra.Command {
	var bundlePath string
	var targetDir string

	cmd := &cobra.Command{
		Use:   "reproduce <bundle-file>",
		Short: "set up a local environment from a debug bundle for issue reproduction",
		Long: `Extracts a debug bundle and copies the mocks and test cases into the keploy
directory so you can immediately run 'keploy test' to reproduce the issue.

Workflow:
  1. Customer shares debug bundle: keploy-debug-bundle.tar.gz
  2. Engineer runs: keploy debug reproduce keploy-debug-bundle.tar.gz --dir ./repro
  3. Engineer runs: cd ./repro && keploy test -c "<app-command>" --debug --path .
  4. Engineer compares captures: keploy debug compare <customer.kpcap> <new.kpcap>
  5. If captures differ → the bug is reproduced. Fix and re-run to verify.`,
		Example: `keploy debug reproduce customer-bundle.tar.gz --dir ./repro`,
		Args:    cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			bundlePath = args[0]
			if targetDir == "" {
				targetDir = "repro"
			}

			// Extract the bundle
			manifest, err := capture.ExtractBundle(bundlePath, targetDir)
			if err != nil {
				utils.LogError(logger, err, "failed to extract bundle")
				return nil
			}

			bundleDir := filepath.Join(targetDir, "keploy-debug-bundle")

			// Copy mocks and tests into the standard keploy directory structure
			keployDir := filepath.Join(targetDir, "keploy")
			if manifest.MockDir != "" {
				srcMocks := filepath.Join(bundleDir, manifest.MockDir)
				dstMocks := filepath.Join(keployDir, "test-set-0")
				if err := copyDir(srcMocks, dstMocks); err != nil {
					logger.Warn("failed to copy mocks", zap.Error(err))
				}
			}
			if manifest.TestDir != "" {
				srcTests := filepath.Join(bundleDir, manifest.TestDir)
				dstTests := filepath.Join(keployDir, "test-set-0", "tests")
				if err := copyDir(srcTests, dstTests); err != nil {
					logger.Warn("failed to copy tests", zap.Error(err))
				}
			}
			if manifest.ConfigFile != "" {
				srcCfg := filepath.Join(bundleDir, manifest.ConfigFile)
				data, err := os.ReadFile(srcCfg)
				if err == nil {
					os.WriteFile(filepath.Join(targetDir, "keploy.yml"), data, 0644)
				}
			}

			// Show the customer's capture analysis
			capturePath := filepath.Join(bundleDir, manifest.CaptureFile)
			fmt.Println("═══════════════════════════════════════════════════")
			fmt.Println("  Reproduction environment set up")
			fmt.Println("═══════════════════════════════════════════════════")
			fmt.Println()
			if manifest.Notes != "" {
				fmt.Printf("  Issue: %s\n", manifest.Notes)
			}
			fmt.Printf("  App:     %s\n", manifest.AppName)
			fmt.Printf("  Mode:    %s\n", manifest.Mode)
			fmt.Printf("  Dir:     %s\n", targetDir)
			fmt.Println()
			fmt.Println("  Customer's capture:")
			report, err := capture.Analyze(capturePath)
			if err == nil {
				for _, conn := range report.Connections {
					fmt.Printf("    - %s → %s (%d packets, %s)\n",
						conn.Protocol, conn.DstAddr, conn.PacketCount,
						formatSize(conn.ClientBytes+conn.ServerBytes))
				}
			}
			fmt.Println()
			fmt.Println("  Next steps:")
			fmt.Println("    1. Start the required services (databases, etc.)")
			fmt.Printf("    2. cd %s && keploy test -c \"<app-command>\" --debug --path .\n", targetDir)
			fmt.Printf("    3. keploy debug compare %s <your-new-capture.kpcap>\n", capturePath)
			fmt.Println("═══════════════════════════════════════════════════")
			return nil
		},
	}
	cmd.Flags().StringVar(&targetDir, "dir", "", "target directory for reproduction (default: repro)")
	return cmd
}

func copyDir(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		relPath, _ := filepath.Rel(src, path)
		targetPath := filepath.Join(dst, relPath)
		if info.IsDir() {
			return os.MkdirAll(targetPath, 0755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(targetPath, data, info.Mode())
	})
}

func formatSize(bytes int64) string {
	if bytes < 1024 {
		return fmt.Sprintf("%d B", bytes)
	}
	return fmt.Sprintf("%.1f KB", float64(bytes)/1024)
}

// findLatestCapture finds the most recently modified .kpcap file in the debug directory.
func findLatestCapture(basePath string) string {
	debugDir := filepath.Join(basePath, "keploy", "debug")
	entries, err := os.ReadDir(debugDir)
	if err != nil {
		return ""
	}

	var latest string
	var latestTime time.Time

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if filepath.Ext(entry.Name()) != ".kpcap" {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if info.ModTime().After(latestTime) {
			latestTime = info.ModTime()
			latest = filepath.Join(debugDir, entry.Name())
		}
	}

	return latest
}
