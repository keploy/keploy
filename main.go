// Package main is the entry point for the keploy application.
package main

import (
	"context"
    "encoding/json"
    "fmt"
    "io/ioutil"
    "net/http"
    "os"
    "strings"

	"go.keploy.io/server/v2/cli"
	"go.keploy.io/server/v2/cli/provider"
	"go.keploy.io/server/v2/config"
	userDb "go.keploy.io/server/v2/pkg/platform/yaml/configdb/user"

	"go.keploy.io/server/v2/utils"
	"go.keploy.io/server/v2/utils/log"
	//pprof for debugging
	//_ "net/http/pprof"
)

// version is the version of the server and will be injected during build by ldflags, same with dsn
// see https://goreleaser.com/customization/build/

var version string
var dsn string

const logo string = `
       ▓██▓▄
    ▓▓▓▓██▓█▓▄
     ████████▓▒
          ▀▓▓███▄      ▄▄   ▄               ▌
         ▄▌▌▓▓████▄    ██ ▓█▀  ▄▌▀▄  ▓▓▌▄   ▓█  ▄▌▓▓▌▄ ▌▌   ▓
       ▓█████████▌▓▓   ██▓█▄  ▓█▄▓▓ ▐█▌  ██ ▓█  █▌  ██  █▌ █▓
      ▓▓▓▓▀▀▀▀▓▓▓▓▓▓▌  ██  █▓  ▓▌▄▄ ▐█▓▄▓█▀ █▓█ ▀█▄▄█▀   █▓█
       ▓▌                           ▐█▌                   █▌
        ▓
`

func main() {

	// Uncomment the following code to enable pprof for debugging
	// go func() {
	// 	fmt.Println("Starting pprof server for debugging...")
	// 	err := http.ListenAndServe("localhost:6060", nil)
	// 	if err != nil {
	// 		fmt.Println("Failed to start the pprof server for debugging", err)
	// 		return
	// 	}
	// }()

	printLogo()
	ctx := utils.NewCtx()
	checkForUpdates()
	start(ctx)
}
type Release struct {
    TagName string `json:"tag_name"`
}
// ReadKeployConfig reads the .keploy file and returns its contents as a map.
func ReadKeployConfig() (map[string]string, error) {
	config := make(map[string]string)
	file, err := os.Open(os.Getenv("HOME") + "/.keploy")
	if err != nil {
		return config, err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		parts := strings.Split(line, "=")
		if len(parts) == 2 {
			config[parts[0]] = parts[1]
		}
	}

	return config, scanner.Err()
}

// WriteKeployConfig writes the given config map to the .keploy file.
func WriteKeployConfig(config map[string]string) error {
	file, err := os.Create(os.Getenv("HOME") + "/.keploy")
	if err != nil {
		return err
	}
	defer file.Close()

	writer := bufio.NewWriter(file)
	for key, value := range config {
		fmt.Fprintf(writer, "%s=%s\n", key, value)
	}

	return writer.Flush()
}
func getLatestRelease() (string, error) {
    url := "https://api.github.com/repos/keploy/keploy/releases/latest"
    resp, err := http.Get(url)
    if err != nil {
        return "", err
    }
    defer resp.Body.Close()

    if resp.StatusCode != http.StatusOK {
        return "", fmt.Errorf("failed to fetch latest release: %v", resp.Status)
    }

    body, err := ioutil.ReadAll(resp.Body)
    if err != nil {
        return "", err
    }

    var release Release
    err = json.Unmarshal(body, &release)
    if err != nil {
        return "", err
    }

    return release.TagName, nil
}

func promptUpdate(currentVersion, latestVersion string) bool {
    fmt.Printf("A new version of Keploy is available: %s (current version: %s)\n", latestVersion, currentVersion)
    fmt.Print("Do you want to update to the latest version? [Y/n]: ")

    var response string
    fmt.Scanln(&response)
    response = strings.ToLower(strings.TrimSpace(response))

    return response == "y" || response == "yes" || response == ""
}

func savePreference(updatePref string) error {
    homeDir, err := os.UserHomeDir()
    if err != nil {
        return err
    }

    keployFile := homeDir + "/.keploy"
    lines, err := ioutil.ReadFile(keployFile)
    if err != nil {
        return err
    }

    content := strings.Split(string(lines), "\n")
    for i, line := range content {
        if strings.HasPrefix(line, "update_pref=") {
            content[i] = "update_pref=" + updatePref
            break
        }
    }

    return ioutil.WriteFile(keployFile, []byte(strings.Join(content, "\n")), 0644)
}

func checkUpdatePreference() (bool, error) {
    homeDir, err := os.UserHomeDir()
    if err != nil {
        return false, err
    }

    keployFile := homeDir + "/.keploy"
    lines, err := ioutil.ReadFile(keployFile)
    if err != nil {
        return false, err
    }

    for _, line := range strings.Split(string(lines), "\n") {
        if strings.HasPrefix(line, "update_pref=") {
            return strings.TrimSpace(strings.Split(line, "=")[1]) == "no", nil
        }
    }

    return false, nil
}

func logWarning(latestVersion string) {
    fmt.Printf("Warning: A new version of Keploy is available: %s.\n", latestVersion)
    fmt.Println("To update, run: keploy update")
}
func checkForUpdates() {
    currentVersion := version // Using the version variable defined globally
    latestVersion, err := getLatestRelease()
    if err != nil {
        fmt.Printf("Error checking for latest release: %v\n", err)
        return
    }

    if latestVersion != "" && currentVersion != latestVersion {
        updatePref, err := checkUpdatePreference()
        if err != nil {
            fmt.Printf("Error checking update preference: %v\n", err)
            return
        }

        if !updatePref {
            if promptUpdate(currentVersion, latestVersion) {
                // Code to update Keploy
            } else {
                if err := savePreference("no"); err != nil {
                    fmt.Printf("Error saving update preference: %v\n", err)
                    return
                }
                logWarning(latestVersion)
            }
        } else {
            logWarning(latestVersion)
        }
    }
}
func printLogo() {
	if version == "" {
		version = "2-dev"
	}
	utils.Version = version
	if binaryToDocker := os.Getenv("BINARY_TO_DOCKER"); binaryToDocker != "true" {
		fmt.Println(logo, " ")
		fmt.Printf("version: %v\n\n", version)
	}
}

func start(ctx context.Context) {
	logger, err := log.New()
	if err != nil {
		fmt.Println("Failed to start the logger for the CLI", err)
		return
	}
	defer func() {
		if err := utils.DeleteFileIfNotExists(logger, "keploy-logs.txt"); err != nil {
			utils.LogError(logger, err, "Failed to delete Keploy Logs")
			return
		}
		if err := utils.DeleteFileIfNotExists(logger, "docker-compose-tmp.yaml"); err != nil {
			utils.LogError(logger, err, "Failed to delete Temporary Docker Compose")
			return
		}

	}()
	defer utils.Recover(logger)

	// The 'umask' command is commonly used in various operating systems to regulate the permissions of newly created files.
	// These 'umask' values subtract from the permissions assigned by the process, effectively lowering the permissions.
	// For example, if a file is created with permissions '777' and the 'umask' is '022', the resulting permissions will be '755',
	// reducing certain permissions for security purposes.
	// Setting 'umask' to '0' ensures that 'keploy' can precisely control the permissions of the files it creates.
	// However, it's important to note that this approach may not work in scenarios involving mounted volumes,
	// as the 'umask' is set by the host system, and cannot be overridden by 'keploy' or individual processes.
	oldMask := utils.SetUmask()
	defer utils.RestoreUmask(oldMask)

	userDb := userDb.New(logger)
	if dsn != "" {
		utils.SentryInit(logger, dsn)
		//logger = utils.ModifyToSentryLogger(ctx, logger, sentry.CurrentHub().Client(), configDb)
	}
	conf := config.New()

	svcProvider := provider.NewServiceProvider(logger, userDb, conf)
	cmdConfigurator := provider.NewCmdConfigurator(logger, conf)
	rootCmd := cli.Root(ctx, logger, svcProvider, cmdConfigurator)
	if err := rootCmd.Execute(); err != nil {
		if strings.HasPrefix(err.Error(), "unknown command") || strings.HasPrefix(err.Error(), "unknown shorthand") {
			fmt.Println("Error: ", err.Error())
			fmt.Println("Run 'keploy --help' for usage.")
			os.Exit(1)
		}
	}
}
